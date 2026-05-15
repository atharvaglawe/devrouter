package heuristics

import (
	"math"
	"testing"
)

func TestComputeUnderRetrieval(t *testing.T) {
	// 0 additional files, 0 tokens, no overlap, no rolling baseline →
	// raw should be exactly 1.0.
	raw, adj := Compute(0, 0, 0, nil, nil, 0)
	if raw != 1.0 {
		t.Errorf("raw with zero penalties should be 1.0, got %g", raw)
	}
	if adj != 1.0 {
		t.Errorf("adjusted with zero baseline should equal raw, got %g", adj)
	}
}

func TestComputeOverRetrieval(t *testing.T) {
	// Token-cost dominates when prompt is huge but follow-up reads are zero
	rawSmall, _ := Compute(0, 0, 5_000, nil, nil, 0)   // 0.10 penalty
	rawLarge, _ := Compute(0, 0, 20_000, nil, nil, 0)  // 0.40 penalty
	if !approx(rawSmall, 0.90) {
		t.Errorf("5k tokens should give raw≈0.90, got %g", rawSmall)
	}
	if !approx(rawLarge, 0.60) {
		t.Errorf("20k tokens should give raw≈0.60, got %g", rawLarge)
	}
	if rawLarge >= rawSmall {
		t.Errorf("larger prompts must penalise more: small=%g large=%g", rawSmall, rawLarge)
	}
}

func TestComputeFollowupReadsDominate(t *testing.T) {
	// 5 additional files = -0.75 penalty, dominates a small token cost
	raw, _ := Compute(5, 0, 1_000, nil, nil, 0)
	if !approx(raw, 0.23) { // 1 - 0.75 - 0.02 = 0.23
		t.Errorf("5 extra files should give raw≈0.23, got %g", raw)
	}
}

func TestComputeClipsToZero(t *testing.T) {
	raw, _ := Compute(100, 100, 100_000, nil, nil, 0)
	if raw != 0 {
		t.Errorf("excessive penalties should clip to 0, got %g", raw)
	}
}

func TestComputeBaselineNormalization(t *testing.T) {
	// Adjusted = raw - rolling_mean
	raw, adj := Compute(2, 0, 5_000, nil, nil, 0.5)
	want := raw - 0.5
	if !approx(adj, want) {
		t.Errorf("adjusted should be raw - rolling_mean: raw=%g rolling=0.5 adj=%g want=%g",
			raw, adj, want)
	}
}

func TestComputeTrimOverlapPenalty(t *testing.T) {
	trimmed := []string{"a.go", "b.go"}
	read := []string{"b.go", "c.go"}
	rawNoOverlap, _ := Compute(0, 0, 0, nil, []string{"x.go"}, 0)
	rawOverlap, _ := Compute(0, 0, 0, trimmed, read, 0)
	if !approx(rawNoOverlap, 1.0) {
		t.Errorf("no overlap should give raw=1.0, got %g", rawNoOverlap)
	}
	if !approx(rawOverlap, 0.80) {
		t.Errorf("overlap should give raw=0.80 (1.0 - 0.20), got %g", rawOverlap)
	}
}

func TestComputeImplicitBands(t *testing.T) {
	tests := []struct {
		sim       float64
		wantRaw   float64
		wantFired bool
	}{
		{0.99, 0.0, true}, // near-identical re-ask
		{0.85, 0.4, true}, // paraphrased repeat
		{0.60, 0.0, false}, // related follow-up — no penalty
		{0.10, 0.0, false}, // unrelated
	}
	for _, tc := range tests {
		raw, fired := ComputeImplicit(tc.sim)
		if fired != tc.wantFired {
			t.Errorf("sim=%g fired=%v want=%v", tc.sim, fired, tc.wantFired)
		}
		if !approx(raw, tc.wantRaw) {
			t.Errorf("sim=%g raw=%g want=%g", tc.sim, raw, tc.wantRaw)
		}
	}
}

func approx(a, b float64) bool {
	return math.Abs(a-b) < 1e-9
}
