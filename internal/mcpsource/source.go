// Package mcpsource is the single, config-driven retrieval source used
// for every external tool DevRouter talks to over MCP (or a thin HTTP
// shim). cmdocs, GitLab — and later ClickUp — are all instances of the
// same Source that differ only by Config: transport, endpoint, auth
// headers, the tool name to invoke, and which result mapper to apply.
//
// Adding a new external tool is therefore one Config row + (if its JSON
// shape is new) one mapper, with no changes to the router pipeline.
package mcpsource

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/atharva-ag/devrouter/internal/retrieval"
)

// Transport kinds.
const (
	TransportHTTPJSON = "http-json" // plain HTTP POST returning JSON (cmdocs sidecar)
	TransportMCPHTTP  = "mcp-http"  // MCP JSON-RPC over Streamable HTTP (GitLab)
	TransportMCPStdio = "mcp-stdio" // MCP JSON-RPC over a subprocess
	TransportOpenAPI  = "openapi"   // REST tool described by an OpenAPI spec
)

// Config describes one external tool. For an MCP or OpenAPI tool only
// Name, Transport and Endpoint are required: ToolName/QueryArg are
// auto-discovered (tools/list or the spec) and Mapper defaults to the
// shape-agnostic "generic" normalizer.
type Config struct {
	Name      string            // stable, low-cardinality (trace/metric label)
	Transport string            // one of the Transport* constants
	Endpoint  string            // URL (http/openapi) or command line (stdio)
	Headers   map[string]string // auth/other headers for http transports
	Env       []string          // extra env ("K=V") for stdio transports
	ToolName  string            // MCP tool / OpenAPI operationId (auto-discovered when empty)
	QueryArg  string            // arg name carrying the query (auto/"query")
	LimitArg  string            // arg name carrying the result cap (auto: max_docs/limit)
	ExtraArgs map[string]any    // static args merged into every call
	Mapper    string            // mapper registry key; empty -> "generic"
	MaxDocs   int               // per-call DocEntry cap (default 5)
	Timeout   time.Duration     // per-call timeout (default 8s)
}

// Source is the generic external retrieval tool. It satisfies
// retrieval.Source.
type Source struct {
	cfg    Config
	tr     transport
	mapFn  mapper
	maxDoc int
}

// New builds a Source from cfg, wiring the transport and mapper. Returns
// an error for an unknown transport/mapper or missing endpoint so
// misconfiguration fails loudly at startup rather than silently at query
// time.
func New(cfg Config) (*Source, error) {
	if cfg.Name == "" {
		return nil, fmt.Errorf("mcpsource: empty Name")
	}
	if cfg.Endpoint == "" {
		return nil, fmt.Errorf("mcpsource %q: empty Endpoint", cfg.Name)
	}
	mapperKey := cfg.Mapper
	if mapperKey == "" {
		mapperKey = "generic"
	}
	mapFn, ok := mappers[mapperKey]
	if !ok {
		return nil, fmt.Errorf("mcpsource %q: unknown mapper %q", cfg.Name, cfg.Mapper)
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 8 * time.Second
	}
	maxDoc := cfg.MaxDocs
	if maxDoc <= 0 {
		maxDoc = 5
	}

	var tr transport
	switch cfg.Transport {
	case TransportHTTPJSON:
		tr = newHTTPJSONTransport(cfg.Endpoint, cfg.Headers, timeout)
	case TransportMCPHTTP:
		tr = newMCPHTTPTransport(cfg.Endpoint, cfg.Headers, timeout)
	case TransportMCPStdio:
		tr = newMCPStdioTransport(strings.Fields(cfg.Endpoint), cfg.Env)
	case TransportOpenAPI:
		oa, oerr := newOpenAPITransport(cfg.Endpoint, cfg.Headers, cfg.ToolName, timeout)
		if oerr != nil {
			return nil, fmt.Errorf("mcpsource %q: %w", cfg.Name, oerr)
		}
		// The spec resolved the query parameter; key the call args by it.
		if cfg.QueryArg == "" {
			cfg.QueryArg = oa.queryParam
		}
		tr = oa
	default:
		return nil, fmt.Errorf("mcpsource %q: unknown transport %q", cfg.Name, cfg.Transport)
	}

	// Self-description: an MCP tool configured with only an endpoint/command
	// (no ToolName) discovers its search tool and query argument via
	// tools/list, so adding a tool is just pointing at it.
	if cfg.ToolName == "" {
		if d, ok := tr.(discoverer); ok {
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			tools, derr := d.listTools(ctx)
			cancel()
			if derr != nil {
				return nil, fmt.Errorf("mcpsource %q: tool discovery failed: %w", cfg.Name, derr)
			}
			td := pickSearchTool(tools)
			if td == nil {
				return nil, fmt.Errorf("mcpsource %q: tools/list returned no tools", cfg.Name)
			}
			cfg.ToolName = td.Name
			if cfg.QueryArg == "" {
				cfg.QueryArg = inferQueryArg(td.InputSchema)
			}
		}
	}

	return &Source{cfg: cfg, tr: tr, mapFn: mapFn, maxDoc: maxDoc}, nil
}

// searchToolHints are substrings that mark a tool as the search/query
// surface during auto-discovery.
var searchToolHints = []string{"search", "query", "find", "lookup", "retrieve"}

// pickSearchTool chooses the search-like tool from a tools/list result:
// the first whose name/description matches a hint, else the only/first
// tool. Returns nil when the list is empty.
func pickSearchTool(tools []toolDesc) *toolDesc {
	if len(tools) == 0 {
		return nil
	}
	for i := range tools {
		hay := strings.ToLower(tools[i].Name + " " + tools[i].Description)
		for _, h := range searchToolHints {
			if strings.Contains(hay, h) {
				return &tools[i]
			}
		}
	}
	return &tools[0]
}

// queryArgHints are the conventional names for the argument that carries
// the search string, tried before falling back to schema inspection.
var queryArgHints = []string{"query", "q", "search", "term", "text", "keywords"}

// inferQueryArg picks the query argument from a tool's JSON-Schema input:
// a conventionally-named property, else the first required string
// property, else the first string property (deterministic), else "query".
func inferQueryArg(schema json.RawMessage) string {
	if len(schema) == 0 {
		return "query"
	}
	var s struct {
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
	}
	if err := json.Unmarshal(schema, &s); err != nil || len(s.Properties) == 0 {
		return "query"
	}
	for _, name := range queryArgHints {
		if _, ok := s.Properties[name]; ok {
			return name
		}
	}
	for _, name := range s.Required {
		if raw, ok := s.Properties[name]; ok && isStringProp(raw) {
			return name
		}
	}
	names := make([]string, 0, len(s.Properties))
	for name := range s.Properties {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if isStringProp(s.Properties[name]) {
			return name
		}
	}
	return "query"
}

func isStringProp(raw json.RawMessage) bool {
	var p struct {
		Type any `json:"type"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return false
	}
	switch t := p.Type.(type) {
	case string:
		return t == "string"
	case []any:
		for _, e := range t {
			if s, ok := e.(string); ok && s == "string" {
				return true
			}
		}
	}
	return false
}

func (s *Source) Name() string { return s.cfg.Name }

// DefaultDocs reports this source's default per-call doc cap, used to
// seed the per-source breadth bandit. Satisfies retrieval.Breadth.
func (s *Source) DefaultDocs() int { return s.maxDoc }

// Search composes the query (folding in memory signals), invokes the
// configured tool, maps the result to DocEntries, and caps the count.
//
// When req.MaxDocs > 0 (the breadth bandit's choice for this source) it
// overrides the static cap AND is injected as the tool's own limit
// argument so the tool returns more rather than us just trimming less.
func (s *Source) Search(ctx context.Context, req retrieval.Request) (retrieval.Result, error) {
	maxDoc := s.maxDoc
	if req.MaxDocs > 0 {
		maxDoc = req.MaxDocs
	}

	args := make(map[string]any, len(s.cfg.ExtraArgs)+2)
	for k, v := range s.cfg.ExtraArgs {
		args[k] = v
	}
	qArg := s.cfg.QueryArg
	if qArg == "" {
		qArg = "query"
	}
	args[qArg] = composeQuery(req)
	// Only inject the result cap as a tool arg when the breadth bandit is
	// actively tuning it (req.MaxDocs > 0); otherwise leave any static
	// ExtraArgs limit untouched so default behaviour is unchanged.
	if req.MaxDocs > 0 {
		if limitArg := s.limitArg(); limitArg != "" {
			args[limitArg] = maxDoc
		}
	}

	text, err := s.tr.call(ctx, s.cfg.ToolName, args)
	if err != nil {
		return retrieval.Result{}, err
	}
	docs, err := s.mapFn(text)
	if err != nil {
		return retrieval.Result{}, fmt.Errorf("mcpsource %q: map result: %w", s.cfg.Name, err)
	}
	// Stamp the source name on entries the (shape-agnostic) mapper left
	// blank so downstream trace/labels stay attributable.
	for i := range docs {
		if docs[i].Source == "" {
			docs[i].Source = s.cfg.Name
		}
	}
	if len(docs) > maxDoc {
		docs = docs[:maxDoc]
	}
	return retrieval.Result{Docs: docs}, nil
}

// limitArg is the argument name carrying the result cap. Empty means the
// tool takes no explicit limit (we only trim our side). Defaults to
// "max_docs" unless the tool already declares a cap via ExtraArgs (in
// which case we respect the configured key) or LimitArg is set.
func (s *Source) limitArg() string {
	if s.cfg.LimitArg != "" {
		return s.cfg.LimitArg
	}
	for _, k := range []string{"max_docs", "limit", "top_k", "k", "count"} {
		if _, ok := s.cfg.ExtraArgs[k]; ok {
			return k
		}
	}
	return "max_docs"
}

// Close releases transport resources (subprocess for stdio).
func (s *Source) Close() error { return s.tr.Close() }

// composeQuery augments the raw query with memory's recalled signal
// terms (and a few recalled paths) so the external tool is "aimed" the
// same way codegraph traversal is. Kept short to avoid drowning the
// tool's own search.
func composeQuery(req retrieval.Request) string {
	parts := []string{req.Query}
	parts = append(parts, take(req.Signals.Terms, 5)...)
	parts = append(parts, take(req.Signals.Paths, 3)...)
	return strings.TrimSpace(strings.Join(dedup(parts), " "))
}

func take(xs []string, n int) []string {
	if len(xs) > n {
		return xs[:n]
	}
	return xs
}

func dedup(xs []string) []string {
	seen := make(map[string]bool, len(xs))
	out := xs[:0]
	for _, x := range xs {
		x = strings.TrimSpace(x)
		if x == "" || seen[x] {
			continue
		}
		seen[x] = true
		out = append(out, x)
	}
	return out
}

var _ retrieval.Source = (*Source)(nil)
