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
	row.HasFeedback = row.FeedbackAt > 0 || row.FeedbackSource != ""
	return row
}

// ProfileDelta is the per-knob diff between the live profile and the
// frozen default. Surfacing this is the single most-asked observability
// question for the heuristics layer: "what has the bandit changed?"
type ProfileDelta struct {
	Knob    string `json:"knob"`
	Current int    `json:"current"`
	Default int    `json:"default"`
	Delta   int    `json:"delta"`
}

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
	Buckets            []HeuristicBucketView     `json:"buckets,omitempty"`
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
			RecentHistory:      is.RecentHistory,
			Buckets:            buckets,
		})
	}
	return view
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
	pairs := []struct {
		name string
		c, d int
	}{
		{"max_trace", cur.MaxTrace, def.MaxTrace},
		{"caller_hops", cur.CallerHops, def.CallerHops},
		{"max_upstream", cur.MaxUpstream, def.MaxUpstream},
		{"max_downstream", cur.MaxDownstream, def.MaxDownstream},
		{"max_importers", cur.MaxImporters, def.MaxImporters},
		{"max_methods", cur.MaxMethods, def.MaxMethods},
		{"max_siblings", cur.MaxSiblings, def.MaxSiblings},
		{"max_snippets", cur.MaxSnippets, def.MaxSnippets},
		{"max_impact", cur.MaxImpact, def.MaxImpact},
		{"max_symbols", cur.MaxSymbols, def.MaxSymbols},
		{"max_primary_ctx", cur.MaxPrimaryCtx, def.MaxPrimaryCtx},
		{"max_decisions", cur.MaxDecisions, def.MaxDecisions},
	}
	out := make([]ProfileDelta, 0, len(pairs))
	for _, p := range pairs {
		out = append(out, ProfileDelta{
			Knob:    p.name,
			Current: p.c,
			Default: p.d,
			Delta:   p.c - p.d,
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
type FlowNode struct {
	Name        string         `json:"name"`
	Repo        string         `json:"repo"`
	Purpose     string         `json:"purpose"`
	Files       []string       `json:"files,omitempty"`
	EntryPoints []string       `json:"entry_points,omitempty"`
	Source      string         `json:"source"`
	UpdatedAt   int64          `json:"updated_at"`
	Subgraph    *FlowSubgraph  `json:"subgraph,omitempty"`
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

// LoadFlows pulls every flow memory for the given repo by enumerating
// the FT.SEARCH index. Pass repo="" to get every flow across every
// repo — the per-row Repo field comes from each hit so the UI can
// tell them apart in the all-repos view.
func LoadFlows(ctx context.Context, rdb *redis.Client, repo string) []FlowNode {
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
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].UpdatedAt > out[j].UpdatedAt
	})
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
