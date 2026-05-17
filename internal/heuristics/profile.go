// Package heuristics owns the per-intent budget/trim profile that
// governs how much graph traversal devrouter performs and how
// aggressively it trims the assembled context.
//
// In Phase 1 the profile is just a snapshot of the previously
// hand-tuned constants in router.graphBudgetFromMemory and
// router.trimCaps; the bandit machinery exists but performs no
// perturbation. Phase 2+ enable per-knob ε-perturbation, K-sample
// promotion, and 3-strike rollback through the same Picker API.
//
// Hard min/max bounds (see Bounds) are enforced on every Profile
// read via Profile.Clip(), so even corrupted Redis state cannot
// produce a profile outside the safe envelope.
package heuristics

import (
	"fmt"
	"hash/fnv"
)

// Profile holds the tunable knobs for a single (intent, query_shape)
// bucket. The shape segment is reserved for v2 and is always "*"
// in v1.
type Profile struct {
	// Graph budget knobs
	MaxTrace   int `json:"max_trace"`
	CallerHops int `json:"caller_hops"`

	// Trim cap knobs
	MaxUpstream   int `json:"max_upstream"`
	MaxDownstream int `json:"max_downstream"`
	MaxImporters  int `json:"max_importers"`
	MaxMethods    int `json:"max_methods"`
	MaxSiblings   int `json:"max_siblings"`
	MaxSnippets   int `json:"max_snippets"`
	MaxImpact     int `json:"max_impact"`
	MaxSymbols    int `json:"max_symbols"`
	MaxPrimaryCtx int `json:"max_primary_ctx"`
	MaxDecisions  int `json:"max_decisions"`
}

// Bounds defines the (min, max) hard guardrails for every Profile knob.
// The bandit cannot escape these even if Redis is corrupted; Profile.Clip()
// enforces them on every read.
var Bounds = struct {
	MaxTrace      [2]int
	CallerHops    [2]int
	MaxUpstream   [2]int
	MaxDownstream [2]int
	MaxImporters  [2]int
	MaxMethods    [2]int
	MaxSiblings   [2]int
	MaxSnippets   [2]int
	MaxImpact     [2]int
	MaxSymbols    [2]int
	MaxPrimaryCtx [2]int
	MaxDecisions  [2]int
}{
	MaxTrace:      [2]int{3, 8},
	CallerHops:    [2]int{0, 3},
	MaxUpstream:   [2]int{3, 25},
	MaxDownstream: [2]int{2, 15},
	MaxImporters:  [2]int{3, 20},
	MaxMethods:    [2]int{3, 20},
	MaxSiblings:   [2]int{3, 30},
	MaxSnippets:   [2]int{1, 10},
	MaxImpact:     [2]int{5, 30},
	MaxSymbols:    [2]int{3, 25},
	MaxPrimaryCtx: [2]int{3, 25},
	MaxDecisions:  [2]int{3, 10},
}

// Default returns the seed profile for an intent. Mirrors the previously
// hand-tuned constants in router.graphBudgetFromMemory and router.trimCaps
// so behaviour at startup is identical to the pre-heuristics baseline.
func Default(intent string) Profile {
	p := Profile{
		MaxTrace:      5,
		CallerHops:    2,
		MaxUpstream:   10,
		MaxDownstream: 5,
		MaxImporters:  10,
		MaxMethods:    8,
		MaxSiblings:   15,
		// Snippet cap raised from 5 to 10 to match the recall budget
		// most agents expect from a "give me the relevant files" call.
		// ApplyMemoryShrink still tightens this aggressively when there
		// are strong primary memories (drops to 2 with memCount≥1, then
		// to 1 with memCount≥3), so the interactive prompt-economy
		// story is preserved — only the cold-start / bench path widens.
		MaxSnippets:   10,
		MaxImpact:     15,
		MaxSymbols:    20,
		MaxPrimaryCtx: 10,
		MaxDecisions:  5,
	}
	switch intent {
	case "debug":
		p.MaxUpstream = 20
		p.MaxDownstream = 10
		p.MaxSnippets = 7
		if p.MaxTrace < 4 {
			p.MaxTrace = 4
		}
		p.CallerHops = 2
	case "explore":
		p.MaxSiblings = 25
		p.MaxUpstream = 5
		p.MaxDownstream = 3
	case "trace":
		p.MaxUpstream = 20
		p.MaxDownstream = 10
		p.MaxImporters = 15
		if p.MaxTrace < 4 {
			p.MaxTrace = 4
		}
		p.CallerHops = 2
	case "refactor":
		p.MaxImpact = 25
		p.MaxImporters = 15
		p.MaxMethods = 12
	}
	return p.Clip()
}

// Clip enforces hard min/max bounds on every knob. Always called
// before a profile is used by the router.
func (p Profile) Clip() Profile {
	p.MaxTrace = clipInt(p.MaxTrace, Bounds.MaxTrace[0], Bounds.MaxTrace[1])
	p.CallerHops = clipInt(p.CallerHops, Bounds.CallerHops[0], Bounds.CallerHops[1])
	p.MaxUpstream = clipInt(p.MaxUpstream, Bounds.MaxUpstream[0], Bounds.MaxUpstream[1])
	p.MaxDownstream = clipInt(p.MaxDownstream, Bounds.MaxDownstream[0], Bounds.MaxDownstream[1])
	p.MaxImporters = clipInt(p.MaxImporters, Bounds.MaxImporters[0], Bounds.MaxImporters[1])
	p.MaxMethods = clipInt(p.MaxMethods, Bounds.MaxMethods[0], Bounds.MaxMethods[1])
	p.MaxSiblings = clipInt(p.MaxSiblings, Bounds.MaxSiblings[0], Bounds.MaxSiblings[1])
	p.MaxSnippets = clipInt(p.MaxSnippets, Bounds.MaxSnippets[0], Bounds.MaxSnippets[1])
	p.MaxImpact = clipInt(p.MaxImpact, Bounds.MaxImpact[0], Bounds.MaxImpact[1])
	p.MaxSymbols = clipInt(p.MaxSymbols, Bounds.MaxSymbols[0], Bounds.MaxSymbols[1])
	p.MaxPrimaryCtx = clipInt(p.MaxPrimaryCtx, Bounds.MaxPrimaryCtx[0], Bounds.MaxPrimaryCtx[1])
	p.MaxDecisions = clipInt(p.MaxDecisions, Bounds.MaxDecisions[0], Bounds.MaxDecisions[1])
	return p
}

// ApplyMemoryShrink mirrors the strong-memory shrink rules previously
// applied inside graphBudgetFromMemory and trimCaps. Lives outside the
// bandit so it doesn't have to relearn that strong primary memory means
// a tighter prompt is fine.
//
// The input is intentionally *file-pointing* memory count, not the raw
// memCount: only `file` and `func` memories carry a path the agent (or
// the bench) can resolve. `flow` and `decision` memories trigger on
// almost every query in repos seeded with generic process flows (see
// bench/memories/mall.jsonl) but their primary_context entries have
// no `file` field, so shrinking the graph budget on their account
// crushes recall without a corresponding gain in memory-derived
// confidence. The original heuristic over-counted those types and
// dropped the bench-visible budget to ≤4 files; the funnel diagnosis
// is in bench/results/_funnel.stderr.log + canvas methodology notes.
func (p Profile) ApplyMemoryShrink(filePointingMemCount int) Profile {
	if filePointingMemCount >= 3 {
		if p.MaxTrace > 2 {
			p.MaxTrace = 2
		}
		if p.CallerHops > 1 {
			p.CallerHops = 1
		}
	} else if filePointingMemCount >= 2 {
		if p.MaxTrace > 3 {
			p.MaxTrace = 3
		}
		if p.CallerHops > 1 {
			p.CallerHops = 1
		}
	}

	if filePointingMemCount >= 1 {
		if p.MaxSymbols > 5 {
			p.MaxSymbols = 5
		}
		if p.MaxSnippets > 2 {
			p.MaxSnippets = 2
		}
		if p.MaxSiblings > 5 {
			p.MaxSiblings = 5
		}
	}
	if filePointingMemCount >= 3 {
		if p.MaxSnippets > 1 {
			p.MaxSnippets = 1
		}
		if p.MaxSiblings > 3 {
			p.MaxSiblings = 3
		}
		if p.MaxSymbols > 3 {
			p.MaxSymbols = 3
		}
	}
	return p
}

// ID returns a stable identifier for a profile based on its knob values.
// Two profiles with identical knobs share the same ID, which is the join
// key on heuristics:reward:* rows.
func (p Profile) ID() string {
	h := fnv.New64a()
	fmt.Fprintf(h, "%d|%d|%d|%d|%d|%d|%d|%d|%d|%d|%d|%d",
		p.MaxTrace, p.CallerHops,
		p.MaxUpstream, p.MaxDownstream, p.MaxImporters, p.MaxMethods,
		p.MaxSiblings, p.MaxSnippets, p.MaxImpact, p.MaxSymbols,
		p.MaxPrimaryCtx, p.MaxDecisions,
	)
	return fmt.Sprintf("%016x", h.Sum64())
}

func clipInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
