package router

import (
	"log"
	"strings"
)

type Intent string

const (
	IntentDebug    Intent = "debug"
	IntentExplore  Intent = "explore"
	IntentTrace    Intent = "trace"
	IntentRefactor Intent = "refactor"
	IntentGeneral  Intent = "general"
)

type intentPattern struct {
	intent   Intent
	keywords []string
}

var intentPatterns = []intentPattern{
	{IntentDebug, []string{
		"debug", "bug", "error", "fix", "crash", "panic", "nil pointer",
		"why does", "broken", "failing", "failure", "wrong", "issue",
		"not working", "unexpected",
	}},
	{IntentTrace, []string{
		"trace", "flow", "propagate", "upstream", "downstream",
		"call chain", "how does", "reach", "path from", "path to",
		"end to end", "e2e", "sequence",
	}},
	{IntentRefactor, []string{
		"refactor", "rename", "move", "impact", "what breaks",
		"who uses", "who calls", "callers of", "dependents",
		"breaking change", "deprecate",
	}},
	{IntentExplore, []string{
		"what is", "explain", "understand", "overview", "how does",
		"describe", "summarize", "architecture", "structure",
		"purpose of", "role of",
	}},
}

// DetectIntent classifies a query into an intent using keyword matching only.
// No LLM is invoked — keyword matching is microseconds and the LLM fallback
// (previously a 5s Ollama call on every keyword-miss) was paying its full
// timeout budget on cold queries while contributing little signal that the
// downstream graph budget + trim caps actually need (general defaults work
// fine when the intent is genuinely ambiguous).
//
// Returns IntentGeneral when no keyword pattern matches.
func DetectIntent(query string) Intent {
	intent, matched := detectByKeywords(query)
	if matched {
		log.Printf("[intent] keyword match: %s", intent)
		return intent
	}
	log.Printf("[intent] no keyword match, fallback: general")
	return IntentGeneral
}

// detectByKeywords returns the best matching intent and whether at least one
// pattern matched. With multiple matches, prefers the more specific intent
// (anything except IntentExplore) — see "how does" which appears in both
// trace and explore patterns.
func detectByKeywords(query string) (Intent, bool) {
	qLower := strings.ToLower(query)

	var matches []Intent
	for _, p := range intentPatterns {
		for _, kw := range p.keywords {
			if strings.Contains(qLower, kw) {
				matches = append(matches, p.intent)
				break
			}
		}
	}

	switch len(matches) {
	case 0:
		return IntentGeneral, false
	case 1:
		return matches[0], true
	default:
		// "how does" matches both trace and explore — prioritize by specificity.
		// Trace/debug/refactor are more specific than explore.
		for _, m := range matches {
			if m != IntentExplore {
				return m, true
			}
		}
		return matches[0], true
	}
}
