package heuristics

import (
	"context"
	"math/rand"
	"testing"
)

// withDeterministicRNG seeds the bandit's RNG so ε-perturbation tests are
// reproducible.
func withDeterministicRNG(b *Bandit, seed int64) {
	b.rng = rand.New(rand.NewSource(seed))
}

// fakeStore is the minimal banditStore implementation used by these
// tests. Records the most recent SetCurrent call so we can assert on
// promotion / rollback behaviour without touching Redis.
type fakeStore struct {
	defaultProfile Profile
	lastSetCurrent *Profile
	historyEvents  []HistoryEntry
}

func (f *fakeStore) SetCurrent(_ context.Context, _ string, p Profile) error {
	cp := p
	f.lastSetCurrent = &cp
	return nil
}

func (f *fakeStore) LoadDefault(_ context.Context, _ string) (Profile, error) {
	return f.defaultProfile, nil
}

func (f *fakeStore) AppendHistory(_ context.Context, _ string, e HistoryEntry) error {
	f.historyEvents = append(f.historyEvents, e)
	return nil
}

func TestBanditNoKnobsAlwaysReturnsBase(t *testing.T) {
	b := NewBandit(nil)
	base := Default("debug")
	for i := 0; i < 100; i++ {
		got := b.Select("debug", base)
		if got.ID() != base.ID() {
			t.Errorf("with no tunable knobs, Select should always return base")
			break
		}
	}
}

func TestBanditPerturbsWithEpsilon(t *testing.T) {
	b := NewBandit(nil)
	b.EnableKnob("max_trace")
	withDeterministicRNG(b, 42)
	base := Default("debug")

	// With ε=0.1, ~10 of 100 selects should differ from base.
	candidateHits := 0
	for i := 0; i < 1000; i++ {
		got := b.Select("debug", base)
		if got.ID() != base.ID() {
			candidateHits++
		}
	}
	// Allow a wide tolerance — once a candidate is generated it sticks
	// for ε of subsequent calls. We just want to confirm > 0 and < 100%.
	if candidateHits == 0 {
		t.Errorf("expected some candidate selects with ε=0.1, got 0/1000")
	}
	if candidateHits == 1000 {
		t.Errorf("expected base to be picked sometimes, got 0 base selects")
	}
}

// forceCandidate triggers Select until a candidate (different from base)
// is generated. Returns the candidate ID, or "" if RNG didn't perturb in
// `tries` calls (test should skip in that case).
func forceCandidate(b *Bandit, intent string, base Profile, tries int) string {
	for i := 0; i < tries; i++ {
		got := b.Select(intent, base)
		if got.ID() != base.ID() {
			return got.ID()
		}
	}
	return ""
}

func TestBanditPromotesOnLift(t *testing.T) {
	store := &fakeStore{}
	b := NewBandit(store)
	b.EnableKnob("max_trace")
	withDeterministicRNG(b, 1)

	base := Default("refactor")
	candidateID := forceCandidate(b, "refactor", base, 500)
	if candidateID == "" {
		t.Skip("RNG didn't perturb in 500 selects (rare); skip")
	}

	for i := 0; i < PromotionWindow; i++ {
		b.Update("refactor", candidateID, 0.9, 1.0)
	}
	for i := 0; i < 5; i++ {
		b.Update("refactor", base.ID(), 0.5, 1.0)
	}

	if store.lastSetCurrent == nil {
		t.Fatalf("expected SetCurrent to fire on promotion")
	}
	if store.lastSetCurrent.ID() != candidateID {
		t.Errorf("promoted profile mismatch: got %s want %s",
			store.lastSetCurrent.ID(), candidateID)
	}
}

func TestBanditRollbackOnThreeStrikes(t *testing.T) {
	store := &fakeStore{defaultProfile: Default("refactor")}
	b := NewBandit(store)
	b.EnableKnob("max_trace")
	withDeterministicRNG(b, 7)

	base := Default("refactor")
	candidateID := forceCandidate(b, "refactor", base, 500)
	if candidateID == "" {
		t.Skip("RNG didn't perturb in 500 selects (rare); skip")
	}

	for i := 0; i < RollbackStrikes; i++ {
		b.Update("refactor", candidateID, 0.10, 1.0)
	}

	if store.lastSetCurrent == nil {
		t.Fatalf("expected SetCurrent (rollback) to fire")
	}
	if store.lastSetCurrent.ID() != store.defaultProfile.ID() {
		t.Errorf("rollback should restore default: got %s want %s",
			store.lastSetCurrent.ID(), store.defaultProfile.ID())
	}
}

func TestBanditDiscardsOnNoLift(t *testing.T) {
	store := &fakeStore{}
	b := NewBandit(store)
	b.EnableKnob("max_trace")
	withDeterministicRNG(b, 99)

	base := Default("refactor")
	candidateID := forceCandidate(b, "refactor", base, 500)
	if candidateID == "" {
		t.Skip("RNG didn't perturb in 500 selects (rare); skip")
	}

	for i := 0; i < PromotionWindow; i++ {
		b.Update("refactor", candidateID, 0.5, 1.0)
	}
	for i := 0; i < 5; i++ {
		b.Update("refactor", base.ID(), 0.5, 1.0)
	}

	if store.lastSetCurrent != nil {
		t.Errorf("discard must not call SetCurrent (no promotion); got %+v",
			store.lastSetCurrent)
	}
}
