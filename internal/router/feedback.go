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
	"github.com/atharva-ag/devrouter/internal/telemetry"
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

	// FlowID joins this feedback to a specific saved flow so the
	// dashboard can score each file in the flow as useful / dead.
	// Format: "{repo}/{flow_name}". Optional — when empty, no flow
	// overlay update happens and the rest of the feedback (bandit,
	// FP attribution, anchor learning) runs unchanged. The agent
	// learns the correct value from the `name` field of any
	// flow-typed entry in primary_context.
	//
	// Intentionally NOT folded into the bandit reward: flow quality
	// scores memory accuracy, the bandit tunes codegraph context
	// shape — different layers. Surfaced on the dashboard's
	// Heuristics tab as the "Flow signal" panel for observability.
	FlowID string `json:"flow_id"`

	// MissingFiles is the slice-2 channel: files the agent had to
	// read to finish the task that *were not* in the matched flow.
	// Comma-separated to match the existing FilePaths convention.
	// Tracked as `missing:{path}` counters on the flow overlay so
	// the dashboard can render an "augmented files" column. Has no
	// effect on the bandit reward — purely a flow-completeness signal.
	MissingFiles string `json:"missing_files"`
}

// sourceExploreFromTrace reads the per-source breadth-bandit explore
// record persisted on a trace hash (src_explore_name / src_explore_val).
// Returns ok=false when no source was sampled for that query.
func sourceExploreFromTrace(f map[string]string) (name string, val int, ok bool) {
	if len(f) == 0 {
		return "", 0, false
	}
	name = f["src_explore_name"]
	if name == "" {
		return "", 0, false
	}
	v, err := strconv.Atoi(f["src_explore_val"])
	if err != nil {
		return "", 0, false
	}
	return name, v, true
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

	// FlowOverlay is populated when flow_id was supplied AND the
	// overlay write succeeded. Lets the caller verify their feedback
	// landed without having to round-trip the dashboard.
	FlowOverlay *FlowOverlayResult `json:"flow_overlay,omitempty"`
}

// FlowOverlayResult is the per-call snapshot of what changed on the
// flow:overlay:{repo}:{name} hash for this feedback event. Returned
// to the caller so they can verify the feedback landed without
// round-tripping the dashboard. Purely observational — the flow
// overlay does NOT feed back into the bandit reward (see comment
// on applyFlowOverlay in SubmitFeedback).
type FlowOverlayResult struct {
	Repo          string `json:"repo"`
	Name          string `json:"name"`
	MarkedUseful  int    `json:"marked_useful"`
	MarkedDead    int    `json:"marked_dead"`
	MarkedMissing int    `json:"marked_missing"`
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
	//
	// repo + topicID identify which per-(intent, repo, topic) bucket
	// served the original query; we route the reward to exactly that
	// bucket so a hot bucket's tuning gets the signal directly instead
	// of fanning it across the whole intent.
	var (
		intent       string
		profileID    string
		repo         string
		topicID      string
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
			repo = fields["repo"]
			topicID = fields["topic_id"]
			promptTokens, _ = strconv.Atoi(fields["prompt_tokens"])
		}
	}

	if queryID == "" && r.LastCalls != nil {
		if e, ok := r.LastCalls.MostRecent(); ok {
			queryID = e.QueryID
			intent = e.Intent
			profileID = e.ProfileID
			repo = e.Repo
			topicID = e.TopicID
			joinedVia = "lru_fallback"
			// Re-load prompt_tokens from the trace if the LRU pointed us
			// at a real one; the LRU itself doesn't carry this.
			fields, err := r.Heuristics.Store.GetTrace(ctx, queryID)
			if err == nil && len(fields) > 0 {
				traceFields = fields
				promptTokens, _ = strconv.Atoi(fields["prompt_tokens"])
				if repo == "" {
					repo = fields["repo"]
				}
				if topicID == "" {
					topicID = fields["topic_id"]
				}
			}
		}
	}

	if queryID == "" || intent == "" {
		telemetry.FeedbackTotal.WithLabelValues("unknown", "dropped").Inc()
		log.Printf("[feedback] dropped: no join (query_id=%q intent=%q)", in.QueryID, intent)
		return FeedbackResult{JoinedVia: "dropped", Note: "no matching trace; Path 2 (implicit repeat) remains the safety net"}
	}

	// trimmedPaths is currently not stored on the trace hash (would
	// require persisting the full pre-trim candidate list). Leave empty
	// for v1; the explicit overlap penalty is best-effort and most of
	// the signal comes from additional_files.
	var trimmedPaths []string

	rollingMean := r.Heuristics.Store.RollingMeanFor(ctx, intent, repo, topicID, 50)
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
	if err := r.Heuristics.Store.AppendRewardFor(ctx, intent, repo, topicID, row); err != nil {
		log.Printf("[feedback] AppendRewardFor error (non-fatal): %v", err)
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

	r.Heuristics.UpdateWithTopic(intent, repo, topicID, profileID, adjusted, heuristics.ExplicitWeight)

	// Route the same reward to the per-source breadth bandit when this
	// query sampled a source (recorded on the trace). No-op otherwise.
	if name, val, ok := sourceExploreFromTrace(traceFields); ok {
		r.Heuristics.UpdateSource(intent, repo, topicID, name, val, adjusted, heuristics.ExplicitWeight)
	}

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

	// Slice 1/2: apply the agent's read+missing file lists to the named
	// flow's overlay. Counters are append-only and idempotent enough
	// that a duplicated event would just slightly bias the ratios.
	// Logged but never errored to the caller — flow overlays must
	// never break the feedback handshake.
	//
	// Intentionally NOT fed back into the bandit reward: the bandit
	// tunes codegraph context-shape knobs (caller_hops, max_snippets,
	// ...) while flow signal scores whether a saved *memory* was
	// accurate. Those are different layers; mixing them muddies both.
	// Stale flows are surfaced via the dashboard observability path
	// and addressed by memory-relevance learning (or human pruning),
	// not by shifting the bandit.
	flowResult := r.applyFlowOverlay(ctx, in, queryID)

	telemetry.FeedbackTotal.WithLabelValues(intent, joinedVia).Inc()
	telemetry.FeedbackRawReward.WithLabelValues(intent).Observe(raw)
	telemetry.FeedbackAdjustedReward.WithLabelValues(intent).Observe(adjusted)
	telemetry.FeedbackAdditionalFiles.WithLabelValues(intent).Observe(float64(in.AdditionalFiles))
	if fpRecorded > 0 {
		telemetry.FeedbackFPRecorded.WithLabelValues(intent).Add(float64(fpRecorded))
	}

	log.Printf("[feedback] joined=%s queryID=%s intent=%s additional=%d revisits=%d raw=%.2f adjusted=%.2f fp_recorded=%d",
		joinedVia, queryID, intent, in.AdditionalFiles, in.RevisitedFiles, raw, adjusted, fpRecorded)

	return FeedbackResult{
		JoinedQueryID:  queryID,
		Intent:         intent,
		ProfileID:      profileID,
		RawReward:      raw,
		AdjustedReward: adjusted,
		JoinedVia:      joinedVia,
		FlowOverlay:    flowResult,
	}
}

// applyFlowOverlay is the FlowID-gated wrapper around Memory.UpdateFlowOverlay.
// It parses the "repo/name" join key, invokes the storage update, and
// returns a FlowOverlayResult for the caller's reply. Returns nil when
// flow_id is absent or the overlay update fails (errors are logged,
// never propagated — feedback path must never break the agent's task).
//
// MarkedUseful is the count of distinct files this event credited to
// the flow (approximate: bounded by the agent's file_paths length;
// the authoritative per-file breakdown lives on the overlay hash).
// MarkedDead is left zero here for the same reason — the dashboard
// reads the overlay directly for accurate numbers.
func (r *Router) applyFlowOverlay(ctx context.Context, in FeedbackInput, queryID string) *FlowOverlayResult {
	if in.FlowID == "" || r.Memory == nil {
		return nil
	}
	repo, name, ok := strings.Cut(in.FlowID, "/")
	if !ok || repo == "" || name == "" {
		log.Printf("[feedback] flow_id %q is not in {repo}/{name} format; skipping overlay", in.FlowID)
		return nil
	}
	readFiles := splitCSV(in.FilePaths)
	missing := splitCSV(in.MissingFiles)

	if err := r.Memory.UpdateFlowOverlay(ctx, repo, name, memory.FlowOverlayUpdate{
		QueryID:         queryID,
		ReadFiles:       readFiles,
		MissingFiles:    missing,
		Success:         in.Success,
		AdditionalFiles: in.AdditionalFiles,
	}); err != nil {
		log.Printf("[feedback] UpdateFlowOverlay(%s/%s) failed (non-fatal): %v", repo, name, err)
		return nil
	}

	return &FlowOverlayResult{
		Repo:          repo,
		Name:          name,
		MarkedUseful:  len(readFiles),
		MarkedMissing: len(missing),
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
