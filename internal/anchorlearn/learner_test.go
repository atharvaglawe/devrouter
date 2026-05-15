package anchorlearn

import (
	"context"
	"strings"
	"testing"
)

// fakeProbe is a Probe stub. Returns true iff path is in `exists`.
type fakeProbe struct {
	exists map[string]bool
}

func (p *fakeProbe) FileExists(_ context.Context, _, path string) bool {
	return p.exists[path]
}

func newGoservingProbe() *fakeProbe {
	return &fakeProbe{exists: map[string]bool{
		"oscar/main.go":              true,
		"oscar/web/web.go":           true,
		"oscar/web/grpc/server.go":   true,
		"oscar/web/http/server.go":   true,
		"oscar/web/routes/route.go":  true,
		"oscar/web/routes/routes.go": true,
		// Discovered path that's NOT in DefaultStaticPatterns.
		"oscar/internal/dispatcher/run.go": true,
	}}
}

// ---------------------------------------------------------------------
// Phase 1 — observation logging
// ---------------------------------------------------------------------

func TestRecordObservationPersistsAnchorState(t *testing.T) {
	store := NewMemStore()
	l := New(store, newGoservingProbe())

	// Decide once with a query that should hit oscar/main.go.
	cs := l.Decide(context.Background(), "goserving",
		"where does oscar service start its HTTP listener", "trace",
		[]string{"oscar"}, 3)
	if len(cs) == 0 {
		t.Fatalf("Decide returned no candidates; expected at least 1")
	}

	// Phase 1: persist what we picked.
	l.RecordObservation(context.Background(), "q-1",
		"goserving", "where does oscar service start its HTTP listener", "trace", cs)

	obs, err := store.GetObservation(context.Background(), "q-1")
	if err != nil {
		t.Fatalf("GetObservation: %v", err)
	}
	if obs == nil {
		t.Fatalf("expected observation, got nil")
	}
	if obs.Repo != "goserving" {
		t.Errorf("obs.Repo = %q, want goserving", obs.Repo)
	}
	if len(obs.Files) != len(cs) {
		t.Errorf("obs.Files len = %d, want %d", len(obs.Files), len(cs))
	}
	if len(obs.PatternIDs) != len(obs.Files) {
		t.Errorf("PatternIDs len mismatch with Files")
	}

	// Per-pattern fire counter must have ticked for each anchor.
	for _, c := range cs {
		st, _ := store.GetPatternStats(context.Background(), "goserving", c.Pattern.ID)
		if st.FiredCount == 0 {
			t.Errorf("pattern %q FiredCount = 0; expected >=1", c.Pattern.ID)
		}
	}
}

// ---------------------------------------------------------------------
// Phase 2 — per-repo posterior scoring
// ---------------------------------------------------------------------

func TestDecideRanksWinningPatternHigherAfterRewards(t *testing.T) {
	store := NewMemStore()
	l := New(store, newGoservingProbe())
	l.SetEpsilon(0) // deterministic

	ctx := context.Background()
	query := "where does oscar start its HTTP listener"
	services := []string{"oscar"}

	// Baseline ranking.
	first := l.Decide(ctx, "goserving", query, "trace", services, 7)
	t.Logf("baseline top: %v", topPathsOf(first))

	// Manually reward web/grpc/server.go heavily — represents a repo
	// where gRPC anchors really pay off.
	for i := 0; i < 5; i++ {
		_ = store.IncPatternFire(ctx, "goserving", "web/grpc/server.go")
		_ = store.IncPatternSuccess(ctx, "goserving", "web/grpc/server.go", 1.0)
	}

	// Same query, same probe set — but now grpc/server.go should
	// have moved up (it had a fixed query-affinity baseline before;
	// now it has both that and accumulated success).
	second := l.Decide(ctx, "goserving", query, "trace", services, 7)
	t.Logf("post-reward top: %v", topPathsOf(second))

	// The path "oscar/web/grpc/server.go" should rank higher (lower
	// index) in `second` than in `first`.
	idxBefore := indexOf(topPathsOf(first), "oscar/web/grpc/server.go")
	idxAfter := indexOf(topPathsOf(second), "oscar/web/grpc/server.go")
	if idxAfter > idxBefore {
		t.Errorf("expected grpc/server.go to rise after rewards; before=%d after=%d", idxBefore, idxAfter)
	}
}

// ---------------------------------------------------------------------
// Phase 3 — file-level discovery
// ---------------------------------------------------------------------

func TestRewardMemorySavePromotesUnknownPath(t *testing.T) {
	store := NewMemStore()
	probe := newGoservingProbe()
	l := New(store, probe)
	l.SetEpsilon(0)

	ctx := context.Background()
	query := "trace oscar dispatcher startup"
	services := []string{"oscar"}

	cs := l.Decide(ctx, "goserving", query, "trace", services, 4)
	l.RecordObservation(ctx, "q-discover", "goserving", query, "trace", cs)
	l.NoteRecentQuery("goserving", "q-discover")

	// Agent saves a memory referencing a file under oscar/ that's
	// NOT in the static portfolio.
	novel := "oscar/internal/dispatcher/run.go"
	l.RewardMemorySave(ctx, "goserving", novel)

	// Phase 3 expectation: novel suffix is now in the discovered set.
	disc, err := store.ListDiscovered(ctx, "goserving")
	if err != nil {
		t.Fatalf("ListDiscovered: %v", err)
	}
	novelSuffix := "internal/dispatcher/run.go"
	found := false
	for _, d := range disc {
		if d == novelSuffix {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected discovered set to contain %q; got %v", novelSuffix, disc)
	}

	// Run Decide again — the discovered pattern should now appear as
	// a candidate (subject to probe-existence). Add the file to the
	// probe map first since the discovered-pattern scoring
	// actually wants to validate it exists.
	probe.exists[novel] = true
	cs2 := l.Decide(ctx, "goserving", query, "trace", services, 6)
	if !contains(topPathsOf(cs2), novel) {
		t.Errorf("expected discovered path %q in second Decide top-K; got %v",
			novel, topPathsOf(cs2))
	}
}

// ---------------------------------------------------------------------
// Phase 4 — ε-greedy exploration + reward update on signals
// ---------------------------------------------------------------------

func TestEpsilonGreedyEverIncludesUnprovenPattern(t *testing.T) {
	store := NewMemStore()
	l := New(store, newGoservingProbe())
	l.SetEpsilon(1.0) // always explore

	// Pre-fire the "obvious" patterns so the unproven set is well
	// defined. Anything below MinFiresBeforeExploit qualifies as
	// unproven; we mark main.go and web/web.go as proven by ticking
	// them past the threshold.
	ctx := context.Background()
	for i := 0; i < MinFiresBeforeExploit; i++ {
		_ = store.IncPatternFire(ctx, "goserving", "main.go")
		_ = store.IncPatternFire(ctx, "goserving", "web/web.go")
	}

	cs := l.Decide(ctx, "goserving", "trace oscar listener", "trace",
		[]string{"oscar"}, 3)

	sawExploration := false
	for _, c := range cs {
		if c.IsExploration {
			sawExploration = true
			break
		}
	}
	if !sawExploration {
		t.Errorf("with ε=1 and unproven patterns available, expected at least one IsExploration=true; got %v",
			topPathsOf(cs))
	}
}

func TestRewardFeedbackCreditsAnchoredFile(t *testing.T) {
	store := NewMemStore()
	l := New(store, newGoservingProbe())
	l.SetEpsilon(0)

	ctx := context.Background()
	cs := l.Decide(ctx, "goserving", "where does oscar start the HTTP server", "trace",
		[]string{"oscar"}, 3)
	l.RecordObservation(ctx, "q-explicit", "goserving",
		"where does oscar start the HTTP server", "trace", cs)

	// Pick the first anchored file as the one the agent reported as
	// useful in dev_feedback.
	if len(cs) == 0 {
		t.Fatalf("Decide produced no candidates")
	}
	usefulFile := cs[0].Path

	l.RewardFeedback(ctx, "q-explicit", []string{usefulFile}, true)

	st, _ := store.GetPatternStats(ctx, "goserving", cs[0].Pattern.ID)
	if st.SuccessCount < 1 {
		t.Errorf("RewardFeedback should have credited pattern %q; got SuccessCount=%d",
			cs[0].Pattern.ID, st.SuccessCount)
	}

	// Keyword affinity must have updated for at least one query
	// keyword × pattern pair.
	got := false
	for _, kw := range []string{"oscar", "http", "server", "where"} {
		v, _ := store.GetKeywordAffinity(ctx, kw, cs[0].Pattern.ID)
		if v > 0 {
			got = true
			break
		}
	}
	if !got {
		t.Errorf("expected at least one keyword affinity to be > 0 after feedback")
	}
}

// ---------------------------------------------------------------------
// Cold-start behaviour — Decide on an empty store must mirror the
// static portfolio's keyword reranking, not return chaos.
// ---------------------------------------------------------------------

func TestColdStartPrefersKeywordAlignedPattern(t *testing.T) {
	store := NewMemStore() // intentionally empty
	l := New(store, newGoservingProbe())
	l.SetEpsilon(0)

	cs := l.Decide(context.Background(), "goserving",
		"where does oscar start its gRPC server", "trace",
		[]string{"oscar"}, 3)

	paths := topPathsOf(cs)
	t.Logf("cold-start top-3: %v", paths)
	// Either web/grpc/server.go (best keyword match) or main.go
	// (fallback) should be the rank-1 hit on a cold store; routes
	// shouldn't outrank either.
	if len(paths) == 0 {
		t.Fatalf("Decide returned empty on cold start")
	}
	if !strings.Contains(paths[0], "grpc") && !strings.HasSuffix(paths[0], "/main.go") {
		t.Errorf("cold-start rank-1 should be grpc/server.go or main.go; got %q", paths[0])
	}
}

// ---------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------

func topPathsOf(cs []Candidate) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.Path
	}
	return out
}

func indexOf(xs []string, x string) int {
	for i, v := range xs {
		if v == x {
			return i
		}
	}
	return len(xs)
}

func contains(xs []string, x string) bool { return indexOf(xs, x) < len(xs) }
