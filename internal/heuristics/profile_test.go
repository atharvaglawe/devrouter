package heuristics

import "testing"

func TestDefaultClipsToBounds(t *testing.T) {
	for _, intent := range []string{"debug", "explore", "trace", "refactor", "general", "unknown"} {
		p := Default(intent)
		if p.MaxTrace < Bounds.MaxTrace[0] || p.MaxTrace > Bounds.MaxTrace[1] {
			t.Errorf("%s: MaxTrace=%d out of bounds %v", intent, p.MaxTrace, Bounds.MaxTrace)
		}
		if p.CallerHops < Bounds.CallerHops[0] || p.CallerHops > Bounds.CallerHops[1] {
			t.Errorf("%s: CallerHops=%d out of bounds %v", intent, p.CallerHops, Bounds.CallerHops)
		}
	}
}

func TestClipEnforcesBounds(t *testing.T) {
	tests := []struct {
		name string
		in   Profile
		want Profile
	}{
		{
			name: "below min",
			in:   Profile{MaxTrace: 0, CallerHops: -5, MaxSnippets: 0, MaxDecisions: 1},
			want: Profile{
				MaxTrace: Bounds.MaxTrace[0], CallerHops: Bounds.CallerHops[0],
				MaxSnippets: Bounds.MaxSnippets[0], MaxDecisions: Bounds.MaxDecisions[0],
			},
		},
		{
			name: "above max",
			in:   Profile{MaxTrace: 999, CallerHops: 999, MaxSnippets: 999, MaxDecisions: 999},
			want: Profile{
				MaxTrace: Bounds.MaxTrace[1], CallerHops: Bounds.CallerHops[1],
				MaxSnippets: Bounds.MaxSnippets[1], MaxDecisions: Bounds.MaxDecisions[1],
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.in.Clip()
			if got.MaxTrace != tc.want.MaxTrace || got.CallerHops != tc.want.CallerHops ||
				got.MaxSnippets != tc.want.MaxSnippets || got.MaxDecisions != tc.want.MaxDecisions {
				t.Errorf("Clip() got=%+v want=%+v", got, tc.want)
			}
		})
	}
}

func TestApplyMemoryShrink(t *testing.T) {
	base := Profile{
		MaxTrace: 5, CallerHops: 2,
		MaxSymbols: 20, MaxSnippets: 5, MaxSiblings: 15,
	}.Clip()

	t.Run("memCount=0 leaves profile alone", func(t *testing.T) {
		got := base.ApplyMemoryShrink(0)
		if got.MaxTrace != 5 || got.MaxSnippets != 5 {
			t.Errorf("memCount=0 should not shrink, got %+v", got)
		}
	})

	t.Run("memCount=1 shrinks trim caps", func(t *testing.T) {
		got := base.ApplyMemoryShrink(1)
		if got.MaxSymbols > 5 || got.MaxSnippets > 2 || got.MaxSiblings > 5 {
			t.Errorf("memCount=1 should shrink trim caps, got %+v", got)
		}
		if got.MaxTrace != 5 {
			t.Errorf("memCount=1 should not shrink graph budget, got MaxTrace=%d", got.MaxTrace)
		}
	})

	t.Run("memCount>=3 shrinks aggressively", func(t *testing.T) {
		got := base.ApplyMemoryShrink(3)
		if got.MaxTrace > 3 {
			t.Errorf("memCount>=3 should cap MaxTrace<=3, got %d", got.MaxTrace)
		}
		if got.MaxSnippets > 1 {
			t.Errorf("memCount>=3 should cap MaxSnippets<=1, got %d", got.MaxSnippets)
		}
	})
}

func TestProfileIDIsStable(t *testing.T) {
	p1 := Default("debug")
	p2 := Default("debug")
	if p1.ID() != p2.ID() {
		t.Errorf("identical profiles should have same ID: %s vs %s", p1.ID(), p2.ID())
	}
	p3 := p1
	p3.MaxTrace++
	if p3.ID() == p1.ID() {
		t.Errorf("perturbed profile should have different ID: both %s", p1.ID())
	}
}
