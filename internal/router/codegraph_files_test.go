package router

import (
	"reflect"
	"testing"

	"github.com/atharva-ag/devrouter/internal/heuristics"
	"github.com/atharva-ag/devrouter/internal/prompt"
)

// TestStageFiles confirms the helper unpacks the codegraph_files
// slice from a StageTrace.Details map, and returns nil cleanly for
// every degenerate input shape — nil stage, nil details, missing
// key, wrong value type. The router writes these stages from
// multiple code paths so the helper has to be tolerant.
func TestStageFiles(t *testing.T) {
	files := []string{"a.go", "b.go"}

	cases := []struct {
		name  string
		stage *prompt.StageTrace
		want  []string
	}{
		{name: "nil stage", stage: nil, want: nil},
		{
			name:  "nil details map",
			stage: &prompt.StageTrace{},
			want:  nil,
		},
		{
			name:  "missing key",
			stage: &prompt.StageTrace{Details: map[string]interface{}{"branch": "main"}},
			want:  nil,
		},
		{
			name:  "wrong type — caller stashed a string instead of []string",
			stage: &prompt.StageTrace{Details: map[string]interface{}{"codegraph_files": "a.go,b.go"}},
			want:  nil,
		},
		{
			name:  "happy path",
			stage: &prompt.StageTrace{Details: map[string]interface{}{"codegraph_files": files}},
			want:  files,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := stageFiles(tc.stage)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("stageFiles = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestCapPaths confirms the per-stage Redis size cap behaves like a
// std-lib slice prefix: shorter / equal lists pass through, longer
// lists truncate to n. Cap=0 or negative is a no-op (the constant
// in router.go is positive, but defending the contract anyway keeps
// future refactors from silently zeroing the field).
func TestCapPaths(t *testing.T) {
	long := make([]string, 100)
	for i := range long {
		long[i] = "file"
	}

	cases := []struct {
		name string
		in   []string
		n    int
		want int
	}{
		{name: "empty input", in: nil, n: 50, want: 0},
		{name: "below cap", in: long[:10], n: 50, want: 10},
		{name: "at cap", in: long[:50], n: 50, want: 50},
		{name: "over cap truncates", in: long, n: 50, want: 50},
		{name: "n=0 is no-op", in: long[:5], n: 0, want: 5},
		{name: "n<0 is no-op", in: long[:5], n: -1, want: 5},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := capPaths(tc.in, tc.n)
			if len(got) != tc.want {
				t.Errorf("capPaths length = %d, want %d", len(got), tc.want)
			}
		})
	}
}

// TestTraceHashFields_CodegraphFiles confirms that the persisted
// hash fields are the source of truth the dashboard reads from.
// Each stage's codegraph_files slice should round-trip onto its
// dedicated CSV field, and an empty slice should leave the field
// absent (omitempty semantics on the wire).
func TestTraceHashFields_CodegraphFiles(t *testing.T) {
	t.Run("populated stages produce CSV fields", func(t *testing.T) {
		trace := &prompt.RetrievalTrace{
			QueryID: "qid-1",
			SearchStage: &prompt.StageTrace{
				Details: map[string]interface{}{
					"codegraph_files": []string{"router.go", "feedback.go"},
				},
			},
			GraphStage: &prompt.StageTrace{
				Details: map[string]interface{}{
					"codegraph_files": []string{"store.go", "bandit.go", "picker.go"},
				},
			},
		}
		got := traceHashFields(trace, "repo", "q", "debug", "p-1", heuristics.Default("debug"), "agent", QueryPlan{}, nil)
		if got["search_files"] != "router.go,feedback.go" {
			t.Errorf("search_files = %v, want %q", got["search_files"], "router.go,feedback.go")
		}
		if got["graph_files"] != "store.go,bandit.go,picker.go" {
			t.Errorf("graph_files = %v, want %q", got["graph_files"], "store.go,bandit.go,picker.go")
		}
	})

	t.Run("empty stages omit the fields entirely", func(t *testing.T) {
		trace := &prompt.RetrievalTrace{QueryID: "qid-2"}
		got := traceHashFields(trace, "repo", "q", "debug", "p-1", heuristics.Default("debug"), "agent", QueryPlan{}, nil)
		if _, ok := got["search_files"]; ok {
			t.Errorf("search_files should be absent when SearchStage is nil")
		}
		if _, ok := got["graph_files"]; ok {
			t.Errorf("graph_files should be absent when GraphStage is nil")
		}
	})
}
