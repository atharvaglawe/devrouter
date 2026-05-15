package prompt

import "strings"

const primaryContextInstruction = `PRIMARY CONTEXT (HIGHEST PRIORITY):
These are high-confidence learned behaviors from prior analysis.
Use these FIRST to understand system behavior.
Only use code/graph to validate or expand.
Do NOT ignore PRIMARY CONTEXT unless directly contradicted by code.`

// GraphFirstInstruction is set by the router when no memories match but
// graph data (symbols, call chains, snippets) is available.
const GraphFirstInstruction = `NO MEMORIES AVAILABLE — USE GRAPH CONTEXT FIRST:
No prior memories exist for this topic. However, the symbols, call chains,
file paths, and code snippets below were retrieved from the code graph.

1. Read the CODE SNIPPETS provided — they are the most relevant files found.
2. Follow the CALL CHAIN to understand upstream callers and downstream callees.
3. Use SYMBOLS and GRAPH relationships to navigate related files.
4. Only search or read additional files if the above context is insufficient.
Do NOT skip the provided context and jump straight to file exploration.`

// Build assembles a DevPrompt from pre-extracted components.
func Build(
	intent string,
	symbols []string,
	impactNames []string,
	primaryContext []PrimaryContextEntry,
	snippets []Snippet,
) DevPrompt {
	dp := DevPrompt{
		Intent:         intent,
		PrimaryContext: primaryContext,
		Symbols:        symbols,
		ImpactRadius:   impactNames,
		CodeSnippets:   snippets,
	}

	if len(primaryContext) > 0 {
		dp.Instructions = primaryContextInstruction
		dp.ContextConfidence = computeConfidence(primaryContext)
		dp.MemoryCoverage = coverageLevel(primaryContext)
	}

	return dp
}

// GenerateSummary extracts 1-2 key lines from a full purpose description.
// Takes the first sentence and caps at ~150 chars to anchor Claude's attention.
func GenerateSummary(purpose string) string {
	if purpose == "" {
		return ""
	}

	// Split on sentence boundaries
	for _, sep := range []string{". ", ".\n", ".\t"} {
		if idx := strings.Index(purpose, sep); idx > 0 && idx < 200 {
			return strings.TrimSpace(purpose[:idx+1])
		}
	}

	// No sentence boundary found — take first line
	if idx := strings.IndexByte(purpose, '\n'); idx > 0 && idx < 200 {
		return strings.TrimSpace(purpose[:idx])
	}

	// Single long sentence — truncate
	if len(purpose) > 150 {
		// Cut at last space before 150
		cut := purpose[:150]
		if idx := strings.LastIndexByte(cut, ' '); idx > 80 {
			return strings.TrimSpace(cut[:idx]) + "..."
		}
		return cut + "..."
	}

	return purpose
}

func computeConfidence(entries []PrimaryContextEntry) float64 {
	if len(entries) == 0 {
		return 0
	}
	sum := 0.0
	for _, e := range entries {
		sum += e.Confidence
	}
	return sum / float64(len(entries))
}

func coverageLevel(entries []PrimaryContextEntry) string {
	nonStale := 0
	for _, e := range entries {
		if !e.Stale {
			nonStale++
		}
	}
	switch {
	case nonStale >= 3:
		return "high"
	case nonStale >= 1:
		return "partial"
	default:
		return "low"
	}
}
