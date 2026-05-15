package memory

import (
	"math"
	"testing"
)

// TestIncrementalMeanFirstSample verifies that the very first FP for a
// memory just stores the embedding verbatim.
func TestIncrementalMeanFirstSample(t *testing.T) {
	sample := []float32{1, 2, 3}
	got := incrementalMean(nil, 0, sample)
	if len(got) != len(sample) {
		t.Fatalf("len: got %d, want %d", len(got), len(sample))
	}
	for i := range sample {
		if got[i] != sample[i] {
			t.Errorf("idx %d: got %f, want %f", i, got[i], sample[i])
		}
	}
}

// TestIncrementalMeanRunningAverage walks through a few FP samples and
// confirms the centroid converges on the arithmetic mean.
func TestIncrementalMeanRunningAverage(t *testing.T) {
	samples := [][]float32{
		{1, 0, 0},
		{0, 1, 0},
		{0, 0, 1},
		{1, 1, 1},
	}
	var cent []float32
	for i, s := range samples {
		cent = incrementalMean(cent, i, s)
	}
	// Expected mean: (2/4, 2/4, 2/4) = (0.5, 0.5, 0.5)
	want := []float32{0.5, 0.5, 0.5}
	for i := range want {
		if math.Abs(float64(cent[i]-want[i])) > 1e-6 {
			t.Errorf("idx %d: got %f, want %f", i, cent[i], want[i])
		}
	}
}

// TestFPDistancePenaltyBands locks in the demotion behaviour:
//   - below FPDemoteSimThreshold: zero penalty (we don't punish a
//     memory just because it's been wrong for unrelated queries).
//   - count<saturation: linearly ramped, so first FP is gentle.
//   - count>=saturation AND sim=1.0: cap at FPMaxDistancePenalty.
func TestFPDistancePenaltyBands(t *testing.T) {
	cases := []struct {
		name string
		fp   FPInfo
		want float64
	}{
		{"never an FP", FPInfo{Sim: 0.99, Count: 0}, 0},
		{"sim below threshold", FPInfo{Sim: 0.50, Count: 5}, 0},
		{"first FP, exact match",
			FPInfo{Sim: 1.0, Count: 1},
			FPMaxDistancePenalty * (1.0 / float64(FPSaturationCount)) * 1.0,
		},
		{"saturated, exact match",
			FPInfo{Sim: 1.0, Count: FPSaturationCount},
			FPMaxDistancePenalty,
		},
		{"saturated, threshold match",
			FPInfo{Sim: FPDemoteSimThreshold, Count: FPSaturationCount},
			FPMaxDistancePenalty * FPDemoteSimThreshold,
		},
		{"over-saturated stays capped",
			FPInfo{Sim: 1.0, Count: 100},
			FPMaxDistancePenalty,
		},
	}
	for _, c := range cases {
		got := FPDistancePenalty(c.fp)
		if math.Abs(got-c.want) > 1e-9 {
			t.Errorf("%s: got %.4f, want %.4f", c.name, got, c.want)
		}
	}
}

// TestCosineFloat32 sanity-checks the local cosine helper. The full
// repeat-detection cosine is exercised in the heuristics package
// already; this guards against a future hand-edit of this file.
func TestCosineFloat32(t *testing.T) {
	a := []float32{1, 0, 0}
	b := []float32{1, 0, 0}
	if got := cosineFloat32(a, b); math.Abs(got-1.0) > 1e-9 {
		t.Errorf("identical vectors: got %f, want 1.0", got)
	}
	c := []float32{0, 1, 0}
	if got := cosineFloat32(a, c); math.Abs(got) > 1e-9 {
		t.Errorf("orthogonal vectors: got %f, want 0", got)
	}
	d := []float32{0, 0, 0}
	if got := cosineFloat32(a, d); got != 0 {
		t.Errorf("zero vector: got %f, want 0", got)
	}
}

// TestBytesToFloat32Roundtrip ensures the centroid serialisation is
// lossless within float32 precision.
func TestBytesToFloat32Roundtrip(t *testing.T) {
	in := []float32{0.1, -0.5, 1e-7, 1234.5678}
	b := Float32ToBytes(in)
	out, err := bytesToFloat32(b)
	if err != nil {
		t.Fatalf("bytesToFloat32: %v", err)
	}
	for i := range in {
		if in[i] != out[i] {
			t.Errorf("idx %d: in=%f out=%f", i, in[i], out[i])
		}
	}
}

// TestFPDistancePenaltyMonotonicInCount confirms the penalty is
// non-decreasing in count for a fixed similarity. Catches accidental
// sign flips.
func TestFPDistancePenaltyMonotonicInCount(t *testing.T) {
	prev := 0.0
	for c := 1; c <= FPSaturationCount*2; c++ {
		got := FPDistancePenalty(FPInfo{Sim: 0.85, Count: c})
		if got < prev {
			t.Errorf("count=%d: penalty %.4f dropped from prev %.4f", c, got, prev)
		}
		prev = got
	}
}
