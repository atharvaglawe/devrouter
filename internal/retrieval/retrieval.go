// Package retrieval defines the tool-agnostic contract that every
// DevRouter retrieval source implements. It is a leaf package: it
// depends only on internal/prompt (for the shared output shapes) and
// the standard library, so both the concrete sources (memory,
// codegraph, the generic MCP source) and the router orchestrator can
// import it without creating an import cycle.
//
// The design goal is "every tool is at the same level": memory,
// codegraph, and the external MCP tools (cmdocs, GitLab, …) all satisfy
// Source, and the router holds them in a single registry. Adding a new
// tool is one registry entry, not a new branch in the pipeline.
//
// Execution is tiered. Memory is cheap and knowledge-bearing, so it runs
// first purely to emit Signals (recalled paths/symbols/terms). The
// orchestrator folds those Signals into the shared Request and then fans
// every Source out in parallel, so a tool that wants to be "aimed" by
// memory can read Request.Signals.
package retrieval

import (
	"context"

	"github.com/atharva-ag/devrouter/internal/prompt"
)

// Signals are the lightweight hints a SignalProducer (today: memory)
// emits in the pre-step so the other tools can target their retrieval.
//   - Paths   — file paths recalled from memory (graph traversal seeds,
//     doc-collection bias).
//   - Symbols — symbol names recalled from memory (graph traversal seeds).
//   - Terms   — extra search terms (plan must/should terms) appended to
//     an external tool's query string.
type Signals struct {
	Paths   []string
	Symbols []string
	Terms   []string
}

// Merge folds other into s, de-duplicating each slice. Used by the
// orchestrator to accumulate signals from multiple producers.
func (s *Signals) Merge(other Signals) {
	s.Paths = dedupAppend(s.Paths, other.Paths)
	s.Symbols = dedupAppend(s.Symbols, other.Symbols)
	s.Terms = dedupAppend(s.Terms, other.Terms)
}

func dedupAppend(dst, src []string) []string {
	seen := make(map[string]bool, len(dst))
	for _, v := range dst {
		seen[v] = true
	}
	for _, v := range src {
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		dst = append(dst, v)
	}
	return dst
}

// Request is the single, immutable input handed to every Source for a
// query. It carries the shared query embedding (so no tool re-embeds),
// the sanitized plan fields (plain slices, deliberately NOT
// router.QueryPlan, to keep this package a leaf), and the Signals
// produced by the tier-1 pre-step.
type Request struct {
	Query  string
	Repo   string
	Branch string
	Embed  []float32
	Intent string

	MustTerms    []string
	ShouldTerms  []string
	ExcludeTerms []string
	ContextHints []string

	Signals Signals

	// MaxDocs is the per-source doc breadth for this call, supplied by
	// the source-breadth bandit (0 = use the source's own default). The
	// orchestrator sets it per source by copying the Request before each
	// fan-out goroutine.
	MaxDocs int

	// Expand is set when this query is a repeat-exploration, asking
	// sources to widen their retrieval for this call.
	Expand bool
}

// Result is a union of optional contributions: each Source fills only
// the fields it produces (memory → PrimaryContext/Decisions, codegraph →
// Symbols/Snippets/CallChain/Graph, the MCP tools → Docs). The
// orchestrator merges whatever is set into the final prompt.DevPrompt.
type Result struct {
	PrimaryContext []prompt.PrimaryContextEntry
	Decisions      []prompt.DecisionContextEntry
	Symbols        []string
	Snippets       []prompt.Snippet
	CallChain      *prompt.CallChain
	Graph          *prompt.GraphLinks
	ImpactRadius   []string
	Docs           []prompt.DocEntry

	// LatencyMs is set by the orchestrator (wall-clock around Search),
	// not by the source itself, and surfaced per-tool in the trace.
	LatencyMs int64
}

// Source is the read/search surface every retrieval tool implements.
// Name must be stable and low-cardinality (used as a trace/metric
// label). Search must be context-aware (the orchestrator imposes a
// per-tool timeout) and must degrade gracefully — returning an empty
// Result rather than blocking the whole query when its backend is down.
type Source interface {
	Name() string
	Search(ctx context.Context, req Request) (Result, error)
}

// SignalProducer is an optional capability a Source may also implement
// to participate in the tier-1 pre-step. The orchestrator calls Signals
// before the parallel fan-out and folds the result into Request.Signals.
// Memory implements it; nothing else is required to.
type SignalProducer interface {
	Signals(ctx context.Context, req Request) (Signals, error)
}

// Breadth is an optional capability a doc Source implements to expose
// its default per-call doc cap. The orchestrator uses it to seed the
// per-source breadth bandit (heuristics.SourceBandit).
type Breadth interface {
	DefaultDocs() int
}
