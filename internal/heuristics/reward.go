package heuristics

// Compute returns (raw, adjusted) rewards for an explicit dev_feedback
// event. The reward has two terms — under-retrieval and over-retrieval —
// so the bandit cannot learn "just retrieve everything":
//
//	raw_reward = clip(
//	    1.0
//	    - 0.15 * additional_files       # under-retrieval penalty
//	    - 2e-5 * total_prompt_tokens    # over-retrieval (token cost)
//	    - 0.05 * revisited_files        # re-reads of same file
//	    - 0.20 * trim_overlap_hit,      # trim was too aggressive
//	    0, 1
//	)
//
//	adjusted_reward = raw_reward - rolling_mean_reward(intent)
//
// Token-cost coefficient is intentionally small: a 5k-token prompt costs
// 0.10, a 20k-token prompt costs 0.40 — enough to prevent gradual
// context inflation without dominating the under-retrieval term.
//
// The trim-overlap penalty fires once if any of agentReadPaths overlaps
// with trimmedPaths. Pass empty slices to skip.
func Compute(
	additionalFiles, revisitedFiles, promptTokens int,
	trimmedPaths, agentReadPaths []string,
	rollingMean float64,
) (raw, adjusted float64) {
	raw = 1.0
	raw -= 0.15 * float64(additionalFiles)
	raw -= 2e-5 * float64(promptTokens)
	raw -= 0.05 * float64(revisitedFiles)
	if hasOverlap(trimmedPaths, agentReadPaths) {
		raw -= 0.20
	}
	if raw < 0 {
		raw = 0
	}
	if raw > 1 {
		raw = 1
	}
	adjusted = raw - rollingMean
	return raw, adjusted
}

// ComputeImplicit returns the raw reward for an implicit-repeat event.
// `sim` is the cosine similarity to the previous query's embedding.
// `fired` is false (and reward 0) when the similarity is below the
// no-penalty floor — used to suppress emit when the new query is just
// a related follow-up rather than a repeat.
//
//   - cosine > 0.95  : near-identical re-ask → raw_reward 0.0 (treated
//     as a 7-file under-retrieval).
//   - cosine 0.70..0.95 : paraphrased repeat → raw_reward 0.4.
//   - cosine 0.50..0.70 : related follow-up → no penalty.
func ComputeImplicit(sim float64) (raw float64, fired bool) {
	switch {
	case sim > 0.95:
		return 0.0, true
	case sim > 0.70:
		return 0.4, true
	default:
		return 0.0, false
	}
}

// ImplicitWeight is the bandit weight for implicit-repeat signals. They
// are noisier than explicit feedback (the agent might be drilling down
// rather than retrying) so updates are damped.
const ImplicitWeight = 0.5

// ExplicitWeight is the bandit weight for explicit dev_feedback signals.
const ExplicitWeight = 1.0

// RepeatSimThreshold is the cosine floor for treating a query as a
// repeat-exploration of a prior query.
const RepeatSimThreshold = 0.70

func hasOverlap(a, b []string) bool {
	if len(a) == 0 || len(b) == 0 {
		return false
	}
	set := make(map[string]struct{}, len(a))
	for _, x := range a {
		if x != "" {
			set[x] = struct{}{}
		}
	}
	for _, y := range b {
		if _, ok := set[y]; ok {
			return true
		}
	}
	return false
}
