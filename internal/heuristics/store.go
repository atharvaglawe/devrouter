package heuristics

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	keyCurrent = "heuristics:current"
	keyDefault = "heuristics:default"
	keyHistory = "heuristics:history"
	keyReward  = "heuristics:reward"
	keyTrace   = "feedback:trace"
	keyRecent  = "recent_queries"

	// Shape segment is reserved for v2 (intent, query_shape) keying.
	// v1 always uses "*" so adding a real shape later doesn't require
	// a Redis-key migration.
	defaultShape = "*"

	traceTTL  = 30 * 24 * time.Hour
	rewardTTL = 90 * 24 * time.Hour
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
func (s *Store) PutTrace(ctx context.Context, queryID string, fields map[string]interface{}) error {
	if queryID == "" || len(fields) == 0 {
		return nil
	}
	key := s.traceKey(queryID)
	pipe := s.rdb.Pipeline()
	pipe.HSet(ctx, key, fields)
	pipe.Expire(ctx, key, traceTTL)
	_, err := pipe.Exec(ctx)
	return err
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
