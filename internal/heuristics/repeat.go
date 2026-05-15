package heuristics

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// RepeatLookbackWindow is how far back we look for a matching prior query
// when deciding whether the current call is a repeat-exploration. Members
// older than this are ignored even if still in the sorted set.
const RepeatLookbackWindow = 15 * time.Minute

// RepeatStoreTTL is the TTL on the recent_queries:{repo} sorted set itself.
const RepeatStoreTTL = 30 * time.Minute

// RepeatMaxEntriesPerRepo bounds the in-memory cosine work per query so
// the detector stays sub-millisecond regardless of repo activity.
const RepeatMaxEntriesPerRepo = 30

// RecentQueryEntry is one member in recent_queries:{repo}. The embedding
// is denormalised on the entry so detection is a single ZRANGEBYSCORE
// followed by in-memory cosine — no per-member round trip to Redis.
type RecentQueryEntry struct {
	QueryID   string    `json:"query_id"`
	Intent    string    `json:"intent"`
	ProfileID string    `json:"profile_id"`
	Embedding []float32 `json:"embedding"`
}

// RepeatHit describes a similarity match against a prior query.
type RepeatHit struct {
	PrevQueryID   string
	PrevIntent    string
	PrevProfileID string
	Sim           float64
}

// DetectRepeat finds the most similar prior query in recent_queries:{repo}
// within the lookback window. Returns RepeatHit{} when there's no signal.
// Caller checks RepeatHit.Sim against RepeatSimThreshold to decide whether
// to act on it.
func (s *Store) DetectRepeat(ctx context.Context, repo string, embed []float32) (RepeatHit, error) {
	if repo == "" || len(embed) == 0 {
		return RepeatHit{}, nil
	}
	key := s.recentKey(repo)
	minScore := strconv.FormatInt(time.Now().Add(-RepeatLookbackWindow).UnixMilli(), 10)
	members, err := s.rdb.ZRangeByScore(ctx, key, &redis.ZRangeBy{
		Min:    minScore,
		Max:    "+inf",
		Offset: 0,
		Count:  int64(RepeatMaxEntriesPerRepo),
	}).Result()
	if err != nil || len(members) == 0 {
		return RepeatHit{}, err
	}
	best := RepeatHit{}
	for _, raw := range members {
		var e RecentQueryEntry
		if err := json.Unmarshal([]byte(raw), &e); err != nil {
			continue
		}
		// Skip "matching against self" — caller may have already pushed
		// the current query before detection. We don't have the current
		// query_id here; that's fine because we compare embeddings, and
		// a brand new query won't appear yet.
		sim := cosine(embed, e.Embedding)
		if sim > best.Sim {
			best.Sim = sim
			best.PrevQueryID = e.QueryID
			best.PrevIntent = e.Intent
			best.PrevProfileID = e.ProfileID
		}
	}
	return best, nil
}

// RecordQuery appends the current query to recent_queries:{repo}. Always
// called AFTER DetectRepeat so we don't match against ourselves.
func (s *Store) RecordQuery(ctx context.Context, repo string, e RecentQueryEntry) error {
	if repo == "" || len(e.Embedding) == 0 {
		return nil
	}
	data, _ := json.Marshal(e)
	score := float64(time.Now().UnixMilli())
	key := s.recentKey(repo)
	pipe := s.rdb.Pipeline()
	pipe.ZAdd(ctx, key, redis.Z{Score: score, Member: data})
	// Trim entries older than the TTL window so the set doesn't grow unbounded
	cutoff := strconv.FormatInt(time.Now().Add(-RepeatStoreTTL).UnixMilli(), 10)
	pipe.ZRemRangeByScore(ctx, key, "-inf", cutoff)
	pipe.Expire(ctx, key, RepeatStoreTTL)
	_, err := pipe.Exec(ctx)
	return err
}

func (s *Store) recentKey(repo string) string {
	return fmt.Sprintf("%s:%s", keyRecent, repo)
}

func cosine(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		x := float64(a[i])
		y := float64(b[i])
		dot += x * y
		na += x * x
		nb += y * y
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}
