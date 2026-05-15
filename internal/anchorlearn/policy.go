package anchorlearn

import (
	"context"
	"math/rand"
	"sort"
	"strings"
)

// Policy hyperparameters. Calibrated to be conservative — aggressive
// values risk regressing the bench (we already paid that price once
// during static-list development; the learning system inherits the
// same risk shape). Prefer bumping these in production after a few
// hundred dev_context calls of observation data accumulate.
const (
	// SmoothingPriorFires / SmoothingPriorSuccess pull a fresh
	// pattern's success rate toward 0.5 until it has fired a handful
	// of times. Without this a one-shot lucky pattern would dominate
	// Decide on its second observation; with this its smoothed rate
	// is (1 + 1) / (2 + 2) = 0.5 — same as an untried pattern.
	SmoothingPriorFires   = 2.0
	SmoothingPriorSuccess = 1.0

	// RepoPriorWeight controls how much the per-repo posterior
	// overrides the global prior in Score. 0.0 = pure global, 1.0 =
	// pure repo-local. Default 0.7 means "repo-local evidence
	// dominates after a few firings but doesn't completely shut out
	// a globally-effective pattern that hasn't been tried here yet".
	RepoPriorWeight = 0.7

	// KeywordAffinityWeight scales the (kw, pattern) co-occurrence
	// score's contribution to the final Score. Capped so a hot
	// keyword can boost rank but can't outvote a pattern with a
	// strong success record.
	KeywordAffinityWeight = 0.3
	KeywordAffinityCap    = 1.0

	// DefaultEpsilon is the exploration rate for ε-greedy: with this
	// probability one slot in the top-K is replaced by a randomly-
	// sampled untried pattern, gathering data on patterns the system
	// would otherwise never try. Decreased over time via
	// DecayEpsilon as the per-repo bandit converges.
	DefaultEpsilon = 0.10

	// MinFiresBeforeExploit is the per-pattern firing threshold below
	// which we treat the pattern as "unproven" and weight its
	// success rate toward the global prior. Prevents one lucky
	// firing from spiking a pattern's repo-local score.
	MinFiresBeforeExploit = 3
)

// score computes Decide's per-candidate ranking signal. The structure
// is a smoothed Bayesian success-rate (cross-repo prior blended with
// repo-local posterior) multiplied by a (1 + keyword affinity) boost.
//
// We deliberately keep the math simple and explicit — every term is a
// single number with a clear bench-rationale, and we avoid introducing
// hyperparameters whose tuning we couldn't justify from the existing
// 30-question goserving signal.
func score(globalStats, repoStats PatternStats, kwScore float64) float64 {
	globalRate := smoothedRate(globalStats.SuccessCount, globalStats.FiredCount)
	repoRate := smoothedRate(repoStats.SuccessCount, repoStats.FiredCount)

	w := RepoPriorWeight
	if repoStats.FiredCount < MinFiresBeforeExploit {
		// Not enough repo-local data — fall back toward global.
		w = float64(repoStats.FiredCount) / float64(MinFiresBeforeExploit) * RepoPriorWeight
	}
	blended := (1-w)*globalRate + w*repoRate

	if kwScore > KeywordAffinityCap {
		kwScore = KeywordAffinityCap
	}
	return blended * (1 + KeywordAffinityWeight*kwScore)
}

func smoothedRate(success, fired int) float64 {
	num := float64(success) + SmoothingPriorSuccess
	den := float64(fired) + SmoothingPriorFires
	if den == 0 {
		return 0.5
	}
	return num / den
}

// epsilonGreedyShuffle implements the ε-greedy exploration step: with
// probability ε, swap the lowest-ranked candidate inside the top-k
// budget for a randomly-sampled candidate that's still unproven (low
// fire count). This guarantees the bandit eventually tries every
// pattern at least a few times even if the initial scoring would
// never select it — without which a static prior with one wrong
// keyword could permanently shadow a useful pattern.
//
// Unproven patterns are those with FiredCount < MinFiresBeforeExploit.
// If none qualify, no swap happens. The exploration slot is marked
// IsExploration so reward attribution can distinguish "this anchor
// was scored highly and worked" from "we were probing and got lucky".
func epsilonGreedyShuffle(
	ctx context.Context,
	store Store,
	repo string,
	ranked []Candidate,
	pool []Candidate,
	k int,
	epsilon float64,
	rng *rand.Rand,
) []Candidate {
	if len(ranked) == 0 || k <= 0 {
		return ranked
	}
	if rng.Float64() >= epsilon {
		return ranked
	}

	// Find unproven patterns from the full pool. We re-score by
	// FiredCount so the most-untried patterns float to the top of
	// the candidate sample — this concentrates exploration on the
	// patterns we know least about.
	var unproven []Candidate
	for _, c := range pool {
		st, _ := store.GetPatternStats(ctx, repo, c.Pattern.ID)
		if st.FiredCount < MinFiresBeforeExploit {
			unproven = append(unproven, c)
		}
	}
	if len(unproven) == 0 {
		return ranked
	}

	chosen := unproven[rng.Intn(len(unproven))]
	chosen.IsExploration = true

	// Replace the lowest-ranked top-k slot if the chosen exploration
	// candidate isn't already there.
	cap := k
	if cap > len(ranked) {
		cap = len(ranked)
	}
	for i := 0; i < cap; i++ {
		if ranked[i].Pattern.ID == chosen.Pattern.ID && ranked[i].Service == chosen.Service {
			return ranked
		}
	}
	if cap >= len(ranked) {
		ranked = append(ranked, chosen)
		// Move the new exploration candidate into the top-k by
		// pushing the previously-last out.
		copy(ranked[1:cap+1], ranked[:cap])
		ranked[0] = chosen
		return ranked
	}
	ranked[cap-1] = chosen
	return ranked
}

// rankByScore produces a stable descending sort of Candidates by
// Score. Stability matters: when two patterns score identically the
// static-portfolio order is the tiebreaker, which matches the v0
// router behaviour and keeps bench results reproducible.
func rankByScore(cs []Candidate) {
	sort.SliceStable(cs, func(i, j int) bool {
		if cs[i].Score != cs[j].Score {
			return cs[i].Score > cs[j].Score
		}
		return cs[i].Pattern.ID < cs[j].Pattern.ID
	})
}

// extractKeywords pulls the simple alphanumeric tokens out of a query
// for keyword-affinity scoring. Lowercased, deduped, length-3+. We
// intentionally do not stem or normalise — the affinity table stores
// raw tokens and the matching policy is exact-equality, so the noise
// floor is bounded.
func extractKeywords(query string) []string {
	cur := strings.Builder{}
	seen := make(map[string]bool)
	out := make([]string, 0, 8)
	flush := func() {
		if cur.Len() == 0 {
			return
		}
		w := strings.ToLower(cur.String())
		cur.Reset()
		if len(w) < 3 || seen[w] {
			return
		}
		seen[w] = true
		out = append(out, w)
	}
	for _, ch := range query {
		if (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') {
			cur.WriteRune(ch)
			continue
		}
		flush()
	}
	flush()
	return out
}
