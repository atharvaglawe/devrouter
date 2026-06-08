package router

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/atharva-ag/devrouter/internal/codegraph"
	"github.com/atharva-ag/devrouter/internal/heuristics"
	"github.com/atharva-ag/devrouter/internal/memory"
	"github.com/atharva-ag/devrouter/internal/prompt"
	"github.com/atharva-ag/devrouter/internal/retrieval"
	"github.com/atharva-ag/devrouter/internal/telemetry"
)

// This file makes the two native retrieval tools — memory and codegraph
// — satisfy the same retrieval.Source contract as the external MCP
// tools, so "every tool is at the same level": one interface, one
// registry. The adapters wrap the existing helper chain rather than
// re-implementing it, so behaviour stays identical to the inline
// pipeline in HandleQueryWithPlan.
//
// Note on wiring: the production query path in HandleQueryWithPlan still
// runs memory + codegraph inline (it threads extra steps — query
// anchors, decision lineage, the heuristics budget — that are
// orchestrated at the router level, not per-source). These adapters are
// the interface-conformance + introspection layer and the migration
// target; memorySource.Signals is the canonical signal-production path
// (the inline pipeline produces the equivalent via memRes.autoHints).

// memorySource adapts the Redis memory store to retrieval.Source and,
// because memory is the cheap recall layer, also to
// retrieval.SignalProducer.
type memorySource struct{ r *Router }

func (m memorySource) Name() string { return "memory" }

// recall runs the same three-stage discipline the inline pipeline uses
// (floor -> plan filter -> FP demotion -> re-floor -> plan re-rank).
func (m memorySource) recall(ctx context.Context, req retrieval.Request) []memory.MemoryHit {
	plan := planFromRequest(req)
	var raw []memory.MemoryHit
	if len(req.Embed) > 0 {
		raw = m.r.Memory.SearchAllWithEmbed(req.Embed, req.Repo, req.Branch)
	} else {
		raw = m.r.Memory.SearchAll(req.Query, req.Repo, req.Branch)
	}
	maxDist := memoryMaxDistance()
	hits := dropBelowFloor(raw, maxDist)
	hits = filterMemoriesByPlan(hits, plan)
	if applyFPPenalties(ctx, m.r.Memory, hits, req.Embed) > 0 {
		hits = dropBelowFloor(hits, maxDist)
	}
	return rankByPlan(hits, plan)
}

func (m memorySource) Search(ctx context.Context, req retrieval.Request) (retrieval.Result, error) {
	if m.r == nil || m.r.Memory == nil {
		return retrieval.Result{}, nil
	}
	memRes := buildAllMemories(m.recall(ctx, req), m.r.Graph.RepoPath(req.Repo))
	return retrieval.Result{PrimaryContext: buildPrimaryContext(memRes)}, nil
}

func (m memorySource) Signals(ctx context.Context, req retrieval.Request) (retrieval.Signals, error) {
	if m.r == nil || m.r.Memory == nil {
		return retrieval.Signals{}, nil
	}
	memRes := buildAllMemories(m.recall(ctx, req), m.r.Graph.RepoPath(req.Repo))
	return signalsFromAutoHints(memRes.autoHints), nil
}

// codegraphSource adapts the codegraph HTTP client (search cascade +
// snippet extraction) to retrieval.Source. It consumes Request.Signals
// — recalled symbols/paths bias the name search the same way the inline
// pipeline merges memRes.autoHints into the traversal seeds.
type codegraphSource struct{ r *Router }

func (c codegraphSource) Name() string { return "codegraph" }

func (c codegraphSource) Search(ctx context.Context, req retrieval.Request) (retrieval.Result, error) {
	if c.r == nil || c.r.Graph == nil {
		return retrieval.Result{}, nil
	}
	plan := planFromRequest(req)
	eq := buildEffectiveQuery(req.Query, plan)
	opts := codegraph.SearchOpts{
		MustTerms:    req.MustTerms,
		ExcludeTerms: req.ExcludeTerms,
		ContextHints: req.ContextHints,
	}
	results, err := c.r.Graph.SearchByNameWithOpts(eq, req.Repo, 10, opts)
	if err != nil || len(results) == 0 {
		if r2, e2 := c.r.Graph.Search(eq, req.Repo, 10); e2 == nil && len(r2) > 0 {
			results = r2
		}
	}
	return retrieval.Result{
		Symbols:  codegraph.SymbolNames(results),
		Snippets: codegraph.ToSnippets(results, 10),
	}, nil
}

// planFromRequest reconstructs the router QueryPlan from the leaf
// retrieval.Request's plain plan slices so the existing plan-aware
// helpers can be reused unchanged.
func planFromRequest(req retrieval.Request) QueryPlan {
	return QueryPlan{
		MustTerms:    req.MustTerms,
		ShouldTerms:  req.ShouldTerms,
		ExcludeTerms: req.ExcludeTerms,
		ContextHints: req.ContextHints,
	}
}

// signalsFromAutoHints splits memory's auto-hint strings into path-like
// vs symbol-like buckets. Paths contain a separator ("/" or "."); the
// rest are treated as symbol names. This mirrors why the inline pipeline
// merges these as traversal seeds (see router.go autoHints handling).
func signalsFromAutoHints(hints []string) retrieval.Signals {
	var sig retrieval.Signals
	for _, h := range hints {
		if h == "" {
			continue
		}
		if strings.ContainsAny(h, "/.") {
			sig.Paths = append(sig.Paths, h)
		} else {
			sig.Symbols = append(sig.Symbols, h)
		}
	}
	return sig
}

// buildRetrievalRequest assembles the single shared Request handed to
// every external source, folding memory's recalled auto-hints into
// Signals (tier-1 → tier-2). The plan must/should terms also ride along
// as Signals.Terms so a tool with no path/symbol awareness still gets
// the extra search vocabulary.
func (r *Router) buildRetrievalRequest(query, repo, branch string, embed []float32, intent string, plan QueryPlan, autoHints []string) retrieval.Request {
	sig := signalsFromAutoHints(autoHints)
	sig.Terms = dedupStrings(append(append([]string{}, plan.MustTerms...), plan.ShouldTerms...))
	return retrieval.Request{
		Query:        query,
		Repo:         repo,
		Branch:       branch,
		Embed:        embed,
		Intent:       intent,
		MustTerms:    plan.MustTerms,
		ShouldTerms:  plan.ShouldTerms,
		ExcludeTerms: plan.ExcludeTerms,
		ContextHints: plan.ContextHints,
		Signals:      sig,
	}
}

// fetchDocSources runs every registered external source in parallel
// (no gating), each under r.sourceTimeout, and returns the merged
// DocEntries plus a per-source StageTrace (latency, docs, errors). A
// failing/slow tool contributes nothing and never blocks the others.
//
// breadths is the per-source doc-cap chosen by the breadth bandit
// (source Name -> cap); each source gets its own Request copy so it can
// be tuned independently. req.Expand (repeat-exploration) is carried on
// every copy.
func (r *Router) fetchDocSources(req retrieval.Request, breadths map[string]int) ([]prompt.DocEntry, map[string]*prompt.StageTrace) {
	if len(r.Sources) == 0 {
		return nil, nil
	}
	results := make([]retrieval.Result, len(r.Sources))
	errs := make([]error, len(r.Sources))

	var wg sync.WaitGroup
	for i, src := range r.Sources {
		wg.Add(1)
		go func(i int, src retrieval.Source) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), r.sourceTimeout)
			defer cancel()
			sreq := req // per-source copy so MaxDocs varies independently
			if b, ok := breadths[src.Name()]; ok && b > 0 {
				sreq.MaxDocs = b
			}
			t0 := time.Now()
			res, err := src.Search(ctx, sreq)
			res.LatencyMs = time.Since(t0).Milliseconds()
			results[i] = res
			errs[i] = err
		}(i, src)
	}
	wg.Wait()

	var docs []prompt.DocEntry
	stages := make(map[string]*prompt.StageTrace, len(r.Sources))
	for i, src := range r.Sources {
		name := src.Name()
		st := &prompt.StageTrace{
			LatencyMs:     results[i].LatencyMs,
			CandidatesOut: len(results[i].Docs),
		}
		if b, ok := breadths[name]; ok && b > 0 {
			st.Details = map[string]interface{}{"max_docs": b}
			if req.Expand {
				st.Details["expanded"] = true
			}
		}
		status := "ok"
		if errs[i] != nil {
			st.Warnings = []string{errs[i].Error()}
			status = "error"
		}
		stages[name] = st
		docs = append(docs, results[i].Docs...)
		telemetry.SourceRequests.WithLabelValues(name, status).Inc()
		telemetry.StageDuration.WithLabelValues("docs", req.Intent).Observe(float64(results[i].LatencyMs) / 1000.0)
	}
	return docs, stages
}

// widenBreadths bumps every source's breadth by ~50% (at least +1),
// clipped to the bandit's upper bound, for a repeat-exploration call.
// Falls back to each source's seed default when no learned value is
// present (e.g. heuristics off).
func widenBreadths(breadths map[string]int, seeds []heuristics.SourceSeed) map[string]int {
	out := make(map[string]int, len(seeds))
	for _, sd := range seeds {
		base := breadths[sd.Name]
		if base <= 0 {
			base = sd.Default
		}
		if base <= 0 {
			base = 5
		}
		widened := base + base/2
		if widened <= base {
			widened = base + 1
		}
		if widened > heuristics.SourceDocsBounds[1] {
			widened = heuristics.SourceDocsBounds[1]
		}
		out[sd.Name] = widened
	}
	return out
}

// sourceSeeds builds the breadth-bandit seed list from the registered
// doc sources: each source's Name plus its default doc cap (via the
// optional retrieval.Breadth capability).
func (r *Router) sourceSeeds() []heuristics.SourceSeed {
	if len(r.Sources) == 0 {
		return nil
	}
	seeds := make([]heuristics.SourceSeed, 0, len(r.Sources))
	for _, src := range r.Sources {
		def := 0
		if b, ok := src.(retrieval.Breadth); ok {
			def = b.DefaultDocs()
		}
		seeds = append(seeds, heuristics.SourceSeed{Name: src.Name(), Default: def})
	}
	return seeds
}

func dedupStrings(xs []string) []string {
	seen := make(map[string]bool, len(xs))
	out := xs[:0]
	for _, x := range xs {
		if x == "" || seen[x] {
			continue
		}
		seen[x] = true
		out = append(out, x)
	}
	return out
}

// Compile-time assertions that the native tools satisfy the contract.
var (
	_ retrieval.Source         = memorySource{}
	_ retrieval.SignalProducer = memorySource{}
	_ retrieval.Source         = codegraphSource{}
)
