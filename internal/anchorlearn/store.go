package anchorlearn

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// Store is the persistence layer for anchor learning. The router holds
// a single Store and feeds Decide / RecordObservation / Reward* through
// it; the Store hides whether the backing is Redis (production) or an
// in-process map (tests, bench, cold-start without Redis configured).
//
// All methods are safe for concurrent use. Errors are returned but the
// caller is encouraged to log-and-continue: anchor learning is a best-
// effort optimisation, never on the critical path of dev_context.
type Store interface {
	// PutObservation persists an Observation under its QueryID for
	// later reward attribution. Lifetime ~30d (matches feedback trace).
	PutObservation(ctx context.Context, obs Observation) error
	// GetObservation looks up the observation written for queryID.
	// Returns (nil, nil) when missing — callers must handle the nil
	// case as "no anchor was injected" rather than as an error.
	GetObservation(ctx context.Context, queryID string) (*Observation, error)

	// IncPatternFire / IncPatternSuccess update the per-pattern
	// counters. Called on every anchor decision (fire) and every
	// successful reward attribution (success). Both maintain a
	// global record (repo == "") plus a per-repo record so Decide
	// can blend cross-repo prior with repo-local evidence.
	IncPatternFire(ctx context.Context, repo, patternID string) error
	IncPatternSuccess(ctx context.Context, repo, patternID string, weight float64) error
	GetPatternStats(ctx context.Context, repo, patternID string) (PatternStats, error)

	// Discovered patterns (Phase 3): file paths the agent kept
	// referencing under <svc>/ that aren't in the static portfolio.
	// AddDiscovered is called by RewardMemorySave; ListDiscovered is
	// consulted by Decide to widen the candidate pool.
	AddDiscovered(ctx context.Context, repo, suffix string) error
	ListDiscovered(ctx context.Context, repo string) ([]string, error)

	// IncKeywordAffinity updates the (kw, pattern) co-occurrence
	// counter. Called when reward attribution credits a pattern;
	// Decide consumes these to bias keyword-aligned patterns higher.
	IncKeywordAffinity(ctx context.Context, kw, patternID string, delta float64) error
	GetKeywordAffinity(ctx context.Context, kw, patternID string) (float64, error)
}

// ---------------------------------------------------------------------
// Redis-backed Store
// ---------------------------------------------------------------------

// RedisStore persists anchor-learning state under the same Redis
// instance that backs memory.Store. We deliberately reuse the shared
// client (passed in from memory.Store.RDB()) so this package never
// owns connection lifecycle — when memory.Store goes down we go down,
// when it comes back up we come back up, exactly the same model FP-
// centroids use.
type RedisStore struct {
	rdb *redis.Client
	ttl time.Duration
}

// NewRedisStore wraps a Redis client. ttl is applied to short-lived
// keys (observations, repo-pattern weights); pattern-global stats and
// discovered sets are long-lived (no TTL).
func NewRedisStore(rdb *redis.Client) *RedisStore {
	return &RedisStore{rdb: rdb, ttl: 30 * 24 * time.Hour}
}

const (
	prefixObs        = "anchor:obs:"
	prefixPattern    = "anchor:pattern:"
	prefixRepo       = "anchor:repo:"
	prefixDiscovered = "anchor:discovered:"
	prefixKeyword    = "anchor:keyword:"
)

func obsKey(queryID string) string { return prefixObs + queryID }
func patternKey(repo, patternID string) string {
	if repo == "" {
		return prefixPattern + patternID
	}
	return prefixRepo + repo + ":pattern:" + patternID
}
func discoveredKey(repo string) string       { return prefixDiscovered + repo }
func keywordKey(kw, patternID string) string { return prefixKeyword + kw + ":" + patternID }

// joinList / splitList serialise []string into a single hash field —
// Redis hashes don't store native arrays and we avoid pulling in a
// JSON library here for what is ultimately a CSV of file paths and
// pattern IDs (neither contains "|" by construction).
const listSep = "|"

func joinList(xs []string) string  { return strings.Join(xs, listSep) }
func splitList(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, listSep)
}

func (s *RedisStore) PutObservation(ctx context.Context, obs Observation) error {
	if s == nil || s.rdb == nil {
		return errors.New("anchorlearn: nil RedisStore")
	}
	key := obsKey(obs.QueryID)
	fields := map[string]any{
		"repo":        obs.Repo,
		"query":       obs.Query,
		"intent":      obs.Intent,
		"files":       joinList(obs.Files),
		"pattern_ids": joinList(obs.PatternIDs),
		"services":   joinList(obs.Services),
		"ts":          obs.Timestamp.Unix(),
	}
	if err := s.rdb.HSet(ctx, key, fields).Err(); err != nil {
		return fmt.Errorf("anchorlearn: HSet obs: %w", err)
	}
	if err := s.rdb.Expire(ctx, key, s.ttl).Err(); err != nil {
		return fmt.Errorf("anchorlearn: expire obs: %w", err)
	}
	return nil
}

func (s *RedisStore) GetObservation(ctx context.Context, queryID string) (*Observation, error) {
	if s == nil || s.rdb == nil {
		return nil, nil
	}
	fields, err := s.rdb.HGetAll(ctx, obsKey(queryID)).Result()
	if err != nil {
		return nil, fmt.Errorf("anchorlearn: HGetAll obs: %w", err)
	}
	if len(fields) == 0 {
		return nil, nil
	}
	ts, _ := strconv.ParseInt(fields["ts"], 10, 64)
	return &Observation{
		QueryID:    queryID,
		Repo:       fields["repo"],
		Query:      fields["query"],
		Intent:     fields["intent"],
		Files:      splitList(fields["files"]),
		PatternIDs: splitList(fields["pattern_ids"]),
		Services:   splitList(fields["services"]),
		Timestamp:  time.Unix(ts, 0),
	}, nil
}

func (s *RedisStore) IncPatternFire(ctx context.Context, repo, patternID string) error {
	if s == nil || s.rdb == nil {
		return nil
	}
	key := patternKey(repo, patternID)
	if err := s.rdb.HIncrBy(ctx, key, "fired", 1).Err(); err != nil {
		return fmt.Errorf("anchorlearn: HIncrBy fired: %w", err)
	}
	return s.rdb.HSet(ctx, key, "last_seen", time.Now().Unix()).Err()
}

func (s *RedisStore) IncPatternSuccess(ctx context.Context, repo, patternID string, weight float64) error {
	if s == nil || s.rdb == nil {
		return nil
	}
	key := patternKey(repo, patternID)
	// Store success as a float so partial credit (e.g. 0.5 for an
	// implicit memory-save signal vs 1.0 for an explicit
	// dev_feedback success) is preserved without requiring a
	// separate field.
	if err := s.rdb.HIncrByFloat(ctx, key, "success", weight).Err(); err != nil {
		return fmt.Errorf("anchorlearn: HIncrByFloat success: %w", err)
	}
	return s.rdb.HSet(ctx, key, "last_seen", time.Now().Unix()).Err()
}

func (s *RedisStore) GetPatternStats(ctx context.Context, repo, patternID string) (PatternStats, error) {
	out := PatternStats{PatternID: patternID, Repo: repo}
	if s == nil || s.rdb == nil {
		return out, nil
	}
	fields, err := s.rdb.HGetAll(ctx, patternKey(repo, patternID)).Result()
	if err != nil {
		return out, fmt.Errorf("anchorlearn: HGetAll pattern: %w", err)
	}
	if v, ok := fields["fired"]; ok {
		if n, err := strconv.Atoi(v); err == nil {
			out.FiredCount = n
		}
	}
	if v, ok := fields["success"]; ok {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			out.SuccessCount = int(f)
		}
	}
	if v, ok := fields["last_seen"]; ok {
		if ts, err := strconv.ParseInt(v, 10, 64); err == nil {
			out.LastSeen = time.Unix(ts, 0)
		}
	}
	return out, nil
}

func (s *RedisStore) AddDiscovered(ctx context.Context, repo, suffix string) error {
	if s == nil || s.rdb == nil {
		return nil
	}
	return s.rdb.SAdd(ctx, discoveredKey(repo), suffix).Err()
}

func (s *RedisStore) ListDiscovered(ctx context.Context, repo string) ([]string, error) {
	if s == nil || s.rdb == nil {
		return nil, nil
	}
	xs, err := s.rdb.SMembers(ctx, discoveredKey(repo)).Result()
	if err != nil {
		return nil, fmt.Errorf("anchorlearn: SMembers discovered: %w", err)
	}
	return xs, nil
}

func (s *RedisStore) IncKeywordAffinity(ctx context.Context, kw, patternID string, delta float64) error {
	if s == nil || s.rdb == nil {
		return nil
	}
	return s.rdb.IncrByFloat(ctx, keywordKey(kw, patternID), delta).Err()
}

func (s *RedisStore) GetKeywordAffinity(ctx context.Context, kw, patternID string) (float64, error) {
	if s == nil || s.rdb == nil {
		return 0, nil
	}
	v, err := s.rdb.Get(ctx, keywordKey(kw, patternID)).Float64()
	if errors.Is(err, redis.Nil) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("anchorlearn: Get kw aff: %w", err)
	}
	return v, nil
}

// ---------------------------------------------------------------------
// In-memory Store (tests, bench, no-Redis fallback)
// ---------------------------------------------------------------------

// MemStore is a thread-safe in-process implementation that's a
// drop-in replacement for RedisStore. Used in unit tests and as the
// graceful fallback when memory.Store is nil (the bench harness, or
// any deployment running without Redis configured). Behaviour is
// identical to RedisStore on a single process; nothing persists
// across restarts.
type MemStore struct {
	mu          sync.Mutex
	obs         map[string]Observation
	patternFire map[string]int     // key = patternKey(repo, id)
	patternSucc map[string]float64 // key = patternKey(repo, id)
	patternSeen map[string]time.Time
	discovered  map[string]map[string]bool
	kwAff       map[string]float64 // key = keywordKey(kw, id)
}

func NewMemStore() *MemStore {
	return &MemStore{
		obs:         make(map[string]Observation),
		patternFire: make(map[string]int),
		patternSucc: make(map[string]float64),
		patternSeen: make(map[string]time.Time),
		discovered:  make(map[string]map[string]bool),
		kwAff:       make(map[string]float64),
	}
}

func (s *MemStore) PutObservation(_ context.Context, o Observation) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := o
	cp.Files = append([]string{}, o.Files...)
	cp.PatternIDs = append([]string{}, o.PatternIDs...)
	cp.Services = append([]string{}, o.Services...)
	s.obs[o.QueryID] = cp
	return nil
}

func (s *MemStore) GetObservation(_ context.Context, queryID string) (*Observation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if o, ok := s.obs[queryID]; ok {
		cp := o
		cp.Files = append([]string{}, o.Files...)
		cp.PatternIDs = append([]string{}, o.PatternIDs...)
		cp.Services = append([]string{}, o.Services...)
		return &cp, nil
	}
	return nil, nil
}

func (s *MemStore) IncPatternFire(_ context.Context, repo, patternID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := patternKey(repo, patternID)
	s.patternFire[key]++
	s.patternSeen[key] = time.Now()
	return nil
}

func (s *MemStore) IncPatternSuccess(_ context.Context, repo, patternID string, w float64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := patternKey(repo, patternID)
	s.patternSucc[key] += w
	s.patternSeen[key] = time.Now()
	return nil
}

func (s *MemStore) GetPatternStats(_ context.Context, repo, patternID string) (PatternStats, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := patternKey(repo, patternID)
	return PatternStats{
		PatternID:    patternID,
		Repo:         repo,
		FiredCount:   s.patternFire[key],
		SuccessCount: int(s.patternSucc[key]),
		LastSeen:     s.patternSeen[key],
	}, nil
}

func (s *MemStore) AddDiscovered(_ context.Context, repo, suffix string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.discovered[repo] == nil {
		s.discovered[repo] = make(map[string]bool)
	}
	s.discovered[repo][suffix] = true
	return nil
}

func (s *MemStore) ListDiscovered(_ context.Context, repo string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.discovered[repo]))
	for k := range s.discovered[repo] {
		out = append(out, k)
	}
	return out, nil
}

func (s *MemStore) IncKeywordAffinity(_ context.Context, kw, patternID string, delta float64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.kwAff[keywordKey(kw, patternID)] += delta
	return nil
}

func (s *MemStore) GetKeywordAffinity(_ context.Context, kw, patternID string) (float64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.kwAff[keywordKey(kw, patternID)], nil
}
