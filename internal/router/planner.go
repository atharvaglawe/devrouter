package router

import (
	"strings"

	"github.com/atharva-ag/devrouter/internal/telemetry"
)

// This file defines the sanitization pipeline that every QueryPlan goes
// through before it reaches retrieval, regardless of where the plan
// originated. Today the only origin is the MCP caller (the agent fills
// in dev_context's `plan` argument). Anything that produces a plan in
// the future (test fixtures, batch tooling, etc.) MUST run it through
// SanitizePlan so the contract is one place, not many.
//
// The caps below are not aesthetic. The downstream scoring in
// HandleQuery is calibrated against plans that look like:
//   must_terms     1-2  (hard anchors; too many collapses recall)
//   should_terms   0-6  (synonym expansion; diluted past 6)
//   exclude_terms  0-3  (test/mock/fixture noise filters)
//   phrases        0-3  (verbatim multi-word matches)
//   context_hints  0-3  (soft path biases, never hard filters)
// Without these caps an enthusiastic model will happily emit 12
// should_terms, which makes the scoring noisier, not better.
const (
	maxMustTerms    = 2
	maxShouldTerms  = 6
	maxExcludeTerms = 3
	maxPhrases      = 3
	maxContextHints = 3
)

// SanitizePlan is the single entry point for normalising a QueryPlan
// before it drives retrieval. It lowercases, trims, dedups, enforces
// length caps per field, and drops obvious garbage. Safe to call on a
// zero-valued QueryPlan (returns a zero-valued QueryPlan).
//
// Per-field drop counters are emitted so a sustained "agent is sending
// 12 should_terms" regression shows up in the metrics surface before
// it visibly degrades retrieval. Counters increment on the diff
// between sanitized-input length and sanitized-output length, so
// reordering / lowercasing on its own doesn't trip them.
func SanitizePlan(in QueryPlan) QueryPlan {
	must := sanitizeTerms(in.MustTerms)
	should := sanitizeTerms(in.ShouldTerms)
	exclude := sanitizeTerms(in.ExcludeTerms)
	phrases := sanitizePhrases(in.Phrases)
	hints := sanitizeHints(in.ContextHints)

	recordPlanDrop("must_terms", len(in.MustTerms), len(must))
	recordPlanDrop("should_terms", len(in.ShouldTerms), len(should))
	recordPlanDrop("exclude_terms", len(in.ExcludeTerms), len(exclude))
	recordPlanDrop("phrases", len(in.Phrases), len(phrases))
	recordPlanDrop("context_hints", len(in.ContextHints), len(hints))

	mustCapped := capTerms(must, maxMustTerms)
	shouldCapped := capTerms(should, maxShouldTerms)
	excludeCapped := capTerms(exclude, maxExcludeTerms)
	phrasesCapped := capTerms(phrases, maxPhrases)
	hintsCapped := capTerms(hints, maxContextHints)

	recordPlanDrop("must_terms", len(must), len(mustCapped))
	recordPlanDrop("should_terms", len(should), len(shouldCapped))
	recordPlanDrop("exclude_terms", len(exclude), len(excludeCapped))
	recordPlanDrop("phrases", len(phrases), len(phrasesCapped))
	recordPlanDrop("context_hints", len(hints), len(hintsCapped))

	return QueryPlan{
		MustTerms:    mustCapped,
		ShouldTerms:  shouldCapped,
		ExcludeTerms: excludeCapped,
		Phrases:      phrasesCapped,
		ContextHints: hintsCapped,
	}
}

func recordPlanDrop(field string, before, after int) {
	if drop := before - after; drop > 0 {
		telemetry.SanitizePlanDrops.WithLabelValues(field).Add(float64(drop))
	}
}

// sanitizeTerms lowercases, trims, dedups, drops empty/garbage entries.
// Keeps short tokens (>=2 chars) so domain abbreviations like "fms" survive.
// Multi-word strings are dropped; use sanitizePhrases for those.
func sanitizeTerms(in []string) []string {
	seen := make(map[string]bool)
	out := make([]string, 0, len(in))
	for _, t := range in {
		t = strings.ToLower(strings.TrimSpace(t))
		t = strings.Trim(t, ".,;:!?\"'()[]{}")
		if len(t) < 2 || len(t) > 40 {
			continue
		}
		if strings.ContainsAny(t, " \t\n/") {
			continue
		}
		if seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	return out
}

// sanitizePhrases preserves multi-word strings (lowercased, deduped). Used
// for the Phrases slot where space-separated text is meaningful.
func sanitizePhrases(in []string) []string {
	seen := make(map[string]bool)
	out := make([]string, 0, len(in))
	for _, p := range in {
		p = strings.ToLower(strings.TrimSpace(p))
		if len(p) < 3 || len(p) > 80 {
			continue
		}
		if seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

// sanitizeHints accepts path-like strings (lower/upper preserved if useful,
// but we lowercase for consistent CONTAINS matching). Allows "/" so package
// paths like "gobackend/fms" pass through.
func sanitizeHints(in []string) []string {
	seen := make(map[string]bool)
	out := make([]string, 0, len(in))
	for _, h := range in {
		h = strings.ToLower(strings.TrimSpace(h))
		h = strings.Trim(h, " .,;:!?\"'()[]{}")
		if len(h) < 2 || len(h) > 80 {
			continue
		}
		if seen[h] {
			continue
		}
		seen[h] = true
		out = append(out, h)
	}
	return out
}

// capTerms truncates a sanitized list to n elements. Server-side
// enforcement of the per-field caps documented in the constants above;
// agent-supplied plans cannot bypass these even if they ignore the
// schema's "0-N" annotations.
func capTerms(in []string, n int) []string {
	if len(in) <= n {
		return in
	}
	return in[:n]
}

// planIsEmpty reports whether a QueryPlan has no signal in any field.
func planIsEmpty(p QueryPlan) bool {
	return len(p.MustTerms) == 0 &&
		len(p.ShouldTerms) == 0 &&
		len(p.ExcludeTerms) == 0 &&
		len(p.Phrases) == 0 &&
		len(p.ContextHints) == 0
}
