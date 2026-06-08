package router

import (
	"testing"

	"github.com/atharva-ag/devrouter/internal/heuristics"
)

func TestWidenBreadths(t *testing.T) {
	seeds := []heuristics.SourceSeed{{Name: "cmdocs", Default: 5}, {Name: "gitlab", Default: 2}}

	// No learned values: widen from seed defaults (~+50%, at least +1).
	out := widenBreadths(nil, seeds)
	if out["cmdocs"] != 7 { // 5 + 5/2 = 7
		t.Fatalf("cmdocs widen: want 7, got %d", out["cmdocs"])
	}
	if out["gitlab"] != 3 { // 2 + 2/2 = 3
		t.Fatalf("gitlab widen: want 3, got %d", out["gitlab"])
	}

	// Learned values are widened and clipped to the upper bound.
	out = widenBreadths(map[string]int{"cmdocs": 14, "gitlab": 4}, seeds)
	if out["cmdocs"] != heuristics.SourceDocsBounds[1] {
		t.Fatalf("cmdocs widen should clip to %d, got %d", heuristics.SourceDocsBounds[1], out["cmdocs"])
	}
	if out["gitlab"] != 6 { // 4 + 4/2 = 6
		t.Fatalf("gitlab widen: want 6, got %d", out["gitlab"])
	}
}

func TestSourceExploreFromTrace(t *testing.T) {
	name, val, ok := sourceExploreFromTrace(map[string]string{
		"src_explore_name": "cmdocs", "src_explore_val": "7", "src_explore_base": "5",
	})
	if !ok || name != "cmdocs" || val != 7 {
		t.Fatalf("valid record: got name=%q val=%d ok=%v", name, val, ok)
	}
	if _, _, ok := sourceExploreFromTrace(nil); ok {
		t.Fatal("nil trace should yield ok=false")
	}
	if _, _, ok := sourceExploreFromTrace(map[string]string{"src_explore_val": "7"}); ok {
		t.Fatal("missing name should yield ok=false")
	}
	if _, _, ok := sourceExploreFromTrace(map[string]string{"src_explore_name": "x", "src_explore_val": "abc"}); ok {
		t.Fatal("non-numeric val should yield ok=false")
	}
}
