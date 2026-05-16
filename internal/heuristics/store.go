package heuristics

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	keyCurrent    = "heuristics:current"
	keyDefault    = "heuristics:default"
	keyHistory    = "heuristics:history"
	keyReward     = "heuristics:reward"
	keyTrace      = "feedback:trace"
	keyTraceIndex = "feedback:trace:index"
	keyRecent     = "recent_queries"

	// Shape segment is reserved for v2 (intent, query_shape) keying.
	// v1 always uses "*" so adding a real shape later doesn't require
	// a Redis-key migration.
	defaultShape = "*"

	traceTTL  = 30 * 24 * time.Hour
	rewardTTL = 90 * 24 * time.Hour

	// TraceIndexCap bounds the feedback:trace:index ZSET so the
	// observability surface (dashboard, debug tooling) can enumerate
	// recent queries in O(log N) without paying Redis SCAN cost. Older
	// entries are trimmed by score on every PutTrace; the underlying
	// feedback:trace:{id} HASH still ages out via traceTTL.
	TraceIndexCap = 500
)

// Store wraps a redis.Client with helpers for the heuristics package.
// Reuses the existing memory.Store's Redis connection so we don't double
// up on connection pools.
type Store struct {
	rdb *redis.Client
}

func NewStore(rdb *redis.Client) *Store {
	return &Store{rdb: rdb}
}

// HistoryEntry records one profile-change event for audit.
type HistoryEntry struct {
	Timestamp int64   `json:"timestamp"`
	Kind      string  `json:"kind"` // "promote" | "discard" | "rollback" | "seed"
	From      Profile `json:"from"`
	To        Profile `json:"to"`
	Reason    string  `json:"reason,omitempty"`
}

// RewardRow is one signal-source-tagged sample on the daily reward list.
type RewardRow struct {
	QueryID         string  `json:"query_id"`
	ProfileID       string  `json:"profile_id"`
	RawReward       float64 `json:"raw_reward"`
	AdjustedReward  float64 `json:"adjusted_reward"`
	PromptTokens    int     `json:"prompt_tokens"`
	AdditionalFiles int     `json:"additional_files"`
	Source          string  `json:"source"` // "explicit" | "implicit_repeat"
	Weight          float64 `json:"weight"`
	Timestamp       int64   `json:"timestamp"`
}

// CurrentProfile loads heuristics:current:{intent}:* — the live profile.
// On first access it self-seeds from Default(intent) and writes both the
// current and the frozen default snapshot.
func (s *Store) CurrentProfile(ctx context.Context, intent string) (Profile, error) {
	key := s.currentKey(intent)
	val, err := s.rdb.Get(ctx, key).Result()
	if err == redis.Nil {
		seed := Default(intent)
		if e := s.SetCurrent(ctx, intent, seed); e != nil {
			return seed, e
		}
		if e := s.EnsureDefault(ctx, intent, seed); e != nil {
			return seed, e
		}
		return seed, nil
	}
	if err != nil {
		return Default(intent), err
	}
	var p Profile
	if err := json.Unmarshal([]byte(val), &p); err != nil {
		return Default(intent), nil
	}
	return p.Clip(), nil
}

// SetCurrent overwrites the live profile for an intent.
func (s *Store) SetCurrent(ctx context.Context, intent string, p Profile) error {
	data, _ := json.Marshal(p.Clip())
	return s.rdb.Set(ctx, s.currentKey(intent), data, 0).Err()
}

// EnsureDefault writes heuristics:default:{intent}:* once. Idempotent —
// on re-run it leaves the existing default untouched. The default is
// the rollback target and must never mutate after first write.
func (s *Store) EnsureDefault(ctx context.Context, intent string, p Profile) error {
	key := s.defaultKey(intent)
	n, err := s.rdb.Exists(ctx, key).Result()
	if err == nil && n > 0 {
		return nil
	}
	data, _ := json.Marshal(p.Clip())
	return s.rdb.Set(ctx, key, data, 0).Err()
}

// LoadDefault reads the frozen default snapshot for rollback.
func (s *Store) LoadDefault(ctx context.Context, intent string) (Profile, error) {
	val, err := s.rdb.Get(ctx, s.defaultKey(intent)).Result()
	if err != nil {
		return Default(intent), err
	}
	var p Profile
	if err := json.Unmarshal([]byte(val), &p); err != nil {
		return Default(intent), err
	}
	return p.Clip(), nil
}

// AppendHistory records a profile change or rollback.
func (s *Store) AppendHistory(ctx context.Context, intent string, entry HistoryEntry) error {
	data, _ := json.Marshal(entry)
	pipe := s.rdb.Pipeline()
	pipe.LPush(ctx, s.historyKey(intent), data)
	pipe.LTrim(ctx, s.historyKey(intent), 0, 199) // cap at 200 entries
	_, err := pipe.Exec(ctx)
	return err
}

// History returns the most recent N profile-change entries for an intent,
// newest first.
func (s *Store) History(ctx context.Context, intent string, n int) []HistoryEntry {
	if n <= 0 {
		n = 10
	}
	items, err := s.rdb.LRange(ctx, s.historyKey(intent), 0, int64(n-1)).Result()
	if err != nil {
		return nil
	}
	out := make([]HistoryEntry, 0, len(items))
	for _, raw := range items {
		var h HistoryEntry
		if err := json.Unmarshal([]byte(raw), &h); err == nil {
			out = append(out, h)
		}
	}
	return out
}

// AppendReward writes a compact daily reward row. Daily keys are used
// (not a rolling window) so historical analysis, regression debugging,
// and replay experiments are trivial later — see plan section "store.go".
func (s *Store) AppendReward(ctx context.Context, intent string, row RewardRow) error {
	if row.Timestamp == 0 {
		row.Timestamp = time.Now().UnixMilli()
	}
	data, _ := json.Marshal(row)
	key := s.rewardKey(intent, time.UnixMilli(row.Timestamp))
	pipe := s.rdb.Pipeline()
	pipe.RPush(ctx, key, data)
	pipe.Expire(ctx, key, rewardTTL)
	_, err := pipe.Exec(ctx)
	return err
}

// RecentRewards returns up to 'limit' most recent reward rows for an intent
// across the last 'days' days. Used for both the rolling-mean baseline and
// for stats reporting.
func (s *Store) RecentRewards(ctx context.Context, intent string, days, limit int) []RewardRow {
	if days <= 0 {
		days = 1
	}
	if limit <= 0 {
		limit = 1000
	}
	var out []RewardRow
	for i := 0; i < days; i++ {
		key := s.rewardKey(intent, time.Now().Add(-time.Duration(i)*24*time.Hour))
		// Walk newest-to-oldest within the day's list (RPUSH appends, so
		// the freshest entries are at the tail). LRange across whole list
		// then iterate in reverse.
		items, err := s.rdb.LRange(ctx, key, 0, -1).Result()
		if err != nil || len(items) == 0 {
			continue
		}
		for j := len(items) - 1; j >= 0; j-- {
			var r RewardRow
			if err := json.Unmarshal([]byte(items[j]), &r); err == nil {
				out = append(out, r)
				if len(out) >= limit {
					return out
				}
			}
		}
	}
	return out
}

// RollingMean returns the unweighted mean raw_reward across recent samples
// for an intent. Used to compute adjusted_reward (see plan section
// "Baseline normalization").
func (s *Store) RollingMean(ctx context.Context, intent string, samples int) float64 {
	rows := s.RecentRewards(ctx, intent, 2, samples)
	if len(rows) == 0 {
		return 0
	}
	sum := 0.0
	for _, r := range rows {
		sum += r.RawReward
	}
	return sum / float64(len(rows))
}

// PutTrace HSETs the decision-side fields onto feedback:trace:{queryID}
// with TTL. Called at end of HandleQuery before the response is written.
//
// Also indexes the trace ID in feedback:trace:index (ZSET scored by
// timestamp, capped at TraceIndexCap) so the dashboard / debug tooling
// can list recent queries in O(log N) without a Redis SCAN.
func (s *Store) PutTrace(ctx context.Context, queryID string, fields map[string]interface{}) error {
	if queryID == "" || len(fields) == 0 {
		return nil
	}
	key := s.traceKey(queryID)
	score := traceScore(fields)
	pipe := s.rdb.Pipeline()
	pipe.HSet(ctx, key, fields)
	pipe.Expire(ctx, key, traceTTL)
	pipe.ZAdd(ctx, keyTraceIndex, redis.Z{Score: score, Member: queryID})
	// Trim to last TraceIndexCap entries (keep the newest by score).
	pipe.ZRemRangeByRank(ctx, keyTraceIndex, 0, int64(-TraceIndexCap-1))
	_, err := pipe.Exec(ctx)
	return err
}

// RecentTraceIDs returns the most recent query IDs from the trace index,
// newest first. Bounded by TraceIndexCap regardless of the requested limit.
func (s *Store) RecentTraceIDs(ctx context.Context, limit int) []string {
	if limit <= 0 {
		limit = 50
	}
	if limit > TraceIndexCap {
		limit = TraceIndexCap
	}
	ids, err := s.rdb.ZRevRange(ctx, keyTraceIndex, 0, int64(limit-1)).Result()
	if err != nil {
		return nil
	}
	return ids
}

// traceScore extracts a millisecond timestamp from the trace fields if
// present, falling back to time.Now() so an entry without a timestamp
// still lands at the top of the index (rather than at the bottom where
// it'd be invisible).
func traceScore(fields map[string]interface{}) float64 {
	if v, ok := fields["timestamp"]; ok {
		switch t := v.(type) {
		case int64:
			return float64(t)
		case int:
			return float64(t)
		case float64:
			return t
		case string:
			// traceHashFields stores timestamp as a stringified unix-ms
			// (fmt.Sprintf("%d", t.Timestamp)); fall back to RFC3339 for
			// any other caller that decides to be ISO-friendly.
			if n, err := strconv.ParseInt(t, 10, 64); err == nil {
				return float64(n)
			}
			if n, err := time.Parse(time.RFC3339, t); err == nil {
				return float64(n.UnixMilli())
			}
		}
	}
	return float64(time.Now().UnixMilli())
}

// PatchTrace HSETs additional fields without rewriting the whole record.
// Used by dev_feedback (feedback-side fields) and by repeat-detection
// (RepeatQuery flag).
func (s *Store) PatchTrace(ctx context.Context, queryID string, fields map[string]interface{}) error {
	if queryID == "" || len(fields) == 0 {
		return nil
	}
	return s.rdb.HSet(ctx, s.traceKey(queryID), fields).Err()
}

// GetTrace returns the full hash for a query.
func (s *Store) GetTrace(ctx context.Context, queryID string) (map[string]string, error) {
	if queryID == "" {
		return nil, nil
	}
	res, err := s.rdb.HGetAll(ctx, s.traceKey(queryID)).Result()
	if err != nil {
		return nil, err
	}
	if len(res) == 0 {
		return nil, nil
	}
	return res, nil
}

func (s *Store) currentKey(intent string) string {
	return fmt.Sprintf("%s:%s:%s", keyCurrent, intent, defaultShape)
}

func (s *Store) defaultKey(intent string) string {
	return fmt.Sprintf("%s:%s:%s", keyDefault, intent, defaultShape)
}

func (s *Store) historyKey(intent string) string {
	return fmt.Sprintf("%s:%s", keyHistory, intent)
}

func (s *Store) rewardKey(intent string, t time.Time) string {
	return fmt.Sprintf("%s:%s:%s", keyReward, intent, t.UTC().Format("2006-01-02"))
}

func (s *Store) traceKey(queryID string) string {
	return fmt.Sprintf("%s:%s", keyTrace, queryID)
}

// ---------------------------------------------------------------------------
// Per-bucket variants (keyed by intent + repo + topic_id)
//
// Per-bucket data lives under a brand-new `heuristics:bucket:*`
// namespace so the legacy `heuristics:current:{intent}:*` /
// `heuristics:reward:{intent}:{day}` / `heuristics:history:{intent}`
// keys keep working unchanged as the cold-start fallback. The
// picker's sample-floor logic decides which surface to read/write
// on a per-query basis — see picker.PickWithTopic.
// ---------------------------------------------------------------------------

// keyBucket is the common Redis prefix for everything per-(intent,
// repo, topic). Subkeys are ":current", ":history", and
// ":reward:{day}". Kept distinct from the legacy keys to avoid
// migration headaches.
const keyBucket = "heuristics:bucket"

// isGlobalBucket reports whether (repo, topic) addresses the legacy
// intent-global surface — i.e. no real bucket. Callers use this to
// short-circuit to the existing (intent)-only methods.
func isGlobalBucket(repo, topic string) bool {
	if topic == "" || topic == IntentGlobalTopic {
		return true
	}
	if repo == "" || repo == globalRepo {
		return true
	}
	return false
}

func (s *Store) bucketCurrentKey(intent, repo, topic string) string {
	return fmt.Sprintf("%s:%s:%s:%s:current", keyBucket, intent, repo, topic)
}

func (s *Store) bucketHistoryKey(intent, repo, topic string) string {
	return fmt.Sprintf("%s:%s:%s:%s:history", keyBucket, intent, repo, topic)
}

func (s *Store) bucketRewardKey(intent, repo, topic string, t time.Time) string {
	return fmt.Sprintf("%s:%s:%s:%s:reward:%s",
		keyBucket, intent, repo, topic, t.UTC().Format("2006-01-02"))
}

// CurrentProfileFor returns the live profile for a (intent, repo,
// topic) bucket. On first access it self-seeds from CurrentProfile
// (the intent-global), which itself self-seeds from Default(intent).
// So a brand-new bucket always starts at the known-good intent-global
// snapshot rather than wherever today's bandit happened to land it.
//
// Global-bucket callers (no real topic) are routed straight to
// CurrentProfile so we don't fragment the existing data surface.
func (s *Store) CurrentProfileFor(ctx context.Context, intent, repo, topic string) (Profile, error) {
	if isGlobalBucket(repo, topic) {
		return s.CurrentProfile(ctx, intent)
	}
	key := s.bucketCurrentKey(intent, repo, topic)
	val, err := s.rdb.Get(ctx, key).Result()
	if err == redis.Nil {
		seed, err := s.CurrentProfile(ctx, intent)
		if err != nil {
			return seed, err
		}
		if e := s.SetCurrentFor(ctx, intent, repo, topic, seed); e != nil {
			return seed, e
		}
		return seed, nil
	}
	if err != nil {
		return Default(intent), err
	}
	var p Profile
	if err := json.Unmarshal([]byte(val), &p); err != nil {
		return Default(intent), nil
	}
	return p.Clip(), nil
}

// SetCurrentFor overwrites the live profile for a bucket. Routes
// global-bucket writes through SetCurrent so legacy data stays in one
// place.
func (s *Store) SetCurrentFor(ctx context.Context, intent, repo, topic string, p Profile) error {
	if isGlobalBucket(repo, topic) {
		return s.SetCurrent(ctx, intent, p)
	}
	data, _ := json.Marshal(p.Clip())
	return s.rdb.Set(ctx, s.bucketCurrentKey(intent, repo, topic), data, 0).Err()
}

// AppendHistoryFor records a profile change for a bucket.
func (s *Store) AppendHistoryFor(ctx context.Context, intent, repo, topic string, entry HistoryEntry) error {
	if isGlobalBucket(repo, topic) {
		return s.AppendHistory(ctx, intent, entry)
	}
	data, _ := json.Marshal(entry)
	pipe := s.rdb.Pipeline()
	pipe.LPush(ctx, s.bucketHistoryKey(intent, repo, topic), data)
	pipe.LTrim(ctx, s.bucketHistoryKey(intent, repo, topic), 0, 199)
	_, err := pipe.Exec(ctx)
	return err
}

// HistoryFor returns the most recent N profile-change entries for a
// bucket, newest first.
func (s *Store) HistoryFor(ctx context.Context, intent, repo, topic string, n int) []HistoryEntry {
	if isGlobalBucket(repo, topic) {
		return s.History(ctx, intent, n)
	}
	if n <= 0 {
		n = 10
	}
	items, err := s.rdb.LRange(ctx, s.bucketHistoryKey(intent, repo, topic), 0, int64(n-1)).Result()
	if err != nil {
		return nil
	}
	out := make([]HistoryEntry, 0, len(items))
	for _, raw := range items {
		var h HistoryEntry
		if err := json.Unmarshal([]byte(raw), &h); err == nil {
			out = append(out, h)
		}
	}
	return out
}

// AppendRewardFor persists a reward row for a bucket. Daily keys
// mirror the legacy reward layout so existing analytics queries
// translate verbatim.
func (s *Store) AppendRewardFor(ctx context.Context, intent, repo, topic string, row RewardRow) error {
	if isGlobalBucket(repo, topic) {
		return s.AppendReward(ctx, intent, row)
	}
	if row.Timestamp == 0 {
		row.Timestamp = time.Now().UnixMilli()
	}
	data, _ := json.Marshal(row)
	key := s.bucketRewardKey(intent, repo, topic, time.UnixMilli(row.Timestamp))
	pipe := s.rdb.Pipeline()
	pipe.RPush(ctx, key, data)
	pipe.Expire(ctx, key, rewardTTL)
	_, err := pipe.Exec(ctx)
	return err
}

// RecentRewardsFor returns up to 'limit' most recent reward rows for
// a bucket across the last 'days' days.
func (s *Store) RecentRewardsFor(ctx context.Context, intent, repo, topic string, days, limit int) []RewardRow {
	if isGlobalBucket(repo, topic) {
		return s.RecentRewards(ctx, intent, days, limit)
	}
	if days <= 0 {
		days = 1
	}
	if limit <= 0 {
		limit = 1000
	}
	var out []RewardRow
	for i := 0; i < days; i++ {
		key := s.bucketRewardKey(intent, repo, topic,
			time.Now().Add(-time.Duration(i)*24*time.Hour))
		items, err := s.rdb.LRange(ctx, key, 0, -1).Result()
		if err != nil || len(items) == 0 {
			continue
		}
		for j := len(items) - 1; j >= 0; j-- {
			var r RewardRow
			if err := json.Unmarshal([]byte(items[j]), &r); err == nil {
				out = append(out, r)
				if len(out) >= limit {
					return out
				}
			}
		}
	}
	return out
}

// RollingMeanFor returns the unweighted mean raw_reward across recent
// samples for a bucket. Same baseline-normalisation use as
// RollingMean — but scoped so a bucket's adjusted reward is centred
// on its own history, not the intent's.
func (s *Store) RollingMeanFor(ctx context.Context, intent, repo, topic string, samples int) float64 {
	if isGlobalBucket(repo, topic) {
		return s.RollingMean(ctx, intent, samples)
	}
	rows := s.RecentRewardsFor(ctx, intent, repo, topic, 2, samples)
	if len(rows) == 0 {
		return 0
	}
	sum := 0.0
	for _, r := range rows {
		sum += r.RawReward
	}
	return sum / float64(len(rows))
}
