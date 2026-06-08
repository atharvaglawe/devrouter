package heuristics

import (
	"context"
	"testing"
)

// fakeSourceStore is an in-memory sourceStore for testing the
// SourceBandit without Redis (mirrors bandit_test.go's fakeStore).
type fakeSourceStore struct {
	current map[string]int
	def     map[string]int
	history map[string][]SourceHistoryEntry
}

func newFakeSourceStore() *fakeSourceStore {
	return &fakeSourceStore{
		current: map[string]int{},
		def:     map[string]int{},
		history: map[string][]SourceHistoryEntry{},
	}
}

func srcKey(intent, repo, topic, source string) string {
	return intent + "|" + repo + "|" + topic + "|" + source
}

func (f *fakeSourceStore) CurrentSourceBreadth(_ context.Context, intent, repo, topic, source string, seed int) int {
	k := srcKey(intent, repo, topic, source)
	if v, ok := f.current[k]; ok {
		return v
	}
	f.current[k] = clipSourceDocs(seed)
	if _, ok := f.def[k]; !ok {
		f.def[k] = clipSourceDocs(seed)
	}
	return f.current[k]
}

func (f *fakeSourceStore) SetSourceBreadth(_ context.Context, intent, repo, topic, source string, val int) error {
	f.current[srcKey(intent, repo, topic, source)] = clipSourceDocs(val)
	return nil
}

func (f *fakeSourceStore) LoadSourceDefault(_ context.Context, intent, repo, topic, source string, fallback int) int {
	if v, ok := f.def[srcKey(intent, repo, topic, source)]; ok {
		return v
	}
	return clipSourceDocs(fallback)
}

func (f *fakeSourceStore) AppendSourceHistory(_ context.Context, intent, repo, topic, source string, e SourceHistoryEntry) error {
	k := srcKey(intent, repo, topic, source)
	f.history[k] = append(f.history[k], e)
	return nil
}

func withEpsilon(v float64, fn func()) {
	old := Epsilon
	Epsilon = v
	defer func() { Epsilon = old }()
	fn()
}

func TestSourceBanditDisabledReturnsSeeded(t *testing.T) {
	b := NewSourceBandit(newFakeSourceStore())
	seeds := []SourceSeed{{Name: "cmdocs", Default: 5}, {Name: "gitlab", Default: 3}}
	breadths, explore := b.SelectAll("debug", "", IntentGlobalTopic, seeds, 0)
	if explore != nil {
		t.Fatalf("disabled bandit should not explore, got %+v", explore)
	}
	if breadths["cmdocs"] != 5 || breadths["gitlab"] != 3 {
		t.Fatalf("expected seeded breadths, got %+v", breadths)
	}
}

func TestSourceBanditSampleFloor(t *testing.T) {
	b := NewSourceBandit(newFakeSourceStore())
	b.Enable()
	withEpsilon(1.0, func() {
		// Real bucket below the sample floor -> no exploration.
		_, explore := b.SelectAll("debug", "repo1", "topic-xyz", []SourceSeed{{Name: "cmdocs", Default: 5}}, 0)
		if explore != nil {
			t.Fatalf("cold real bucket should not explore, got %+v", explore)
		}
	})
}

func TestSourceBanditExploresOneSource(t *testing.T) {
	b := NewSourceBandit(newFakeSourceStore())
	b.Enable()
	withEpsilon(1.0, func() {
		seeds := []SourceSeed{{Name: "cmdocs", Default: 5}, {Name: "gitlab", Default: 5}}
		breadths, explore := b.SelectAll("debug", "", IntentGlobalTopic, seeds, 0)
		if explore == nil {
			t.Fatal("expected an explore record on the global bucket")
		}
		if explore.Base != 5 || explore.Val == 5 {
			t.Fatalf("expected perturbed candidate != base 5, got %+v", explore)
		}
		// The served breadth for the explored source must equal the candidate.
		if breadths[explore.Source] != explore.Val {
			t.Fatalf("served breadth %d != candidate %d", breadths[explore.Source], explore.Val)
		}
	})
}

func TestSourceBanditPromotes(t *testing.T) {
	fs := newFakeSourceStore()
	b := NewSourceBandit(fs)
	b.Enable()
	intent, repo, topic := "debug", "", IntentGlobalTopic

	var explore *ExploreRec
	withEpsilon(1.0, func() {
		_, explore = b.SelectAll(intent, repo, topic, []SourceSeed{{Name: "cmdocs", Default: 5}}, 0)
	})
	if explore == nil {
		t.Fatal("expected candidate")
	}

	// Feed enough base + winning candidate samples to trigger promotion.
	for i := 0; i < 5; i++ {
		b.Update(intent, repo, topic, "cmdocs", explore.Base, 0.0, 1.0)
	}
	for i := 0; i < PromotionWindow; i++ {
		b.Update(intent, repo, topic, "cmdocs", explore.Val, 1.0, 1.0)
	}

	if got := fs.current[srcKey(intent, repo, topic, "cmdocs")]; got != explore.Val {
		t.Fatalf("expected promotion to %d, current=%d", explore.Val, got)
	}
}

func TestSourceBanditRollsBack(t *testing.T) {
	fs := newFakeSourceStore()
	b := NewSourceBandit(fs)
	b.Enable()
	intent, repo, topic := "debug", "", IntentGlobalTopic

	var explore *ExploreRec
	withEpsilon(1.0, func() {
		_, explore = b.SelectAll(intent, repo, topic, []SourceSeed{{Name: "cmdocs", Default: 5}}, 0)
	})
	if explore == nil {
		t.Fatal("expected candidate")
	}

	for i := 0; i < RollbackStrikes; i++ {
		b.Update(intent, repo, topic, "cmdocs", explore.Val, 0.0, 1.0)
	}

	hist := fs.history[srcKey(intent, repo, topic, "cmdocs")]
	if len(hist) == 0 || hist[len(hist)-1].Kind != "rollback" {
		t.Fatalf("expected a rollback history entry, got %+v", hist)
	}
	// A fresh SelectAll can start a new candidate (active was cleared).
	withEpsilon(1.0, func() {
		if _, e := b.SelectAll(intent, repo, topic, []SourceSeed{{Name: "cmdocs", Default: 5}}, 0); e == nil {
			t.Fatal("active candidate should have been cleared after rollback")
		}
	})
}
