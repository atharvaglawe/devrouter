package mcpsource

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/atharva-ag/devrouter/internal/retrieval"
)

func TestCmdocsMapper(t *testing.T) {
	in := `{"results":[
		{"doc_id":"d1","doc_name":"Retry Guide","collection":"sre","sections":[{"page":3,"content":"use exponential backoff"},{"page":4,"content":"cap at 30s"}]},
		{"doc_id":"d2","doc_name":"Empty","collection":"sre","sections":[]}
	]}`
	docs, err := cmdocsMapper(in)
	if err != nil {
		t.Fatalf("cmdocsMapper: %v", err)
	}
	if len(docs) != 1 {
		t.Fatalf("want 1 doc (empty-content doc dropped), got %d", len(docs))
	}
	d := docs[0]
	if d.Source != "cmdocs" || d.ID != "d1" || d.Title != "Retry Guide" || d.Collection != "sre" {
		t.Fatalf("unexpected doc metadata: %+v", d)
	}
	if !strings.Contains(d.Content, "exponential backoff") || !strings.Contains(d.Content, "cap at 30s") {
		t.Fatalf("sections not concatenated: %q", d.Content)
	}
}

func TestGitlabMapperShapes(t *testing.T) {
	cases := map[string]string{
		"bare array": `[{"iid":7,"title":"Failover bug","web_url":"https://gl/issues/7","description":"provider down","state":"opened","references":{"full":"grp/repo#7"}}]`,
		"items obj":   `{"items":[{"iid":7,"title":"Failover bug","web_url":"https://gl/issues/7","description":"provider down"}]}`,
		"results obj": `{"results":[{"id":7,"name":"Failover bug","url":"https://gl/issues/7","body":"provider down"}]}`,
	}
	for name, in := range cases {
		docs, err := gitlabMapper(in)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(docs) != 1 {
			t.Fatalf("%s: want 1 doc, got %d", name, len(docs))
		}
		d := docs[0]
		if d.Source != "gitlab" || d.ID != "7" || d.Title != "Failover bug" {
			t.Fatalf("%s: unexpected doc: %+v", name, d)
		}
		if d.URL == "" || !strings.Contains(d.Content, "provider down") {
			t.Fatalf("%s: missing url/content: %+v", name, d)
		}
	}
}

func TestGitlabMapperUnknownShapeFallback(t *testing.T) {
	docs, err := gitlabMapper(`"some opaque string result"`)
	if err != nil {
		t.Fatalf("gitlabMapper: %v", err)
	}
	if len(docs) != 1 || docs[0].Source != "gitlab" || docs[0].Content == "" {
		t.Fatalf("expected single fallback doc, got %+v", docs)
	}
}

func TestComposeQueryFoldsSignals(t *testing.T) {
	got := composeQuery(retrieval.Request{
		Query: "why does failover loop",
		Signals: retrieval.Signals{
			Terms: []string{"failover", "failover", "retry"},
			Paths: []string{"internal/provider/failover.go"},
		},
	})
	if !strings.Contains(got, "why does failover loop") ||
		!strings.Contains(got, "retry") ||
		!strings.Contains(got, "internal/provider/failover.go") {
		t.Fatalf("composeQuery missing parts: %q", got)
	}
	// "failover" appears in the query phrase, once as a standalone term,
	// and inside the path = 3 occurrences. A 4th would mean the duplicate
	// term survived (whole-part dedup failed).
	if n := strings.Count(got, "failover"); n != 3 {
		t.Fatalf("composeQuery did not dedup the duplicate term (got %d occurrences): %q", n, got)
	}
}

// http-json transport drives the cmdocs sidecar.
func TestSourceHTTPJSON(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"results":[{"doc_id":"d1","doc_name":"Guide","collection":"sre","sections":[{"page":1,"content":"hello"}]}]}`)
	}))
	defer srv.Close()

	src, err := New(Config{
		Name: "cmdocs", Transport: TransportHTTPJSON, Endpoint: srv.URL,
		ToolName: "pageindex_search", QueryArg: "query",
		ExtraArgs: map[string]any{"max_docs": 3}, Mapper: "cmdocs",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := src.Search(context.Background(), retrieval.Request{Query: "retry policy"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res.Docs) != 1 || res.Docs[0].Content != "hello" {
		t.Fatalf("unexpected docs: %+v", res.Docs)
	}
	if gotBody["query"] != "retry policy" || gotBody["max_docs"].(float64) != 3 {
		t.Fatalf("request body not as expected: %+v", gotBody)
	}
}

// mcp-http transport: simulate an MCP server's initialize + tools/call.
func TestSourceMCPHTTP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req rpcRequest
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &req)
		w.Header().Set("Mcp-Session-Id", "sess-1")
		switch req.Method {
		case "initialize":
			writeJSONResult(w, req.ID, `{"protocolVersion":"2025-06-18","serverInfo":{"name":"gl"}}`)
		case "notifications/initialized":
			w.WriteHeader(http.StatusOK)
		case "tools/call":
			gl := `[{"iid":7,"title":"Failover bug","web_url":"https://gl/7","description":"down"}]`
			payload, _ := json.Marshal(gl)
			writeJSONResult(w, req.ID, `{"content":[{"type":"text","text":`+string(payload)+`}]}`)
		default:
			http.Error(w, "unknown", http.StatusBadRequest)
		}
	}))
	defer srv.Close()

	src, err := New(Config{
		Name: "gitlab", Transport: TransportMCPHTTP, Endpoint: srv.URL,
		Headers: map[string]string{"Authorization": "Bearer tok"},
		ToolName: "search_issues", QueryArg: "search", Mapper: "gitlab",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := src.Search(context.Background(), retrieval.Request{Query: "failover"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res.Docs) != 1 || res.Docs[0].ID != "7" || res.Docs[0].Source != "gitlab" {
		t.Fatalf("unexpected docs: %+v", res.Docs)
	}
}

// mcp-http transport over SSE (text/event-stream) response.
func TestSourceMCPHTTPSSE(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req rpcRequest
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &req)
		switch req.Method {
		case "initialize":
			writeSSEResult(w, req.ID, `{"protocolVersion":"2025-06-18"}`)
		case "notifications/initialized":
			w.WriteHeader(http.StatusOK)
		case "tools/call":
			writeSSEResult(w, req.ID, `{"content":[{"type":"text","text":"{\"items\":[{\"iid\":9,\"title\":\"x\",\"description\":\"y\"}]}"}]}`)
		}
	}))
	defer srv.Close()

	src, _ := New(Config{
		Name: "gitlab", Transport: TransportMCPHTTP, Endpoint: srv.URL,
		ToolName: "search_issues", QueryArg: "search", Mapper: "gitlab",
	})
	res, err := src.Search(context.Background(), retrieval.Request{Query: "x"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res.Docs) != 1 || res.Docs[0].ID != "9" {
		t.Fatalf("unexpected docs from SSE: %+v", res.Docs)
	}
}

func TestNewRejectsBadConfig(t *testing.T) {
	if _, err := New(Config{Name: "x", Endpoint: "u", Transport: "bogus", Mapper: "cmdocs"}); err == nil {
		t.Fatal("expected error for unknown transport")
	}
	if _, err := New(Config{Name: "x", Endpoint: "u", Transport: TransportHTTPJSON, Mapper: "nope"}); err == nil {
		t.Fatal("expected error for unknown mapper")
	}
	if _, err := New(Config{Name: "x", Transport: TransportHTTPJSON, Mapper: "cmdocs"}); err == nil {
		t.Fatal("expected error for empty endpoint")
	}
}

func writeJSONResult(w http.ResponseWriter, id int64, result string) {
	w.Header().Set("Content-Type", "application/json")
	io.WriteString(w, `{"jsonrpc":"2.0","id":`+itoa(id)+`,"result":`+result+`}`)
}

func writeSSEResult(w http.ResponseWriter, id int64, result string) {
	w.Header().Set("Content-Type", "text/event-stream")
	io.WriteString(w, "event: message\ndata: "+`{"jsonrpc":"2.0","id":`+itoa(id)+`,"result":`+result+`}`+"\n\n")
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}
