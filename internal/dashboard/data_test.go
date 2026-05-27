package dashboard

import (
	"reflect"
	"testing"
)

// TestTraceFieldsToRow_CodegraphFiles verifies the CSV → slice
// parsing for the new search_files / graph_files trace hash fields.
// The dashboard's Live Queries detail panel is the only consumer of
// these slices, so we need to make sure the parsing handles the
// shapes the router actually writes (single file, many files,
// missing field entirely, CSV with whitespace) without dropping
// entries or surfacing empty strings.
func TestTraceFieldsToRow_CodegraphFiles(t *testing.T) {
	cases := []struct {
		name        string
		fields      map[string]string
		wantSearch  []string
		wantGraph   []string
		wantMemKeys []string
	}{
		{
			name: "both populated",
			fields: map[string]string{
				"search_files": "internal/router/router.go,internal/router/feedback.go",
				"graph_files":  "internal/heuristics/bandit.go,internal/heuristics/store.go,internal/memory/store.go",
			},
			wantSearch: []string{"internal/router/router.go", "internal/router/feedback.go"},
			wantGraph:  []string{"internal/heuristics/bandit.go", "internal/heuristics/store.go", "internal/memory/store.go"},
		},
		{
			name:       "only search populated (graph stage skipped, e.g. memory-only query)",
			fields:     map[string]string{"search_files": "cmd/router/main.go"},
			wantSearch: []string{"cmd/router/main.go"},
			wantGraph:  nil,
		},
		{
			name:       "missing on both — legacy trace pre-dating the field",
			fields:     map[string]string{},
			wantSearch: nil,
			wantGraph:  nil,
		},
		{
			name: "whitespace + empty entries are dropped",
			fields: map[string]string{
				"search_files": " internal/router/router.go ,, internal/heuristics/store.go ",
			},
			wantSearch: []string{"internal/router/router.go", "internal/heuristics/store.go"},
		},
		{
			name: "alongside memory_keys — three CSV fields cohabit cleanly",
			fields: map[string]string{
				"search_files": "a.go,b.go",
				"graph_files":  "c.go",
				"memory_keys":  "mem:r:file:foo,mem:r:func:bar",
			},
			wantSearch:  []string{"a.go", "b.go"},
			wantGraph:   []string{"c.go"},
			wantMemKeys: []string{"mem:r:file:foo", "mem:r:func:bar"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			row := traceFieldsToRow("query-id", tc.fields)
			if !reflect.DeepEqual(row.SearchFiles, tc.wantSearch) {
				t.Errorf("SearchFiles = %v, want %v", row.SearchFiles, tc.wantSearch)
			}
			if !reflect.DeepEqual(row.GraphFiles, tc.wantGraph) {
				t.Errorf("GraphFiles = %v, want %v", row.GraphFiles, tc.wantGraph)
			}
			if tc.wantMemKeys != nil && !reflect.DeepEqual(row.MemoryKeys, tc.wantMemKeys) {
				t.Errorf("MemoryKeys = %v, want %v", row.MemoryKeys, tc.wantMemKeys)
			}
		})
	}
}
