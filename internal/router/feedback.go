package router

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/atharva-ag/devrouter/internal/heuristics"
	"github.com/atharva-ag/devrouter/internal/memory"
)

// FeedbackInput is the payload for SubmitFeedback (mapped 1:1 to the
// dev_feedback MCP tool arguments). Only AdditionalFiles is required.
//
// QueryID is optional: when missing, the router falls back to the
// last-call-on-connection LRU. When that also misses, the feedback is
// dropped silently — Path 2 (implicit repeat detection) remains the
// safety net.
// FilePaths is a raw comma-separated string (matches the convention used
// by every other tool in this server: memory_save_file.key_symbols,
// memory_save_func.callers, decision_save.files, etc.). The router splits
// it on commas where it consumes the list. Accepting a JSON array here
// would diverge from that convention and break agents that follow the
// documented "comma-separated" contract.
type FeedbackInput struct {
	QueryID         string `json:"query_id"`
	AdditionalFiles int    `json:"additional_files"`
	RevisitedFiles  int    `json:"revisited_files"`
	FilePaths       string `json:"file_paths"`
	Success         bool   `json:"success"`
}

// splitCSV is the standard comma-separated parser used across the router
// for agent-supplied list fields. Trims whitespace and drops empty entries.
func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := parts[:0]
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// FeedbackResult is what we return to the caller for transparency.
type FeedbackResult struct {
	JoinedQueryID  string  `json:"joined_query_id,omitempty"`
	Intent         string  `json:"intent,omitempty"`
	ProfileID      string  `json:"profile_id,omitempty"`
	RawReward      float64 `json:"raw_reward,omitempty"`
	AdjustedReward float64 `json:"adjusted_reward,omitempty"`
	JoinedVia      string  `json:"joined_via,omitempty"` // "explicit" | "lru_fallback" | "dropped"
	Note           string  `json:"note,omitempty"`
}

// SubmitFeedback computes the reward for one dev_feedback call, persists
// it under heuristics:reward:{intent}:{day}, HSETs the feedback-side
// fields onto feedback:trace:{queryID}, and feeds the bandit (no-op when
// frozen).
//
// Best-effort: a missing trace is logged and dropped, never returned as
// an error to the caller — feedback should never break a Cursor session.
func (r *Router) SubmitFeedback(in FeedbackInput) FeedbackResult {
	if r.Heuristics == nil {
		return FeedbackResult{JoinedVia: "dropped", Note: "heuristics not configured"}
	}
	ctx := context.Background()

	queryID := in.QueryID
	joinedVia := "explicit"

	// Resolve trace -> intent + profile_id + prompt_tokens + trimmed paths.
	// traceFields is retained beyond the join so the FP-attribution block
	// below can read query + memory_keys without a second HGETALL.
	var (
		intent       string
		profileID    string
		promptTokens int
		traceFields  map[string]string
	)

	if queryID != "" {
		fields, err := r.Heuristics.Store.GetTrace(ctx, queryID)
		if err != nil || len(fields) == 0 {
			queryID = "" // force fallback
		} else {
			traceFields = fields
			intent = fields["intent"]
			profileID = fields["heuristic_profile_id"]
			promptTokens, _ = strconv.Atoi(fields["prompt_tokens"])
		}
	}

	if queryID == "" && r.LastCalls != nil {
		if e, ok := r.LastCalls.MostRecent(); ok {
			queryID = e.QueryID
			intent = e.Intent
			profileID = e.ProfileID
			joinedVia = "lru_fallback"
			// Re-load prompt_tokens from the trace if the LRU pointed us
			// at a real one; the LRU itself doesn't carry this.
			fields, err := r.Heuristics.Store.GetTrace(ctx, queryID)
			if err == nil && len(fields) > 0 {
				traceFields = fields
				promptTokens, _ = strconv.Atoi(fields["prompt_tokens"])
			}
		}
	}

	if queryID == "" || intent == "" {
		log.Printf("[feedback] dropped: no join (query_id=%q intent=%q)", in.QueryID, intent)
		return FeedbackResult{JoinedVia: "dropped", Note: "no matching trace; Path 2 (implicit repeat) remains the safety net"}
	}

	// trimmedPaths is currently not stored on the trace hash (would
	// require persisting the full pre-trim candidate list). Leave empty
	// for v1; the explicit overlap penalty is best-effort and most of
	// the signal comes from additional_files.
	var trimmedPaths []string

	rollingMean := r.Heuristics.Store.RollingMean(ctx, intent, 50)
	raw, adjusted := heuristics.Compute(
		in.AdditionalFiles, in.RevisitedFiles, promptTokens,
		trimmedPaths, splitCSV(in.FilePaths),
		rollingMean,
	)
	now := time.Now().UnixMilli()

	row := heuristics.RewardRow{
		QueryID:         queryID,
		ProfileID:       profileID,
		RawReward:       raw,
		AdjustedReward:  adjusted,
		PromptTokens:    promptTokens,
		AdditionalFiles: in.AdditionalFiles,
		Source:          "explicit",
		Weight:          heuristics.ExplicitWeight,
		Timestamp:       now,
	}
	if err := r.Heuristics.Store.AppendReward(ctx, intent, row); err != nil {
		log.Printf("[feedback] AppendReward error (non-fatal): %v", err)
	}

	patch := map[string]interface{}{
		"additional_files": fmt.Sprintf("%d", in.AdditionalFiles),
		"revisits":         fmt.Sprintf("%d", in.RevisitedFiles),
		"explicit_success": fmt.Sprintf("%t", in.Success),
		"raw_reward":       fmt.Sprintf("%g", raw),
		"adjusted_reward":  fmt.Sprintf("%g", adjusted),
		"feedback_source":  "explicit",
		"feedback_at":      fmt.Sprintf("%d", now),
	}
	if err := r.Heuristics.Store.PatchTrace(ctx, queryID, patch); err != nil {
		log.Printf("[feedback] PatchTrace error (non-fatal): %v", err)
	}

	r.Heuristics.Update(intent, profileID, adjusted, heuristics.ExplicitWeight)

	// AnchorLearner reward attribution. dev_feedback's file_paths are
	// the agent's authoritative "I actually read these" list; any
	// anchored file in that set gets credited to its pattern, lifting
	// that pattern's success ratio in Decide() for next time. No-op
	// when no anchors were injected for this query (the observation
	// hash is absent → RewardFeedback returns silently). Decoupled
	// from FP attribution because the signal is symmetric (both
	// pointing the same direction) but credited to disjoint state
	// (FP centroids vs anchor patterns).
	if r.AnchorLearner != nil {
		r.AnchorLearner.RewardFeedback(ctx, queryID, splitCSV(in.FilePaths), in.Success)
	}

	// FP attribution: when the agent reported under-retrieval AND named
	// which files they actually read, mark every returned memory whose
	// own files don't overlap as a false positive for queries embedding-
	// similar to this one. Best-effort, never blocks the feedback reply.
	fpRecorded := r.attributeFalsePositives(ctx, traceFields, in)

	log.Printf("[feedback] joined=%s queryID=%s intent=%s additional=%d revisits=%d raw=%.2f adjusted=%.2f fp_recorded=%d",
		joinedVia, queryID, intent, in.AdditionalFiles, in.RevisitedFiles, raw, adjusted, fpRecorded)

	return FeedbackResult{
		JoinedQueryID:  queryID,
		Intent:         intent,
		ProfileID:      profileID,
		RawReward:      raw,
		AdjustedReward: adjusted,
		JoinedVia:      joinedVia,
	}
}

// attributeFalsePositives walks the memories returned by the original
// dev_context call (memory_keys CSV on the trace) and records a false
// positive against any memory whose own files don't overlap with the
// files the agent actually consulted.
//
// Preconditions for any work happening:
//   - additional_files > 0  (agent had to look elsewhere)
//   - file_paths supplied   (we know what the agent actually read)
//   - trace has memory_keys (the call returned at least one memory)
//   - trace has query       (we can re-embed for the FP centroid)
//
// Returns the number of FPs recorded so the caller can log it. Errors
// are logged and swallowed — relevance learning must never break the
// feedback handshake.
func (r *Router) attributeFalsePositives(ctx context.Context, traceFields map[string]string, in FeedbackInput) int {
	if r.Memory == nil || r.Heuristics == nil || traceFields == nil {
		return 0
	}
	if in.AdditionalFiles <= 0 {
		return 0
	}
	agentFiles := splitCSV(in.FilePaths)
	if len(agentFiles) == 0 {
		return 0
	}
	memKeys := splitCSV(traceFields["memory_keys"])
	if len(memKeys) == 0 {
		return 0
	}
	queryText := traceFields["query"]
	if queryText == "" {
		return 0
	}

	emb, err := memory.Embed(queryText)
	if err != nil {
		log.Printf("[feedback] FP embed failed (non-fatal): %v", err)
		return 0
	}

	// Load each memory's own file references so we know what counts as
	// overlap. One pipelined batch keeps this O(1) round-trips.
	memFiles := r.Memory.LoadMemoryFiles(ctx, memKeys)

	recorded := 0
	for _, mk := range memKeys {
		owned := memFiles[mk]
		if filesOverlap(owned, agentFiles) {
			continue
		}
		if err := r.Memory.RecordFalsePositive(ctx, mk, emb); err != nil {
			log.Printf("[feedback] RecordFalsePositive(%s) failed (non-fatal): %v", mk, err)
			continue
		}
		recorded++
	}
	return recorded
}

// filesOverlap is a deliberately permissive substring match: the agent
// might quote a file with or without a leading "/", with or without a
// trailing line range, etc. The seat-provider regression had ZERO
// substring overlap so even a loose check catches it.
func filesOverlap(memoryOwned, agentRead []string) bool {
	if len(memoryOwned) == 0 || len(agentRead) == 0 {
		return false
	}
	for _, m := range memoryOwned {
		m = normalisePath(m)
		if m == "" {
			continue
		}
		for _, a := range agentRead {
			a = normalisePath(a)
			if a == "" {
				continue
			}
			if a == m || strings.Contains(a, m) || strings.Contains(m, a) {
				return true
			}
		}
	}
	return false
}

func normalisePath(p string) string {
	p = strings.TrimSpace(p)
	p = strings.TrimPrefix(p, "/")
	if i := strings.IndexByte(p, ':'); i > 0 {
		// Strip ":line[-line]" suffixes Cursor sometimes appends.
		p = p[:i]
	}
	return strings.ToLower(p)
}

// FeedbackStats returns the per-intent reward distribution + bandit state.
func (r *Router) FeedbackStats() heuristics.Snapshot {
	if r.Heuristics == nil {
		return heuristics.Snapshot{}
	}
	return r.Heuristics.Stats()
}

// ResetHeuristics manually rolls a single intent (or all intents) back to
// the frozen default profile. Used by the dev_heuristics_reset MCP tool
// for incident recovery.
func (r *Router) ResetHeuristics(intent string) ([]string, error) {
	if r.Heuristics == nil {
		return nil, fmt.Errorf("heuristics not configured")
	}
	targets := []string{intent}
	if intent == "" || intent == "all" {
		targets = heuristics.KnownIntents
	}
	var reset []string
	for _, t := range targets {
		if _, err := r.Heuristics.Reset(t); err != nil {
			return reset, fmt.Errorf("reset %s: %w", t, err)
		}
		reset = append(reset, t)
	}
	return reset, nil
}
