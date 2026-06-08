package mcpsource

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/atharva-ag/devrouter/internal/retrieval"
)

func TestGenericMapperShapes(t *testing.T) {
	cases := map[string]struct {
		in    string
		want  int
		check func(t *testing.T, docs []any)
	}{
		"array":    {in: `[{"id":"1","title":"A","content":"alpha"},{"title":"B","body":"beta"}]`, want: 2},
		"wrapped":  {in: `{"results":[{"doc_id":"9","name":"C","description":"gamma","web_url":"http://x/9"}]}`, want: 1},
		"single":   {in: `{"id":"7","title":"Solo","content":"delta"}`, want: 1},
		"text":     {in: `not json at all`, want: 1},
		"sections": {in: `{"documents":[{"doc_id":"d1","doc_name":"Guide","sections":[{"content":"one"},{"content":"two"}]}]}`, want: 1},
	}
	for name, tc := range cases {
		docs, err := genericMapper(tc.in)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(docs) != tc.want {
			t.Fatalf("%s: want %d docs, got %d (%+v)", name, tc.want, len(docs), docs)
		}
	}

	// Field heuristics resolve aliases.
	docs, _ := genericMapper(`[{"iid":42,"name":"Bug","html_url":"http://gl/42","project":"grp/repo","description":"boom"}]`)
	if len(docs) != 1 {
		t.Fatalf("want 1 doc, got %d", len(docs))
	}
	d := docs[0]
	if d.ID != "42" || d.Title != "Bug" || d.URL != "http://gl/42" || d.Collection != "grp/repo" || !strings.Contains(d.Content, "boom") {
		t.Fatalf("alias resolution wrong: %+v", d)
	}

	// sections concatenation
	sd, _ := genericMapper(`{"documents":[{"doc_id":"d1","doc_name":"Guide","sections":[{"content":"one"},{"content":"two"}]}]}`)
	if len(sd) != 1 || !strings.Contains(sd[0].Content, "one") || !strings.Contains(sd[0].Content, "two") {
		t.Fatalf("sections not joined: %+v", sd)
	}
}

func TestExtractTextPrefersStructuredContent(t *testing.T) {
	result := json.RawMessage(`{"content":[{"type":"text","text":"ignored prose"}],"structuredContent":{"results":[{"id":"1","title":"A","content":"x"}]}}`)
	got, err := extractText(result)
	if err != nil {
		t.Fatalf("extractText: %v", err)
	}
	if !strings.Contains(got, `"results"`) || strings.Contains(got, "ignored prose") {
		t.Fatalf("expected structuredContent JSON, got %q", got)
	}
	// Falls back to text blocks when no structuredContent.
	got2, _ := extractText(json.RawMessage(`{"content":[{"type":"text","text":"hello"}]}`))
	if got2 != "hello" {
		t.Fatalf("want text fallback 'hello', got %q", got2)
	}
}

func TestInferQueryArg(t *testing.T) {
	cases := map[string]string{
		`{"properties":{"query":{"type":"string"}}}`:                                                "query",
		`{"properties":{"q":{"type":"string"}}}`:                                                    "q",
		`{"properties":{"foo":{"type":"number"},"needle":{"type":"string"}},"required":["needle"]}`: "needle",
		`{"properties":{}}`: "query",
		``:                  "query",
	}
	for schema, want := range cases {
		if got := inferQueryArg(json.RawMessage(schema)); got != want {
			t.Fatalf("inferQueryArg(%s) = %q, want %q", schema, got, want)
		}
	}
}

func TestPickSearchTool(t *testing.T) {
	tools := []toolDesc{{Name: "create_issue"}, {Name: "list_things"}, {Name: "search_docs", Description: "search the docs"}}
	if td := pickSearchTool(tools); td == nil || td.Name != "search_docs" {
		t.Fatalf("expected search_docs, got %+v", td)
	}
	// Falls back to first when nothing matches.
	if td := pickSearchTool([]toolDesc{{Name: "alpha"}, {Name: "beta"}}); td == nil || td.Name != "alpha" {
		t.Fatalf("expected first tool fallback, got %+v", td)
	}
	if td := pickSearchTool(nil); td != nil {
		t.Fatalf("expected nil for empty list")
	}
}

func TestParseConfigs(t *testing.T) {
	data := []byte(`[
		{"name":"clickup","transport":"mcp-stdio","endpoint":"clickup-mcp"},
		{"name":"petstore","transport":"openapi","endpoint":"https://api/openapi.json","timeout_ms":1500,"headers":{"Authorization":"Bearer t"}}
	]`)
	cfgs, err := ParseConfigs(data)
	if err != nil {
		t.Fatalf("ParseConfigs: %v", err)
	}
	if len(cfgs) != 2 {
		t.Fatalf("want 2 configs, got %d", len(cfgs))
	}
	if cfgs[0].Name != "clickup" || cfgs[0].Transport != TransportMCPStdio {
		t.Fatalf("unexpected cfg[0]: %+v", cfgs[0])
	}
	if cfgs[1].Transport != TransportOpenAPI || cfgs[1].Timeout.Milliseconds() != 1500 || cfgs[1].Headers["Authorization"] != "Bearer t" {
		t.Fatalf("unexpected cfg[1]: %+v", cfgs[1])
	}
}

// Discovery: an MCP server with no configured ToolName should self-pick
// the search tool and infer its query argument from tools/list.
func TestSourceMCPDiscovery(t *testing.T) {
	var calledTool string
	var calledArgs map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req rpcRequest
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &req)
		switch req.Method {
		case "initialize":
			writeJSONResult(w, req.ID, `{"protocolVersion":"2025-06-18"}`)
		case "notifications/initialized":
			w.WriteHeader(http.StatusOK)
		case "tools/list":
			writeJSONResult(w, req.ID, `{"tools":[{"name":"create_ticket","inputSchema":{"properties":{"title":{"type":"string"}}}},{"name":"search_tickets","description":"search tickets","inputSchema":{"properties":{"term":{"type":"string"}}}}]}`)
		case "tools/call":
			p, _ := req.Params.(map[string]any)
			calledTool, _ = p["name"].(string)
			calledArgs, _ = p["arguments"].(map[string]any)
			writeJSONResult(w, req.ID, `{"content":[{"type":"text","text":"[{\"id\":\"1\",\"title\":\"T\",\"content\":\"c\"}]"}]}`)
		}
	}))
	defer srv.Close()

	src, err := New(Config{Name: "tickets", Transport: TransportMCPHTTP, Endpoint: srv.URL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := src.Search(context.Background(), retrieval.Request{Query: "broken login"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if calledTool != "search_tickets" {
		t.Fatalf("discovery picked wrong tool: %q", calledTool)
	}
	if calledArgs["term"] != "broken login" {
		t.Fatalf("query not bound to inferred arg 'term': %+v", calledArgs)
	}
	if len(res.Docs) != 1 || res.Docs[0].Source != "tickets" {
		t.Fatalf("unexpected docs: %+v", res.Docs)
	}
}

// req.MaxDocs both caps results and is injected as the tool's limit arg.
func TestSearchHonorsMaxDocs(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `[{"id":"1","content":"a"},{"id":"2","content":"b"},{"id":"3","content":"c"}]`)
	}))
	defer srv.Close()

	src, err := New(Config{Name: "docs", Transport: TransportHTTPJSON, Endpoint: srv.URL, ToolName: "x"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := src.Search(context.Background(), retrieval.Request{Query: "q", MaxDocs: 2})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res.Docs) != 2 {
		t.Fatalf("expected cap to 2 docs, got %d", len(res.Docs))
	}
	if v, ok := gotBody["max_docs"]; !ok || fmt.Sprint(v) != "2" {
		t.Fatalf("expected max_docs=2 injected, body=%+v", gotBody)
	}
}

// OpenAPI: spec is the discovery source; selects the search GET and binds
// the query to its parameter.
func TestSourceOpenAPI(t *testing.T) {
	mux := http.NewServeMux()
	var gotQuery string
	srv := httptest.NewServer(mux)
	defer srv.Close()
	spec := `{
		"openapi":"3.0.0",
		"info":{"title":"t","version":"1"},
		"servers":[{"url":"` + srv.URL + `"}],
		"paths":{"/search":{"get":{
			"operationId":"searchDocs","summary":"search documents",
			"parameters":[{"name":"q","in":"query","required":true,"schema":{"type":"string"}}],
			"responses":{"200":{"description":"ok"}}
		}}}
	}`
	mux.HandleFunc("/openapi.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, spec)
	})
	mux.HandleFunc("/search", func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query().Get("q")
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"results":[{"id":"1","title":"Doc","content":"hello world"}]}`)
	})

	src, err := New(Config{Name: "petstore", Transport: TransportOpenAPI, Endpoint: srv.URL + "/openapi.json"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := src.Search(context.Background(), retrieval.Request{Query: "greetings"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if gotQuery != "greetings" {
		t.Fatalf("query not bound to 'q': %q", gotQuery)
	}
	if len(res.Docs) != 1 || res.Docs[0].Title != "Doc" || res.Docs[0].Source != "petstore" {
		t.Fatalf("unexpected docs: %+v", res.Docs)
	}
}
