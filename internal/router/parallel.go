package router

import (
	"os"
	"strconv"
	"sync"
)

// graphFanOut bounds the in-flight goroutine count for parallel
// codegraph fan-out (symbol traversal, importer keywords, related
// files, anchor probing). 8 is the sweet spot we observed against
// LadybugDB on a laptop: above ~12 the codegraph server starts
// queueing internally and per-RT latency creeps up. Override via
// DEVROUTER_GRAPH_FANOUT for stress tests.
var graphFanOut = func() int {
	if v := os.Getenv("DEVROUTER_GRAPH_FANOUT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 8
}()

// parallelDo runs fn(0..n-1) concurrently with at most `limit`
// in-flight goroutines. The work itself is expected to be I/O-bound
// (codegraph HTTP roundtrips); CPU-bound work would not benefit.
//
// Each fn call must own its own output slot (typically a pre-sized
// result slice indexed by i) — parallelDo intentionally does not
// hand back per-task return values so callers don't have to allocate
// and re-assemble. Merge in deterministic order *after* parallelDo
// returns.
//
// limit <= 0 falls back to a sane default (8) which matches the
// codegraph server's per-request concurrency comfort zone on a
// typical laptop.
func parallelDo(limit int, n int, fn func(i int)) {
	if n <= 0 {
		return
	}
	if n == 1 {
		fn(0)
		return
	}
	if limit <= 0 {
		limit = 8
	}
	if limit > n {
		limit = n
	}
	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		sem <- struct{}{}
		go func(idx int) {
			defer wg.Done()
			defer func() { <-sem }()
			fn(idx)
		}(i)
	}
	wg.Wait()
}
