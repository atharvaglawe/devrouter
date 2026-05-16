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
	QueryID         string  `json:"query_id"`
	Query           string  `json:"query"`
	Repo            string  `json:"repo"`
	Intent          string  `json:"intent"`
	ProfileID       string  `json:"heuristic_profile_id"`
	Timestamp       int64   `json:"timestamp"`
	LatencyMs       int     `json:"latency_ms"`
	PromptTokens    int     `json:"prompt_tokens"`
	FilesReturned   int     `json:"files_returned"`
	SymbolsReturned int     `json:"symbols_returned"`
	SnippetsReturned int    `json:"snippets_returned"`
	TrimmedFiles    int     `json:"trimmed_files"`
	BudgetUsed      float64 `json:"budget_used_fraction"`
	MemoryKeys      []string `json:"memory_keys,omitempty"`

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
}

// HeuristicsView is the full payload for the Heuristics tab.
type HeuristicsView struct {
	Frozen       bool                  `json:"frozen"`
	TunableKnobs []string              `json:"tunable_knobs"`
	Intents      []HeuristicIntentView `json:"intents"`
}

// LoadHeuristics composes the Heuristics view from picker.Stats() plus
// the per-intent frozen default (loaded directly from Redis so the
// dashboard can show the bandit's accumulated drift).
func LoadHeuristics(ctx context.Context, picker *heuristics.Picker) HeuristicsView {
	if picker == nil {
		return HeuristicsView{}
	}
	snap := picker.Stats()
	view := HeuristicsView{
		Frozen:       snap.Frozen,
		TunableKnobs: snap.TunableKnobs,
		Intents:      make([]HeuristicIntentView, 0, len(snap.Intents)),
	}
	for _, is := range snap.Intents {
		def, err := picker.Store.LoadDefault(ctx, is.Intent)
		if err != nil {
			def = heuristics.Default(is.Intent)
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
		})
	}
	return view
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
// superseded). The ordering puts roots (no supersedes) first so the UI
// can render top-down chains naturally.
func LoadDecisions(_ context.Context, mem *memory.Store, repo string) []DecisionNode {
	if mem == nil || repo == "" {
		return nil
	}
	hits := mem.ListDecisions(repo, "", "", "", true)
	out := make([]DecisionNode, 0, len(hits))
	for _, h := range hits {
		out = append(out, DecisionNode{
			Name:         h.Fields["name"],
			Repo:         repo,
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
type FlowNode struct {
	Name        string   `json:"name"`
	Repo        string   `json:"repo"`
	Purpose     string   `json:"purpose"`
	Files       []string `json:"files,omitempty"`
	EntryPoints []string `json:"entry_points,omitempty"`
	Source      string   `json:"source"`
	UpdatedAt   int64    `json:"updated_at"`
}

// LoadFlows pulls every flow memory for the given repo by enumerating
// the FT.SEARCH index. Scoped to "*" (any text) so the UI never misses
// flows that fall outside the embedding match window.
func LoadFlows(ctx context.Context, rdb *redis.Client, repo string) []FlowNode {
	if rdb == nil || repo == "" {
		return nil
	}
	filter := "@mem_type:{flow} @repo:{" + escapeTag(repo) + "}"
	res, err := rdb.Do(ctx,
		"FT.SEARCH", "idx:mem:flow",
		filter,
		"LIMIT", "0", "200",
		"DIALECT", "2",
	).Result()
	if err != nil {
		return nil
	}
	hits := parseFTHits(res)
	out := make([]FlowNode, 0, len(hits))
	for _, h := range hits {
		out = append(out, FlowNode{
			Name:        h["name"],
			Repo:        repo,
			Purpose:     h["purpose"],
			Files:       splitCSV(h["files"]),
			EntryPoints: splitCSV(h["entry_points"]),
			Source:      h["source"],
			UpdatedAt:   parseInt64(h["updated_at"]),
		})
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
