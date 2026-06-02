package anchorlearn

import (
	"context"
	"log"
	"math/rand"
	"strings"
	"sync"
	"time"

	"github.com/atharva-ag/devrouter/internal/telemetry"
)

// Probe is the abstraction the Learner uses to ask the codegraph layer
// "does <repo>/<path> exist with non-empty content?". The router
// satisfies this by wrapping codegraph.Client.ReadFile — kept as an
// interface so unit tests can fake it without spinning up a codegraph
// server.
type Probe interface {
	FileExists(ctx context.Context, repo, path string) bool
}

// Learner is the public type the router holds. It blends the four
// rollout phases:
//
//	Phase 1 — RecordObservation persists every anchor decision
//	Phase 2 — Decide consults observation-derived weights when scoring
//	Phase 3 — RewardMemorySave promotes agent-referenced files into
//	          the per-repo discovered pattern set
//	Phase 4 — Decide includes an ε-greedy exploration slot
//
// Phases 2-4 degrade gracefully on cold-start: with zero observation
// data the smoothing prior makes Decide score-equivalent to the
// static-list ordering, so day-one users see no behavioural difference
// from the v0 router.
type Learner struct {
	store    Store
	probe    Probe
	patterns []Pattern // current in-process snapshot of the static portfolio
	epsilon  float64

	rngMu  sync.Mutex
	rng    *rand.Rand
	recent *recentRing // lazy-initialised, see recentRing()
}

// New constructs a Learner from a Store + Probe pair. Pass nil for
// store to fall back to an in-memory MemStore (tests / no-Redis
// deployments). Pass nil for probe to disable file-existence
// validation, which is appropriate only in unit tests.
func New(store Store, probe Probe) *Learner {
	if store == nil {
		store = NewMemStore()
	}
	return &Learner{
		store:    store,
		probe:    probe,
		patterns: append([]Pattern{}, DefaultStaticPatterns...),
		epsilon:  DefaultEpsilon,
		rng:      rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

// SetEpsilon overrides the default exploration rate. Useful for the
// bench harness which sets ε=0 to make scoring deterministic across
// runs (the bench is the main place where reproducibility trumps
// long-term improvement).
func (l *Learner) SetEpsilon(e float64) { l.epsilon = e }

// Decide returns up to k anchor Candidates for the given query against
// repo. It runs the full pipeline:
//
//  1. Build candidate pool from static portfolio ∪ discovered patterns
//     (×) detected service tokens — the cartesian gives all
//     <svc>/<suffix> probes worth considering.
//  2. Probe each candidate via the codegraph /api/file abstraction
//     and discard ones whose file doesn't exist. This step is the
//     real work, the rest is bookkeeping.
//  3. Score each surviving candidate via score() blending global
//     prior, repo posterior, and keyword affinity.
//  4. Sort descending and apply ε-greedy exploration shuffle.
//  5. Truncate to k and return.
//
// Decide does NOT call RecordObservation — the router does that after
// it has materialised the anchors as PrimaryContext entries (so the
// observation reflects what the agent will actually see, not what the
// learner *proposed*).
func (l *Learner) Decide(
	ctx context.Context,
	repo string,
	query string,
	intent string,
	services []string,
	k int,
) []Candidate {
	if l == nil || k <= 0 || len(services) == 0 {
		return nil
	}

	keywords := extractKeywords(strings.ToLower(query))

	// Build pool: static + discovered, expanded across each service.
	patterns := append([]Pattern{}, l.patterns...)
	if discovered, _ := l.store.ListDiscovered(ctx, repo); len(discovered) > 0 {
		for _, suf := range discovered {
			patterns = append(patterns, Pattern{
				ID:     suf,
				Suffix: suf,
				Source: "discovered",
				// Discovered patterns inherit no static keyword
				// affinities — that's what the per-(kw, pattern)
				// affinity table is for.
			})
		}
	}

	pool := make([]Candidate, 0, len(patterns)*len(services))
	for _, svc := range services {
		for _, p := range patterns {
			path := svc + "/" + p.Suffix
			pool = append(pool, Candidate{
				Service: svc,
				Path:    path,
				Pattern: p,
			})
		}
	}

	// Probe in parallel: each FileExists is an independent codegraph
	// /api/file roundtrip. Scoring is local (no I/O) so we keep it
	// in the merge phase, not the fan-out, to avoid mutating shared
	// stats reads from goroutines.
	exists := make([]bool, len(pool))
	if l.probe != nil {
		probeFanOut := defaultProbeFanOut
		runParallel(probeFanOut, len(pool), func(i int) {
			exists[i] = l.probe.FileExists(ctx, repo, pool[i].Path)
		})
	}

	scored := make([]Candidate, 0, len(pool))
	for i, c := range pool {
		if l.probe != nil && !exists[i] {
			continue
		}
		// Pull stats — best-effort. Absent stats yield smoothed
		// rate 0.5 which is the right cold-start default.
		globalStats, _ := l.store.GetPatternStats(ctx, "", c.Pattern.ID)
		repoStats, _ := l.store.GetPatternStats(ctx, repo, c.Pattern.ID)
		// Keyword affinity: sum across query keywords × this
		// pattern; capped inside score().
		var kwScore float64
		for _, kw := range keywords {
			a, _ := l.store.GetKeywordAffinity(ctx, kw, c.Pattern.ID)
			kwScore += a
		}
		// Static-portfolio keyword affinity is folded into the
		// prior here (vs only the learned table) so the cold-start
		// behaviour matches today's rankSuffixesByQuery.
		for _, kw := range c.Pattern.Keywords {
			if containsToken(keywords, kw) {
				kwScore += 0.5
			}
		}
		c.Score = score(globalStats, repoStats, kwScore)
		scored = append(scored, c)
	}

	rankByScore(scored)

	// ε-greedy: replace the worst slot in the top-k with an
	// unproven exploration probe. Skipped in deterministic mode
	// (epsilon=0).
	if l.epsilon > 0 {
		l.rngMu.Lock()
		ranked := epsilonGreedyShuffle(ctx, l.store, repo, scored, pool, k, l.epsilon, l.rng)
		l.rngMu.Unlock()
		scored = ranked
	}

	if len(scored) > k {
		scored = scored[:k]
	}
	return scored
}

// HasDiscovered returns true when the per-repo discovered set has
// at least one entry. Used by the router to decide whether the
// "service-token-only, no verb" fallback gate should fire — that
// path is intentionally skipped on cold-start (no learning yet, so
// firing static patterns would be over-firing) but starts paying
// off once Phase 3 has promoted the agent's first repo-specific
// suffixes.
func (l *Learner) HasDiscovered(ctx context.Context, repo string) bool {
	if l == nil {
		return false
	}
	xs, err := l.store.ListDiscovered(ctx, repo)
	if err != nil {
		return false
	}
	return len(xs) > 0
}

// DecideDiscoveredOnly is the post-learning fallback path: same
// scoring + probing as Decide, but the pattern pool is restricted
// to the per-repo *discovered* set. Used when the query has a
// service token but no service-trace verb — a shape that is too
// generic to risk firing the full static portfolio against, but
// that *is* worth firing learned, repo-specific patterns against.
//
// Returns an empty slice when the discovered set is empty (the
// router gates on HasDiscovered before calling this, so that path
// is unreachable in practice but kept defensive).
func (l *Learner) DecideDiscoveredOnly(
	ctx context.Context,
	repo string,
	query string,
	intent string,
	services []string,
	k int,
) []Candidate {
	if l == nil || k <= 0 || len(services) == 0 {
		return nil
	}
	discovered, _ := l.store.ListDiscovered(ctx, repo)
	if len(discovered) == 0 {
		return nil
	}
	keywords := extractKeywords(strings.ToLower(query))

	patterns := make([]Pattern, 0, len(discovered))
	for _, suf := range discovered {
		patterns = append(patterns, Pattern{
			ID:     suf,
			Suffix: suf,
			Source: "discovered",
		})
	}

	pool := make([]Candidate, 0, len(patterns)*len(services))
	for _, svc := range services {
		for _, p := range patterns {
			pool = append(pool, Candidate{
				Service: svc,
				Path:    svc + "/" + p.Suffix,
				Pattern: p,
			})
		}
	}

	exists := make([]bool, len(pool))
	if l.probe != nil {
		runParallel(defaultProbeFanOut, len(pool), func(i int) {
			exists[i] = l.probe.FileExists(ctx, repo, pool[i].Path)
		})
	}

	scored := make([]Candidate, 0, len(pool))
	for i, c := range pool {
		if l.probe != nil && !exists[i] {
			continue
		}
		globalStats, _ := l.store.GetPatternStats(ctx, "", c.Pattern.ID)
		repoStats, _ := l.store.GetPatternStats(ctx, repo, c.Pattern.ID)
		var kwScore float64
		for _, kw := range keywords {
			a, _ := l.store.GetKeywordAffinity(ctx, kw, c.Pattern.ID)
			kwScore += a
		}
		c.Score = score(globalStats, repoStats, kwScore)
		scored = append(scored, c)
	}

	rankByScore(scored)
	if len(scored) > k {
		scored = scored[:k]
	}
	return scored
}

// RecordObservation persists the anchors we actually injected for
// queryID. Called by the router after Decide -> probe -> materialise.
// Increments per-pattern fire counts so smoothed rates have a
// denominator regardless of whether reward signals ever arrive.
func (l *Learner) RecordObservation(
	ctx context.Context,
	queryID, repo, query, intent string,
	candidates []Candidate,
) {
	if l == nil || queryID == "" || len(candidates) == 0 {
		return
	}
	files := make([]string, 0, len(candidates))
	patternIDs := make([]string, 0, len(candidates))
	services := make(map[string]bool, len(candidates))
	for _, c := range candidates {
		files = append(files, c.Path)
		patternIDs = append(patternIDs, c.Pattern.ID)
		services[c.Service] = true
		_ = l.store.IncPatternFire(ctx, "", c.Pattern.ID)
		_ = l.store.IncPatternFire(ctx, repo, c.Pattern.ID)
	}
	svcList := make([]string, 0, len(services))
	for s := range services {
		svcList = append(svcList, s)
	}
	obs := Observation{
		QueryID:    queryID,
		Repo:       repo,
		Query:      query,
		Intent:     intent,
		Files:      files,
		PatternIDs: patternIDs,
		Services:   svcList,
		Timestamp:  time.Now(),
	}
	if err := l.store.PutObservation(ctx, obs); err != nil {
		log.Printf("[anchorlearn] PutObservation: %v", err)
	}
}

// RewardFeedback handles the explicit dev_feedback path. We treat the
// caller's success flag as the primary signal and the file_paths
// argument as the precise attribution channel: any anchored file that
// also appears in additionalFiles gets credited; the rest neither
// gain nor lose, which keeps learning monotonically positive on
// explicit signal (the bandit can demote via under-firing only).
func (l *Learner) RewardFeedback(
	ctx context.Context,
	queryID string,
	additionalFiles []string,
	success bool,
) {
	if l == nil || queryID == "" {
		return
	}
	obs, err := l.store.GetObservation(ctx, queryID)
	if err != nil || obs == nil {
		return
	}
	addSet := make(map[string]bool, len(additionalFiles))
	for _, f := range additionalFiles {
		addSet[f] = true
	}
	weight := 0.5
	if success {
		weight = 1.0
	}
	keywords := extractKeywords(strings.ToLower(obs.Query))
	for i, file := range obs.Files {
		if !addSet[file] {
			continue
		}
		patternID := ""
		if i < len(obs.PatternIDs) {
			patternID = obs.PatternIDs[i]
		}
		if patternID == "" {
			continue
		}
		_ = l.store.IncPatternSuccess(ctx, "", patternID, weight)
		_ = l.store.IncPatternSuccess(ctx, obs.Repo, patternID, weight)
		for _, kw := range keywords {
			_ = l.store.IncKeywordAffinity(ctx, kw, patternID, weight)
		}
		telemetry.AnchorObservations.WithLabelValues("rewarded").Inc()
		log.Printf("[anchorlearn] feedback: rewarded pattern=%s repo=%s w=%.2f file=%s",
			patternID, obs.Repo, weight, file)
	}
}

// RewardMemorySave handles the implicit "agent saved a memory
// referencing this file" signal. Drives Phase 3 discovery as well as
// Phase 4 reward update:
//
//   - If the saved file matches an anchor we recently injected, treat
//     it the same as a positive dev_feedback (smaller weight because
//     the signal is implicit).
//   - If the saved file lives under <svc>/ where svc was a service
//     token we recently anchored against, but is NOT itself in the
//     static portfolio's set of <svc>/<suffix> paths, promote its
//     suffix into the discovered set so future Decide calls include
//     it as a candidate.
//
// Discovery is bounded: we only look back over the last few minutes
// of observations (the dev_feedback trace TTL is 30d but the implicit
// signal is much noisier, so we apply a tighter join window).
func (l *Learner) RewardMemorySave(ctx context.Context, repo, file string) {
	if l == nil || file == "" {
		return
	}
	// Walk the index of recent observations for this repo. The Store
	// interface intentionally doesn't expose a "list recent" call —
	// we use the Learner's per-process recentObs cache (see
	// NoteRecentQuery / recordRecent below) to avoid scanning Redis.
	matches := l.snapshotRecent(repo)
	for _, obs := range matches {
		l.attributeMemorySave(ctx, obs, file)
	}
}

// attributeMemorySave does the per-observation reward / discovery
// work. Split out so the snapshotRecent loop stays readable.
func (l *Learner) attributeMemorySave(ctx context.Context, obs Observation, file string) {
	const memorySaveWeight = 0.5
	keywords := extractKeywords(strings.ToLower(obs.Query))

	// 1. Direct anchor hit?
	for i, anchored := range obs.Files {
		if anchored != file {
			continue
		}
		patternID := ""
		if i < len(obs.PatternIDs) {
			patternID = obs.PatternIDs[i]
		}
		if patternID == "" {
			continue
		}
		_ = l.store.IncPatternSuccess(ctx, "", patternID, memorySaveWeight)
		_ = l.store.IncPatternSuccess(ctx, obs.Repo, patternID, memorySaveWeight)
		for _, kw := range keywords {
			_ = l.store.IncKeywordAffinity(ctx, kw, patternID, memorySaveWeight)
		}
		telemetry.AnchorObservations.WithLabelValues("rewarded").Inc()
		log.Printf("[anchorlearn] memory_save: rewarded pattern=%s repo=%s file=%s",
			patternID, obs.Repo, file)
		return
	}

	// 2. Discovery: file is under a service we anchored against.
	for _, svc := range obs.Services {
		prefix := svc + "/"
		if !strings.HasPrefix(file, prefix) {
			continue
		}
		suffix := file[len(prefix):]
		if suffix == "" || strings.Contains(suffix, "..") {
			continue
		}
		// Skip if this suffix is already a static or discovered
		// pattern — no need to re-promote.
		if l.isKnownSuffix(ctx, obs.Repo, suffix) {
			return
		}
		if err := l.store.AddDiscovered(ctx, obs.Repo, suffix); err != nil {
			log.Printf("[anchorlearn] AddDiscovered: %v", err)
			return
		}
		// Seed the discovered pattern with one fire+success so it
		// outranks an unproven static pattern on next Decide.
		_ = l.store.IncPatternFire(ctx, obs.Repo, suffix)
		_ = l.store.IncPatternSuccess(ctx, obs.Repo, suffix, memorySaveWeight)
		for _, kw := range keywords {
			_ = l.store.IncKeywordAffinity(ctx, kw, suffix, memorySaveWeight)
		}
		telemetry.AnchorObservations.WithLabelValues("discovered").Inc()
		log.Printf("[anchorlearn] discovered new pattern repo=%s suffix=%s", obs.Repo, suffix)
		return
	}
}

func (l *Learner) isKnownSuffix(ctx context.Context, repo, suffix string) bool {
	for _, p := range l.patterns {
		if p.Suffix == suffix {
			return true
		}
	}
	if discovered, _ := l.store.ListDiscovered(ctx, repo); len(discovered) > 0 {
		for _, d := range discovered {
			if d == suffix {
				return true
			}
		}
	}
	return false
}

// ---------------------------------------------------------------------
// Recent-observation cache
// ---------------------------------------------------------------------
//
// RewardMemorySave needs to find "the few observations that just
// happened for this repo". Rather than scan Redis (linear in TTL'd
// keys, blows up at scale), we keep a small per-repo ring of
// QueryIDs and load each from the Store on demand. Lifetime is
// process-local so a router restart resets it; that's fine because
// the dev_feedback path doesn't depend on this cache.

const recentObsRingSize = 32

func (l *Learner) recentRing() *recentRing {
	l.rngMu.Lock()
	defer l.rngMu.Unlock()
	if l.recent == nil {
		l.recent = newRecentRing(recentObsRingSize)
	}
	return l.recent
}

// NoteRecentQuery is called by the router right after RecordObservation
// to register queryID in the per-repo recent ring. RewardMemorySave
// scans this ring on memory-save events.
func (l *Learner) NoteRecentQuery(repo, queryID string) {
	if l == nil || queryID == "" {
		return
	}
	l.recentRing().push(repo, queryID)
}

func (l *Learner) snapshotRecent(repo string) []Observation {
	ring := l.recentRing()
	ids := ring.snapshot(repo)
	out := make([]Observation, 0, len(ids))
	for _, id := range ids {
		obs, err := l.store.GetObservation(context.Background(), id)
		if err != nil || obs == nil {
			continue
		}
		out = append(out, *obs)
	}
	return out
}

type recentRing struct {
	mu    sync.Mutex
	cap   int
	byRepo map[string][]string
}

func newRecentRing(cap int) *recentRing {
	return &recentRing{cap: cap, byRepo: make(map[string][]string)}
}

func (r *recentRing) push(repo, queryID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	xs := r.byRepo[repo]
	if len(xs) >= r.cap {
		xs = xs[1:]
	}
	r.byRepo[repo] = append(xs, queryID)
}

func (r *recentRing) snapshot(repo string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	xs := r.byRepo[repo]
	out := make([]string, len(xs))
	copy(out, xs)
	return out
}

// containsToken reports whether tok appears in tokens (case-insensitive
// against an already-lowercased haystack — extractKeywords lowercases
// for us).
func containsToken(tokens []string, tok string) bool {
	tok = strings.ToLower(tok)
	for _, t := range tokens {
		if t == tok {
			return true
		}
	}
	return false
}
