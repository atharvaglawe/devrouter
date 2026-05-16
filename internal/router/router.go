package router

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/atharva-ag/devrouter/internal/anchorlearn"
	"github.com/atharva-ag/devrouter/internal/codegraph"
	"github.com/atharva-ag/devrouter/internal/heuristics"
	"github.com/atharva-ag/devrouter/internal/memory"
	"github.com/atharva-ag/devrouter/internal/prompt"
)

type Router struct {
	Graph      *codegraph.Client
	Memory     *memory.Store
	Heuristics *heuristics.Picker
	LastCalls  *heuristics.LastCallLRU // last-N dev_context calls for dev_feedback fallback

	// AnchorLearner is the self-improving service-entry-point bandit.
	// Replaces the v1 hardcoded suffix list with a static-prior +
	// per-repo posterior + ε-greedy exploration policy. Backing store
	// is Redis when memory.Store is wired, an in-process MemStore
	// otherwise — the latter keeps bench/local runs deterministic
	// when no Redis is configured.
	AnchorLearner *anchorlearn.Learner

	// dirsCache memoises codegraph.TopLevelDirs(repo) per-repo so that
	// the per-request service-token gate (used by anchor injection) is
	// in-memory after the first call rather than a Cypher round trip
	// per query. The dir list is small (≤ a few hundred entries even
	// for very large monorepos) and stable across queries.
	dirsCacheMu sync.Mutex
	dirsCache   map[string][]string
}

// codegraphProbe satisfies anchorlearn.Probe by delegating to
// codegraph.Client.ReadFile. Centralised here so the learner package
// stays free of codegraph import dependencies.
type codegraphProbe struct{ g *codegraph.Client }

func (p *codegraphProbe) FileExists(_ context.Context, repo, path string) bool {
	if p == nil || p.g == nil {
		return false
	}
	fr, err := p.g.ReadFile(path, repo)
	return err == nil && fr != nil && fr.Content != ""
}

func New(graph *codegraph.Client, mem *memory.Store) *Router {
	r := &Router{
		Graph:     graph,
		Memory:    mem,
		dirsCache: make(map[string][]string),
	}
	if mem != nil {
		r.Heuristics = heuristics.NewPicker(mem.RDB())
	}
	r.LastCalls = heuristics.NewLastCallLRU(16, 10*time.Minute)

	// Anchor learner. Prefer Redis-backed when a memory store is
	// available so observations + weights survive process restarts;
	// fall back to in-memory when running standalone (bench, tests).
	var store anchorlearn.Store
	if mem != nil {
		store = anchorlearn.NewRedisStore(mem.RDB())
	}
	r.AnchorLearner = anchorlearn.New(store, &codegraphProbe{g: graph})
	// Bench harness sets DEVROUTER_ANCHOR_EPSILON=0 to force
	// deterministic anchor ordering for reproducible R@K. Production
	// uses the package default (10% exploration).
	if v := os.Getenv("DEVROUTER_ANCHOR_EPSILON"); v != "" {
		if e, err := strconv.ParseFloat(v, 64); err == nil && e >= 0 && e <= 1 {
			r.AnchorLearner.SetEpsilon(e)
		}
	}

	return r
}

// QueryPlan is the structured retrieval plan that drives router scoring.
// It is supplied by the MCP caller via dev_context's optional `plan`
// argument — the agent (Claude/Cursor/etc.) is responsible for producing
// it, since it has the conversation context that bare-query keyword
// extraction in a sidecar LLM never had. All fields are advisory: the
// router merges them with deterministic tokenization rather than
// replacing it, and an empty/missing plan still produces a useful
// retrieval via auto-anchoring (see ensureMustAnchor).
//
// Semantics:
//
//	MustTerms    — file-level filter. Results are kept only if they live
//	               in a file where ANY must term appears (in name, path,
//	               or content). Keep this list small (1–2 anchors); too
//	               many tokens collapses recall.
//	ShouldTerms  — synonyms / expansions of query intent. Used as extra
//	               vocabulary in name/content search and IDF-weighted in
//	               ranking. Do NOT duplicate morphological variants here
//	               (the tokenizer already stems).
//	ExcludeTerms — drop or heavily penalise matches associated with
//	               these tokens. Targeted rules (test/mock convention)
//	               rather than substring contains, so "test" doesn't
//	               nuke "requestSettings".
//	Phrases      — multi-word strings (kept verbatim) for future content
//	               scoring. Currently logged but not used in retrieval.
//	ContextHints — soft path bias. Results whose filePath contains any
//	               hint get a score multiplier; hints are NEVER a hard
//	               filter (so a wrong hint can't blackhole the query).
type QueryPlan struct {
	MustTerms    []string
	ShouldTerms  []string
	ExcludeTerms []string
	Phrases      []string
	ContextHints []string

	// MustAutoAnchored is true when MustTerms was synthesised by
	// ensureMustAnchor (rarest query token in the graph) rather than
	// supplied by the caller's plan field. Filters that want to be
	// conservative about an auto-anchor — notably the memory-side
	// must-gate — branch on this flag.
	//
	// Why: the auto-anchor picks the rarest token in the codebase
	// (good for narrowing codegraph search noise), which is often a
	// generic English word like "user" or "order" rather than a
	// semantically central CamelCase symbol. Hard-gating an
	// agent-saved memory on that token routinely drops on-topic
	// memories whose file path doesn't happen to contain the word.
	// See filterMemoriesByPlan for how the gate is relaxed in this case.
	MustAutoAnchored bool
}

// graphBudget controls how much graph traversal to perform based on
// memory strength and query intent.
type graphBudget struct {
	maxTrace     int
	callerHops   int // 1 = direct callers only, 2 = also grandparent chain
	fetchCallees bool
	fetchExtends bool
	fetchMethods bool
	fetchImports bool
}

// graphBudgetFromProfile derives the runtime graph budget from a Profile
// (numeric knobs from the heuristics package) layered with intent-driven
// boolean fetch flags and the strong-memory shrink rules.
//
// The numeric knobs (maxTrace, callerHops) come from the Profile, which
// is the bandit-tunable surface. The boolean fetch flags (fetchCallees,
// fetchExtends, fetchMethods, fetchImports) stay as intent-driven defaults
// because turning them off entirely is a coarser decision than the bandit
// is ready to make in v1.
func graphBudgetFromProfile(p heuristics.Profile, memCount int, intent Intent) graphBudget {
	p = p.ApplyMemoryShrink(memCount)
	gb := graphBudget{
		maxTrace:     p.MaxTrace,
		callerHops:   p.CallerHops,
		fetchCallees: true,
		fetchExtends: true,
		fetchMethods: true,
		fetchImports: true,
	}

	// Boolean fetch flag overrides — mirror the previous strong-memory
	// shrink behaviour for booleans only (numeric shrink already applied
	// by ApplyMemoryShrink above).
	if memCount >= 3 {
		gb.fetchExtends = false
		gb.fetchMethods = false
		gb.fetchImports = false
	} else if memCount >= 2 {
		gb.fetchExtends = false
		gb.fetchMethods = false
	}

	// Intent-driven fetch flag overrides
	switch intent {
	case IntentTrace:
		gb.fetchImports = true
	case IntentRefactor:
		gb.fetchImports = true
		gb.fetchMethods = true
		gb.fetchExtends = true
	}

	return gb
}

// buildDecisionContext converts DecisionMemoryHit objects to DecisionContextEntry for JSON output.
// All decisions are agent-written with fixed 0.9 confidence (no staleness tracking).
func buildDecisionContext(decisions []prompt.DecisionMemoryHit) []prompt.DecisionContextEntry {
	if len(decisions) == 0 {
		return nil
	}
	out := make([]prompt.DecisionContextEntry, 0, len(decisions))
	for _, d := range decisions {
		out = append(out, prompt.DecisionContextEntry{
			Name:         d.Name,
			DecisionType: d.DecisionType,
			Decision:     d.Decision,
			Rationale:    d.Rationale,
			Alternatives: d.Alternatives,
			Constraint:   d.Constraint,
			Scope:        d.Scope,
			Status:       d.Status,
			Supersedes:   d.Supersedes,
			SupersededBy: d.SupersededBy,
			Confidence:   0.9,
		})
	}
	return out
}

// buildDecisionInstruction creates type-aware instructions based on decision types present.
// Returns a multi-line string that should be prepended to the main instructions.
// Only considers active decisions when building instructions.
func buildDecisionInstruction(decisions []prompt.DecisionMemoryHit) string {
	typesSeen := make(map[string]bool)
	for _, d := range decisions {
		// Skip superseded decisions when building instructions
		if d.Status == "superseded" {
			continue
		}
		typesSeen[d.DecisionType] = true
	}

	var lines []string
	if typesSeen["refactor"] {
		lines = append(lines, "REFACTOR DECISIONS: Before refactoring, review the existing decisions below — some refactors were explicitly rejected.")
	}
	if typesSeen["optimization"] {
		lines = append(lines, "OPTIMIZATION DECISIONS: Performance targets and prior optimization decisions are recorded below.")
	}
	if typesSeen["coding_standard"] {
		lines = append(lines, "CODING STANDARDS: Coding standards are in effect for this area — apply them to any new code.")
	}
	if typesSeen["architecture"] {
		lines = append(lines, "ARCHITECTURE DECISIONS: Architectural choices have been recorded — do not contradict them without explicit discussion.")
	}
	if typesSeen["constraint"] {
		lines = append(lines, "CONSTRAINTS: Hard constraints limit design choices in this area — see decisions below.")
	}
	if typesSeen["tradeoff"] {
		lines = append(lines, "TRADEOFF DECISIONS: Tradeoffs were deliberately made — understand them before suggesting alternatives.")
	}
	return strings.Join(lines, "\n")
}

// diagnosePlannerEmpty returns diagnostic info about why planner returned empty.
func diagnosePlannerEmpty(query string) []string {
	diagnostics := []string{}

	// Check query characteristics
	words := strings.Fields(query)
	if len(words) == 0 {
		diagnostics = append(diagnostics, "query is empty")
		return diagnostics
	}

	diagnostics = append(diagnostics, fmt.Sprintf("query words: %d", len(words)))
	diagnostics = append(diagnostics, fmt.Sprintf("query length: %d chars", len(query)))

	// Check if query is too generic
	genericTerms := map[string]bool{
		"how":   true,
		"what":  true,
		"where": true,
		"when":  true,
		"why":   true,
		"does":  true,
		"is":    true,
		"work":  true,
		"the":   true,
		"a":     true,
		"an":    true,
		"and":   true,
		"or":    true,
	}

	contentWords := 0
	for _, w := range words {
		w = strings.ToLower(w)
		if !genericTerms[w] && len(w) > 2 {
			contentWords++
		}
	}

	if contentWords == 0 {
		diagnostics = append(diagnostics, "REASON: query contains only generic/short words")
	} else {
		diagnostics = append(diagnostics, fmt.Sprintf("content words: %d (good)", contentWords))
	}

	return diagnostics
}

// recordStage records timing and metadata for a retrieval pipeline stage.
// Call at the beginning of a stage with the stage name, then call the returned
// function when the stage completes to record latency.
func recordStage(trace *prompt.RetrievalTrace, stageName string) func(*prompt.StageTrace) {
	startTime := time.Now()
	return func(st *prompt.StageTrace) {
		st.LatencyMs = time.Since(startTime).Milliseconds()
		switch stageName {
		case "planner":
			trace.PlannerStage = st
		case "search":
			trace.SearchStage = st
		case "graph":
			trace.GraphStage = st
		case "rerank":
			trace.RerankStage = st
		case "packing":
			trace.PackingStage = st
		}
	}
}

// HandleQuery runs the full context-assembly pipeline:
//  1. Intent — classify the query type
//  2. Memory — two-phase retrieval (agent mems for prompt, auto hints for graph seeding)
//  3. Search — find symbols matching the query (scoped to package path if detected)
//  4. Graph — budgeted traversal (depth/breadth controlled by memory strength + intent)
//  5. Merge — build unified ContextNodes grouping memories + graph data by file
//  6. Trim — adaptive caps based on intent
// HandleQuery is the back-compat entry point: no caller-supplied plan,
// retrieval falls back to deterministic tokenization + auto-anchoring.
// New callers should use HandleQueryWithPlan and pass the structured
// plan they produced (the MCP `dev_context` tool plumbs it through).
func (r *Router) HandleQuery(query, repo string) (prompt.DevPrompt, error) {
	return r.HandleQueryWithPlan(query, repo, nil)
}

// HandleQueryWithPlan runs the retrieval pipeline. If providedPlan is
// non-nil it is sanitized (SanitizePlan) and used as-is; if nil the
// router relies on auto-anchoring from the rarest query token to
// guarantee a hard anchor without any LLM call.
func (r *Router) HandleQueryWithPlan(query, repo string, providedPlan *QueryPlan) (prompt.DevPrompt, error) {
	queryStartTime := time.Now()
	ctx := context.Background()

	// Initialize retrieval trace
	trace := &prompt.RetrievalTrace{
		QueryID:   uuid.New().String(),
		Query:     query,
		Timestamp: time.Now().UnixMilli(),
		Signals:   make(map[string]float64),
	}

	// --- INTENT ---
	intent := DetectIntent(query)
	trace.Intent = string(intent)
	trace.Repo = repo

	repoPath := r.Graph.RepoPath(repo)
	branch := memory.CurrentBranch(repoPath)

	// --- EMBED ONCE ---
	// SearchAll, topic resolution, and repeat detection all need the
	// same query embedding. Embed synchronously first so we pay nomic
	// once per query and feed the result into all three downstream
	// paths. On embed failure we degrade: SearchAll falls back to
	// its own retry, topic resolve returns IntentGlobalTopic, and
	// repeat detection is skipped.
	var queryEmbed []float32
	if emb, err := memory.Embed(query); err == nil {
		queryEmbed = emb
	} else {
		log.Printf("[router] embed error (non-fatal, falling back): %v", err)
	}

	// --- TOPIC RESOLVE ---
	// Always resolved (even when below the bandit floor) so the trace
	// records which bucket the query landed in — that's how
	// dev_feedback / repeat-detection later route the reward to the
	// right place to drive bucket warm-up.
	topicID := heuristics.IntentGlobalTopic
	var topicLabel string
	if r.Heuristics != nil && r.Heuristics.Topics != nil && len(queryEmbed) > 0 {
		topicID, topicLabel = r.Heuristics.Topics.Resolve(ctx, string(intent), repo, queryEmbed, query)
	}
	trace.TopicID = topicID
	trace.TopicLabel = topicLabel

	// --- HEURISTIC PROFILE ---
	// PickWithTopic returns the bucket's profile when the bucket is
	// hot enough (>= TopicBanditSampleFloor centroid samples);
	// otherwise it serves the intent-global profile so a cold bucket
	// behaves exactly like today's pre-topic pipeline.
	var (
		profileID string
		profile   heuristics.Profile
		fromTopic bool
	)
	if r.Heuristics != nil {
		profileID, profile, fromTopic = r.Heuristics.PickWithTopic(string(intent), repo, topicID)
	} else {
		profile = heuristics.Default(string(intent))
		profileID = profile.ID()
	}
	trace.HeuristicProfileID = profileID
	trace.HeuristicFromTopic = fromTopic

	// --- MEMORY + REPEAT DETECTION (run in parallel) ---
	//
	// Two parallel branches with no data dependency:
	//   - SearchAllWithEmbed reuses the embedding we already computed
	//   - Repeat detection cosines that same vector vs recent_queries:{repo}
	//
	// The query plan is supplied by the MCP caller in the dev_context
	// arguments — see HandleQueryWithPlan.
	var (
		rawHits            []memory.MemoryHit
		searchLatency      int64
		plan               QueryPlan
		planSource         string // "agent" if provided via dev_context, "auto" otherwise
		plannerEmptyReason string
		planLatency        int64
		repeatHit          heuristics.RepeatHit
		wg                 sync.WaitGroup
	)

	// Apply the caller's plan synchronously. SanitizePlan is microsecond-
	// scale (string lowercase + dedupe), so there's nothing to gain from
	// shoving it into a goroutine.
	planStart := time.Now()
	if providedPlan != nil {
		plan = SanitizePlan(*providedPlan)
		planSource = "agent"
		if planIsEmpty(plan) {
			plannerEmptyReason = "agent supplied empty plan"
		}
	} else {
		planSource = "auto"
		plannerEmptyReason = "no plan supplied (auto-anchor only)"
	}
	planLatency = time.Since(planStart).Milliseconds()

	wg.Add(1)
	go func() {
		defer wg.Done()
		t0 := time.Now()
		if len(queryEmbed) > 0 {
			rawHits = r.Memory.SearchAllWithEmbed(queryEmbed, repo, branch)
		} else {
			rawHits = r.Memory.SearchAll(query, repo, branch)
		}
		searchLatency = time.Since(t0).Milliseconds()
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		if r.Heuristics == nil || repo == "" || len(queryEmbed) == 0 {
			return
		}
		hit, err := r.Heuristics.Store.DetectRepeat(context.Background(), repo, queryEmbed)
		if err != nil {
			log.Printf("[router] repeat detect error (non-fatal): %v", err)
			return
		}
		repeatHit = hit
	}()

	wg.Wait()

	// --- HANDLE REPEAT-EXPLORATION FALLOUT ---
	// If the current query is similar enough to a recent prior query,
	// retroactively penalise that prior query's profile and stamp this
	// trace so retrieve_debug surfaces the chain. Reward routing is
	// per-(intent, repo, topic): the bucket whose profile served the
	// PRIOR query takes the hit, so a topic that consistently triggers
	// repeats sees its own bandit signal drop while unrelated topics
	// stay untouched.
	if r.Heuristics != nil && repeatHit.Sim >= heuristics.RepeatSimThreshold {
		raw, fired := heuristics.ComputeImplicit(repeatHit.Sim)
		if fired {
			trace.RepeatedExplorationOf = repeatHit.PrevQueryID
			rollingMean := r.Heuristics.Store.RollingMeanFor(ctx,
				repeatHit.PrevIntent, repeatHit.PrevRepo, repeatHit.PrevTopicID, 50)
			adjusted := raw - rollingMean
			row := heuristics.RewardRow{
				QueryID:        repeatHit.PrevQueryID,
				ProfileID:      repeatHit.PrevProfileID,
				RawReward:      raw,
				AdjustedReward: adjusted,
				Source:         "implicit_repeat",
				Weight:         heuristics.ImplicitWeight,
				Timestamp:      time.Now().UnixMilli(),
			}
			_ = r.Heuristics.Store.AppendRewardFor(ctx,
				repeatHit.PrevIntent, repeatHit.PrevRepo, repeatHit.PrevTopicID, row)
			_ = r.Heuristics.Store.PatchTrace(ctx, repeatHit.PrevQueryID, map[string]interface{}{
				"repeat_query":    "true",
				"raw_reward":      fmt.Sprintf("%g", raw),
				"adjusted_reward": fmt.Sprintf("%g", adjusted),
				"feedback_source": "implicit_repeat",
				"feedback_at":     fmt.Sprintf("%d", row.Timestamp),
			})
			r.Heuristics.UpdateWithTopic(repeatHit.PrevIntent, repeatHit.PrevRepo, repeatHit.PrevTopicID,
				repeatHit.PrevProfileID, adjusted, heuristics.ImplicitWeight)
			log.Printf("[heuristics] implicit_repeat: prev=%s sim=%.3f raw=%.2f adjusted=%.2f bucket=%s/%s",
				repeatHit.PrevQueryID, repeatHit.Sim, raw, adjusted,
				repeatHit.PrevRepo, repeatHit.PrevTopicID)
		}
	}

	// Always record the current query so the next dev_context can detect
	// repeats against it. Repo + TopicID are denormalised onto the entry
	// so the next repeat lookup can immediately route the reward without
	// having to re-load the prior query's trace.
	if r.Heuristics != nil && repo != "" && len(queryEmbed) > 0 {
		_ = r.Heuristics.Store.RecordQuery(context.Background(), repo, heuristics.RecentQueryEntry{
			QueryID:   trace.QueryID,
			Intent:    string(intent),
			ProfileID: profileID,
			Repo:      repo,
			TopicID:   topicID,
			Embedding: queryEmbed,
		})
	}

	trace.SearchStage = &prompt.StageTrace{
		LatencyMs:     searchLatency,
		CandidatesOut: len(rawHits),
		Details: map[string]interface{}{
			"branch": branch,
		},
	}

	trace.PlannerStage = &prompt.StageTrace{
		LatencyMs:   planLatency,
		Description: fmt.Sprintf("must=%v should=%v exclude=%v", plan.MustTerms, plan.ShouldTerms, plan.ExcludeTerms),
		Details: map[string]interface{}{
			"query":         query,
			"plan_source":   planSource,
			"must_count":    len(plan.MustTerms),
			"should_count":  len(plan.ShouldTerms),
			"exclude_count": len(plan.ExcludeTerms),
			"plan_phrases":  len(plan.Phrases),
			"plan_hints":    len(plan.ContextHints),
		},
		Warnings: []string{},
	}

	if plannerEmptyReason != "" {
		trace.PlannerStage.Warnings = append(trace.PlannerStage.Warnings, "empty: "+plannerEmptyReason)
		diagnostics := diagnosePlannerEmpty(query)
		for _, d := range diagnostics {
			trace.PlannerStage.Warnings = append(trace.PlannerStage.Warnings, "diag: "+d)
		}
	}

	// Auto-promote the rarest query token to MustTerms when no plan
	// supplied one. Guarantees a hard anchor (e.g. "fms") even if the
	// caller skipped the plan field entirely. Cheap: one COUNT cypher
	// per token.
	preAnchorMust := len(plan.MustTerms)
	plan.MustTerms = ensureMustAnchor(r.Graph, query, plan.MustTerms, repo)
	autoAnchored := preAnchorMust == 0 && len(plan.MustTerms) > 0
	plan.MustAutoAnchored = autoAnchored

	if !planIsEmpty(plan) {
		log.Printf("[router] plan(%s): must=%v should=%v exclude=%v hints=%v phrases=%v",
			planSource, plan.MustTerms, plan.ShouldTerms, plan.ExcludeTerms,
			plan.ContextHints, plan.Phrases)
	}

	// Vector recall is high-recall by design. Apply a three-stage discipline
	// before any of these hits reach the prompt:
	//   1. Cosine-distance floor — drop matches the embedding model itself
	//      barely classified as related.
	//   2. Plan-driven must/exclude filter — must matched against structural
	//      fields only (name/path/symbols), never free-text purpose, so a
	//      memory that incidentally mentions a must term in prose doesn't
	//      slip through.
	//   3. Should-term re-rank — combined cosine similarity + structural
	//      should overlap, so memories that name the right symbols outrank
	//      ones that just happen to be embedding-adjacent.
	beforeFilter := len(rawHits)
	maxDist := memoryMaxDistance()
	flooredHits := dropBelowFloor(rawHits, maxDist)
	if floored := beforeFilter - len(flooredHits); floored > 0 {
		log.Printf("[router] memory floor dropped %d/%d hits (max_distance=%.2f)",
			floored, beforeFilter, maxDist)
	}
	filteredHits := filterMemoriesByPlan(flooredHits, plan)
	if dropped := len(flooredHits) - len(filteredHits); dropped > 0 {
		log.Printf("[router] memory plan-filter dropped %d/%d hits (must=%v exclude=%v)",
			dropped, len(flooredHits), plan.MustTerms, plan.ExcludeTerms)
	}
	// Memory-relevance learning: demote any candidate whose stored FP
	// centroid is close (cosine sim >= FPDemoteSimThreshold) to the
	// current query embedding. The penalty rides on top of the cosine
	// distance so a sufficiently-flagged memory falls back through the
	// floor we just applied — re-run the floor after demotion to remove
	// it from consideration entirely.
	demoted := applyFPPenalties(ctx, r.Memory, filteredHits, queryEmbed)
	if demoted > 0 {
		filteredHits = dropBelowFloor(filteredHits, maxDist)
		log.Printf("[router] memory FP demoted=%d post-demotion=%d", demoted, len(filteredHits))
	}
	rerankedHits := rankByPlan(filteredHits, plan)
	memRes := buildAllMemories(rerankedHits, repoPath)

	memCount := 0
	if memRes.agent != nil {
		memCount = len(memRes.agent.Files) + len(memRes.agent.Functions) + len(memRes.agent.Flows) + len(memRes.agent.Decisions)
	}
	log.Printf("[router] intent=%s memory_agent=%d auto_hints=%d",
		intent, memCount, len(memRes.autoHints))

	// effectiveQuery folds plan terms into the search vocabulary so name/content
	// Cypher and queryKeywords pick them up. Deterministic when planner is nil.
	effectiveQuery := buildEffectiveQuery(query, plan)
	searchOpts := codegraph.SearchOpts{
		MustTerms:    plan.MustTerms,
		ExcludeTerms: plan.ExcludeTerms,
		ContextHints: plan.ContextHints,
	}

	// --- SEARCH ---
	var searchResults []codegraph.SearchResult
	var err error

	pkgPath := extractPackagePath(query)

	if fp := codegraph.ExtractFilePath(query); fp != "" {
		log.Printf("[router] detected file path in query: %q", fp)
		searchResults, err = r.Graph.SearchByFilePath(fp, repo, 20)
		if err != nil {
			log.Printf("[router] file path search error (non-fatal): %v", err)
		}
	}

	if len(searchResults) == 0 && pkgPath != "" {
		log.Printf("[router] detected package path in query: %q", pkgPath)
		searchResults, err = r.Graph.SearchByFilePath(pkgPath, repo, 15)
		if err != nil {
			log.Printf("[router] package path search error (non-fatal): %v", err)
		}
	}

	if len(searchResults) == 0 {
		searchResults, err = r.Graph.Search(effectiveQuery, repo, 10)
		if err != nil {
			log.Printf("[router] search error (non-fatal): %v", err)
		}
	}

	if len(searchResults) == 0 {
		searchResults, err = r.Graph.SearchByNameWithOpts(effectiveQuery, repo, 10, searchOpts)
		if err != nil {
			log.Printf("[router] name search error (non-fatal): %v", err)
		}
	}

	if pkgPath != "" {
		searchResults = boostByPath(searchResults, pkgPath)
	}

	symbols := codegraph.SymbolNames(searchResults)

	// Merge auto hints as additional seed symbols for graph traversal
	hintSymbols := dedupAppend(symbols, memRes.autoHints)

	// --- BUDGETED GRAPH TRAVERSAL ---
	graphStartTime := time.Now()
	gb := graphBudgetFromProfile(profile, memCount, intent)
	log.Printf("[router] graph budget: maxTrace=%d callerHops=%d callees=%v extends=%v methods=%v imports=%v profile=%s",
		gb.maxTrace, gb.callerHops, gb.fetchCallees, gb.fetchExtends, gb.fetchMethods, gb.fetchImports, profileID)

	maxTrace := gb.maxTrace
	if len(hintSymbols) < maxTrace {
		maxTrace = len(hintSymbols)
	}

	var chain *prompt.CallChain
	var impactNames []string
	impactSeen := make(map[string]bool)
	graph := &prompt.GraphLinks{}
	edgeSeen := make(map[string]bool)

	// Build a flat list of independent codegraph traversal tasks for
	// the maxTrace symbols and run them concurrently. The merge that
	// follows is strictly sequential so dedup (edgeSeen / impactSeen)
	// order matches the previous serial implementation byte-for-byte.
	//
	// 2-hop upstream (grandparent chain) is NOT issued via the
	// codegraph 2-hop Cypher (`UpstreamChain`) — that pattern fans
	// out as a nested-loop join over the CALLS edge set on
	// LadybugDB and lands at ~380ms per call (50× the cost of every
	// other traversal). Instead we issue a *second* 1-hop pass over
	// the unique parents we discovered in batch 1. Two index-
	// friendly RTs at ~7ms each beat one 2-hop RT at 380ms by
	// roughly 25× per symbol. See bench/perf notes from 2026-05-14.
	type traceKind int
	const (
		kindCallers traceKind = iota
		kindCallees
		kindExtends
		kindMethods
	)
	type traceTask struct {
		kind traceKind
		sym  string
		out  []codegraph.CallEdge
	}
	tracedSyms := hintSymbols[:maxTrace]
	if len(tracedSyms) > 0 {
		chain = &prompt.CallChain{}
	}
	for _, sym := range tracedSyms {
		log.Printf("[router] tracing symbol: %q", sym)
	}
	var traceTasks []*traceTask
	for _, sym := range tracedSyms {
		traceTasks = append(traceTasks, &traceTask{kind: kindCallers, sym: sym})
		if gb.fetchCallees {
			traceTasks = append(traceTasks, &traceTask{kind: kindCallees, sym: sym})
		}
		if gb.fetchExtends {
			traceTasks = append(traceTasks, &traceTask{kind: kindExtends, sym: sym})
		}
		if gb.fetchMethods {
			traceTasks = append(traceTasks, &traceTask{kind: kindMethods, sym: sym})
		}
	}
	parallelDo(graphFanOut, len(traceTasks), func(i int) {
		t := traceTasks[i]
		switch t.kind {
		case kindCallers:
			t.out, _ = r.Graph.CallersWithPath(t.sym, repo)
		case kindCallees:
			t.out, _ = r.Graph.CalleesWithPath(t.sym, repo)
		case kindExtends:
			t.out, _ = r.Graph.Extends(t.sym, repo)
		case kindMethods:
			t.out, _ = r.Graph.Methods(t.sym, repo)
		}
	})
	for _, t := range traceTasks {
		switch t.kind {
		case kindCallers:
			for _, e := range t.out {
				key := "up:" + e.From + ">" + e.To
				if !edgeSeen[key] {
					edgeSeen[key] = true
					chain.Upstream = append(chain.Upstream, prompt.CallEdge{
						From: e.From, To: e.To, FilePath: e.FilePath,
					})
				}
				if !impactSeen[e.From] {
					impactSeen[e.From] = true
					impactNames = append(impactNames, e.From)
				}
			}
		case kindCallees:
			for _, e := range t.out {
				key := "dn:" + e.From + ">" + e.To
				if !edgeSeen[key] {
					edgeSeen[key] = true
					chain.Downstream = append(chain.Downstream, prompt.CallEdge{
						From: e.From, To: e.To, FilePath: e.FilePath,
					})
				}
			}
		case kindExtends:
			for _, e := range t.out {
				key := "ext:" + e.From + ">" + e.To
				if !edgeSeen[key] {
					edgeSeen[key] = true
					graph.Extends = append(graph.Extends, prompt.GraphEdge{
						From: e.From, To: e.To, FilePath: e.FilePath,
					})
				}
			}
		case kindMethods:
			for _, e := range t.out {
				key := "meth:" + e.From + ">" + e.To
				if !edgeSeen[key] {
					edgeSeen[key] = true
					graph.Methods = append(graph.Methods, prompt.GraphEdge{
						From: e.From, To: e.To, FilePath: e.FilePath,
					})
				}
			}
		}
	}

	// --- 2-HOP UPSTREAM (grandparent chain) via two 1-hop passes ---
	// Replaces the old `UpstreamChain` 2-hop Cypher which on
	// LadybugDB took ~380ms per call (vs ~7ms for an indexed
	// 1-hop CallersWithPath). Strategy: collect the unique parent
	// names we discovered in batch 1, then issue a single parallel
	// batch of CallersWithPath against each parent. Each returned
	// edge (gp.name, parent.name, gp.filePath) is exactly what the
	// old 2-hop Cypher emitted — semantically identical, ~25×
	// cheaper.
	if gb.callerHops >= 2 && chain != nil {
		parentSet := make(map[string]struct{})
		for _, t := range traceTasks {
			if t.kind != kindCallers {
				continue
			}
			for _, e := range t.out {
				if e.From == "" {
					continue
				}
				parentSet[e.From] = struct{}{}
			}
		}
		// Skip parents we've already traced as primary symbols —
		// CallersWithPath against them would just re-issue the
		// same Cypher we ran in batch 1.
		tracedSet := make(map[string]struct{}, len(tracedSyms))
		for _, s := range tracedSyms {
			tracedSet[s] = struct{}{}
		}
		var parents []string
		for p := range parentSet {
			if _, dup := tracedSet[p]; dup {
				continue
			}
			parents = append(parents, p)
		}
		// Bound the fan-out to keep the worst case (a hub with
		// hundreds of callers) from melting the codegraph
		// process — the original UpstreamChain capped output at
		// LIMIT 10, and the plan-filter clips noise downstream
		// anyway. 32 covers every realistic call site.
		const maxParents = 32
		if len(parents) > maxParents {
			sort.Strings(parents)
			parents = parents[:maxParents]
		}
		gpResults := make([][]codegraph.CallEdge, len(parents))
		parallelDo(graphFanOut, len(parents), func(i int) {
			edges, _ := r.Graph.CallersWithPath(parents[i], repo)
			gpResults[i] = edges
		})
		for _, edges := range gpResults {
			for _, e := range edges {
				key := "up:" + e.From + ">" + e.To
				if !edgeSeen[key] {
					edgeSeen[key] = true
					chain.Upstream = append(chain.Upstream, prompt.CallEdge{
						From: e.From, To: e.To, FilePath: e.FilePath,
					})
				}
			}
		}
	}

	// --- IMPORTS ---
	// Same parallelisation pattern as the symbol-trace fan-out:
	// per-keyword ImportersByPackage and per-symbol Importers are
	// independent HTTP/Cypher RTs. Merge sequentially after to
	// preserve deterministic dedup order.
	if gb.fetchImports {
		importSeen := make(map[string]bool)
		impWords := queryKeywords(effectiveQuery)
		sort.Slice(impWords, func(i, j int) bool { return len(impWords[i]) > len(impWords[j]) })
		// Filter words upfront so the task index lines up with the
		// keyword we'll merge. Same length-filter as before.
		var impKeywords []string
		for _, w := range impWords {
			if len(w) < 3 {
				continue
			}
			impKeywords = append(impKeywords, w)
		}
		impByKeyword := make([][]codegraph.CallEdge, len(impKeywords))
		parallelDo(graphFanOut, len(impKeywords), func(i int) {
			edges, _ := r.Graph.ImportersByPackage(impKeywords[i], repo)
			impByKeyword[i] = edges
		})
		for _, edges := range impByKeyword {
			for _, e := range edges {
				key := "imp:" + e.From + ">" + e.To
				if !importSeen[key] {
					importSeen[key] = true
					graph.Importers = append(graph.Importers, prompt.GraphEdge{
						From: e.From, To: e.To, FilePath: e.FilePath,
					})
				}
			}
		}
		impBySym := make([][]codegraph.CallEdge, len(tracedSyms))
		parallelDo(graphFanOut, len(tracedSyms), func(i int) {
			edges, _ := r.Graph.Importers(tracedSyms[i], repo)
			impBySym[i] = edges
		})
		for _, edges := range impBySym {
			for _, e := range edges {
				key := "imp:" + e.From + ">" + e.To
				if !importSeen[key] {
					importSeen[key] = true
					graph.Importers = append(graph.Importers, prompt.GraphEdge{
						From: e.From, To: e.To, FilePath: e.FilePath,
					})
				}
			}
		}
	}

	// --- RELATED FILES (always fetched — cheap, but per-keyword
	// RelatedFiles is still its own RT; fan it out the same way). ---
	relWords := queryKeywords(effectiveQuery)
	var sibKeywords []string
	for _, w := range relWords {
		if len(w) < 3 {
			continue
		}
		sibKeywords = append(sibKeywords, w)
	}
	relByKeyword := make([][]string, len(sibKeywords))
	parallelDo(graphFanOut, len(sibKeywords), func(i int) {
		files, _ := r.Graph.RelatedFiles(sibKeywords[i], repo, 100)
		relByKeyword[i] = files
	})
	sibSeen := make(map[string]bool)
	for _, files := range relByKeyword {
		for _, f := range files {
			if !sibSeen[f] {
				sibSeen[f] = true
				graph.Siblings = append(graph.Siblings, f)
			}
		}
	}

	// Apply the relevance gate (must / exclude terms from the query
	// plan) to graph traversal output, mirroring what
	// filterMemoriesByPlan already does on the memory side. Without
	// this the codegraph half of the prompt is only count-clipped, not
	// noise-filtered — which is how 35–43 KB hub files (managers,
	// main.go, *getters.go) used to dominate devrouter's wrong-file
	// noise on the goserving bench. See bench/results FINDINGS.
	//
	// Seed-anchor bypass: edges whose endpoints touch a known seed
	// (search-result symbol name or file path) skip the must-term
	// check. Without this we over-pruned 0.05 R@10 on goserving
	// because legitimate adjacency edges to seed packages whose own
	// path didn't carry the must term were being dropped.
	seeds := newSeedAnchors(searchResults)
	preFilter := 0
	postFilter := 0
	if chain != nil {
		preFilter += len(chain.Upstream) + len(chain.Downstream)
		// Call-chain edges are 1-hop from a seed by construction —
		// applyMust=false. Hub leakage isn't the failure mode here;
		// the failure mode of must-on-call-chain is dropping
		// legitimate adjacent context whose paths/names happen not
		// to spell out the auto-anchor token.
		chain.Upstream = filterCallChainByPlan(chain.Upstream, plan, seeds, false)
		chain.Downstream = filterCallChainByPlan(chain.Downstream, plan, seeds, false)
		postFilter += len(chain.Upstream) + len(chain.Downstream)
	}
	preFilter += len(graph.Importers) + len(graph.Extends) + len(graph.Methods) + len(graph.Siblings)
	// Importers is the main hub-leak bucket (random importing packages
	// pulling in 35 KB managers/getters). applyMust=true.
	graph.Importers = filterGraphLinksByPlan(graph.Importers, plan, seeds, true)
	// Methods/Extends are tightly typed structural relationships —
	// each is a near neighbour of the seed by construction.
	// applyMust=false.
	graph.Extends = filterGraphLinksByPlan(graph.Extends, plan, seeds, false)
	graph.Methods = filterGraphLinksByPlan(graph.Methods, plan, seeds, false)
	// Siblings are flat keyword-anchored paths — exclude-only as
	// before (must on a flat path with no symbol context over-prunes).
	graph.Siblings = filterSiblingsByPlan(graph.Siblings, plan)
	postFilter += len(graph.Importers) + len(graph.Extends) + len(graph.Methods) + len(graph.Siblings)
	if dropped := preFilter - postFilter; dropped > 0 {
		log.Printf("[router] graph plan-filter dropped %d/%d edges "+
			"(must=%v exclude=%v seed_files=%d)",
			dropped, preFilter, plan.MustTerms, plan.ExcludeTerms,
			len(seeds.Files))
	}

	if chain != nil && len(chain.Upstream) == 0 && len(chain.Downstream) == 0 {
		chain = nil
	}
	if len(graph.Importers) == 0 && len(graph.Extends) == 0 && len(graph.Methods) == 0 && len(graph.Siblings) == 0 {
		graph = nil
	}

	graphLatency := time.Since(graphStartTime).Milliseconds()
	graphEdgeCount := 0
	if chain != nil {
		graphEdgeCount += len(chain.Upstream) + len(chain.Downstream)
	}
	if graph != nil {
		graphEdgeCount += len(graph.Importers) + len(graph.Extends) + len(graph.Methods) + len(graph.Siblings)
	}
	trace.GraphStage = &prompt.StageTrace{
		LatencyMs:     graphLatency,
		CandidatesIn:  len(hintSymbols),
		CandidatesOut: maxTrace,
		Details: map[string]interface{}{
			"seed_symbols":   len(hintSymbols),
			"traced_symbols": maxTrace,
			"edges_found":    graphEdgeCount,
			"caller_hops":    gb.callerHops,
			"fetch_imports":  gb.fetchImports,
			"fetch_methods":  gb.fetchMethods,
			"fetch_extends":  gb.fetchExtends,
			"fetch_callees":  gb.fetchCallees,
		},
	}

	// Snippet cap is profile-driven: the bench-style "raw search" path
	// asks for K=10, while interactive flows tighten this through
	// MaxSnippets when there are strong primary memories (see
	// Profile.ApplyMemoryShrink). Previously this was hardcoded to 5,
	// which capped *recall* below the bench's top-K and made the MCP
	// wrapper look worse than the underlying codegraph engine on R@K
	// metrics — see bench/results FINDINGS for the regression.
	snippetCap := profile.MaxSnippets
	if snippetCap < 1 {
		snippetCap = 10
	}
	snippets := codegraph.ToSnippets(searchResults, snippetCap)

	// --- PRIMARY CONTEXT (flat list from agent memories) ---
	primaryCtx := buildPrimaryContext(memRes)

	// --- QUERY-CONTENT-DRIVEN ANCHOR INJECTION ---
	// For repo-shape questions ("which top-level services live in
	// this monorepo") and service-entry-point questions ("where does
	// oscar start its HTTP listener"), codegraph's symbol index has
	// systematic blind spots: it never returns the repo-root README,
	// and structural entry-point files (`<svc>/main.go`,
	// `<svc>/web/web.go`, `<svc>/web/routes/*`) get out-ranked by
	// sub-directory matches. The anchor pass probes a fixed set of
	// canonical paths under /api/file and prepends the hits as
	// PrimaryContext so they outrank search results.
	//
	// Critical: gating is content-based, NOT intent-based. The router's
	// keyword intent classifier collapses ~85% of bench queries to
	// "general", so an intent gate fires the pass on debug/refactor
	// queries and burns rank slots with off-topic doc anchors —
	// goserving's R@5 dropped from 0.428 to 0.261 in the first attempt.
	// matchesAny against docAnchorTriggers / serviceTraceVerbs is
	// precise enough that wrong-firing is rare.
	if anchors := r.injectQueryAnchors(ctx, trace.QueryID, query, repo, string(intent)); len(anchors) > 0 {
		primaryCtx = append(anchors, primaryCtx...)
	}

	// --- DECISIONS ---
	var decisionCtx []prompt.DecisionContextEntry
	if memRes.agent != nil && len(memRes.agent.Decisions) > 0 {
		// Separate active and superseded decisions
		var activeDecisions []prompt.DecisionMemoryHit
		supersededByName := make(map[string]prompt.DecisionMemoryHit)
		for _, d := range memRes.agent.Decisions {
			if d.Status == "superseded" {
				supersededByName[d.Name] = d
			} else {
				activeDecisions = append(activeDecisions, d)
			}
		}

		// Build context from active decisions only
		decisionCtx = buildDecisionContext(activeDecisions)

		// Add lineage: if an active decision supersedes another, append the superseded decision
		for _, active := range activeDecisions {
			if active.Supersedes != "" {
				if superseded, ok := supersededByName[active.Supersedes]; ok {
					decisionCtx = append(decisionCtx, prompt.DecisionContextEntry{
						Name:         superseded.Name,
						DecisionType: superseded.DecisionType,
						Decision:     superseded.Decision,
						Rationale:    superseded.Rationale,
						Alternatives: superseded.Alternatives,
						Constraint:   superseded.Constraint,
						Scope:        superseded.Scope,
						Status:       superseded.Status,
						Supersedes:   superseded.Supersedes,
						SupersededBy: superseded.SupersededBy,
						Confidence:   0.9,
					})
				}
			}
		}
	}

	dp := prompt.Build(string(intent), symbols, impactNames, primaryCtx, snippets)
	dp.Decisions = decisionCtx
	// Prepend decision-type-aware instructions if decisions exist
	if len(decisionCtx) > 0 {
		dp.Instructions = buildDecisionInstruction(memRes.agent.Decisions) + "\n\n" + dp.Instructions
	}
	dp.CallChain = chain
	dp.Graph = graph
	dp.ModelHint = modelHint(memCount)

	// Always surface the active plan so dev_context callers can see what
	// drove retrieval (or that nothing did) without grepping stderr.
	// Source = "agent" when the caller supplied a plan, "auto" when only
	// auto-anchoring filled in must_terms.
	dp.QueryPlan = &prompt.PlanDebug{
		Source:       planSource,
		MustTerms:    plan.MustTerms,
		ShouldTerms:  plan.ShouldTerms,
		ExcludeTerms: plan.ExcludeTerms,
		Phrases:      plan.Phrases,
		ContextHints: plan.ContextHints,
		AutoAnchored: autoAnchored,
	}

	// When no memories exist but graph returned data, tell Claude to
	// use the graph context before exploring on its own.
	if len(primaryCtx) == 0 {
		hasGraphData := len(symbols) > 0 || len(snippets) > 0 || chain != nil || graph != nil
		if hasGraphData {
			dp.Instructions = prompt.GraphFirstInstruction
			dp.MemoryCoverage = "none"
		}
	}

	trimmed := trimResponse(&dp, profile, memCount)

	// Record packing stage and finalize trace
	finalTokens := estimateTokens(&dp)
	trace.PackingStage = &prompt.StageTrace{
		LatencyMs:     0, // negligible
		CandidatesIn:  memCount + len(symbols),
		CandidatesOut: len(primaryCtx) + len(decisionCtx) + len(snippets),
		Details: map[string]interface{}{
			"memory_entries": memCount,
			"snippet_count":  len(snippets),
			"decision_count": len(decisionCtx),
		},
	}
	trace.FinalTokens = finalTokens
	trace.TotalLatencyMs = time.Since(queryStartTime).Milliseconds()

	// Populate signal scores for transparency. These are the actual
	// numbers used by ranking — no more placeholders. Consumers (dashboards,
	// dev_feedback reward tuning) can rely on them being honest.
	//
	//   semantic_similarity  — top cosine similarity across kept agent hits
	//   primary_context_match — mean cosine similarity of returned primary
	//                          context entries (matches Confidence in the
	//                          per-entry view)
	//   memory_coverage      — count-based, capped at 1.0
	//   graph_proximity      — fraction of seed symbols that yielded any
	//                          traced caller/callee edge in this run
	topSim, meanSim := agentSimilarityStats(memRes)
	trace.Signals["semantic_similarity"] = topSim
	if memCount > 0 {
		cov := float64(memCount) / 10.0
		if cov > 1 {
			cov = 1
		}
		trace.Signals["memory_coverage"] = cov
	}
	if gp := graphProximityFromTrace(trace.GraphStage); gp >= 0 {
		trace.Signals["graph_proximity"] = gp
	}
	if len(decisionCtx) > 0 {
		// Decisions are agent-curated and high-trust by definition; keep
		// this stable until we wire decision-specific cosine scoring.
		trace.Signals["decision_relevance"] = 0.9
	}
	if len(primaryCtx) > 0 {
		trace.Signals["primary_context_match"] = meanSim
	}

	// --- OUTCOME (decision side) ---
	// Captured here so dev_feedback / repeat-detection can later HSET the
	// feedback-side fields onto the same Redis hash without rewriting the
	// whole record. See plan section "RetrievalOutcome".
	filesReturned := 0
	for _, e := range dp.PrimaryContext {
		if e.File != "" {
			filesReturned++
		}
	}
	for _, s := range dp.CodeSnippets {
		if s.File != "" {
			filesReturned++
		}
	}
	budgetUsed := 0.0
	if profile.MaxTrace > 0 {
		budgetUsed = float64(maxTrace) / float64(profile.MaxTrace)
	}
	trace.Outcome = &prompt.RetrievalOutcome{
		PromptTokens:       finalTokens,
		FilesReturned:      filesReturned,
		SymbolsReturned:    len(dp.Symbols),
		SnippetsReturned:   len(dp.CodeSnippets),
		TrimmedFiles:       trimmed,
		BudgetUsedFraction: budgetUsed,
		LatencyMs:          int(trace.TotalLatencyMs),
	}

	dp.QueryID = trace.QueryID
	dp.RetrievalTrace = trace

	// --- PERSIST TRACE for dev_feedback join ---
	// HSET (not JSON blob) so dev_feedback can patch only the feedback-side
	// fields without rewriting the whole record. Best-effort: a Redis blip
	// must not break the dev_context response.
	if r.Heuristics != nil {
		memKeys := agentMemoryKeys(memRes)
		fields := traceHashFields(trace, repo, query, string(intent), profileID, profile, memKeys)
		if err := r.Heuristics.Store.PutTrace(context.Background(), trace.QueryID, fields); err != nil {
			log.Printf("[router] persist trace failed (non-fatal): %v", err)
		}
		// Push to last-call LRU as the connection-scoped fallback for
		// dev_feedback when the agent forgets to echo query_id.
		if r.LastCalls != nil {
			r.LastCalls.Push(heuristics.LastCallEntry{
				QueryID:   trace.QueryID,
				Intent:    string(intent),
				ProfileID: profileID,
				Repo:      repo,
				TopicID:   topicID,
				Timestamp: time.Now(),
			})
		}
	}

	return dp, nil
}

// traceHashFields flattens the decision-side trace + outcome into a Redis
// hash. Only string-formatted fields are stored; feedback-side numeric
// fields are written later by dev_feedback as separate HSETs.
//
// memory_keys (CSV) is added when this trace returned at least one
// agent-written memory. dev_feedback uses it to attribute false
// positives back to specific memory records.
func traceHashFields(t *prompt.RetrievalTrace, repo, query, intent, profileID string, p heuristics.Profile, memKeys []string) map[string]interface{} {
	fields := map[string]interface{}{
		"query_id":             t.QueryID,
		"query":                query,
		"repo":                 repo,
		"intent":               intent,
		"timestamp":            fmt.Sprintf("%d", t.Timestamp),
		"heuristic_profile_id": profileID,
		"profile_max_trace":    fmt.Sprintf("%d", p.MaxTrace),
		"profile_caller_hops":  fmt.Sprintf("%d", p.CallerHops),
		"final_tokens":         fmt.Sprintf("%d", t.FinalTokens),
		"total_latency_ms":     fmt.Sprintf("%d", t.TotalLatencyMs),
	}
	if len(memKeys) > 0 {
		fields["memory_keys"] = strings.Join(memKeys, ",")
	}
	if t.RepeatedExplorationOf != "" {
		fields["repeated_exploration_of"] = t.RepeatedExplorationOf
	}
	if t.TopicID != "" {
		fields["topic_id"] = t.TopicID
	}
	if t.TopicLabel != "" {
		fields["topic_label"] = t.TopicLabel
	}
	if t.HeuristicFromTopic {
		fields["heuristic_from_topic"] = "true"
	}
	if t.Outcome != nil {
		fields["prompt_tokens"] = fmt.Sprintf("%d", t.Outcome.PromptTokens)
		fields["files_returned"] = fmt.Sprintf("%d", t.Outcome.FilesReturned)
		fields["symbols_returned"] = fmt.Sprintf("%d", t.Outcome.SymbolsReturned)
		fields["snippets_returned"] = fmt.Sprintf("%d", t.Outcome.SnippetsReturned)
		fields["trimmed_files"] = fmt.Sprintf("%d", t.Outcome.TrimmedFiles)
		fields["budget_used_fraction"] = fmt.Sprintf("%g", t.Outcome.BudgetUsedFraction)
		fields["latency_ms"] = fmt.Sprintf("%d", t.Outcome.LatencyMs)
	}
	return fields
}

func modelHint(memCount int) *prompt.ModelHint {
	switch {
	case memCount >= 3:
		return &prompt.ModelHint{Model: "haiku", Reason: "strong memory coverage"}
	case memCount >= 1:
		return &prompt.ModelHint{Model: "sonnet", Reason: "partial memory coverage"}
	default:
		return &prompt.ModelHint{Model: "opus", Reason: "no memories, needs deep exploration"}
	}
}

// estimateTokens roughly estimates token count in a DevPrompt.
// Uses ~4 characters = 1 token heuristic for rough calculation.
func estimateTokens(dp *prompt.DevPrompt) int {
	totalChars := 0

	// Instructions
	totalChars += len(dp.Instructions)

	// Intent
	totalChars += len(dp.Intent)

	// Primary context
	for _, entry := range dp.PrimaryContext {
		totalChars += len(entry.Summary) + len(entry.Details)
	}

	// Decisions
	for _, d := range dp.Decisions {
		totalChars += len(d.Decision) + len(d.Rationale)
	}

	// Symbols
	for _, s := range dp.Symbols {
		totalChars += len(s)
	}

	// Code snippets
	for _, s := range dp.CodeSnippets {
		totalChars += len(s.Content)
	}

	// Rough estimate: 4 chars per token
	return (totalChars / 4) + 100 // +100 buffer for overhead
}

// extractPackagePath detects directory-style package references in the query
// like "cmpkg/abtestv2" or "kosmos/matchengine". Returns the path or "".
func extractPackagePath(query string) string {
	for _, word := range strings.Fields(query) {
		word = strings.Trim(word, ".,;:!?\"'()[]{}")
		if strings.Contains(word, "/") && !strings.Contains(word, ".") {
			return word
		}
	}
	return ""
}

// boostByPath moves search results whose FilePath contains the given path
// prefix to the front, keeping their relative order.
func boostByPath(results []codegraph.SearchResult, path string) []codegraph.SearchResult {
	pathLower := strings.ToLower(path)
	var matched, rest []codegraph.SearchResult
	for _, r := range results {
		if strings.Contains(strings.ToLower(r.FilePath), pathLower) {
			matched = append(matched, r)
		} else {
			rest = append(rest, r)
		}
	}
	return append(matched, rest...)
}

type trimConfig struct {
	maxUpstream   int
	maxDownstream int
	maxImporters  int
	maxMethods    int
	maxSiblings   int
	maxSnippets   int
	maxImpact     int
	maxSymbols    int
	maxPrimaryCtx int
	maxDecisions  int
}

// trimCapsFromProfile derives the runtime trim caps from a Profile, with
// the strong-memory shrink rules applied on top.
func trimCapsFromProfile(p heuristics.Profile, memCount int) trimConfig {
	p = p.ApplyMemoryShrink(memCount)
	return trimConfig{
		maxUpstream:   p.MaxUpstream,
		maxDownstream: p.MaxDownstream,
		maxImporters:  p.MaxImporters,
		maxMethods:    p.MaxMethods,
		maxSiblings:   p.MaxSiblings,
		maxSnippets:   p.MaxSnippets,
		maxImpact:     p.MaxImpact,
		maxSymbols:    p.MaxSymbols,
		maxPrimaryCtx: p.MaxPrimaryCtx,
		maxDecisions:  p.MaxDecisions,
	}
}

// trimResponse caps each section based on the chosen profile and memory
// strength. Returns the count of items that were dropped (across all
// sections) so the caller can record it as Outcome.TrimmedFiles for
// trim-aggressiveness analysis.
func trimResponse(dp *prompt.DevPrompt, profile heuristics.Profile, memCount int) int {
	tc := trimCapsFromProfile(profile, memCount)
	dropped := 0

	if dp.CallChain != nil {
		if n := len(dp.CallChain.Upstream); n > tc.maxUpstream {
			dropped += n - tc.maxUpstream
			dp.CallChain.Upstream = dp.CallChain.Upstream[:tc.maxUpstream]
		}
		if n := len(dp.CallChain.Downstream); n > tc.maxDownstream {
			dropped += n - tc.maxDownstream
			dp.CallChain.Downstream = dp.CallChain.Downstream[:tc.maxDownstream]
		}
	}
	if dp.Graph != nil {
		if n := len(dp.Graph.Importers); n > tc.maxImporters {
			dropped += n - tc.maxImporters
			dp.Graph.Importers = dp.Graph.Importers[:tc.maxImporters]
		}
		if n := len(dp.Graph.Methods); n > tc.maxMethods {
			dropped += n - tc.maxMethods
			dp.Graph.Methods = dp.Graph.Methods[:tc.maxMethods]
		}
		if n := len(dp.Graph.Siblings); n > tc.maxSiblings {
			dropped += n - tc.maxSiblings
			dp.Graph.Siblings = dp.Graph.Siblings[:tc.maxSiblings]
		}
	}
	if n := len(dp.CodeSnippets); n > tc.maxSnippets {
		dropped += n - tc.maxSnippets
		dp.CodeSnippets = dp.CodeSnippets[:tc.maxSnippets]
	}
	if n := len(dp.ImpactRadius); n > tc.maxImpact {
		dropped += n - tc.maxImpact
		dp.ImpactRadius = dp.ImpactRadius[:tc.maxImpact]
	}
	if n := len(dp.Symbols); n > tc.maxSymbols {
		dropped += n - tc.maxSymbols
		dp.Symbols = dp.Symbols[:tc.maxSymbols]
	}
	if n := len(dp.PrimaryContext); n > tc.maxPrimaryCtx {
		dropped += n - tc.maxPrimaryCtx
		dp.PrimaryContext = dp.PrimaryContext[:tc.maxPrimaryCtx]
	}
	if n := len(dp.Decisions); n > tc.maxDecisions {
		dropped += n - tc.maxDecisions
		dp.Decisions = dp.Decisions[:tc.maxDecisions]
	}
	return dropped
}

// memoryResults holds the two-phase retrieval output.
type memoryResults struct {
	agent     *prompt.StructuredMemories // agent-written memories for the prompt
	allHits   []memory.MemoryHit         // all hits including auto, for node merging
	autoHints []string                   // file paths / symbol names from auto hits to seed graph search
}

// buildAllMemories implements two-phase retrieval:
//
//	Phase 1 (Recall): keep all hits (auto + agent)
//	Phase 2 (Rank):   agent entries go into the prompt; auto entries provide
//	                  seed hints for graph traversal without appearing in output.
func buildAllMemories(hits []memory.MemoryHit, repoPath string) memoryResults {
	res := memoryResults{allHits: hits}
	if len(hits) == 0 {
		return res
	}

	sm := &prompt.StructuredMemories{}
	hintSeen := make(map[string]bool)

	for _, h := range hits {
		isAuto := h.Fields["source"] == "auto"

		if isAuto {
			// Extract file paths and symbol names as graph seed hints.
			// Skip placeholder purposes — they have no semantic value for the prompt.
			for _, key := range []string{"path", "file", "name"} {
				if v := h.Fields[key]; v != "" && !hintSeen[v] {
					hintSeen[v] = true
					res.autoHints = append(res.autoHints, v)
				}
			}
			continue
		}

		sim := hitSimilarity(h.Score)
		switch h.Type {
		case "file":
			stale := isStale(repoPath, h.Fields["path"], h.Fields["git_hash"])
			sm.Files = append(sm.Files, prompt.FileMemoryHit{
				Path:       h.Fields["path"],
				Purpose:    h.Fields["purpose"],
				KeySymbols: h.Fields["key_symbols"],
				Source:     h.Fields["source"],
				Stale:      stale,
				Sim:        sim,
				Key:        h.Key,
			})
		case "func":
			stale := isStale(repoPath, h.Fields["file"], h.Fields["git_hash"])
			sm.Functions = append(sm.Functions, prompt.FuncMemoryHit{
				Name:    h.Fields["name"],
				File:    h.Fields["file"],
				Purpose: h.Fields["purpose"],
				Callers: h.Fields["callers"],
				Callees: h.Fields["callees"],
				Source:  h.Fields["source"],
				Stale:   stale,
				Sim:     sim,
				Key:     h.Key,
			})
		case "flow":
			stale := isFlowStale(repoPath, h.Fields["files"], h.Fields["git_hash"])
			sm.Flows = append(sm.Flows, prompt.FlowMemoryHit{
				Name:        h.Fields["name"],
				Purpose:     h.Fields["purpose"],
				Files:       h.Fields["files"],
				EntryPoints: h.Fields["entry_points"],
				Source:      h.Fields["source"],
				Stale:       stale,
				Sim:         sim,
				Key:         h.Key,
			})
		case "decision":
			// Decisions are always agent-written, no staleness tracking
			sm.Decisions = append(sm.Decisions, prompt.DecisionMemoryHit{
				Name:         h.Fields["name"],
				DecisionType: h.Fields["decision_type"],
				Decision:     h.Fields["decision"],
				Rationale:    h.Fields["rationale"],
				Alternatives: h.Fields["alternatives"],
				Constraint:   h.Fields["constraint"],
				Scope:        h.Fields["scope"],
				Files:        h.Fields["files"],
				Status:       h.Fields["status"],
				Supersedes:   h.Fields["supersedes"],
				SupersededBy: h.Fields["superseded_by"],
				Source:       h.Fields["source"],
			})
		}
	}

	if len(sm.Files) > 0 || len(sm.Functions) > 0 || len(sm.Flows) > 0 || len(sm.Decisions) > 0 {
		res.agent = sm
	}
	return res
}

// isStale checks if a file has changed since the memory was saved.
func isStale(repoPath, filePath, savedHash string) bool {
	if repoPath == "" || filePath == "" || savedHash == "" {
		return false // can't determine staleness without data
	}
	currentHash := memory.GitFileHash(repoPath, filePath)
	if currentHash == "" {
		return false
	}
	return currentHash != savedHash
}

// isFlowStale checks if ANY file in a flow's file list has changed.
// Flow memories store a comma-separated list of files but a single git_hash
// (from the first file at save time), so we check the first file only.
func isFlowStale(repoPath, filesCSV, savedHash string) bool {
	if repoPath == "" || filesCSV == "" || savedHash == "" {
		return false
	}
	firstFile := strings.TrimSpace(strings.SplitN(filesCSV, ",", 2)[0])
	if firstFile == "" {
		return false
	}
	currentHash := memory.GitFileHash(repoPath, firstFile)
	if currentHash == "" {
		return false
	}
	return currentHash != savedHash
}

// queryKeywords delegates to codegraph.SplitQueryWords so router and graph
// agree on tokenization, stop-word handling, and stemming. Kept as a thin
// wrapper for call-site readability.
func queryKeywords(q string) []string {
	return codegraph.SplitQueryWords(q)
}

// ensureMustAnchor guarantees at least one rarity-grounded must term.
// If the planner already supplied terms, they're returned unchanged. Else
// the rarest non-stoplist token from the query (by NameHitCount) is
// promoted. If counts are all zero or the count cypher fails, returns the
// existing list — the must-filter is a soft anchor, not a hard requirement.
func ensureMustAnchor(graph *codegraph.Client, query string, existing []string, repo string) []string {
	if len(existing) > 0 {
		return existing
	}
	tokens := codegraph.SplitQueryWords(query)
	if len(tokens) == 0 {
		return existing
	}

	bestTok := ""
	bestCnt := -1
	for _, t := range tokens {
		cnt, err := graph.NameHitCount(t, repo)
		if err != nil || cnt <= 0 {
			continue
		}
		if bestTok == "" || cnt < bestCnt {
			bestTok = t
			bestCnt = cnt
		}
	}
	if bestTok == "" {
		return existing
	}
	log.Printf("[router] auto-anchored must=%q (hits=%d)", bestTok, bestCnt)
	return []string{bestTok}
}

// buildEffectiveQuery folds planner-supplied terms into the raw query so
// downstream tokenization picks them up. Order is: original query first
// (preserves any phrase-ish signal hybrid search may use), then must
// terms, then should terms. Empty plan -> original query unchanged.
func buildEffectiveQuery(query string, plan QueryPlan) string {
	if len(plan.MustTerms) == 0 && len(plan.ShouldTerms) == 0 {
		return query
	}
	parts := []string{query}
	parts = append(parts, plan.MustTerms...)
	parts = append(parts, plan.ShouldTerms...)
	return strings.Join(parts, " ")
}

// defaultMemoryMaxDistance is the cosine-distance ceiling above which a
// vector hit is dropped before any plan filtering. nomic-embed-text
// distances cluster: <0.2 paraphrase, 0.2-0.4 same topic, 0.4-0.6 weak
// topical, >0.6 incidental. We default to 0.6 (= ≥0.40 cosine similarity)
// which removes blatant off-topic matches without dropping borderline
// cross-package ones. Override with DEVROUTER_MEMORY_MAX_DISTANCE.
const defaultMemoryMaxDistance = 0.60

func memoryMaxDistance() float64 {
	if v := os.Getenv("DEVROUTER_MEMORY_MAX_DISTANCE"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 {
			return f
		}
	}
	return defaultMemoryMaxDistance
}

// dropBelowFloor drops vector hits whose cosine distance exceeds maxDist.
// FT.SEARCH does not enforce a similarity floor — it always returns top-K —
// so this is the only place that says "no, this match is too weak to count."
// Hits without a populated score (e.g. lexical or graph-derived future hits)
// are passed through.
func dropBelowFloor(hits []memory.MemoryHit, maxDist float64) []memory.MemoryHit {
	if len(hits) == 0 || maxDist <= 0 {
		return hits
	}
	kept := make([]memory.MemoryHit, 0, len(hits))
	for _, h := range hits {
		if h.Score > 0 && h.Score > maxDist {
			continue
		}
		kept = append(kept, h)
	}
	return kept
}

// hitSimilarity converts the cosine distance returned by FT.SEARCH KNN
// into a [0,1] similarity. Distance is in [0, 2] for non-normalised
// vectors; for unit-normalised vectors (which nomic-embed-text returns)
// the practical range is [0, 1]. We clamp regardless to keep downstream
// math sane even if a future embedding model breaks the assumption.
func hitSimilarity(score float64) float64 {
	sim := 1.0 - score
	if sim < 0 {
		return 0
	}
	if sim > 1 {
		return 1
	}
	return sim
}

// filterMemoriesByPlan applies the plan's must/exclude discipline to the
// vector recall. Two filters:
//
//  1. MustTerms — the memory's STRUCTURAL text (name, path, file, files,
//     key_symbols, entry_points) must contain at least one must term as a
//     substring. Free-text fields (purpose, rationale, decision) are NOT
//     checked, because they routinely contain common words like "error"
//     incidentally — letting must-matching against free text through is
//     how we ended up returning the seat-provider-mapping flow for an
//     "error logging in tag-based clearing" query (its purpose mentioned
//     "if FM value is incomplete or error" in passing).
//
//     EXCEPTION — `plan.MustAutoAnchored`: when MustTerms was synthesised
//     by ensureMustAnchor (rarest-query-token heuristic) rather than
//     supplied by the caller, we DOWNGRADE must to should on the memory
//     side. The auto-anchor is good at narrowing codegraph search (large,
//     noisy result set) but too aggressive on memory hits, where the
//     structural text is often just the file path. The mall-memory bench
//     showed this dropping 8/8 cosine-passing memories for "Where is the
//     storefront cart-to-order conversion handled when a user places an
//     order?" because the auto-anchor was "user" and none of the
//     on-topic memories' paths contained that token. ExcludeTerms is
//     still applied even when auto-anchored — it's a path-pattern
//     blocklist, not a substring narrower.
//
//  2. ExcludeTerms — conventional path/name patterns from
//     shouldExcludeMemory. Same rationale as above: structural markers
//     only, never substring matches against free text.
//
// Returns the filtered slice in original order. A nil/empty plan or empty
// hits list is a no-op.
//
// See filterCallChainByPlan / filterGraphLinksByPlan for the graph-side
// equivalents — they apply the same must/exclude logic to codegraph
// traversal output so the relevance gate covers both halves of the
// retrieval surface, not just memories. Those filters keep the
// auto-anchor as a hard gate because their input population is the
// graph hub-set (large, noisy) rather than the cosine-filtered
// top-K memory hits (small, already on-topic).
func filterMemoriesByPlan(hits []memory.MemoryHit, plan QueryPlan) []memory.MemoryHit {
	if len(hits) == 0 {
		return hits
	}
	// When must is auto-anchored, treat it as advisory on memory:
	// rankByPlan still uses ShouldTerms (matched on this same plan)
	// to up-weight memories that DO contain the anchor, so we keep
	// the signal — we just stop using it as a hard kill switch.
	applyMust := len(plan.MustTerms) > 0 && !plan.MustAutoAnchored
	if !applyMust && len(plan.ExcludeTerms) == 0 {
		return hits
	}
	kept := make([]memory.MemoryHit, 0, len(hits))
	for _, h := range hits {
		if applyMust {
			structural := memoryStructuralText(h)
			if !containsAnyTerm(structural, plan.MustTerms) {
				continue
			}
		}
		if shouldExcludeMemory(h, plan.ExcludeTerms) {
			continue
		}
		kept = append(kept, h)
	}
	return kept
}

// SeedAnchors carries the files that the upstream search stage
// already certified as on-topic. The graph filter uses it as a
// must-term BYPASS: any edge whose FilePath is a seed file is treated
// as relevant by virtue of adjacency, even if its structural text
// doesn't carry the must token directly.
//
// Why files only (and not symbol names): symbol-name anchoring was
// tried and over-matched. Common Go names (Init, New, Run, Get) appear
// in many graph edges, so any of them being present in hintSymbols
// reopened the hub-file leak we were trying to close (only 1 % uniform
// token reduction vs 30 % for must-only). File paths are unique
// strings — exact path match is a much stronger signal that the edge
// actually lives inside a search-certified region of the codebase.
//
// Why bypass at all (not "must everywhere"): the must term is a
// substring rule, but call-chain semantics are about *adjacency* to
// a known-relevant region. An edge from `clientip.go → ip.go` is
// genuinely relevant to an "IP detection" query even if neither path
// contains the auto-anchor token verbatim — because clientip.go was
// already certified by the search layer. On goserving this fix
// recovers 0.05 R@10 lost when must was applied uniformly, while
// still keeping ~30 % of the uniform-token reduction the filter
// achieves.
//
// All keys are pre-lowercased; callers use newSeedAnchors().
type SeedAnchors struct {
	Files map[string]bool
}

// newSeedAnchors builds a SeedAnchors set from the top search hits.
//
// Files are taken from search-result `FilePath` values, lowercased,
// exact-match. Capped at TopK (default 10) so a flood of weak BM25
// hits can't dilute the anchor set into something equivalent to "the
// whole repo".
func newSeedAnchors(results []codegraph.SearchResult) SeedAnchors {
	const topK = 10
	a := SeedAnchors{Files: make(map[string]bool)}
	for i, r := range results {
		if i >= topK {
			break
		}
		fp := strings.ToLower(strings.TrimSpace(r.FilePath))
		if fp == "" {
			continue
		}
		a.Files[fp] = true
	}
	return a
}

// edgeTouchesSeed reports whether the edge's FilePath matches a known
// seed file. Used by the graph filters to bypass must-term checks for
// edges that live inside search-certified files.
func (a SeedAnchors) edgeTouchesSeed(_, _, filePath string) bool {
	if len(a.Files) == 0 || filePath == "" {
		return false
	}
	return a.Files[strings.ToLower(filePath)]
}

// filterCallChainByPlan applies the relevance gate to graph call-chain
// edges (upstream callers, downstream callees).
//
// Why this exists: codegraph's graph traversal is exhaustive — for
// every seed symbol we pull every caller, every callee, every method,
// every importer that exists in the graph. With no relevance signal,
// hub files (managers, main.go, getters.go) end up dominating noise
// (35–43 KB wrong-file p99 in the goserving bench, vs agentmemory's
// 10 KB ceiling). Without this filter the whole DevPrompt — the place
// where devrouter is supposed to add value — was just count-clipped,
// not noise-filtered.
//
// Filter shape, by edge type:
//
//   - applyMust=false (call-chain): edges 1-hop from a search-certified
//     seed symbol. Adjacency trust: the search layer already validated
//     this neighbourhood is on-topic, even if the auto-anchor token
//     happens not to appear in the seed name (e.g. must="connection",
//     seed="DBClient"). Exclude-only — pruning hubs that genuinely
//     called/were-called-by a seed is too aggressive.
//   - applyMust=true (importers, siblings): these are the buckets
//     where hubs leak in (random importing packages, keyword-anchored
//     siblings). Apply must-term substring filter against the edge
//     structural text. Seed-file bypass still applies for edges that
//     happen to live in a search-certified file.
//
// Exclude rule (always): drop edges whose target name/path looks
// test/mock/fixture by the same narrow conventions shouldExcludeMemory
// uses. Won't over-prune on incidental substrings.
//
// nil/empty plan or empty edges → no-op. Returns a possibly-shorter
// slice in original order.
func filterCallChainByPlan(
	edges []prompt.CallEdge,
	plan QueryPlan,
	seeds SeedAnchors,
	applyMust bool,
) []prompt.CallEdge {
	if len(edges) == 0 {
		return edges
	}
	if len(plan.MustTerms) == 0 && len(plan.ExcludeTerms) == 0 {
		return edges
	}
	kept := make([]prompt.CallEdge, 0, len(edges))
	for _, e := range edges {
		if applyMust && len(plan.MustTerms) > 0 {
			anchored := seeds.edgeTouchesSeed(e.From, e.To, e.FilePath)
			if !anchored {
				structural := graphEdgeStructuralText(e.From, e.To, e.FilePath)
				if !containsAnyTerm(structural, plan.MustTerms) {
					continue
				}
			}
		}
		if shouldExcludeGraphTarget(e.To, e.FilePath, plan.ExcludeTerms) {
			continue
		}
		kept = append(kept, e)
	}
	return kept
}

// filterGraphLinksByPlan is the GraphEdge equivalent of
// filterCallChainByPlan. Same parameter contract — applyMust=false
// for adjacency buckets (Methods, Extends), applyMust=true for the
// hub-prone buckets (Importers).
func filterGraphLinksByPlan(
	edges []prompt.GraphEdge,
	plan QueryPlan,
	seeds SeedAnchors,
	applyMust bool,
) []prompt.GraphEdge {
	if len(edges) == 0 {
		return edges
	}
	if len(plan.MustTerms) == 0 && len(plan.ExcludeTerms) == 0 {
		return edges
	}
	kept := make([]prompt.GraphEdge, 0, len(edges))
	for _, e := range edges {
		if applyMust && len(plan.MustTerms) > 0 {
			anchored := seeds.edgeTouchesSeed(e.From, e.To, e.FilePath)
			if !anchored {
				structural := graphEdgeStructuralText(e.From, e.To, e.FilePath)
				if !containsAnyTerm(structural, plan.MustTerms) {
					continue
				}
			}
		}
		if shouldExcludeGraphTarget(e.To, e.FilePath, plan.ExcludeTerms) {
			continue
		}
		kept = append(kept, e)
	}
	return kept
}

// filterSiblingsByPlan applies ONLY the exclude filter to sibling
// (related-file) paths.
//
// Why no must-filter: siblings are returned by RelatedFiles(word, ...),
// which is already keyword-anchored to the query, but the result is
// just a flat list of file paths — no symbol context. Applying must
// here would silently nuke legitimate adjacent files whose path
// happens not to spell out the must term (e.g. must=["ratelimit"]
// would drop `proxy.go` even when the file contains the rate-limiter
// call site). Exclude (test/mock/fixture) is always safe.
func filterSiblingsByPlan(paths []string, plan QueryPlan) []string {
	if len(paths) == 0 || len(plan.ExcludeTerms) == 0 {
		return paths
	}
	kept := make([]string, 0, len(paths))
	for _, p := range paths {
		if shouldExcludeGraphTarget("", p, plan.ExcludeTerms) {
			continue
		}
		kept = append(kept, p)
	}
	return kept
}

// graphEdgeStructuralText is the graph-edge analog of
// memoryStructuralText: lowercase concatenation of the structural
// surface (caller name, callee name, file path) used as the haystack
// for must-term substring matching.
func graphEdgeStructuralText(from, to, filePath string) string {
	var b strings.Builder
	b.Grow(len(from) + len(to) + len(filePath) + 2)
	b.WriteString(strings.ToLower(from))
	b.WriteByte(' ')
	b.WriteString(strings.ToLower(to))
	b.WriteByte(' ')
	b.WriteString(strings.ToLower(filePath))
	return b.String()
}

// shouldExcludeGraphTarget mirrors shouldExcludeMemory but operates
// on the (target name, target file path) pair instead of a memory
// hit's structured fields. Same narrow semantics — only structural
// markers (path segments, name prefixes) count, never substring
// matches against arbitrary identifiers.
func shouldExcludeGraphTarget(name, filePath string, excludes []string) bool {
	if len(excludes) == 0 {
		return false
	}
	n := strings.ToLower(name)
	p := strings.ToLower(filePath)
	for _, e := range excludes {
		switch strings.ToLower(strings.TrimSpace(e)) {
		case "test":
			if hasTestMarker(p) {
				return true
			}
			if strings.HasPrefix(n, "test") {
				return true
			}
		case "mock":
			if strings.Contains(p, "/mock") {
				return true
			}
			if strings.HasPrefix(n, "mock") {
				return true
			}
		case "fixture":
			if strings.Contains(p, "/fixture") || strings.Contains(p, "/testdata") {
				return true
			}
		}
	}
	return false
}

// rankByPlan re-ranks hits using a combined score:
//
//	rank = cosine_similarity + 0.10 * structural_should_overlap + 0.05 * freetext_should_overlap
//
// where overlap is the count of distinct should terms that match. The
// structural bonus dominates so a memory that names the right symbols
// outranks one that just happens to describe them in free text. Hits
// without a populated cosine score (defensive — shouldn't happen post-
// dropBelowFloor) sort to the bottom.
//
// Stable: when scores tie we preserve the input order (which itself was
// the FT.SEARCH ordering).
func rankByPlan(hits []memory.MemoryHit, plan QueryPlan) []memory.MemoryHit {
	if len(hits) == 0 {
		return hits
	}
	type scored struct {
		h    memory.MemoryHit
		rank float64
		ord  int
	}
	scoredHits := make([]scored, 0, len(hits))
	for i, h := range hits {
		rank := hitSimilarity(h.Score)
		if len(plan.ShouldTerms) > 0 {
			structural := memoryStructuralText(h)
			freetext := memoryFreetextText(h)
			rank += 0.10 * float64(countMatchingTerms(structural, plan.ShouldTerms))
			rank += 0.05 * float64(countMatchingTerms(freetext, plan.ShouldTerms))
		}
		scoredHits = append(scoredHits, scored{h: h, rank: rank, ord: i})
	}
	sort.SliceStable(scoredHits, func(i, j int) bool {
		if scoredHits[i].rank != scoredHits[j].rank {
			return scoredHits[i].rank > scoredHits[j].rank
		}
		return scoredHits[i].ord < scoredHits[j].ord
	})
	out := make([]memory.MemoryHit, len(scoredHits))
	for i := range scoredHits {
		out[i] = scoredHits[i].h
	}
	return out
}

// memoryStructuralText concatenates only the STRUCTURAL fields — names,
// paths, files, symbols. These are the fields where a substring match
// reliably signals "this memory is about this thing" rather than "this
// memory mentions this thing in passing."
func memoryStructuralText(h memory.MemoryHit) string {
	keys := []string{"name", "path", "file", "files", "key_symbols", "entry_points", "scope"}
	return joinLower(h.Fields, keys)
}

// memoryFreetextText concatenates the prose fields. Used as a soft
// signal for should-rerank; never used for hard must-filter rejection.
func memoryFreetextText(h memory.MemoryHit) string {
	keys := []string{"purpose", "rationale", "decision"}
	return joinLower(h.Fields, keys)
}

func joinLower(fields map[string]string, keys []string) string {
	var b strings.Builder
	for _, k := range keys {
		if v := fields[k]; v != "" {
			b.WriteString(strings.ToLower(v))
			b.WriteByte(' ')
		}
	}
	return b.String()
}

// countMatchingTerms returns how many of the (lowercased, trimmed) terms
// appear at least once as a substring of text. Each term is counted at
// most once even if it occurs multiple times — this is a relevance
// score, not a frequency one.
func countMatchingTerms(text string, terms []string) int {
	if text == "" || len(terms) == 0 {
		return 0
	}
	n := 0
	for _, t := range terms {
		t = strings.ToLower(strings.TrimSpace(t))
		if t == "" {
			continue
		}
		if strings.Contains(text, t) {
			n++
		}
	}
	return n
}

// containsAnyTerm reports whether text contains at least one of the
// (already-lowercased) terms as a substring. Empty/whitespace terms are
// skipped.
func containsAnyTerm(text string, terms []string) bool {
	for _, t := range terms {
		t = strings.ToLower(strings.TrimSpace(t))
		if t == "" {
			continue
		}
		if strings.Contains(text, t) {
			return true
		}
	}
	return false
}

// shouldExcludeMemory returns true if a memory hit matches any conventional
// exclude pattern. Intentionally narrow: only structural markers count, not
// arbitrary substring matches against free-text purpose fields. Prevents
// false positives like dropping a memory whose `purpose` reads "this is the
// test harness for the production decoder".
func shouldExcludeMemory(h memory.MemoryHit, excludes []string) bool {
	if len(excludes) == 0 {
		return false
	}
	name := strings.ToLower(h.Fields["name"])
	path := strings.ToLower(h.Fields["path"])
	file := strings.ToLower(h.Fields["file"])
	files := strings.ToLower(h.Fields["files"])
	for _, e := range excludes {
		switch strings.ToLower(strings.TrimSpace(e)) {
		case "test":
			if hasTestMarker(path) || hasTestMarker(file) || hasTestMarker(files) {
				return true
			}
			if strings.HasPrefix(name, "test") {
				return true
			}
		case "mock":
			if strings.Contains(path, "/mock") || strings.Contains(file, "/mock") ||
				strings.Contains(files, "/mock") {
				return true
			}
			if strings.HasPrefix(name, "mock") {
				return true
			}
		case "fixture":
			if strings.Contains(path, "/fixture") || strings.Contains(file, "/fixture") ||
				strings.Contains(files, "/fixture") {
				return true
			}
		}
	}
	return false
}

// hasTestMarker reports whether a path string contains a recognized
// Go-conventional test marker.
func hasTestMarker(p string) bool {
	if p == "" {
		return false
	}
	return strings.Contains(p, "_test.go") ||
		strings.Contains(p, "/test/") ||
		strings.Contains(p, "/tests/") ||
		strings.HasPrefix(p, "test/") ||
		strings.HasPrefix(p, "tests/")
}

// dedupAppend appends items from extra to base, skipping duplicates.
func dedupAppend(base, extra []string) []string {
	seen := make(map[string]bool, len(base))
	for _, s := range base {
		seen[s] = true
	}
	out := make([]string, len(base))
	copy(out, base)
	for _, s := range extra {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// rootDocCandidates is a tiny ordered list of repo-root markdown
// filenames probed (in order) when the query is unambiguously a
// repo-shape question (see docAnchorTriggers). The first hit wins.
//
// Bench rationale: on goserving, "Which top-level services live in
// the monorepo?" and "Is this a single Go module or multi-module?"
// both gold-list ReadMe.md, but codegraph's symbol index never
// surfaces it. agentmemory wins those two questions via vector
// search over markdown content — recovering them is worth 0.07 R@5
// overall. We keep the list short (3 entries) so a wrong probe
// can't displace more than one search hit even in the unlikely case
// every fallback resolves.
var rootDocCandidates = []string{
	"ReadMe.md",
	"README.md",
	"readme.md",
}

// serviceEntryPatterns is the canonical entry-point file suffix
// catalogue attached to a service token. Each entry has a list of
// query-keyword "affinities" that the router uses to re-rank the
// list per query — a question about "gRPC server" should probe
// `web/grpc/server.go` before `web/web.go`, while "HTTP listener"
// should put `web/http/server.go` first. Without this the fixed
// list hands out the wrong slots first and burns the budget cap on
// off-topic anchors (bench: weaver/web/http/server.go was missed
// for goserving-014 because main.go and route.go absorbed the
// budget).
//
// Calibrated on the goserving "trace" failures (q-001, 003, 006,
// 013, 014) and the q-002 "controller" failure: every gold target
// maps to one of these patterns OR the keyword-driven controller
// probe in injectControllerAnchors.
var serviceEntryPatterns = []serviceEntryPattern{
	{"main.go", []string{"main", "boot", "start", "init", "shutdown", "interrupt", "install"}},
	{"web/web.go", []string{"web", "server", "listener", "start", "listen"}},
	{"web/http/server.go", []string{"http", "listener", "server", "rest", "listen"}},
	{"web/grpc/server.go", []string{"grpc", "server", "rpc"}},
	{"web/routes/route.go", []string{"route", "routes", "register", "endpoint", "path"}},
	{"web/routes/routes.go", []string{"route", "routes", "register", "endpoint", "path"}},
	{"handlers/web/routes/routes.go", []string{"route", "routes", "wire", "register", "handler"}},
}

type serviceEntryPattern struct {
	suffix      string
	keywordHits []string // query tokens that bias this suffix higher
}

// docAnchorTriggers are the substrings that switch the doc-anchor
// pass on. We deliberately do NOT gate on intent because the
// router's keyword-only intent classifier collapses almost all
// queries to "general" (any query without "debug"/"bug"/"refactor"
// in it falls through), so an intent-gate over-fires by ~25x.
// Content gating is precise: these strings only appear in queries
// that are genuinely asking about repo shape / module layout /
// service inventory.
var docAnchorTriggers = []string{
	"monorepo",
	"top-level",
	"top level",
	"multi-module",
	"multi module",
	"canonical list",
	"list of services",
	"directory layout",
	"repo structure",
	"repository structure",
	"go module", "go modules",
}

// serviceTraceVerbs gate the structural-anchor pass for trace-shape
// questions. Queries about service entry points use one of these
// infrastructure verbs: "where does X *start* its listener", "how
// are routes *registered*", "where is osinterrupt *installed*". We
// require BOTH a service token (extractServiceTokens) AND one of
// these verbs to fire — verb alone (e.g. "start" in unrelated
// contexts) or token alone ("the oscar logger config") aren't
// enough.
var serviceTraceVerbs = []string{
	"start",
	"listen",
	"register",
	"wire ",
	"wires ",
	"wired ",
	"serve",
	"expose",
	"install",
	"mount",
	"attach",
	"boot",
	"spin up",
	"initialise",
	"initialize",
}

// topLevelDirs returns the cached top-level directory list for
// repo, fetching from codegraph and memoising on first call. Empty
// list is returned (and cached) on cypher failure so transient
// errors don't fan out queries on every subsequent request.
func (r *Router) topLevelDirs(repo string) []string {
	r.dirsCacheMu.Lock()
	defer r.dirsCacheMu.Unlock()
	if dirs, ok := r.dirsCache[repo]; ok {
		return dirs
	}
	dirs, err := r.Graph.TopLevelDirs(repo)
	if err != nil {
		log.Printf("[router] anchor: TopLevelDirs(%q) error: %v", repo, err)
		dirs = nil
	}
	r.dirsCache[repo] = dirs
	return dirs
}

// extractServiceTokens scans the query for words that match a known
// top-level directory. Used by trace-intent anchor injection to
// decide which service to probe canonical entry-point paths for.
//
// Detection is deliberately conservative: we only treat a word as a
// service token if (a) it's at least 4 chars (avoids "lib", "cmd"
// catching every query), (b) it's not in a small stoplist of common
// prose, and (c) it appears verbatim in the cached dir list.
//
// We also accept partial matches — a query saying "smartcache" hits
// the dir "smartcacheserving". This is what agentmemory's vector
// search effectively does for these queries.
func extractServiceTokens(query string, dirs []string) []string {
	if len(dirs) == 0 {
		return nil
	}
	stop := map[string]bool{
		"service": true, "server": true, "package": true, "module": true,
		"file": true, "files": true, "code": true, "logic": true,
		"where": true, "which": true, "what": true, "when": true,
	}
	dirSet := make(map[string]string, len(dirs)) // lowercase → canonical
	for _, d := range dirs {
		dirSet[strings.ToLower(d)] = d
	}

	// Tokenise on non-alphanumerics so "advertiserblocker's" → "advertiserblocker".
	words := make([]string, 0, 16)
	cur := strings.Builder{}
	for _, ch := range query {
		if (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') {
			cur.WriteRune(ch)
			continue
		}
		if cur.Len() > 0 {
			words = append(words, strings.ToLower(cur.String()))
			cur.Reset()
		}
	}
	if cur.Len() > 0 {
		words = append(words, strings.ToLower(cur.String()))
	}

	seen := make(map[string]bool)
	var tokens []string
	for _, w := range words {
		if len(w) < 4 || stop[w] {
			continue
		}
		// Exact match.
		if canon, ok := dirSet[w]; ok && !seen[canon] {
			seen[canon] = true
			tokens = append(tokens, canon)
			continue
		}
		// Prefix match (smartcache → smartcacheserving).
		for low, canon := range dirSet {
			if seen[canon] {
				continue
			}
			if strings.HasPrefix(low, w) || strings.HasPrefix(w, low) {
				seen[canon] = true
				tokens = append(tokens, canon)
				break
			}
		}
		if len(tokens) >= 3 {
			break // cap fan-out
		}
	}
	return tokens
}

// matchesAny reports whether ql contains any of the given lowercase
// triggers. Helper for the content-based anchor gates.
func matchesAny(ql string, triggers []string) bool {
	for _, t := range triggers {
		if strings.Contains(ql, t) {
			return true
		}
	}
	return false
}

// rankSuffixesByQuery returns a copy of patterns reordered so that
// the suffix whose keyword affinity best matches the query comes
// first. Stable for ties — patterns with no keyword match keep their
// original relative order at the back of the list.
//
// Bench rationale: goserving-014 ("weaver service start its gRPC
// server alongside its HTTP server") gold-lists three files —
// `weaver/web/grpc/server.go`, `weaver/web/http/server.go`,
// `weaver/web/web.go` — but the static suffix order put main.go and
// web/routes/route.go first, so the 3-anchor budget burned on those
// and missed `web/http/server.go`. Re-ranking by query terms picks
// the right three first.
func rankSuffixesByQuery(patterns []serviceEntryPattern, ql string) []serviceEntryPattern {
	type scored struct {
		idx     int
		score   int
		pattern serviceEntryPattern
	}
	out := make([]scored, len(patterns))
	for i, p := range patterns {
		s := 0
		for _, kw := range p.keywordHits {
			if strings.Contains(ql, kw) {
				s++
			}
		}
		out[i] = scored{idx: i, score: s, pattern: p}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].score != out[j].score {
			return out[i].score > out[j].score
		}
		return out[i].idx < out[j].idx
	})
	res := make([]serviceEntryPattern, len(out))
	for i, s := range out {
		res[i] = s.pattern
	}
	return res
}

// controllerStop is the stop-list for noun extraction in
// probeControllerAnchor. Service names and generic verbs would
// otherwise pollute the candidate list (e.g. "oscar" probing
// `oscar/web/controllers/oscar.go` is a wasted round trip).
var controllerStop = map[string]bool{
	"controller": true, "service": true, "endpoint": true, "endpoints": true,
	"serves": true, "serve": true, "the": true, "and": true, "for": true,
	"in": true, "on": true, "of": true, "what": true, "which": true,
	"where": true, "does": true, "do": true, "is": true, "are": true,
	"with": true, "from": true, "to": true, "its": true, "their": true,
	"response": true, "request": true,
}

// probeControllerAnchor handles the "which controller serves the
// <noun> endpoint in <svc>" query shape. It extracts noun-like
// tokens from the query (length-3+, not in controllerStop, not
// equal to svc) and probes `<svc>/web/controllers/<noun>.go` for
// each. Returns the first hit's path, or "" if nothing exists.
//
// Calibrated on goserving-002 ("Which controller serves the health
// endpoint in oscar"): noun=health → `oscar/web/controllers/health.go`
// is the gold answer and exists, so the anchor is injected.
func (r *Router) probeControllerAnchor(svc string, query string, repo string) string {
	svcLow := strings.ToLower(svc)
	cur := strings.Builder{}
	var tokens []string
	for _, ch := range query {
		if (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') {
			cur.WriteRune(ch)
			continue
		}
		if cur.Len() > 0 {
			tokens = append(tokens, strings.ToLower(cur.String()))
			cur.Reset()
		}
	}
	if cur.Len() > 0 {
		tokens = append(tokens, strings.ToLower(cur.String()))
	}

	seen := make(map[string]bool, len(tokens))
	for _, t := range tokens {
		if len(t) < 3 || controllerStop[t] || seen[t] {
			continue
		}
		if t == svcLow || strings.HasPrefix(svcLow, t) || strings.HasPrefix(t, svcLow) {
			continue
		}
		seen[t] = true
		path := svc + "/web/controllers/" + t + ".go"
		fr, err := r.Graph.ReadFile(path, repo)
		if err == nil && fr != nil && fr.Content != "" {
			return path
		}
	}
	return ""
}

// injectQueryAnchors materialises a small set of PrimaryContextEntry
// records when the query has high-precision repo-shape or
// service-entry-point signals. The pass is intent-CLASSIFIER-FREE:
// content gating is more reliable than the keyword intent classifier
// (which collapses ~85% of bench queries to "general" because they
// don't contain literal "debug"/"refactor" tokens). Wrong anchors
// burn rank slots, so the cost of over-firing is high — we err on
// the side of NOT injecting.
//
// Two passes:
//
//  1. Doc anchor — fires only when the query mentions repo-shape
//     concepts (monorepo, top-level, modules, services list, …).
//     Probes rootDocCandidates in order, takes the first existing
//     hit, returns AT MOST ONE entry.
//  2. Service entry-point anchor — fires only when both a known
//     top-level dir is referenced (extractServiceTokens) AND an
//     infrastructure verb appears (serviceTraceVerbs). Probes the
//     full serviceEntryPatterns suffix list under the matched
//     service(s), capped at 3 entries total.
//
// All probes go through codegraph's existing /api/file endpoint, so
// missing files fail fast (HTTP 404) and we only pay for hits.
// Returns nil for queries without precise signals — the hot path for
// most queries is one substring scan with no I/O.
func (r *Router) injectQueryAnchors(
	ctx context.Context, queryID, query, repo, intent string,
) []prompt.PrimaryContextEntry {
	if r == nil || r.Graph == nil {
		return nil
	}
	ql := strings.ToLower(query)
	out := make([]prompt.PrimaryContextEntry, 0, 4)

	// --- Doc-anchor pass (root README, AGENTS, ARCHITECTURE, …) ---
	// Untouched by the learner: it's a 3-element fixed portfolio
	// (one hit per query maximum) and the bench shows it works
	// across repos without per-repo tuning.
	if matchesAny(ql, docAnchorTriggers) {
		for _, path := range rootDocCandidates {
			fr, err := r.Graph.ReadFile(path, repo)
			if err == nil && fr != nil && fr.Content != "" {
				out = append(out, prompt.PrimaryContextEntry{
					Type:       "anchor",
					Name:       path,
					File:       path,
					Summary:    "Repo doc anchor",
					Confidence: 0.95,
				})
				break // one hit is enough; don't displace more
			}
		}
	}

	// --- Service entry-point pass (anchorlearn-driven) ---
	// The learner replaces the static suffix list. Decide() returns
	// already-probed Candidates whose Score blends:
	//   * cross-repo prior (fired/success across all repos)
	//   * per-repo posterior (fired/success in this repo)
	//   * keyword affinity (this query's tokens × this pattern)
	// plus an ε-greedy exploration slot to gather data on patterns
	// the system has never tried in this repo.
	//
	// Two-tier gate:
	//   1. Verb gate — query has a service-trace verb. Fires the FULL
	//      static + discovered portfolio. This is the cold-start path
	//      and the calibration target on goserving.
	//   2. Discovered-only fallback — query has only a service token
	//      (no verb), but the repo has at least one Phase-3 discovered
	//      pattern. This means the bandit *has already learned* that
	//      file paths under this service are worth retrieving for this
	//      repo, so we fire just those discovered patterns. Static
	//      patterns are NOT included here — they would over-fire on
	//      every "How does X in mall-portal work" type question.
	//      Without this fallback, mall's R@5 cannot benefit from any
	//      learning at all because mall's natural questions are
	//      operational ("how is JWT validated", "where are returns
	//      handled") rather than entry-point exploration ("where does
	//      mall-admin start its listener").
	verbHit := matchesAny(ql, serviceTraceVerbs)
	dirs := r.topLevelDirs(repo)
	tokens := extractServiceTokens(query, dirs)
	hasDiscovered := false
	if !verbHit && r.AnchorLearner != nil && len(tokens) > 0 {
		hasDiscovered = r.AnchorLearner.HasDiscovered(ctx, repo)
	}
	if (verbHit || hasDiscovered) && r.AnchorLearner != nil {
		if len(tokens) > 0 {
			const maxServiceAnchors = 3
			var candidates []anchorlearn.Candidate
			if verbHit {
				candidates = r.AnchorLearner.Decide(ctx, repo, query, intent, tokens, maxServiceAnchors)
			} else {
				candidates = r.AnchorLearner.DecideDiscoveredOnly(ctx, repo, query, intent, tokens, maxServiceAnchors)
			}
			recorded := make([]anchorlearn.Candidate, 0, len(candidates))
			for _, c := range candidates {
				summary := "Service entry point"
				if c.IsExploration {
					summary = "Service entry point (exploration)"
				}
				out = append(out, prompt.PrimaryContextEntry{
					Type:       "anchor",
					Name:       c.Path,
					File:       c.Path,
					Summary:    summary,
					Confidence: 0.95,
				})
				recorded = append(recorded, c)
			}

			// Controller-aware probe. Queries like "Which controller
			// serves the health endpoint in oscar" gold-list
			// `<svc>/web/controllers/<noun>.go`. Kept distinct from
			// the suffix-pattern learner because the right filename
			// varies per question (the noun is extracted from the
			// query itself). We add it as a separate observation
			// pattern so its success ratio is tracked independently.
			if len(out) < maxServiceAnchors+1 && strings.Contains(ql, "controller") {
				for _, svc := range tokens {
					if path := r.probeControllerAnchor(svc, query, repo); path != "" {
						out = append(out, prompt.PrimaryContextEntry{
							Type:       "anchor",
							Name:       path,
							File:       path,
							Summary:    "Controller entry point",
							Confidence: 0.95,
						})
						recorded = append(recorded, anchorlearn.Candidate{
							Service: svc,
							Path:    path,
							Pattern: anchorlearn.Pattern{
								ID:     "web/controllers/<noun>.go",
								Suffix: "web/controllers/<noun>.go",
								Source: "static",
							},
						})
						break
					}
				}
			}

			// Phase 1: persist what we actually injected so future
			// dev_feedback / memory_save signals can credit the
			// patterns that fired here. Done unconditionally — even
			// a zero-anchor decision is informative (reflects a
			// query that found no service-shaped match in this repo).
			if len(recorded) > 0 && queryID != "" {
				r.AnchorLearner.RecordObservation(ctx, queryID, repo, query, intent, recorded)
				r.AnchorLearner.NoteRecentQuery(repo, queryID)
			}
			if len(recorded) > 0 {
				log.Printf("[router] anchor: trace svc=%v added=%d query_id=%s",
					tokens, len(recorded), queryID)
			}
		}
	}

	if len(out) > 0 {
		log.Printf("[router] anchor: injected n=%d", len(out))
	}
	return out
}

// buildPrimaryContext converts agent memories into a flat list of PrimaryContextEntry.
// Each entry has a short summary to anchor Claude's attention, the full details,
// and a confidence score based on source quality and staleness.
func buildPrimaryContext(memRes memoryResults) []prompt.PrimaryContextEntry {
	if memRes.agent == nil {
		return nil
	}

	var entries []prompt.PrimaryContextEntry

	for _, f := range memRes.agent.Files {
		entries = append(entries, prompt.PrimaryContextEntry{
			Type:       "file",
			Name:       f.Path,
			File:       f.Path,
			Summary:    prompt.GenerateSummary(f.Purpose),
			Details:    f.Purpose,
			Confidence: confidence(f.Sim, f.Stale),
			Stale:      f.Stale,
		})
	}

	for _, f := range memRes.agent.Functions {
		details := f.Purpose
		if f.Callers != "" {
			details += "\nCallers: " + f.Callers
		}
		if f.Callees != "" {
			details += "\nCallees: " + f.Callees
		}

		entries = append(entries, prompt.PrimaryContextEntry{
			Type:       "func",
			Name:       f.Name,
			File:       f.File,
			Summary:    prompt.GenerateSummary(f.Purpose),
			Details:    details,
			Confidence: confidence(f.Sim, f.Stale),
			Stale:      f.Stale,
		})
	}

	for _, f := range memRes.agent.Flows {
		details := f.Purpose
		if f.Files != "" {
			details += "\nFiles: " + f.Files
		}
		if f.EntryPoints != "" {
			details += "\nEntry points: " + f.EntryPoints
		}

		entries = append(entries, prompt.PrimaryContextEntry{
			Type:       "flow",
			Name:       f.Name,
			Summary:    prompt.GenerateSummary(f.Purpose),
			Details:    details,
			Confidence: confidence(f.Sim, f.Stale),
			Stale:      f.Stale,
		})
	}

	// Sort by confidence descending
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Confidence > entries[j].Confidence
	})

	return entries
}

// applyFPPenalties consults the FP store for every candidate hit and
// adds a cosine-distance penalty in-place when the current query
// embedding is close to a memory's accumulated FP centroid. Returns the
// number of hits that received a non-zero penalty so the caller can log
// and re-floor.
//
// Best-effort: when FP lookup fails or the query embedding wasn't
// computed (e.g. repeat-detection skipped this query) we return 0 and
// leave scores untouched.
func applyFPPenalties(ctx context.Context, mem *memory.Store, hits []memory.MemoryHit, queryEmbed []float32) int {
	if mem == nil || len(hits) == 0 || len(queryEmbed) == 0 {
		return 0
	}
	keys := make([]string, 0, len(hits))
	for _, h := range hits {
		if h.Key != "" {
			keys = append(keys, h.Key)
		}
	}
	if len(keys) == 0 {
		return 0
	}
	fpMap, err := mem.BatchFalsePositiveSimilarity(ctx, keys, queryEmbed)
	if err != nil || len(fpMap) == 0 {
		return 0
	}
	demoted := 0
	for i := range hits {
		fp, ok := fpMap[hits[i].Key]
		if !ok {
			continue
		}
		penalty := memory.FPDistancePenalty(fp)
		if penalty <= 0 {
			continue
		}
		hits[i].Score += penalty
		demoted++
		log.Printf("[router] FP demote key=%s sim=%.3f count=%d penalty=%.3f new_score=%.3f",
			hits[i].Key, fp.Sim, fp.Count, penalty, hits[i].Score)
	}
	return demoted
}

// agentMemoryKeys returns the underlying Redis keys of every agent
// memory the response carries. Used to persist memory_keys onto the
// trace so dev_feedback can later attribute false positives to the
// specific records it returned.
func agentMemoryKeys(memRes memoryResults) []string {
	if memRes.agent == nil {
		return nil
	}
	var keys []string
	for _, f := range memRes.agent.Files {
		if f.Key != "" {
			keys = append(keys, f.Key)
		}
	}
	for _, f := range memRes.agent.Functions {
		if f.Key != "" {
			keys = append(keys, f.Key)
		}
	}
	for _, f := range memRes.agent.Flows {
		if f.Key != "" {
			keys = append(keys, f.Key)
		}
	}
	return keys
}

// agentSimilarityStats returns (topSim, meanSim) across all kept agent
// memories. Both are in [0,1]. (0,0) when there are no agent hits — the
// caller is responsible for not publishing a misleading "0.0" signal in
// that case (we just don't set the signal key when memCount==0).
func agentSimilarityStats(memRes memoryResults) (topSim, meanSim float64) {
	if memRes.agent == nil {
		return 0, 0
	}
	var sims []float64
	for _, f := range memRes.agent.Files {
		sims = append(sims, f.Sim)
	}
	for _, f := range memRes.agent.Functions {
		sims = append(sims, f.Sim)
	}
	for _, f := range memRes.agent.Flows {
		sims = append(sims, f.Sim)
	}
	if len(sims) == 0 {
		return 0, 0
	}
	sum := 0.0
	for _, s := range sims {
		if s > topSim {
			topSim = s
		}
		sum += s
	}
	meanSim = sum / float64(len(sims))
	return topSim, meanSim
}

// graphProximityFromTrace derives a [0,1] proximity score from the graph
// expansion stage's own counters. We use traced_symbols / seed_symbols —
// the fraction of seeds that produced at least one caller/callee edge —
// because it directly answers "did the graph have anything useful to say
// about the symbols we cared about?". Returns -1 when the stage didn't
// run or didn't record the counters (caller skips the signal).
func graphProximityFromTrace(stage *prompt.StageTrace) float64 {
	if stage == nil || stage.Details == nil {
		return -1
	}
	seed, sok := stage.Details["seed_symbols"].(int)
	traced, tok := stage.Details["traced_symbols"].(int)
	if !sok || !tok || seed <= 0 {
		return -1
	}
	gp := float64(traced) / float64(seed)
	if gp > 1 {
		gp = 1
	}
	if gp < 0 {
		gp = 0
	}
	return gp
}

// confidence is the per-entry relevance score reported on each
// PrimaryContextEntry. Replaces the previous source-quality lookup
// (which was a fixed 0.9 for any agent-written memory regardless of
// query relevance — the cause of the lying signals reported in
// retrieval_trace.signals before this change).
//
// sim is the cosine similarity between the query embedding and the
// memory's stored embedding, in [0,1]. Stale memories (whose underlying
// file has changed since the memory was saved) are damped by 0.6 to
// reflect the increased risk of stale knowledge.
func confidence(sim float64, stale bool) float64 {
	c := sim
	if c < 0 {
		c = 0
	}
	if c > 1 {
		c = 1
	}
	if stale {
		c *= 0.6
	}
	return c
}

// SaveMemory persists a symbol summary for future recall (legacy).
func (r *Router) SaveMemory(symbol, file, summary string) error {
	if symbol == "" || summary == "" {
		return fmt.Errorf("symbol and summary are required")
	}
	return r.Memory.Save(memory.Entry{
		Symbol:  symbol,
		File:    file,
		Summary: summary,
	})
}

// SaveFileMemory persists a file-level memory.
func (r *Router) SaveFileMemory(repo, path, purpose, scope string) error {
	if repo == "" || path == "" || purpose == "" {
		return fmt.Errorf("repo, path, and purpose are required")
	}
	if scope == "" {
		scope = memory.ScopeForFile(r.Graph.RepoPath(repo), path)
	}
	err := r.Memory.SaveFile(memory.FileMemory{
		Repo:     repo,
		Path:     path,
		Purpose:  purpose,
		Source:   "agent",
		Scope:    scope,
		RepoPath: r.Graph.RepoPath(repo),
	})
	// Implicit reward signal: the agent caring enough about a file
	// to save a memory for it is a strong "this file was useful"
	// signal. Drives both Phase-2 reward and Phase-3 discovery in
	// the anchor learner. Best-effort, fires after a successful
	// save so a Redis hiccup on the learner side can't fail the
	// memory write.
	if err == nil && r.AnchorLearner != nil {
		r.AnchorLearner.RewardMemorySave(context.Background(), repo, path)
	}
	return err
}

// SaveFuncMemory persists a function-level memory.
func (r *Router) SaveFuncMemory(repo, name, file, purpose, callers, callees, scope string) error {
	if repo == "" || name == "" || purpose == "" {
		return fmt.Errorf("repo, name, and purpose are required")
	}
	if scope == "" {
		scope = memory.ScopeForFile(r.Graph.RepoPath(repo), file)
	}
	err := r.Memory.SaveFunc(memory.FuncMemory{
		Repo:     repo,
		Name:     name,
		File:     file,
		Purpose:  purpose,
		Callers:  callers,
		Callees:  callees,
		Source:   "agent",
		Scope:    scope,
		RepoPath: r.Graph.RepoPath(repo),
	})
	if err == nil && r.AnchorLearner != nil && file != "" {
		r.AnchorLearner.RewardMemorySave(context.Background(), repo, file)
	}
	return err
}

// SaveFlowMemory persists a flow-level memory.
//
// In addition to the user-supplied fields, SaveFlowMemory snapshots the
// codegraph neighbourhood (callers, callees, importers, extends,
// methods) around each entry point and freezes it onto the FlowMemory.
// That snapshot is what the dashboard renders as the per-flow graph,
// giving the human reader the same call-chain view the LLM saw during
// dev_context for this flow's seed symbols — without re-querying
// codegraph at view time (which would be slow) or losing the view
// when codegraph is reindexed (which would be lossy).
//
// The snapshot is best-effort: any failure (entry_points empty,
// codegraph unreachable, all queries returning nothing) is logged and
// swallowed so the flow itself still saves cleanly. Without a
// snapshot, the dashboard falls back to the legacy bipartite SVG.
func (r *Router) SaveFlowMemory(repo, name, purpose, files, entryPoints, scope string) error {
	if repo == "" || name == "" || purpose == "" {
		return fmt.Errorf("repo, name, and purpose are required")
	}
	if scope == "" {
		scope = memory.ScopeForFiles(r.Graph.RepoPath(repo), files)
	}

	subgraphJSON := snapshotFlowSubgraph(r.Graph, repo, entryPoints, name)

	err := r.Memory.SaveFlow(memory.FlowMemory{
		Repo:         repo,
		Name:         name,
		Purpose:      purpose,
		Files:        files,
		EntryPoints:  entryPoints,
		Source:       "agent",
		Scope:        scope,
		SubgraphJSON: subgraphJSON,
	})
	// Flow memories list multiple files (CSV) — credit each one as
	// an implicit "useful" signal for whichever pattern anchored it.
	if err == nil && r.AnchorLearner != nil {
		for _, f := range splitCSV(files) {
			r.AnchorLearner.RewardMemorySave(context.Background(), repo, f)
		}
	}
	return err
}

// snapshotFlowSubgraph captures the codegraph neighbourhood for the
// flow's entry-point symbols and returns it as a JSON string ready to
// drop into FlowMemory.SubgraphJSON. Returns "" on any failure — that
// signals the dashboard to fall back to the existing bipartite SVG
// without breaking the save path.
func snapshotFlowSubgraph(graph *codegraph.Client, repo, entryPoints, flowName string) string {
	if graph == nil || repo == "" {
		return ""
	}
	seeds := splitCSV(entryPoints)
	if len(seeds) == 0 {
		return ""
	}
	sg, err := graph.Subgraph(repo, seeds)
	if err != nil {
		log.Printf("[flow] subgraph snapshot %q (%s): %v (non-fatal)", flowName, repo, err)
		return ""
	}
	if sg == nil || (len(sg.Nodes) == 0 && len(sg.Edges) == 0) {
		// Codegraph reached but nothing relevant — common when the
		// agent supplied entry_points that aren't in the symbol
		// table (typos, qualified names like "pkg.Func" vs bare
		// "Func", or flows describing routes/configs that aren't
		// node-shaped). Quietly return "" rather than persisting
		// an empty subgraph that just adds bytes for no UI value.
		return ""
	}
	raw, err := json.Marshal(sg)
	if err != nil {
		log.Printf("[flow] subgraph marshal %q: %v (non-fatal)", flowName, err)
		return ""
	}
	return string(raw)
}

// SaveDecisionMemory persists a developer decision.
// Returns a list of conflict warnings (save is not blocked by conflicts).
// The scope parameter is auto-detected if empty: "global" if files are unchanged vs origin/release, else current branch.
func (r *Router) SaveDecisionMemory(repo, name, decisionType, decision, rationale, alternatives, constraint, decScope, files, scope string) ([]string, error) {
	if repo == "" || name == "" || decisionType == "" || decision == "" || rationale == "" {
		return nil, fmt.Errorf("repo, name, decision_type, decision, and rationale are required")
	}
	if scope == "" {
		scope = memory.ScopeForDecision(r.Graph.RepoPath(repo), files)
	}
	return r.Memory.SaveDecision(memory.DecisionMemory{
		Repo:         repo,
		Name:         name,
		DecisionType: decisionType,
		Decision:     decision,
		Rationale:    rationale,
		Alternatives: alternatives,
		Constraint:   constraint,
		Scope:        scope,
		Files:        files,
		Source:       "agent",
	})
}

// ListDecisionMemory returns decisions filtered by optional criteria.
// Shows both active and superseded decisions to display full history.
func (r *Router) ListDecisionMemory(repo, decisionType, scope, files string) []memory.MemoryHit {
	if repo == "" {
		return nil
	}
	return r.Memory.ListDecisions(repo, decisionType, scope, files, true) // include superseded
}

// SupersedeDecision marks an existing decision as superseded by a new decision.
func (r *Router) SupersedeDecision(repo, oldName, newName string) error {
	if repo == "" || oldName == "" || newName == "" {
		return fmt.Errorf("repo, old_name, and new_name are required")
	}
	return r.Memory.SupersedeDecision(repo, oldName, newName)
}

// PopulateMemories triggers auto-population from GitNexus for a repo.
func (r *Router) PopulateMemories(repo string, maxFiles, maxFuncs, maxFlows int) error {
	if repo == "" {
		return fmt.Errorf("repo is required")
	}
	return memory.Populate(r.Memory, memory.PopulateConfig{
		Repo:      repo,
		GnBaseURL: r.Graph.BaseURL,
		MaxFiles:  maxFiles,
		MaxFuncs:  maxFuncs,
		MaxFlows:  maxFlows,
	})
}
