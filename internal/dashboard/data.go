// Package dashboard exposes a lightweight read-only HTTP UI over the
// state DevRouter already keeps in Redis: live queries, heuristic
// profile evolution, saved decisions (with supersession lineage), and
// flow memories (with the symbols/files they touch).
//
// The dashboard is strictly observational. It never writes to Redis,
// never participates in the MCP request path, and never mutates the
// bandit. Safe to enable in production behind any cluster-internal
// address (default disabled — opt-in via DEVROUTER_DASHBOARD_ADDR).
package dashboard

import (
	"context"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/atharva-ag/devrouter/internal/heuristics"
	"github.com/atharva-ag/devrouter/internal/memory"
)

// QueryRow is one entry in the live-query feed. Fields mirror what
// router.traceHashFields persists onto feedback:trace:{id}, plus the
// dev_feedback-side patch fields when the loop has closed.
type QueryRow struct {
	QueryID          string   `json:"query_id"`
	Query            string   `json:"query"`
	Repo             string   `json:"repo"`
	Intent           string   `json:"intent"`
	ProfileID        string   `json:"heuristic_profile_id"`
	Timestamp        int64    `json:"timestamp"`
	LatencyMs        int      `json:"latency_ms"`
	PromptTokens     int      `json:"prompt_tokens"`
	FilesReturned    int      `json:"files_returned"`
	SymbolsReturned  int      `json:"symbols_returned"`
	SnippetsReturned int      `json:"snippets_returned"`
	TrimmedFiles     int      `json:"trimmed_files"`
	BudgetUsed       float64  `json:"budget_used_fraction"`
	MemoryKeys       []string `json:"memory_keys,omitempty"`

	// SearchFiles / GraphFiles are the codegraph file paths the
	// router persisted on the trace hash (search_files /
	// graph_files CSVs, capped at router.codegraphFileFieldCap).
	// SearchFiles is the post-dedupe output of /api/search; GraphFiles
	// is the union of snippet files + callers/callees/importers/
	// extends/methods/siblings discovered during graph traversal.
	// Empty when codegraph returned nothing (degraded path) or when
	// the trace pre-dates this field's introduction.
	SearchFiles []string `json:"search_files,omitempty"`
	GraphFiles  []string `json:"graph_files,omitempty"`

	// TopicID identifies the per-(intent, repo, topic) heuristics
	// bucket this query was scored against. "*" or empty means the
	// query landed on the intent-global fallback (topics disabled,
	// or bucket below the bandit sample floor).
	TopicID            string `json:"topic_id,omitempty"`
	TopicLabel         string `json:"topic_label,omitempty"`
	HeuristicFromTopic bool   `json:"heuristic_from_topic,omitempty"`

	// Feedback side — zero until dev_feedback joins.
	AdditionalFiles int     `json:"additional_files,omitempty"`
	Revisits        int     `json:"revisits,omitempty"`
	RawReward       float64 `json:"raw_reward,omitempty"`
	AdjustedReward  float64 `json:"adjusted_reward,omitempty"`
	FeedbackSource  string  `json:"feedback_source,omitempty"`
	FeedbackAt      int64   `json:"feedback_at,omitempty"`
	HasFeedback     bool    `json:"has_feedback"`

	RepeatedExplorationOf string `json:"repeated_exploration_of,omitempty"`
}

// LoadRecentQueries pulls the most recent N traces from the bounded
// feedback:trace:index ZSET, batches HGETALL calls into a single Redis
// pipeline so the dashboard's poll cost stays flat regardless of fan-out.
func LoadRecentQueries(ctx context.Context, store *heuristics.Store, rdb *redis.Client, limit int) []QueryRow {
	if store == nil || rdb == nil {
		return nil
	}
	ids := store.RecentTraceIDs(ctx, limit)
	if len(ids) == 0 {
		return nil
	}
	pipe := rdb.Pipeline()
	getters := make([]*redis.MapStringStringCmd, len(ids))
	for i, id := range ids {
		getters[i] = pipe.HGetAll(ctx, "feedback:trace:"+id)
	}
	if _, err := pipe.Exec(ctx); err != nil && err != redis.Nil {
		return nil
	}
	out := make([]QueryRow, 0, len(ids))
	for i, id := range ids {
		fields, err := getters[i].Result()
		if err != nil || len(fields) == 0 {
			continue
		}
		out = append(out, traceFieldsToRow(id, fields))
	}
	return out
}

func traceFieldsToRow(id string, f map[string]string) QueryRow {
	row := QueryRow{
		QueryID:               id,
		Query:                 f["query"],
		Repo:                  f["repo"],
		Intent:                f["intent"],
		ProfileID:             f["heuristic_profile_id"],
		TopicID:               f["topic_id"],
		TopicLabel:            f["topic_label"],
		HeuristicFromTopic:    f["heuristic_from_topic"] == "true",
		Timestamp:             parseInt64(f["timestamp"]),
		LatencyMs:             parseInt(f["latency_ms"]),
		PromptTokens:          parseInt(f["prompt_tokens"]),
		FilesReturned:         parseInt(f["files_returned"]),
		SymbolsReturned:       parseInt(f["symbols_returned"]),
		SnippetsReturned:      parseInt(f["snippets_returned"]),
		TrimmedFiles:          parseInt(f["trimmed_files"]),
		BudgetUsed:            parseFloat(f["budget_used_fraction"]),
		AdditionalFiles:       parseInt(f["additional_files"]),
		Revisits:              parseInt(f["revisits"]),
		RawReward:             parseFloat(f["raw_reward"]),
		AdjustedReward:        parseFloat(f["adjusted_reward"]),
		FeedbackSource:        f["feedback_source"],
		FeedbackAt:            parseInt64(f["feedback_at"]),
		RepeatedExplorationOf: f["repeated_exploration_of"],
	}
	if mk := f["memory_keys"]; mk != "" {
		row.MemoryKeys = splitCSV(mk)
	}
	if sf := f["search_files"]; sf != "" {
		row.SearchFiles = splitCSV(sf)
	}
	if gf := f["graph_files"]; gf != "" {
		row.GraphFiles = splitCSV(gf)
	}
	row.HasFeedback = row.FeedbackAt > 0 || row.FeedbackSource != ""
	return row
}

// ProfileDelta is the per-knob diff between the live profile and the
// frozen default. Surfacing this is the single most-asked observability
// question for the heuristics layer: "what has the bandit changed?"
//
// Min/Max mirror heuristics.Bounds so the dashboard can render each
// knob's current value as a position inside its valid range (no need
// to round-trip back to Go for bounds metadata).
type ProfileDelta struct {
	Knob    string `json:"knob"`
	Current int    `json:"current"`
	Default int    `json:"default"`
	Delta   int    `json:"delta"`
	Min     int    `json:"min"`
	Max     int    `json:"max"`
}

// RewardBucket is one downsampled cell on the per-intent reward
// time-series surfaced under HeuristicIntentView.RewardHistory. The
// dashboard renders the slice as an hourly sparkline so devs can spot
// regressions (mean dropping) without tailing logs.
//
// Buckets cover the trailing RewardHistoryHours hours, oldest first.
// A bucket with Count==0 is still emitted so the chart preserves
// chronological spacing — the UI just renders it as a gap.
type RewardBucket struct {
	TimestampMs int64   `json:"ts"`
	Count       int     `json:"n"`
	Mean        float64 `json:"mean,omitempty"`
}

// RewardHistoryHours is the rolling window the reward sparkline covers.
// 24 hourly buckets gives a "last day" view that lines up cleanly with
// the SamplesToday counter already on the panel header.
const RewardHistoryHours = 24

// HeuristicIntentView aggregates everything the dashboard wants to
// show for a single intent: live + default profile, the per-knob deltas
// the bandit has accumulated, reward distribution, and recent history.
// Buckets is the per-(repo, topic) breakdown — empty when topics are
// off or the intent has no real buckets yet.
type HeuristicIntentView struct {
	Intent             string                    `json:"intent"`
	Current            heuristics.Profile        `json:"current_profile"`
	Default            heuristics.Profile        `json:"default_profile"`
	CurrentProfileID   string                    `json:"current_profile_id"`
	Deltas             []ProfileDelta            `json:"deltas"`
	SamplesToday       int                       `json:"samples_today"`
	Samples7d          int                       `json:"samples_7d"`
	MeanRawReward7d    float64                   `json:"mean_raw_reward_7d"`
	P50RawReward7d     float64                   `json:"p50_raw_reward_7d"`
	P95RawReward7d     float64                   `json:"p95_raw_reward_7d"`
	ExplicitFraction7d float64                   `json:"explicit_fraction_7d"`
	ImplicitFraction7d float64                   `json:"implicit_fraction_7d"`
	RecentHistory      []heuristics.HistoryEntry `json:"recent_history"`
	// RewardHistory is a 24-bucket hourly time-series of mean
	// raw_reward, oldest first. Lets the dashboard render a sparkline
	// next to the knob drift chart so users can correlate "bandit
	// changed X" with "reward shifted Y". Empty when the intent has
	// no reward rows in the window.
	RewardHistory []RewardBucket        `json:"reward_history,omitempty"`
	Buckets       []HeuristicBucketView `json:"buckets,omitempty"`
}

// HeuristicBucketView is the per-(repo, topic) row in the nested
// Heuristics tab. Deltas are vs the intent's default profile, so a
// reader can see "this bucket runs hot at MaxTrace=8 vs default 4"
// at a glance.
type HeuristicBucketView struct {
	Repo             string                    `json:"repo"`
	TopicID          string                    `json:"topic_id"`
	TopicLabel       string                    `json:"topic_label,omitempty"`
	CentroidSamples  int                       `json:"centroid_samples"`
	HotEnough        bool                      `json:"hot_enough"`
	Current          heuristics.Profile        `json:"current_profile"`
	CurrentProfileID string                    `json:"current_profile_id"`
	Deltas           []ProfileDelta            `json:"deltas"`
	Samples7d        int                       `json:"samples_7d"`
	MeanRawReward7d  float64                   `json:"mean_raw_reward_7d"`
	RecentHistory    []heuristics.HistoryEntry `json:"recent_history,omitempty"`
}

// HeuristicsView is the full payload for the Heuristics tab.
type HeuristicsView struct {
	Frozen           bool                  `json:"frozen"`
	TopicsEnabled    bool                  `json:"topics_enabled"`
	TopicSampleFloor int                   `json:"topic_sample_floor"`
	TunableKnobs     []string              `json:"tunable_knobs"`
	Intents          []HeuristicIntentView `json:"intents"`
}

// TopicView is one row in the /api/topics payload. TopicLabel is the
// human-readable kebab-case tag derived from the seed query at topic
// creation (e.g. "redis-session-cache"); it is empty for legacy
// topics that pre-date the labelling feature — the dashboard falls
// back to TopicID in that case.
type TopicView struct {
	Intent          string `json:"intent"`
	Repo            string `json:"repo"`
	TopicID         string `json:"topic_id"`
	TopicLabel      string `json:"topic_label,omitempty"`
	CentroidSamples int    `json:"centroid_samples"`
	CreatedAt       int64  `json:"created_at_ms"`
	LastSeen        int64  `json:"last_seen_ms"`
	HotEnough       bool   `json:"hot_enough"`
}

// LoadHeuristics composes the Heuristics view from picker.Stats() plus
// the per-intent frozen default (loaded directly from Redis so the
// dashboard can show the bandit's accumulated drift). When topics are
// enabled, each intent gets a per-(repo, topic) breakdown so the UI
// can render a nested cards view.
func LoadHeuristics(ctx context.Context, picker *heuristics.Picker) HeuristicsView {
	if picker == nil {
		return HeuristicsView{}
	}
	snap := picker.Stats()
	view := HeuristicsView{
		Frozen:           snap.Frozen,
		TopicsEnabled:    snap.TopicsOn,
		TopicSampleFloor: snap.SampleFloor,
		TunableKnobs:     snap.TunableKnobs,
		Intents:          make([]HeuristicIntentView, 0, len(snap.Intents)),
	}
	for _, is := range snap.Intents {
		def, err := picker.Store.LoadDefault(ctx, is.Intent)
		if err != nil {
			def = heuristics.Default(is.Intent)
		}
		buckets := make([]HeuristicBucketView, 0, len(is.Buckets))
		for _, b := range is.Buckets {
			buckets = append(buckets, HeuristicBucketView{
				Repo:             b.Repo,
				TopicID:          b.TopicID,
				TopicLabel:       b.TopicLabel,
				CentroidSamples:  b.CentroidSamples,
				HotEnough:        b.HotEnough,
				Current:          b.CurrentProfile,
				CurrentProfileID: b.CurrentProfileID,
				Deltas:           computeDeltas(b.CurrentProfile, def),
				Samples7d:        b.Samples7d,
				MeanRawReward7d:  b.MeanRawReward7d,
				RecentHistory:    b.RecentHistory,
			})
		}
		// picker.Stats() caps history at 5 to keep its payload tight
		// for non-dashboard callers. For the drift chart we want
		// enough events to actually show a trajectory — re-fetch
		// directly from the store with a deeper window.
		history := picker.Store.History(ctx, is.Intent, 30)
		if len(history) == 0 {
			history = is.RecentHistory
		}
		view.Intents = append(view.Intents, HeuristicIntentView{
			Intent:             is.Intent,
			Current:            is.CurrentProfile,
			Default:            def,
			CurrentProfileID:   is.CurrentProfileID,
			Deltas:             computeDeltas(is.CurrentProfile, def),
			SamplesToday:       is.SamplesToday,
			Samples7d:          is.Samples7d,
			MeanRawReward7d:    is.MeanRawReward7d,
			P50RawReward7d:     is.P50RawReward7d,
			P95RawReward7d:     is.P95RawReward7d,
			ExplicitFraction7d: is.ExplicitFraction7d,
			ImplicitFraction7d: is.ImplicitFraction7d,
			RecentHistory:      history,
			RewardHistory:      buildRewardHistory(picker.Store.RecentRewards(ctx, is.Intent, 2, 5000), RewardHistoryHours),
			Buckets:            buckets,
		})
	}
	return view
}

// buildRewardHistory downsamples raw reward rows into a fixed-width
// hourly time-series, oldest-first. Empty buckets are kept so the
// dashboard's SVG sparkline can render time spacing correctly (a 4h
// gap looks like 4 zero-count bars, not a single thin bar).
//
// Returns nil when no rewards fall inside the window so the JSON
// payload omits the field entirely for cold intents.
func buildRewardHistory(rows []heuristics.RewardRow, hours int) []RewardBucket {
	if hours <= 0 {
		return nil
	}
	if len(rows) == 0 {
		return nil
	}
	bucketMs := int64(60 * 60 * 1000)
	now := nowMs()
	// Align the right edge to the current hour boundary so successive
	// dashboard refreshes don't shift the entire chart horizontally
	// every second.
	endMs := (now/bucketMs + 1) * bucketMs
	startMs := endMs - int64(hours)*bucketMs

	type acc struct {
		sum   float64
		count int
	}
	buckets := make([]acc, hours)
	any := false
	for _, r := range rows {
		if r.Timestamp < startMs || r.Timestamp >= endMs {
			continue
		}
		idx := int((r.Timestamp - startMs) / bucketMs)
		if idx < 0 || idx >= hours {
			continue
		}
		buckets[idx].sum += r.RawReward
		buckets[idx].count++
		any = true
	}
	if !any {
		return nil
	}
	out := make([]RewardBucket, hours)
	for i := 0; i < hours; i++ {
		b := RewardBucket{
			TimestampMs: startMs + int64(i)*bucketMs,
			Count:       buckets[i].count,
		}
		if buckets[i].count > 0 {
			b.Mean = buckets[i].sum / float64(buckets[i].count)
		}
		out[i] = b
	}
	return out
}

// LoadTopics returns every centroid registry entry across all known
// intents. Used by the /api/topics endpoint to populate the dashboard's
// flat topic browser. id_only labelling — no attempt to auto-name
// clusters; users opted in to that mode explicitly.
func LoadTopics(ctx context.Context, picker *heuristics.Picker) []TopicView {
	if picker == nil || picker.Topics == nil {
		return nil
	}
	var out []TopicView
	for _, intent := range heuristics.KnownIntents {
		for _, repo := range picker.Topics.ListRepos(ctx, intent) {
			for _, t := range picker.Topics.List(ctx, intent, repo) {
				out = append(out, TopicView{
					Intent:          intent,
					Repo:            repo,
					TopicID:         t.ID,
					TopicLabel:      t.Label,
					CentroidSamples: t.Samples,
					CreatedAt:       t.CreatedAt,
					LastSeen:        t.LastSeen,
					HotEnough:       t.Samples >= heuristics.TopicBanditSampleFloor,
				})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Intent != out[j].Intent {
			return out[i].Intent < out[j].Intent
		}
		if out[i].Repo != out[j].Repo {
			return out[i].Repo < out[j].Repo
		}
		return out[i].TopicID < out[j].TopicID
	})
	return out
}

// computeDeltas walks every knob in canonical order and returns the
// non-zero (or zero, see below) diff against the default. Zero-delta
// knobs are included too so the UI can render the full grid; the
// caller filters when it wants only the interesting ones.
func computeDeltas(cur, def heuristics.Profile) []ProfileDelta {
	b := heuristics.Bounds
	pairs := []struct {
		name      string
		c, d      int
		min, maxv int
	}{
		{"max_trace", cur.MaxTrace, def.MaxTrace, b.MaxTrace[0], b.MaxTrace[1]},
		{"caller_hops", cur.CallerHops, def.CallerHops, b.CallerHops[0], b.CallerHops[1]},
		{"max_upstream", cur.MaxUpstream, def.MaxUpstream, b.MaxUpstream[0], b.MaxUpstream[1]},
		{"max_downstream", cur.MaxDownstream, def.MaxDownstream, b.MaxDownstream[0], b.MaxDownstream[1]},
		{"max_importers", cur.MaxImporters, def.MaxImporters, b.MaxImporters[0], b.MaxImporters[1]},
		{"max_methods", cur.MaxMethods, def.MaxMethods, b.MaxMethods[0], b.MaxMethods[1]},
		{"max_siblings", cur.MaxSiblings, def.MaxSiblings, b.MaxSiblings[0], b.MaxSiblings[1]},
		{"max_snippets", cur.MaxSnippets, def.MaxSnippets, b.MaxSnippets[0], b.MaxSnippets[1]},
		{"max_impact", cur.MaxImpact, def.MaxImpact, b.MaxImpact[0], b.MaxImpact[1]},
		{"max_symbols", cur.MaxSymbols, def.MaxSymbols, b.MaxSymbols[0], b.MaxSymbols[1]},
		{"max_primary_ctx", cur.MaxPrimaryCtx, def.MaxPrimaryCtx, b.MaxPrimaryCtx[0], b.MaxPrimaryCtx[1]},
		{"max_decisions", cur.MaxDecisions, def.MaxDecisions, b.MaxDecisions[0], b.MaxDecisions[1]},
	}
	out := make([]ProfileDelta, 0, len(pairs))
	for _, p := range pairs {
		out = append(out, ProfileDelta{
			Knob:    p.name,
			Current: p.c,
			Default: p.d,
			Delta:   p.c - p.d,
			Min:     p.min,
			Max:     p.maxv,
		})
	}
	return out
}

// DecisionNode is one decision plus its lineage links so the UI can
// render the supersession chain as a tree.
type DecisionNode struct {
	Name         string   `json:"name"`
	Repo         string   `json:"repo"`
	DecisionType string   `json:"decision_type"`
	Decision     string   `json:"decision"`
	Rationale    string   `json:"rationale"`
	Alternatives string   `json:"alternatives,omitempty"`
	Constraint   string   `json:"constraint,omitempty"`
	Scope        string   `json:"scope,omitempty"`
	Files        []string `json:"files,omitempty"`
	Status       string   `json:"status"`
	Supersedes   string   `json:"supersedes,omitempty"`
	SupersededBy string   `json:"superseded_by,omitempty"`
	UpdatedAt    int64    `json:"updated_at"`
}

// LoadDecisions returns every decision for the given repo (active +
// superseded). Pass repo="" to get the cross-repo view — the per-row
// Repo field is sourced from the hit fields in that case so the UI
// can show which repo each decision came from. The ordering puts
// roots (no supersedes) first so the UI can render top-down chains
// naturally.
func LoadDecisions(_ context.Context, mem *memory.Store, repo string) []DecisionNode {
	if mem == nil {
		return nil
	}
	hits := mem.ListDecisions(repo, "", "", "", true)
	out := make([]DecisionNode, 0, len(hits))
	for _, h := range hits {
		// When repo arg is empty we're in the all-repos view, so the
		// row's repo has to come from the hit. When it's set, prefer
		// the arg (it's already authoritative + cheaper).
		rowRepo := repo
		if rowRepo == "" {
			rowRepo = h.Fields["repo"]
		}
		out = append(out, DecisionNode{
			Name:         h.Fields["name"],
			Repo:         rowRepo,
			DecisionType: h.Fields["decision_type"],
			Decision:     h.Fields["decision"],
			Rationale:    h.Fields["rationale"],
			Alternatives: h.Fields["alternatives"],
			Constraint:   h.Fields["constraint"],
			Scope:        h.Fields["scope"],
			Files:        splitCSV(h.Fields["files"]),
			Status:       h.Fields["status"],
			Supersedes:   h.Fields["supersedes"],
			SupersededBy: h.Fields["superseded_by"],
			UpdatedAt:    parseInt64(h.Fields["updated_at"]),
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		// In the all-repos view, group by repo first so the dashboard's
		// chain renderer (which expects parent/child decisions to sit
		// adjacent) keeps each repo's lineage intact.
		if out[i].Repo != out[j].Repo {
			return out[i].Repo < out[j].Repo
		}
		// Roots first; then by recency within each cohort.
		ri := out[i].Supersedes == ""
		rj := out[j].Supersedes == ""
		if ri != rj {
			return ri
		}
		return out[i].UpdatedAt > out[j].UpdatedAt
	})
	return out
}

// FlowNode is one flow memory with the files / entry points / linked
// funcs that the UI renders as a small dependency graph.
//
// Subgraph (when present) is the codegraph neighbourhood snapshot
// captured at memory_save_flow time — the dashboard renders it as a
// real callers/callees graph instead of the legacy bipartite files↔
// entries view. Nil when the flow had no entry points or the snapshot
// failed; in that case the UI falls back to the bipartite renderer.
//
// FileStats, AugmentedFiles and the *Feedback fields are populated from
// the flow:overlay:{keyspace}:{repo}:{name} hash that dev_feedback
// writes when agents echo flow_id. They are absent (omitempty) for
// flows that have never received feedback, so the UI can render them
// as neutral.
type FlowNode struct {
	Name        string        `json:"name"`
	Repo        string        `json:"repo"`
	Purpose     string        `json:"purpose"`
	Files       []string      `json:"files,omitempty"`
	EntryPoints []string      `json:"entry_points,omitempty"`
	Source      string        `json:"source"`
	UpdatedAt   int64         `json:"updated_at"`
	Subgraph    *FlowSubgraph `json:"subgraph,omitempty"`

	FileStats      []FileFeedbackStat `json:"file_stats,omitempty"`      // slice 1: per-file useful/dead
	AugmentedFiles []FileFeedbackStat `json:"augmented_files,omitempty"` // slice 2: agent-reported missing
	TotalFeedback  int                `json:"total_feedback,omitempty"`
	LastFeedbackAt int64              `json:"last_feedback_at,omitempty"`

	// Topics is the set of distinct topic labels of queries that have
	// historically pulled this flow in via memory_keys. Empty when no
	// query has referenced the flow under a non-* topic — a flow that
	// only ever gets retrieved on the intent-global fallback won't
	// appear under any topic filter. Built per /api/flows call by
	// scanning the bounded feedback:trace:index ZSET (capped at
	// TraceIndexCap=500), so cost stays flat regardless of fan-out.
	Topics []string `json:"topics,omitempty"`
}

// FlowSubgraph is the JSON shape the dashboard renders. It mirrors
// codegraph.Subgraph exactly but lives in the dashboard package to
// keep dashboard from importing internal/codegraph (which would create
// an awkward dependency graph and force the dashboard to compile in
// the codegraph client).
type FlowSubgraph struct {
	Seeds     []string           `json:"seeds"`
	Nodes     []FlowSubgraphNode `json:"nodes"`
	Edges     []FlowSubgraphEdge `json:"edges"`
	Truncated bool               `json:"truncated,omitempty"`
}

type FlowSubgraphNode struct {
	Name     string `json:"name"`
	FilePath string `json:"file,omitempty"`
	Role     string `json:"role"`
	// Depth is the BFS distance from the nearest seed along CALLS:
	// 0 = the seed itself, +N = N hops downstream (callee chain),
	// -N = N hops upstream (caller chain). Aux roles (importer /
	// extends / method) live at Depth=1. The UI's per-flow depth
	// slider filters which nodes render based on this field.
	Depth int `json:"depth"`
}

type FlowSubgraphEdge struct {
	From string `json:"from"`
	To   string `json:"to"`
	Type string `json:"type"`
}

// FileFeedbackStat is the dashboard-facing per-file aggregate. Mirrors
// memory.FlowFileStat but carries the path inline so the UI can render
// a flat list without ordering tricks.
type FileFeedbackStat struct {
	Path   string `json:"path"`
	Useful int    `json:"useful,omitempty"`
	Dead   int    `json:"dead,omitempty"`
	// Missing is set for AugmentedFiles (slice 2) and counts how many
	// agents reported needing this file outside the flow.
	Missing int `json:"missing,omitempty"`
}

// LoadFlows pulls every flow memory for the given repo by enumerating
// the FT.SEARCH index. Pass repo="" to get every flow across every
// repo — the per-row Repo field comes from each hit so the UI can
// tell them apart in the all-repos view.
//
// When store is non-nil and len(hits)>0, overlays are bulk-loaded in
// one pipelined batch per (distinct) repo and merged into the matching
// FlowNodes so /api/flows is still ~constant round-trips. store may
// safely be nil in tests that pass only a redis client; overlay fields
// just stay empty.
//
// hStore (the heuristics store, holding the bounded
// feedback:trace:index ZSET) is used to derive each flow's Topics
// array from the queries that historically referenced it via
// memory_keys. May be nil — Topics just stays empty in that case, the
// rest of the response renders unchanged.
func LoadFlows(ctx context.Context, rdb *redis.Client, store *memory.Store, hStore *heuristics.Store, repo string) []FlowNode {
	if rdb == nil {
		return nil
	}
	filter := "@mem_type:{flow}"
	if repo != "" {
		filter += " @repo:{" + escapeTag(repo) + "}"
	}
	res, err := rdb.Do(ctx,
		"FT.SEARCH", "idx:mem:flow",
		filter,
		"LIMIT", "0", "500",
		"DIALECT", "2",
	).Result()
	if err != nil {
		return nil
	}
	hits := parseFTHits(res)
	out := make([]FlowNode, 0, len(hits))
	names := make([]string, 0, len(hits))
	for _, h := range hits {
		// Per-row repo: prefer the constructor arg (authoritative when
		// set), fall back to the hit's repo field for the all-repos view.
		rowRepo := repo
		if rowRepo == "" {
			rowRepo = h["repo"]
		}
		node := FlowNode{
			Name:        h["name"],
			Repo:        rowRepo,
			Purpose:     h["purpose"],
			Files:       splitCSV(h["files"]),
			EntryPoints: splitCSV(h["entry_points"]),
			Source:      h["source"],
			UpdatedAt:   parseInt64(h["updated_at"]),
		}
		// subgraph_json is a raw codegraph.Subgraph snapshot. Parse
		// best-effort: corruption (truncation, manual redis-cli edit,
		// schema drift) just means the UI shows the legacy bipartite
		// view for that one card — never break the whole flows page.
		if raw := h["subgraph_json"]; raw != "" {
			var sg FlowSubgraph
			if err := json.Unmarshal([]byte(raw), &sg); err == nil && len(sg.Nodes) > 0 {
				node.Subgraph = &sg
			}
		}
		out = append(out, node)
		names = append(names, h["name"])
	}

	// Attach overlays. Store may be nil under tests/test harnesses that
	// pass only a redis client; skip cleanly when so. Missing overlays
	// (flow has no feedback yet) come back as zero-value entries from
	// LoadFlowOverlays and emit nothing onto the FlowNode.
	//
	// LoadFlowOverlays is per-repo (it pipelines HGETALL on N hashes
	// in one repo's keyspace). For the all-repos view (repo=="") we
	// have to bucket names by their row's Repo and call once per
	// bucket — still O(distinct_repos) round-trips, which is bounded
	// by the handful of repos a devrouter instance tracks.
	if store != nil && len(out) > 0 {
		overlays := make(map[string]map[string]memory.FlowOverlay) // repo -> name -> overlay
		if repo != "" {
			overlays[repo] = store.LoadFlowOverlays(ctx, repo, names)
		} else {
			byRepo := make(map[string][]string)
			for _, n := range out {
				if n.Repo == "" {
					continue
				}
				byRepo[n.Repo] = append(byRepo[n.Repo], n.Name)
			}
			for r, ns := range byRepo {
				overlays[r] = store.LoadFlowOverlays(ctx, r, ns)
			}
		}
		for i := range out {
			ov, ok := overlays[out[i].Repo][out[i].Name]
			if !ok || ov.TotalFeedback == 0 {
				continue
			}
			out[i].TotalFeedback = ov.TotalFeedback
			out[i].LastFeedbackAt = ov.LastFeedbackAt
			if len(ov.Files) > 0 {
				stats := make([]FileFeedbackStat, 0, len(ov.Files))
				for path, st := range ov.Files {
					if st.Useful == 0 && st.Dead == 0 {
						continue
					}
					stats = append(stats, FileFeedbackStat{
						Path:   path,
						Useful: st.Useful,
						Dead:   st.Dead,
					})
				}
				sort.SliceStable(stats, func(a, b int) bool {
					return stats[a].Path < stats[b].Path
				})
				out[i].FileStats = stats
			}
			if len(ov.Missing) > 0 {
				aug := make([]FileFeedbackStat, 0, len(ov.Missing))
				for path, n := range ov.Missing {
					if n == 0 {
						continue
					}
					aug = append(aug, FileFeedbackStat{Path: path, Missing: n})
				}
				sort.SliceStable(aug, func(a, b int) bool {
					if aug[a].Missing != aug[b].Missing {
						return aug[a].Missing > aug[b].Missing
					}
					return aug[a].Path < aug[b].Path
				})
				out[i].AugmentedFiles = aug
			}
		}
	}

	// Topic join: each flow inherits the distinct topic labels of the
	// queries that historically referenced it via memory_keys. Bounded
	// by TraceIndexCap (=500) traces, so cost is flat and decoupled
	// from how many flows we're rendering. When no query has ever
	// landed under a real topic (today: topic clustering disabled or
	// no centroids yet), Topics stays nil and the Flows-tab Topic
	// dropdown shows just "all" — degrades gracefully.
	if hStore != nil && len(out) > 0 {
		flowTopics := flowTopicIndex(ctx, hStore, rdb)
		for i := range out {
			key := "mem:" + out[i].Repo + ":flow:" + out[i].Name
			if topics, ok := flowTopics[key]; ok && len(topics) > 0 {
				out[i].Topics = topics
			}
		}
	}

	sort.SliceStable(out, func(i, j int) bool {
		return out[i].UpdatedAt > out[j].UpdatedAt
	})
	return out
}

// flowTopicIndex scans the bounded feedback:trace:index ZSET once and
// returns flowMemoryKey -> sorted distinct topic labels. Reuses
// LoadRecentQueries' pipelined HGETALL so we do exactly one batched
// Redis round-trip regardless of trace count.
//
// We key on `mem:{repo}:flow:{name}` (the literal memory_keys CSV
// element) rather than {repo,name} so the lookup in LoadFlows is a
// single string concat + map probe per flow. Topic preference is
// label-then-id, mirroring the Queries tab; an empty/"*" topic
// (intent-global fallback) is intentionally dropped so flows that
// only ever fall back never pollute the Topic dropdown.
func flowTopicIndex(ctx context.Context, hStore *heuristics.Store, rdb *redis.Client) map[string][]string {
	rows := LoadRecentQueries(ctx, hStore, rdb, heuristics.TraceIndexCap)
	if len(rows) == 0 {
		return nil
	}
	tmp := make(map[string]map[string]struct{}, 64)
	for _, q := range rows {
		topic := q.TopicLabel
		if topic == "" {
			topic = q.TopicID
		}
		if topic == "" || topic == "*" {
			continue
		}
		for _, mk := range q.MemoryKeys {
			if !strings.Contains(mk, ":flow:") {
				continue
			}
			if tmp[mk] == nil {
				tmp[mk] = make(map[string]struct{}, 2)
			}
			tmp[mk][topic] = struct{}{}
		}
	}
	if len(tmp) == 0 {
		return nil
	}
	out := make(map[string][]string, len(tmp))
	for k, set := range tmp {
		topics := make([]string, 0, len(set))
		for t := range set {
			topics = append(topics, t)
		}
		sort.Strings(topics)
		out[k] = topics
	}
	return out
}

// FlowSignal is the cross-flow observability aggregate for the
// Heuristics tab. Computed by walking every flow:overlay:* hash once
// per dashboard refresh — bounded by total flows in Redis, which is
// O(hundreds) in practice, so doing it on the request path is fine.
//
// This view is explicitly NOT folded into the bandit (the flow signal
// scores memory accuracy, not codegraph context-shape — different
// layer). It lives on the Heuristics tab purely so devs can spot
// stale flows next to the bandit drift that those flows might be
// quietly biasing through false-positive memory retrieval.
type FlowSignal struct {
	TotalFlows           int             `json:"total_flows"`              // flows with any feedback at all
	TotalFeedbackEvents  int             `json:"total_feedback_events"`    // sum of total_feedback across overlays
	FilesUseful          int             `json:"files_useful"`             // sum of file_useful counters
	FilesDead            int             `json:"files_dead"`               // sum of file_dead counters
	FilesMissing         int             `json:"files_missing"`            // sum of missing counters
	ValidatedRate        float64         `json:"validated_rate"`           // useful / (useful + dead); 0 if denominator 0
	StaleFlows           []FlowSignalRow `json:"stale_flows,omitempty"`    // top dead-ratio flows (≥3 events)
	UnderspecifiedFlows  []FlowSignalRow `json:"underspecified_flows,omitempty"` // top missing-count flows
}

// FlowSignalRow is one flow's roll-up for the stale / underspecified
// leaderboards. Sorted by the relevant metric on the server side so
// the UI can render top-N without re-sorting.
type FlowSignalRow struct {
	Repo          string  `json:"repo"`
	Name          string  `json:"name"`
	TotalFeedback int     `json:"total_feedback"`
	Useful        int     `json:"useful"`
	Dead          int     `json:"dead"`
	Missing       int     `json:"missing"`
	DeadRatio     float64 `json:"dead_ratio,omitempty"`
}

// flowSignalMinFeedback gates entries into the stale-flow leaderboard.
// A flow needs at least this many feedback events before its dead-ratio
// is shown to humans — below this the signal is too noisy to act on.
const flowSignalMinFeedback = 3

// LoadFlowSignal walks every flow overlay in Redis and rolls them up
// into the cross-flow summary the Heuristics tab renders.
//
// Two leaderboards are computed:
//   - StaleFlows: top-N by dead_ratio, gated by flowSignalMinFeedback
//   - UnderspecifiedFlows: top-N by total missing-file reports
//
// Both lists are bounded at 8 entries so the panel stays compact.
func LoadFlowSignal(ctx context.Context, store *memory.Store) FlowSignal {
	if store == nil {
		return FlowSignal{}
	}
	overlays := store.LoadAllFlowOverlays(ctx)
	out := FlowSignal{}

	stale := make([]FlowSignalRow, 0, len(overlays))
	missing := make([]FlowSignalRow, 0, len(overlays))

	for _, ov := range overlays {
		if ov.TotalFeedback == 0 {
			continue
		}
		out.TotalFlows++
		out.TotalFeedbackEvents += ov.TotalFeedback

		var useful, dead int
		for _, st := range ov.Files {
			useful += st.Useful
			dead += st.Dead
		}
		var missingCount int
		for _, n := range ov.Missing {
			missingCount += n
		}
		out.FilesUseful += useful
		out.FilesDead += dead
		out.FilesMissing += missingCount

		row := FlowSignalRow{
			Repo:          ov.Repo,
			Name:          ov.Name,
			TotalFeedback: ov.TotalFeedback,
			Useful:        useful,
			Dead:          dead,
			Missing:       missingCount,
		}
		if total := useful + dead; total > 0 {
			row.DeadRatio = float64(dead) / float64(total)
		}
		if ov.TotalFeedback >= flowSignalMinFeedback && row.DeadRatio > 0 {
			stale = append(stale, row)
		}
		if missingCount > 0 {
			missing = append(missing, row)
		}
	}

	if total := out.FilesUseful + out.FilesDead; total > 0 {
		out.ValidatedRate = float64(out.FilesUseful) / float64(total)
	}

	sort.SliceStable(stale, func(i, j int) bool {
		if stale[i].DeadRatio != stale[j].DeadRatio {
			return stale[i].DeadRatio > stale[j].DeadRatio
		}
		return stale[i].TotalFeedback > stale[j].TotalFeedback
	})
	sort.SliceStable(missing, func(i, j int) bool {
		if missing[i].Missing != missing[j].Missing {
			return missing[i].Missing > missing[j].Missing
		}
		return missing[i].TotalFeedback > missing[j].TotalFeedback
	})

	const maxRows = 8
	if len(stale) > maxRows {
		stale = stale[:maxRows]
	}
	if len(missing) > maxRows {
		missing = missing[:maxRows]
	}
	out.StaleFlows = stale
	out.UnderspecifiedFlows = missing
	return out
}

// LoadRepos returns the set of repos that have at least one memory
// stored under mem:{repo}:{type}:{name} — derived by SCAN so it
// always reflects the actual state of Redis (no separate registry to
// drift).
//
// We deliberately skip keys whose 3rd segment isn't a known mem_type:
// the false-positive index lives under mem:fp:mem:{repo}:{type}:{name}
// and would otherwise leak the literal "fp" as a phantom repo.
func LoadRepos(ctx context.Context, rdb *redis.Client) []string {
	if rdb == nil {
		return nil
	}
	knownTypes := map[string]struct{}{
		"file": {}, "func": {}, "flow": {}, "decision": {},
	}
	seen := map[string]struct{}{}
	var cursor uint64
	for {
		keys, next, err := rdb.Scan(ctx, cursor, "mem:*", 500).Result()
		if err != nil {
			break
		}
		for _, k := range keys {
			parts := strings.SplitN(k, ":", 4)
			if len(parts) < 4 {
				continue
			}
			if _, ok := knownTypes[parts[2]]; !ok {
				continue
			}
			seen[parts[1]] = struct{}{}
		}
		cursor = next
		if cursor == 0 {
			break
		}
	}
	out := make([]string, 0, len(seen))
	for r := range seen {
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}

// SummaryStats is a tiny top-of-dashboard widget: total queries indexed
// in the last day, count of repos, count of active decisions, count of
// recent feedback joins.
type SummaryStats struct {
	RecentQueries24h    int `json:"recent_queries_24h"`
	FeedbackJoined24h   int `json:"feedback_joined_24h"`
	Repos               int `json:"repos"`
	TraceIndexCap       int `json:"trace_index_cap"`
	TraceIndexCount     int `json:"trace_index_count"`
}

// LoadSummary scrapes the cheap counters: ZCARD of the trace index, the
// repo count, and a per-row scan of the most recent 200 traces to count
// how many landed within the last 24h and how many of those have a
// feedback record.
func LoadSummary(ctx context.Context, store *heuristics.Store, rdb *redis.Client) SummaryStats {
	if store == nil || rdb == nil {
		return SummaryStats{TraceIndexCap: heuristics.TraceIndexCap}
	}
	rows := LoadRecentQueries(ctx, store, rdb, 200)
	cutoff := nowMs() - 24*60*60*1000
	q24, f24 := 0, 0
	for _, r := range rows {
		if r.Timestamp >= cutoff {
			q24++
			if r.HasFeedback {
				f24++
			}
		}
	}
	cnt, _ := rdb.ZCard(ctx, "feedback:trace:index").Result()
	return SummaryStats{
		RecentQueries24h:  q24,
		FeedbackJoined24h: f24,
		Repos:             len(LoadRepos(ctx, rdb)),
		TraceIndexCap:     heuristics.TraceIndexCap,
		TraceIndexCount:   int(cnt),
	}
}

// ---------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------

func parseInt(s string) int {
	if s == "" {
		return 0
	}
	n, _ := strconv.Atoi(s)
	return n
}

func parseInt64(s string) int64 {
	if s == "" {
		return 0
	}
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
}

func parseFloat(s string) float64 {
	if s == "" {
		return 0
	}
	f, _ := strconv.ParseFloat(s, 64)
	return f
}

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

// escapeTag mirrors the tag-escape rules used by memory.Store so
// FT.SEARCH filter strings produced here behave identically to the
// router's queries.
func escapeTag(s string) string {
	replacer := strings.NewReplacer(
		"-", "\\-",
		".", "\\.",
		"@", "\\@",
		" ", "\\ ",
	)
	return replacer.Replace(s)
}

// parseFTHits flattens the FT.SEARCH wire format ([count, key1,
// fields1, key2, fields2, …]) into per-key field maps. The "embedding"
// field is dropped because it's a binary blob.
func parseFTHits(raw interface{}) []map[string]string {
	arr, ok := raw.([]interface{})
	if !ok || len(arr) < 2 {
		return nil
	}
	out := make([]map[string]string, 0, len(arr)/2)
	for i := 1; i+1 < len(arr); i += 2 {
		fields, ok := arr[i+1].([]interface{})
		if !ok {
			continue
		}
		m := make(map[string]string, len(fields)/2)
		for j := 0; j+1 < len(fields); j += 2 {
			name, _ := fields[j].(string)
			val, _ := fields[j+1].(string)
			if name == "embedding" {
				continue
			}
			m[name] = val
		}
		out = append(out, m)
	}
	return out
}

// nowMs is a small indirection point that test code can stub if we
// ever want to drive deterministic 24h-window snapshots.
func nowMs() int64 {
	return time.Now().UnixMilli()
}
