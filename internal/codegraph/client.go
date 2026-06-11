// Package codegraph is the Go HTTP client for the in-tree codegraph service
// — a thin Node sidecar (see /codegraph) that wraps the MIT-licensed
// @colbymchenry/codegraph engine and re-exposes the small API devrouter
// consumes on :4747.
//
// The sidecar replaced the previous GitNexus-derived engine (PolyForm
// Noncommercial). With it, the wire format dropped raw Cypher (`/api/query`)
// in favour of purpose-built JSON endpoints; this client maps each former
// Cypher method onto one of those endpoints while keeping every exported
// signature and return type (SearchResult, CallEdge, Subgraph, …) identical
// so router/crossrepo/Subgraph callers are unaffected.
package codegraph

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/atharva-ag/devrouter/internal/prompt"
	"github.com/atharva-ag/devrouter/internal/telemetry"
)

// instrumentedDo wraps an outbound codegraph HTTP request with Prometheus
// instrumentation. endpoint is the static label fed to the metric (one of
// "search", "graph", "file", "repos") so cardinality stays bounded
// regardless of how query strings or repo names vary.
//
// Errors at the transport layer are tagged status="error"; non-2xx
// responses are tagged with their status-class bucket ("4xx", "5xx").
// The caller still owns response-body parsing and the higher-level
// error wrapping — this helper only times the request and records the
// outcome.
func (c *Client) instrumentedDo(endpoint string, req *http.Request) (*http.Response, error) {
	telemetry.CodegraphInflight.WithLabelValues(endpoint).Inc()
	defer telemetry.CodegraphInflight.WithLabelValues(endpoint).Dec()

	start := time.Now()
	resp, err := c.httpClient.Do(req)
	telemetry.CodegraphRequestDuration.WithLabelValues(endpoint).Observe(time.Since(start).Seconds())

	status := "error"
	if err == nil && resp != nil {
		status = strconv.Itoa(resp.StatusCode/100) + "xx"
	}
	telemetry.CodegraphRequests.WithLabelValues(endpoint, status).Inc()
	return resp, err
}

// postJSON is the instrumented POST helper for the JSON-body codegraph
// endpoints (/api/search and the /api/graph/* family).
func (c *Client) postJSON(endpoint, path string, body []byte) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodPost, c.BaseURL+path, bytes.NewReader(body))
	if err != nil {
		telemetry.CodegraphRequests.WithLabelValues(endpoint, "error").Inc()
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return c.instrumentedDo(endpoint, req)
}

// getURL is the instrumented GET helper for the codegraph endpoints
// that take query-string parameters (/api/file, /api/repos).
func (c *Client) getURL(endpoint, fullURL string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodGet, fullURL, nil)
	if err != nil {
		telemetry.CodegraphRequests.WithLabelValues(endpoint, "error").Inc()
		return nil, err
	}
	return c.instrumentedDo(endpoint, req)
}

// postDecode marshals body, POSTs it to path, and decodes a 200 response
// into out (skip with out=nil). Non-2xx responses become errors carrying
// the body, matching the previous Cypher helper's behaviour.
func (c *Client) postDecode(endpoint, path string, body any, out any) error {
	payload, _ := json.Marshal(body)
	resp, err := c.postJSON(endpoint, path, payload)
	if err != nil {
		return fmt.Errorf("codegraph %s: %w", endpoint, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("codegraph %s %d: %s", endpoint, resp.StatusCode, raw)
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("codegraph %s decode: %w", endpoint, err)
	}
	return nil
}

type Client struct {
	BaseURL    string
	httpClient *http.Client
}

func NewClient(baseURL string) *Client {
	if baseURL == "" {
		baseURL = "http://localhost:4747"
	}
	return &Client{
		BaseURL:    baseURL,
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}
}

// ---------------------------------------------------------------------------
// POST /api/search — FTS / hybrid / BM25 search (semantic falls back to FTS)
// ---------------------------------------------------------------------------

// SearchResult mirrors the shape returned by the codegraph sidecar.
type SearchResult struct {
	NodeID   string `json:"nodeId"`
	ID       string `json:"id"`
	Name     string `json:"name"`
	FilePath string `json:"filePath"`
	Label    string `json:"label"`
	Content  string `json:"content"`
	// Source is populated when the search request sets
	// `include_source: true`. The sidecar slices it from the file by
	// [startLine,endLine] (the new engine doesn't store symbol bodies),
	// capped at 64 KB. Always empty otherwise.
	Source string  `json:"source"`
	Score  float64 `json:"score"`

	StartLine int `json:"startLine"`
	EndLine   int `json:"endLine"`

	Connections *struct {
		Outgoing []ConnectionEntry `json:"outgoing"`
		Incoming []ConnectionEntry `json:"incoming"`
	} `json:"connections,omitempty"`
}

type ConnectionEntry struct {
	Name       string  `json:"name"`
	Type       string  `json:"type"`
	Confidence float64 `json:"confidence"`
}

type searchResponse struct {
	Results []SearchResult `json:"results"`
	Error   string         `json:"error,omitempty"`
}

// Valid search modes accepted by the codegraph /api/search endpoint.
//
// The MIT engine's search is lexical (FTS5 + LIKE + fuzzy + BM25); there is
// no vector/semantic index. Modes are kept for source compatibility — the
// sidecar treats hybrid/bm25/semantic identically (lexical) — and
// SearchModeForIntent no longer routes anything to semantic.
const (
	SearchModeHybrid   = "hybrid"
	SearchModeBM25     = "bm25"
	SearchModeSemantic = "semantic"
	// SearchModeAuto is a router-side sentinel, not understood by the
	// codegraph API. Translated to one of the three above before
	// hitting the wire.
	SearchModeAuto = ""
)

// Search calls POST /api/search in hybrid mode (the historical
// default). For intent-aware mode selection use SearchWithMode.
func (c *Client) Search(query string, repo string, limit int) ([]SearchResult, error) {
	return c.SearchWithMode(query, repo, limit, SearchModeHybrid)
}

// SearchWithMode is the mode-explicit variant of Search. With the MIT engine
// every mode resolves to the same lexical search, but the parameter is kept
// so callers (and tests) don't need to change.
//
// An empty mode string falls back to "hybrid".
func (c *Client) SearchWithMode(
	query string, repo string, limit int, mode string,
) ([]SearchResult, error) {
	if limit <= 0 {
		limit = 10
	}
	if mode == "" {
		mode = SearchModeHybrid
	}
	body := map[string]any{
		"query": query,
		"limit": limit,
		"mode":  mode,
		// Inline the matched source so downstream callers (router →
		// ToSnippets → DevPrompt) get function bodies in one round
		// trip. The sidecar slices [startLine,endLine] from disk.
		"include_source": true,
	}
	if repo != "" {
		body["repo"] = repo
	}

	var sr searchResponse
	if err := c.postDecode("search", "/api/search", body, &sr); err != nil {
		return nil, err
	}
	if sr.Error != "" {
		return nil, fmt.Errorf("codegraph search: %s", sr.Error)
	}
	return sr.Results, nil
}

// SearchModeForIntent maps a query intent (as classified by the router) to a
// codegraph search mode.
//
// The MIT engine has no vector index, so paraphrase-friendly "semantic"
// search is unavailable; every intent routes to a lexical mode. general/trace
// (previously semantic) fall back to hybrid — the safest lexical default. A
// vector layer can be added to the sidecar later (see codegraph/MIGRATION.md),
// at which point this mapping can route general/trace back to semantic.
func SearchModeForIntent(intent string) string {
	switch intent {
	case "refactor":
		return SearchModeBM25
	case "debug", "explore", "general", "trace":
		return SearchModeHybrid
	default:
		return SearchModeHybrid
	}
}

// ---------------------------------------------------------------------------
// Call graph: callers / callees / upstream
// ---------------------------------------------------------------------------

// CallEdge is a caller->callee relationship with file location.
type CallEdge struct {
	From     string
	To       string
	FilePath string
}

type edgeJSON struct {
	From string `json:"from"`
	To   string `json:"to"`
	File string `json:"file"`
}

type edgesResponse struct {
	Edges []edgeJSON `json:"edges"`
	Error string     `json:"error,omitempty"`
}

// graphEdges POSTs to a /api/graph/* endpoint and decodes the {edges:[...]}
// response into []CallEdge.
func (c *Client) graphEdges(path string, body map[string]any) ([]CallEdge, error) {
	var er edgesResponse
	if err := c.postDecode("graph", path, body, &er); err != nil {
		return nil, err
	}
	if er.Error != "" {
		return nil, fmt.Errorf("codegraph graph: %s", er.Error)
	}
	edges := make([]CallEdge, 0, len(er.Edges))
	for _, e := range er.Edges {
		if e.From == "" || e.To == "" {
			continue
		}
		edges = append(edges, CallEdge{From: e.From, To: e.To, FilePath: e.File})
	}
	return edges, nil
}

func (c *Client) graphPaths(path string, body map[string]any) ([]string, error) {
	var pr struct {
		Paths []string `json:"paths"`
		Error string   `json:"error,omitempty"`
	}
	if err := c.postDecode("graph", path, body, &pr); err != nil {
		return nil, err
	}
	if pr.Error != "" {
		return nil, fmt.Errorf("codegraph graph: %s", pr.Error)
	}
	return pr.Paths, nil
}

// Callers returns d=1 upstream callers of a symbol by name.
func (c *Client) Callers(symbolName string, repo string) ([]string, error) {
	edges, err := c.CallersWithPath(symbolName, repo)
	if err != nil {
		return nil, err
	}
	return edgeNames(edges, func(e CallEdge) string { return e.From }), nil
}

// Callees returns d=1 downstream symbols called by a symbol.
func (c *Client) Callees(symbolName string, repo string) ([]string, error) {
	edges, err := c.CalleesWithPath(symbolName, repo)
	if err != nil {
		return nil, err
	}
	return edgeNames(edges, func(e CallEdge) string { return e.To }), nil
}

func edgeNames(edges []CallEdge, pick func(CallEdge) string) []string {
	seen := make(map[string]bool, len(edges))
	var names []string
	for _, e := range edges {
		n := pick(e)
		if n != "" && !seen[n] {
			seen[n] = true
			names = append(names, n)
		}
	}
	return names
}

// CallersWithPath returns d=1 upstream callers with file paths.
func (c *Client) CallersWithPath(symbolName string, repo string) ([]CallEdge, error) {
	return c.graphEdges("/api/graph/callers", map[string]any{"name": symbolName, "repo": repo, "limit": 15})
}

// CalleesWithPath returns d=1 downstream callees with file paths.
func (c *Client) CalleesWithPath(symbolName string, repo string) ([]CallEdge, error) {
	return c.graphEdges("/api/graph/callees", map[string]any{"name": symbolName, "repo": repo, "limit": 15})
}

// UpstreamChain traces callers 2 hops deep: grandparent -> parent -> target.
func (c *Client) UpstreamChain(symbolName string, repo string) ([]CallEdge, error) {
	return c.graphEdges("/api/graph/upstream", map[string]any{"name": symbolName, "repo": repo, "limit": 10})
}

// ---------------------------------------------------------------------------
// Structural relationships: IMPORTS, EXTENDS, HAS_METHOD
// ---------------------------------------------------------------------------

// Importers finds files that reference the symbol from another file (the
// resolved cross-file reference graph stands in for IMPORTS+DEFINES on the
// new schema). Reveals factory/registry files that wire up a component.
func (c *Client) Importers(symbolName string, repo string) ([]CallEdge, error) {
	return c.graphEdges("/api/graph/importers", map[string]any{"name": symbolName, "repo": repo, "limit": 15})
}

// ImportersByPackage finds files that import packages whose name contains the
// query word. Broader than Importers.
func (c *Client) ImportersByPackage(pkgWord string, repo string) ([]CallEdge, error) {
	return c.graphEdges("/api/graph/importers-by-package", map[string]any{"pkg": strings.ToLower(pkgWord), "repo": repo, "limit": 30})
}

// Extends finds EXTENDS/IMPLEMENTS relationships involving the symbol (both
// directions). Catches struct embedding and interface implementations.
func (c *Client) Extends(symbolName string, repo string) ([]CallEdge, error) {
	return c.graphEdges("/api/graph/extends", map[string]any{"name": symbolName, "repo": repo, "limit": 15})
}

// Methods finds the members a struct/interface/class contains (HAS_METHOD).
func (c *Client) Methods(symbolName string, repo string) ([]CallEdge, error) {
	return c.graphEdges("/api/graph/methods", map[string]any{"name": symbolName, "repo": repo, "limit": 15})
}

// CrossWireCallees returns 1-hop "wire transitions" — route handlers reachable
// from `symbolName` across an HTTP/RPC boundary.
//
// On the MIT schema, route nodes link to their handler via reference/call
// edges (route -> handler). The sidecar joins them and returns matching
// route/handler pairs. The caller->route ("FETCHES") side is best-effort:
// it depends on the engine's framework route extraction.
//
// CallEdge.From = caller/seed name; CallEdge.To = handler name;
// CallEdge.FilePath = handler file (so the dashboard can place it in the
// same column band as a regular callee).
func (c *Client) CrossWireCallees(symbolName string, repo string) ([]CallEdge, error) {
	return c.graphEdges("/api/graph/cross-wire", map[string]any{"name": symbolName, "repo": repo, "direction": "callees", "limit": 15})
}

// CrossWireCallers is the upstream mirror of CrossWireCallees — for a handler
// symbol, recover the route binding / callers that hit the route it serves.
func (c *Client) CrossWireCallers(symbolName string, repo string) ([]CallEdge, error) {
	return c.graphEdges("/api/graph/cross-wire", map[string]any{"name": symbolName, "repo": repo, "direction": "callers", "limit": 15})
}

// Siblings finds other files in the same directory as the given file.
// Reveals related files (BO, config, click handler) in the same package.
func (c *Client) Siblings(filePath string, repo string) ([]string, error) {
	parts := strings.Split(filePath, "/")
	if len(parts) < 2 {
		return nil, nil
	}
	return c.graphPaths("/api/graph/siblings", map[string]any{"filePath": filePath, "repo": repo, "limit": 20})
}

// ---------------------------------------------------------------------------
// Subgraph aggregator — snapshot of the codegraph neighbourhood around a
// set of seed symbols. Used by Router.SaveFlowMemory to freeze the
// codegraph view at the moment an agent commits a flow memory, so the
// dashboard can render the call-chain graph long after the trace TTL
// has expired.
// ---------------------------------------------------------------------------

// SubgraphNode is one function/symbol vertex in the captured neighbourhood.
// Role separates seeds (the agent-supplied entry points) from the
// callers/callees/related symbols that codegraph pulled in around them —
// the dashboard uses Role to colour and column-place nodes.
//
// Depth is the BFS distance from the nearest seed along the CALLS
// relation: 0 = seed itself, positive N = N hops downstream (callee
// chain), negative N = N hops upstream (caller chain). Aux roles
// (importer / extends / method) are always at Depth=1 since they're
// structural neighbours rather than part of the call chain.
type SubgraphNode struct {
	Name     string `json:"name"`
	FilePath string `json:"file,omitempty"`
	Role     string `json:"role"`  // "seed" | "caller" | "callee" | "importer" | "extends" | "method"
	Depth    int    `json:"depth"` // 0 for seed, +N for callees, -N for callers, 1 for aux
}

// SubgraphEdge is one directed relationship between two SubgraphNode names.
// Type matches the relation the edge was derived from ("CALLS", plus
// "IMPORTS" / "EXTENDS" / "HAS_METHOD" for the supplementary structural
// relations, plus "INVOKES" for synthetic wire-cross edges that fold a
// route -> handler hop into a single client-to-handler edge).
type SubgraphEdge struct {
	From string `json:"from"`
	To   string `json:"to"`
	Type string `json:"type"`
}

// Subgraph is the captured neighbourhood payload. Sized so it round-trips
// through Redis comfortably (the dashboard caps cards at ~50 nodes anyway,
// and Subgraph itself enforces a hard ceiling so a pathological seed like
// "init" with thousands of callers doesn't blow up the FlowMemory hash).
type Subgraph struct {
	Seeds []string       `json:"seeds"`
	Nodes []SubgraphNode `json:"nodes"`
	Edges []SubgraphEdge `json:"edges"`
	// Truncated is true when at least one seed had more callers/callees
	// than the per-relation LIMIT in the underlying query (currently 15).
	Truncated bool `json:"truncated,omitempty"`
}

// SubgraphCaps bounds the aggregate output so a single flow can never
// blow up the FlowMemory hash. With deep BFS the explosion can be
// dramatic (each level is up-to ~15× larger than the previous), so we
// rely on three independent backstops: node ceiling, edge ceiling,
// and per-level frontier cap. Whichever trips first stops the walk
// and sets Truncated=true so the dashboard can warn the user.
const (
	subgraphMaxSeeds    = 8    // first N entry_points used as seeds; rest ignored
	subgraphMaxNodes    = 500  // hard ceiling on total distinct nodes
	subgraphMaxEdges    = 1000 // hard ceiling on total distinct edges
	subgraphMaxDepth    = 5    // BFS hops per direction (callers up / callees down)
	subgraphMaxFrontier = 40   // per-level frontier cap — bounds BFS fan-out
)

// Subgraph snapshots the codegraph neighbourhood around the given seed
// symbol names using a bounded BFS in both directions along the CALLS
// relation. It returns *only* what codegraph already knows — no
// inference, no LLM, no embeddings — so the dashboard can render the
// exact set of relationships the agent saw at dev_context time.
//
// For each seed:
//   - Walks upstream CALLS up to subgraphMaxDepth hops    → role="caller", Depth<0
//   - Walks downstream CALLS up to subgraphMaxDepth hops  → role="callee", Depth>0
//   - 1-hop IMPORTS importers                              → role="importer", Depth=1
//   - 1-hop EXTENDS extends                                → role="extends",  Depth=1
//   - 1-hop HAS_METHOD methods                             → role="method",   Depth=1
//
// IMPORTS/EXTENDS/HAS_METHOD stay at 1-hop because they're structural
// relations rather than part of the call chain — walking them deeper
// adds noise without illuminating execution flow.
//
// Qualified seed names ("TaskInstance.run", "health.ServeRequest") are
// transparently handled by also querying the bare tail after the last
// dot ("run", "ServeRequest"). Codegraph indexes symbols by their
// declared identifier alone, so without this fallback agent-supplied
// qualified names would silently return empty subgraphs.
//
// Empty seeds, missing repo, or codegraph errors all return (nil, err).
// The caller (SaveFlowMemory) treats this as non-fatal: a flow without
// a snapshotted subgraph still saves successfully, the dashboard just
// falls back to the existing bipartite SVG.
func (c *Client) Subgraph(repo string, seeds []string) (*Subgraph, error) {
	if c == nil || c.BaseURL == "" {
		return nil, fmt.Errorf("subgraph: codegraph client not configured")
	}
	cleaned := make([]string, 0, len(seeds))
	seenSeed := make(map[string]struct{}, len(seeds))
	for _, s := range seeds {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if _, dup := seenSeed[s]; dup {
			continue
		}
		seenSeed[s] = struct{}{}
		cleaned = append(cleaned, s)
		if len(cleaned) >= subgraphMaxSeeds {
			break
		}
	}
	if len(cleaned) == 0 {
		return nil, fmt.Errorf("subgraph: no usable seed symbols")
	}

	sg := &Subgraph{Seeds: cleaned}
	nodes := make(map[string]SubgraphNode, 64)
	edges := make(map[string]SubgraphEdge, 64)

	// upsertNode keeps first-write semantics: the first depth/role we
	// see for a name wins. BFS guarantees we visit closer nodes first,
	// so "first" naturally means "closest to seed", which is what the
	// UI's depth slider needs to filter on. Returns true when the node
	// was added (used by BFS to decide whether to recurse).
	upsertNode := func(name, file, role string, depth int) bool {
		if name == "" {
			return false
		}
		if _, ok := nodes[name]; ok {
			return false
		}
		if len(nodes) >= subgraphMaxNodes {
			sg.Truncated = true
			return false
		}
		nodes[name] = SubgraphNode{Name: name, FilePath: file, Role: role, Depth: depth}
		return true
	}
	addEdge := func(from, to, etype string) {
		if from == "" || to == "" || from == to {
			return
		}
		key := etype + "|" + from + "|" + to
		if _, ok := edges[key]; ok {
			return
		}
		if len(edges) >= subgraphMaxEdges {
			sg.Truncated = true
			return
		}
		edges[key] = SubgraphEdge{From: from, To: to, Type: etype}
	}

	// expandCalleesBFS walks downstream from the seed. Each level
	// queries CalleesWithPath for every frontier node, records the
	// new callees at depth d, and uses the *newly-added* set as the
	// next frontier. Capping the frontier keeps the BFS bounded even
	// on dense graphs (init/main-style functions).
	//
	// Wire-cross step: at each frontier node we also probe the route ->
	// handler join via CrossWireCallees so the BFS jumps across the
	// HTTP boundary. The handler symbol enters the next frontier as
	// a "callee", which keeps the dashboard's column placement
	// intact (callees stay on the right) while letting the BFS
	// continue walking CALLS into the handler's downstream graph.
	expandCalleesBFS := func(seedName string) {
		frontier := []string{seedName}
		for d := 1; d <= subgraphMaxDepth && len(frontier) > 0; d++ {
			next := make([]string, 0, len(frontier)*4)
			for _, name := range frontier {
				callees, err := c.CalleesWithPath(name, repo)
				if err == nil {
					if len(callees) >= 15 {
						sg.Truncated = true
					}
					for _, e := range callees {
						if upsertNode(e.To, e.FilePath, "callee", d) {
							next = append(next, e.To)
						}
						addEdge(e.From, e.To, "CALLS")
					}
					if len(nodes) >= subgraphMaxNodes {
						return
					}
				}
				// Wire-cross — pull in HTTP/RPC handlers reached from
				// this node and seed them into the next frontier so
				// CALLS BFS continues on the handler side.
				if wires, err := c.CrossWireCallees(name, repo); err == nil {
					for _, e := range wires {
						if upsertNode(e.To, e.FilePath, "callee", d) {
							next = append(next, e.To)
						}
						addEdge(e.From, e.To, "INVOKES")
					}
					if len(nodes) >= subgraphMaxNodes {
						return
					}
				}
			}
			if len(next) > subgraphMaxFrontier {
				// Deterministic trim — sort and take the first N so
				// re-snapshots produce identical subgraphs across
				// runs. Without sort, map-iteration order would make
				// the truncation non-deterministic.
				sort.Strings(next)
				next = next[:subgraphMaxFrontier]
				sg.Truncated = true
			}
			frontier = next
		}
	}
	// expandCallersBFS is the upstream mirror — same algorithm walking
	// CallersWithPath. Negative depth so the UI can place callers in
	// columns to the left of the seed.
	expandCallersBFS := func(seedName string) {
		frontier := []string{seedName}
		for d := 1; d <= subgraphMaxDepth && len(frontier) > 0; d++ {
			next := make([]string, 0, len(frontier)*4)
			for _, name := range frontier {
				callers, err := c.CallersWithPath(name, repo)
				if err == nil {
					if len(callers) >= 15 {
						sg.Truncated = true
					}
					for _, e := range callers {
						if upsertNode(e.From, e.FilePath, "caller", -d) {
							next = append(next, e.From)
						}
						addEdge(e.From, e.To, "CALLS")
					}
					if len(nodes) >= subgraphMaxNodes {
						return
					}
				}
				if wires, err := c.CrossWireCallers(name, repo); err == nil {
					for _, e := range wires {
						if upsertNode(e.From, e.FilePath, "caller", -d) {
							next = append(next, e.From)
						}
						addEdge(e.From, e.To, "INVOKES")
					}
					if len(nodes) >= subgraphMaxNodes {
						return
					}
				}
			}
			if len(next) > subgraphMaxFrontier {
				sort.Strings(next)
				next = next[:subgraphMaxFrontier]
				sg.Truncated = true
			}
			frontier = next
		}
	}
	// addAux pulls the 1-hop structural relations for a seed. These
	// stay shallow because deeper IMPORTS/EXTENDS/HAS_METHOD walks
	// drift into structural noise unrelated to the call chain.
	addAux := func(seedName string) {
		if importers, err := c.Importers(seedName, repo); err == nil {
			for _, e := range importers {
				upsertNode(e.From, e.FilePath, "importer", 1)
				addEdge(e.From, seedName, "IMPORTS")
			}
		}
		if extends, err := c.Extends(seedName, repo); err == nil {
			for _, e := range extends {
				upsertNode(e.From, e.FilePath, "extends", 1)
				addEdge(e.From, seedName, "EXTENDS")
			}
		}
		if methods, err := c.Methods(seedName, repo); err == nil {
			for _, e := range methods {
				upsertNode(e.To, e.FilePath, "method", 1)
				addEdge(e.From, e.To, "HAS_METHOD")
			}
		}
	}

	// snapshotSeed runs the full per-seed pipeline (callers BFS +
	// callees BFS + aux). Returns the number of nodes added so the
	// qualified-name fallback below can detect "this seed name
	// matched nothing in codegraph".
	snapshotSeed := func(seedName string) int {
		before := len(nodes)
		expandCalleesBFS(seedName)
		expandCallersBFS(seedName)
		addAux(seedName)
		return len(nodes) - before
	}

	for _, seed := range cleaned {
		upsertNode(seed, "", "seed", 0)
		added := snapshotSeed(seed)
		if added > 0 {
			continue
		}
		// Qualified-name fallback: try the bare tail after the last
		// dot ("health.ServeRequest" → "ServeRequest").
		tail := bareTail(seed)
		if tail == "" || tail == seed {
			continue
		}
		if isCommonShortSymbol(tail) {
			continue
		}
		if hits, err := c.NameHitCount(tail, repo); err == nil && hits > bareTailRarityCeiling {
			continue
		}
		upsertNode(tail, "", "seed", 0)
		snapshotSeed(tail)
	}

	// Stable output order — sort nodes by (role rank, name) and edges
	// by (type, from, to). Deterministic JSON output makes the round-trip
	// through Redis byte-stable, which matters for idempotent re-saves.
	sg.Nodes = make([]SubgraphNode, 0, len(nodes))
	for _, n := range nodes {
		sg.Nodes = append(sg.Nodes, n)
	}
	sort.Slice(sg.Nodes, func(i, j int) bool {
		ri, rj := roleRank(sg.Nodes[i].Role), roleRank(sg.Nodes[j].Role)
		if ri != rj {
			return ri < rj
		}
		return sg.Nodes[i].Name < sg.Nodes[j].Name
	})
	sg.Edges = make([]SubgraphEdge, 0, len(edges))
	for _, e := range edges {
		sg.Edges = append(sg.Edges, e)
	}
	sort.Slice(sg.Edges, func(i, j int) bool {
		if sg.Edges[i].Type != sg.Edges[j].Type {
			return sg.Edges[i].Type < sg.Edges[j].Type
		}
		if sg.Edges[i].From != sg.Edges[j].From {
			return sg.Edges[i].From < sg.Edges[j].From
		}
		return sg.Edges[i].To < sg.Edges[j].To
	})
	return sg, nil
}

// bareTailRarityCeiling is the maximum NameHitCount(tail) we'll tolerate
// before suppressing the qualified-name fallback. Real domain symbols rarely
// have more than ~30 namesakes in a repo, while generic verbs like Init/Run
// routinely hit 200–1000.
const bareTailRarityCeiling = 30

// commonShortSymbols are method names so generic that any bare-tail
// fallback to them would always swamp the snapshot.
var commonShortSymbols = map[string]bool{
	"init":  true,
	"new":   true,
	"run":   true,
	"start": true,
	"stop":  true,
	"close": true,
	"open":  true,
	"read":  true,
	"write": true,
	"get":   true,
	"set":   true,
	"add":   true,
	"do":    true,
	"main":  true,
}

func isCommonShortSymbol(tail string) bool {
	return commonShortSymbols[strings.ToLower(tail)]
}

// bareTail returns the substring after the last "." in name, or "" if
// name has no dot or the tail is too short to be a useful symbol name.
func bareTail(name string) string {
	i := strings.LastIndex(name, ".")
	if i < 0 || i == len(name)-1 {
		return ""
	}
	tail := name[i+1:]
	if len(tail) < 3 {
		return ""
	}
	return tail
}

// roleRank orders node roles so seeds always render first and ancillary
// roles trail. The dashboard relies on this ordering to position columns.
func roleRank(role string) int {
	switch role {
	case "seed":
		return 0
	case "caller":
		return 1
	case "callee":
		return 2
	case "method":
		return 3
	case "extends":
		return 4
	case "importer":
		return 5
	default:
		return 9
	}
}

// RelatedFiles finds all files whose path contains the keyword.
func (c *Client) RelatedFiles(keyword string, repo string, limit int) ([]string, error) {
	if limit <= 0 {
		limit = 100
	}
	return c.graphPaths("/api/graph/related-files", map[string]any{"keyword": strings.ToLower(keyword), "repo": repo, "limit": limit})
}

// ---------------------------------------------------------------------------
// GET /api/file — read source file content
// ---------------------------------------------------------------------------

type FileResult struct {
	Content    string `json:"content"`
	TotalLines int    `json:"totalLines"`
}

// ReadFile fetches a source file's content from the indexed repo.
func (c *Client) ReadFile(filePath string, repo string) (*FileResult, error) {
	u := fmt.Sprintf("%s/api/file?path=%s", c.BaseURL, url.QueryEscape(filePath))
	if repo != "" {
		u += "&repo=" + url.QueryEscape(repo)
	}
	resp, err := c.getURL("file", u)
	if err != nil {
		return nil, fmt.Errorf("codegraph file: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("codegraph file %d: %s", resp.StatusCode, raw)
	}

	var fr FileResult
	if err := json.NewDecoder(resp.Body).Decode(&fr); err != nil {
		return nil, fmt.Errorf("codegraph file decode: %w", err)
	}
	return &fr, nil
}

// TopLevelDirs returns the distinct first-segment directory names
// that appear under the repo root (e.g. "oscar", "weaver", "lib").
//
// We resolve the repo's filesystem path via /api/repos and read the
// directory directly. Used by the router to gate service-aware anchor
// injection: only query tokens that match a known top-level dir are
// treated as service tokens. The result is small and stable, so callers
// should cache.
func (c *Client) TopLevelDirs(repo string) ([]string, error) {
	repoPath := c.RepoPath(repo)
	if repoPath == "" {
		return nil, fmt.Errorf("codegraph: repo %q not found", repo)
	}
	entries, err := os.ReadDir(repoPath)
	if err != nil {
		return nil, fmt.Errorf("codegraph TopLevelDirs readdir %s: %w", repoPath, err)
	}
	dirs := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		// Skip dot-dirs (.git, .vscode, etc.) and node_modules — they
		// never host service entry points.
		if strings.HasPrefix(name, ".") || name == "node_modules" {
			continue
		}
		dirs = append(dirs, name)
	}
	return dirs, nil
}

// ---------------------------------------------------------------------------
// GET /api/repos — list indexed repos
// ---------------------------------------------------------------------------

type RepoInfo struct {
	Name      string `json:"name"`
	Path      string `json:"path"`
	IndexedAt string `json:"indexedAt"`
}

// RepoPath resolves a repo name to its filesystem path via the codegraph API.
// Returns "" if the repo is not found.
func (c *Client) RepoPath(repoName string) string {
	repos, err := c.ListRepos()
	if err != nil {
		return ""
	}
	for _, r := range repos {
		if r.Name == repoName {
			return r.Path
		}
	}
	return ""
}

// ListRepos returns all indexed repositories.
func (c *Client) ListRepos() ([]RepoInfo, error) {
	resp, err := c.getURL("repos", c.BaseURL+"/api/repos")
	if err != nil {
		return nil, fmt.Errorf("codegraph repos: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("codegraph repos %d: %s", resp.StatusCode, raw)
	}

	var repos []RepoInfo
	if err := json.NewDecoder(resp.Body).Decode(&repos); err != nil {
		return nil, fmt.Errorf("codegraph repos decode: %w", err)
	}
	return repos, nil
}

// ---------------------------------------------------------------------------
// File-path-aware search
// ---------------------------------------------------------------------------

// SearchByFilePath finds all symbols defined in a file matching the given
// path. The sidecar tries exact match, then suffix, then CONTAINS.
func (c *Client) SearchByFilePath(filePath string, repo string, limit int) ([]SearchResult, error) {
	if limit <= 0 {
		limit = 20
	}
	var sr searchResponse
	if err := c.postDecode("search", "/api/search-by-path", map[string]any{
		"filePath": filePath, "repo": repo, "limit": limit,
	}, &sr); err != nil {
		return nil, err
	}
	if sr.Error != "" {
		return nil, fmt.Errorf("codegraph search-by-path: %s", sr.Error)
	}
	return sr.Results, nil
}

// ExtractFilePath detects if a query contains an explicit file path (e.g.
// "kosmos/matchengine/rule.go") and returns it. Returns "" if none found.
func ExtractFilePath(query string) string {
	for _, word := range strings.Fields(query) {
		word = strings.Trim(word, ".,;:!?\"'()[]{}")
		if strings.Contains(word, "/") && strings.Contains(word, ".") {
			return word
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// Name-based symbol search
// ---------------------------------------------------------------------------

// SearchOpts is an optional bundle of plan-driven retrieval signals for
// SearchByNameWithOpts. All fields are advisory; an empty SearchOpts
// produces identical behaviour to the legacy SearchByName.
type SearchOpts struct {
	// MustTerms acts as a file-level filter. Results are kept only if their
	// file appears in the union of files where ANY must term hits
	// name/path/signature. Empty = no filter.
	MustTerms []string
	// ExcludeTerms drop matches via convention-based rules (test/mock):
	// filePath ending "_<term>.go" or containing "/<term>/" is dropped;
	// symbol name with title-case prefix "<Term>" is dropped.
	ExcludeTerms []string
	// ContextHints multiply the score of results whose filePath contains
	// any hint (case-insensitive). NEVER a hard filter.
	ContextHints []string
}

// SearchByName preserves the legacy zero-config behaviour. New callers
// should prefer SearchByNameWithOpts.
func (c *Client) SearchByName(query string, repo string, limit int) ([]SearchResult, error) {
	return c.SearchByNameWithOpts(query, repo, limit, SearchOpts{})
}

// SearchByNameWithOpts finds functions/methods/classes matching query words,
// with optional plan-driven filtering and boosting. The tokenization,
// per-term IDF * surface scoring, exclude rules, must-term file filter and
// context-hint boost all run server-side in the sidecar (which has direct
// SQL access); this method just forwards the request.
func (c *Client) SearchByNameWithOpts(query string, repo string, limit int, opts SearchOpts) ([]SearchResult, error) {
	if limit <= 0 {
		limit = 10
	}
	if len(SplitQueryWords(query)) == 0 {
		return nil, nil
	}
	body := map[string]any{
		"query": query,
		"repo":  repo,
		"limit": limit,
	}
	if len(opts.MustTerms) > 0 {
		body["mustTerms"] = opts.MustTerms
	}
	if len(opts.ExcludeTerms) > 0 {
		body["excludeTerms"] = opts.ExcludeTerms
	}
	if len(opts.ContextHints) > 0 {
		body["contextHints"] = opts.ContextHints
	}
	var sr searchResponse
	if err := c.postDecode("search", "/api/search-by-name", body, &sr); err != nil {
		return nil, err
	}
	if sr.Error != "" {
		return nil, fmt.Errorf("codegraph search-by-name: %s", sr.Error)
	}
	return sr.Results, nil
}

// NameHitCount returns the number of nodes whose name contains term in the
// given repo. Used by the router for cheap rarity estimation when
// auto-promoting a query token to a must-anchor.
func (c *Client) NameHitCount(term, repo string) (int, error) {
	if term == "" {
		return 0, nil
	}
	var nr struct {
		Count int    `json:"count"`
		Error string `json:"error,omitempty"`
	}
	if err := c.postDecode("graph", "/api/graph/name-hits", map[string]any{
		"term": strings.ToLower(term), "repo": repo,
	}, &nr); err != nil {
		return 0, err
	}
	if nr.Error != "" {
		return 0, fmt.Errorf("codegraph name-hits: %s", nr.Error)
	}
	return nr.Count, nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// SymbolNames extracts deduplicated symbol names from search results.
func SymbolNames(results []SearchResult) []string {
	seen := make(map[string]bool)
	names := make([]string, 0, len(results))
	for _, r := range results {
		name := r.Name
		if name == "" {
			name = r.ID
		}
		if name != "" && !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	return names
}

// FilePaths extracts deduplicated file paths from search results.
func FilePaths(results []SearchResult) []string {
	seen := make(map[string]bool)
	var paths []string
	for _, r := range results {
		if r.FilePath != "" && !seen[r.FilePath] {
			seen[r.FilePath] = true
			paths = append(paths, r.FilePath)
		}
	}
	return paths
}

// ToSnippets converts search results to prompt snippets, deduplicated
// by file path so a single file with N matching symbols doesn't
// monopolise the snippet budget at the expense of other on-topic files.
func ToSnippets(results []SearchResult, max int) []prompt.Snippet {
	snippets := make([]prompt.Snippet, 0, max)
	seenFiles := make(map[string]bool, max)
	for _, r := range results {
		if len(snippets) >= max {
			break
		}
		// Path-dedup: keep the first (best-ranked) match per file.
		if r.FilePath != "" {
			if seenFiles[r.FilePath] {
				continue
			}
			seenFiles[r.FilePath] = true
		}
		lines := ""
		if r.StartLine > 0 {
			lines = fmt.Sprintf("%d-%d", r.StartLine, r.EndLine)
		}
		// Prefer Source (the [startLine,endLine] slice the sidecar returns
		// when include_source is set) over Content (empty for most hits).
		content := r.Source
		if content == "" {
			content = r.Content
		}
		if len(content) > 2000 {
			content = content[:2000] + "\n... (truncated)"
		}
		snippets = append(snippets, prompt.Snippet{
			File:    r.FilePath,
			Lines:   lines,
			Content: content,
		})
	}
	return snippets
}

// stopWords are query words with no retrieval value. Kept small on purpose
// so we don't accidentally drop short domain abbreviations like "fms" or "rs4c".
var stopWords = map[string]bool{
	"the": true, "a": true, "an": true, "and": true, "or": true, "but": true,
	"is": true, "in": true, "on": true, "at": true, "to": true, "for": true,
	"of": true, "with": true, "by": true, "from": true, "as": true, "into": true,
	"how": true, "what": true, "why": true, "where": true, "when": true, "which": true,
	"does": true, "do": true, "did": true, "can": true, "could": true, "should": true,
	"would": true, "will": true, "this": true, "that": true, "these": true, "those": true,
	"be": true, "been": true, "being": true, "have": true, "has": true, "had": true,
	"it": true, "its": true, "i": true, "we": true, "you": true, "me": true, "my": true,
	"about": true, "any": true, "all": true,
}

// SplitQueryWords tokenizes a query into normalized search terms.
// It lowercases, strips stop words, dedups, and applies light stemming
// so morphological variants like "unmarshalling" surface symbols named
// "Unmarshal". Single-character tokens are dropped; everything else is
// preserved so short domain abbreviations (fms, rs4c, kbb) survive.
func SplitQueryWords(q string) []string {
	seen := make(map[string]bool)
	var words []string
	for _, raw := range strings.Fields(q) {
		raw = strings.Trim(raw, ".,;:!?\"'()[]{}")
		if len(raw) < 2 {
			continue
		}
		lower := strings.ToLower(raw)
		if stopWords[lower] {
			continue
		}
		norm := stemTerm(lower)
		if seen[norm] {
			continue
		}
		seen[norm] = true
		words = append(words, norm)
	}
	return words
}

// stemTerm strips a few common English suffixes for tokens long enough that
// over-stemming is unlikely. Also collapses British "doubled" stems like
// "unmarshalling" -> "unmarshall" -> "unmarshal" so query terms align with
// canonical Go identifier spelling.
func stemTerm(w string) string {
	if len(w) <= 6 {
		return w
	}
	for _, suf := range []string{"ings", "ing", "ers", "ed", "es", "s"} {
		if !strings.HasSuffix(w, suf) {
			continue
		}
		stripped := w[:len(w)-len(suf)]
		if len(stripped) >= 6 && strings.HasSuffix(stripped, "ll") {
			stripped = stripped[:len(stripped)-1]
		}
		if len(stripped) >= 4 {
			return stripped
		}
	}
	return w
}

// splitQueryWords is the legacy internal alias, kept to minimise churn in
// callers. Prefer SplitQueryWords going forward.
func splitQueryWords(q string) []string { return SplitQueryWords(q) }
