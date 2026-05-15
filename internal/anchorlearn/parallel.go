package anchorlearn

import (
	"os"
	"strconv"
	"sync"
)

// defaultProbeFanOut bounds the in-flight FileExists probes Decide
// makes against the codegraph /api/file endpoint. The default
// portfolio holds ~30 patterns; with 1-3 detected service tokens
// that's 30-90 candidates per query. Serially probing them at
// ~30-50ms each adds up. 8 concurrent probes drops the wall time
// to roughly ceil(N/8) * 50ms — for the goserving bench (~30
// candidates) that's ~200ms instead of ~1500ms.
//
// Override via DEVROUTER_ANCHOR_PROBE_FANOUT; 0 or negative falls
// back to 8.
var defaultProbeFanOut = func() int {
	if v := os.Getenv("DEVROUTER_ANCHOR_PROBE_FANOUT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 8
}()

// runParallel runs fn(0..n-1) concurrently with at most `limit`
// in-flight goroutines. Mirrors router.parallelDo — duplicated
// here so the anchorlearn package stays a leaf with no cross-
// package dep on router internals.
func runParallel(limit, n int, fn func(i int)) {
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
