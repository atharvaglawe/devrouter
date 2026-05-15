package memory

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// False-positive (FP) memory tracking — closes the relevance loop that
// the heuristics bandit can't:
//
//   The bandit tunes how MUCH to retrieve (graph hops, snippet caps,
//   memory shrink). It cannot tell us WHICH memories are wrong-for-this-
//   query, because every reward signal is profile-keyed, not memory-keyed.
//
// FP tracking sits orthogonal to the bandit. For each memory the router
// returns, if dev_feedback later reports the agent had to read entirely
// different files, we credit that memory with a "false positive for
// queries embedding-similar to this one" event. We persist:
//
//   mem:fp:{memKey}.cent  — running mean of FP query embeddings (bytes)
//   mem:fp:{memKey}.count — number of FPs accumulated
//
// The next time a query embedding lands close to a memory's FP centroid
// during retrieval, the router demotes that memory before ranking. So
// "stop returning seat-provider for cache-clearing queries" is learned
// per-memory, per-(query-shape), without retraining anything.
//
// Storage cost: one 768-float32 centroid (~3 KB) + one int per memory
// that has ever been an FP. Cleared by TTL after 14 days of inactivity,
// so memories that improve naturally fall out of the demotion list.

const (
	fpKeyPrefix = "mem:fp"

	// FPTTL is how long an FP record survives without new FP signals.
	// 14 days is long enough to outlast a normal sprint cadence (a
	// memory that recovers gets a fresh start) but short enough that
	// a stale grudge from a long-since-corrected memory naturally
	// fades.
	FPTTL = 14 * 24 * time.Hour

	fpFieldCentroid = "cent"
	fpFieldCount    = "count"
)

// FPInfo is what BatchFalsePositiveSimilarity returns for one memory.
type FPInfo struct {
	// Sim is the cosine similarity between the supplied query
	// embedding and the memory's running FP centroid, in [0,1].
	Sim float64
	// Count is the number of FP samples that built the centroid.
	// Used to ramp the demotion penalty — one FP shouldn't fully
	// suppress a memory, but three or more should.
	Count int
}

// RecordFalsePositive updates memKey's FP centroid using an incremental
// mean over the supplied query embedding, increments the count, and
// refreshes the TTL.
//
// Best-effort: returns an error so the caller can log, but the caller
// should never abort feedback handling on FP write failure.
func (s *Store) RecordFalsePositive(ctx context.Context, memKey string, queryEmbedding []float32) error {
	if memKey == "" {
		return errors.New("memKey required")
	}
	if len(queryEmbedding) != EmbedDim {
		return fmt.Errorf("query embedding has dim %d, want %d", len(queryEmbedding), EmbedDim)
	}
	key := fpKey(memKey)

	// Read current centroid + count, compute new centroid in Go,
	// write back. Race-tolerant by design: concurrent FPs may lose
	// one sample to overwrite, which is fine for a relevance signal
	// that is itself probabilistic.
	prevCent, prevCount, err := s.getFP(ctx, key)
	if err != nil {
		return err
	}

	newCent := incrementalMean(prevCent, prevCount, queryEmbedding)
	newCount := prevCount + 1

	pipe := s.rdb.Pipeline()
	pipe.HSet(ctx, key, map[string]interface{}{
		fpFieldCentroid: float32ToBytesNoCopy(newCent),
		fpFieldCount:    newCount,
	})
	pipe.Expire(ctx, key, FPTTL)
	_, err = pipe.Exec(ctx)
	return err
}

// BatchFalsePositiveSimilarity returns FP info for every memKey in one
// pipeline round-trip. Memories with no FP history are absent from the
// returned map (rather than mapped to a zero-similarity entry) so the
// caller can skip the demotion math entirely for the common path.
func (s *Store) BatchFalsePositiveSimilarity(
	ctx context.Context, memKeys []string, queryEmbedding []float32,
) (map[string]FPInfo, error) {
	if len(memKeys) == 0 {
		return nil, nil
	}
	if len(queryEmbedding) != EmbedDim {
		return nil, fmt.Errorf("query embedding has dim %d, want %d", len(queryEmbedding), EmbedDim)
	}

	pipe := s.rdb.Pipeline()
	cmds := make([]*redis.MapStringStringCmd, len(memKeys))
	for i, mk := range memKeys {
		cmds[i] = pipe.HGetAll(ctx, fpKey(mk))
	}
	if _, err := pipe.Exec(ctx); err != nil && err != redis.Nil {
		return nil, err
	}

	out := make(map[string]FPInfo, len(memKeys))
	for i, mk := range memKeys {
		fields, err := cmds[i].Result()
		if err != nil || len(fields) == 0 {
			continue
		}
		centRaw, ok := fields[fpFieldCentroid]
		if !ok || centRaw == "" {
			continue
		}
		cent, err := bytesToFloat32([]byte(centRaw))
		if err != nil || len(cent) != EmbedDim {
			continue
		}
		count, _ := parseIntField(fields[fpFieldCount])
		if count <= 0 {
			continue
		}
		sim := cosineFloat32(queryEmbedding, cent)
		if sim < 0 {
			sim = 0
		}
		out[mk] = FPInfo{Sim: sim, Count: count}
	}
	return out, nil
}

// FPDemoteSimThreshold is the cosine-similarity floor below which an FP
// signal is too weak to act on. Tuned so a related-but-different query
// (e.g. "auth callbacks" when the FPs were about "auth tokens") doesn't
// inherit a grudge meant for near-duplicates of the original FP query.
const FPDemoteSimThreshold = 0.70

// FPMaxDistancePenalty caps how much we can add to a hit's cosine
// distance from FP signals. 0.20 means a fully-saturated FP can push a
// hit from "borderline" (distance ≈ 0.40) past the default floor (0.60).
const FPMaxDistancePenalty = 0.20

// FPSaturationCount is the number of FPs at which the demotion penalty
// reaches its maximum. Below this it ramps linearly, so a memory's
// first false positive doesn't immediately suppress it.
const FPSaturationCount = 3

// FPDistancePenalty returns the cosine-distance delta the router should
// add to a candidate hit given its FP info against the current query.
// Returns 0 when the FP signal is below the action threshold or when
// the memory has never been flagged.
func FPDistancePenalty(fp FPInfo) float64 {
	if fp.Count == 0 || fp.Sim < FPDemoteSimThreshold {
		return 0
	}
	w := float64(fp.Count) / float64(FPSaturationCount)
	if w > 1 {
		w = 1
	}
	// Penalty scales with both how strongly this query resembles the
	// FP centroid AND how many FPs have accumulated. The 0.70 floor on
	// Sim means the multiplier ranges over [0.70, 1.0].
	return FPMaxDistancePenalty * w * fp.Sim
}

// ResetFalsePositives deletes the FP record for one memory key. Used by
// admin tooling when a memory is deliberately re-curated.
func (s *Store) ResetFalsePositives(ctx context.Context, memKey string) error {
	if memKey == "" {
		return errors.New("memKey required")
	}
	return s.rdb.Del(ctx, fpKey(memKey)).Err()
}

// LoadMemoryFiles returns, for each memory key, the list of file paths
// that memory references. For file memories this is `path`; for func
// memories it's `file`; for flow memories it's the comma-separated
// `files` field. One pipelined batch — used by the FP attribution code
// path so feedback handling stays single-round-trip per Redis call.
//
// Memories that don't exist in Redis (deleted, evicted) map to nil so
// the caller can detect "no overlap possible" without a special case.
func (s *Store) LoadMemoryFiles(ctx context.Context, memKeys []string) map[string][]string {
	out := make(map[string][]string, len(memKeys))
	if len(memKeys) == 0 {
		return out
	}
	pipe := s.rdb.Pipeline()
	cmds := make([]*redis.SliceCmd, len(memKeys))
	for i, k := range memKeys {
		cmds[i] = pipe.HMGet(ctx, k, "path", "file", "files")
	}
	if _, err := pipe.Exec(ctx); err != nil && err != redis.Nil {
		return out
	}
	for i, k := range memKeys {
		vals, err := cmds[i].Result()
		if err != nil || len(vals) == 0 {
			continue
		}
		var files []string
		for _, v := range vals {
			s, _ := v.(string)
			if s == "" {
				continue
			}
			for _, p := range strings.Split(s, ",") {
				if p = strings.TrimSpace(p); p != "" {
					files = append(files, p)
				}
			}
		}
		if len(files) > 0 {
			out[k] = files
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Internals
// ---------------------------------------------------------------------------

func fpKey(memKey string) string {
	return fmt.Sprintf("%s:%s", fpKeyPrefix, memKey)
}

func (s *Store) getFP(ctx context.Context, key string) ([]float32, int, error) {
	res, err := s.rdb.HGetAll(ctx, key).Result()
	if err != nil {
		return nil, 0, err
	}
	if len(res) == 0 {
		return nil, 0, nil
	}
	count, _ := parseIntField(res[fpFieldCount])
	cent, _ := bytesToFloat32([]byte(res[fpFieldCentroid]))
	return cent, count, nil
}

func parseIntField(s string) (int, error) {
	if s == "" {
		return 0, nil
	}
	var n int
	_, err := fmt.Sscanf(s, "%d", &n)
	return n, err
}

func bytesToFloat32(b []byte) ([]float32, error) {
	if len(b)%4 != 0 {
		return nil, fmt.Errorf("byte length %d not divisible by 4", len(b))
	}
	out := make([]float32, len(b)/4)
	r := bytes.NewReader(b)
	for i := range out {
		var u uint32
		if err := binary.Read(r, binary.LittleEndian, &u); err != nil {
			return nil, err
		}
		out[i] = math.Float32frombits(u)
	}
	return out, nil
}

// float32ToBytesNoCopy is identical to Float32ToBytes (in embeddings.go)
// but avoids the public-name collision; kept private to make it clear
// callers should use Float32ToBytes for index writes.
func float32ToBytesNoCopy(v []float32) []byte {
	return Float32ToBytes(v)
}

// incrementalMean computes ((count * prev) + sample) / (count + 1).
// When prev is nil/empty (first FP) it returns a copy of sample.
func incrementalMean(prev []float32, count int, sample []float32) []float32 {
	if len(prev) != len(sample) || count == 0 {
		out := make([]float32, len(sample))
		copy(out, sample)
		return out
	}
	out := make([]float32, len(sample))
	cf := float32(count)
	denom := cf + 1
	for i := range sample {
		out[i] = (cf*prev[i] + sample[i]) / denom
	}
	return out
}

// cosineFloat32 mirrors heuristics.cosine but lives here so the memory
// package doesn't depend on heuristics (which would create a cycle:
// heuristics already uses memory.Embed and the redis client).
func cosineFloat32(a, b []float32) float64 {
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
