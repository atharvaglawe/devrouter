// Package codegraph is the Go HTTP client for the in-tree codegraph service
// (the slim graph-engine vendored at /codegraph). The package was renamed
// from "gitnexus" when the upstream fork was trimmed for devrouter; the
// HTTP wire format and endpoints did not change.
package codegraph

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/atharva-ag/devrouter/internal/prompt"
)

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
// POST /api/search — hybrid / BM25 / semantic search
// ---------------------------------------------------------------------------

// SearchResult mirrors the enriched shape returned by codegraph /api/search.
type SearchResult struct {
	NodeID   string  `json:"nodeId"`
	ID       string  `json:"id"`
	Name     string  `json:"name"`
	FilePath string  `json:"filePath"`
	Label    string  `json:"label"`
	Content  string  `json:"content"`
	// Source is populated when the search request sets
	// `include_source: true`. For symbol-shaped labels it's the
	// [startLine,endLine] slice; for file-level hits it's the file
	// head capped at 64 KB. Always empty otherwise.
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
// Mode picks how BM25 (lexical) and vector (semantic) signals are
// combined. Per-intent benchmarking on goserving showed each side has
// strengths the other can't match:
//
//   - BM25-heavy (hybrid / bm25): wins on identifier-anchored intents
//     (debug, refactor, explore) where exact symbol/word match
//     dominates. R@5 +0.10 to +0.33 vs vector-heavy.
//   - Vector-heavy (semantic): wins on paraphrase intents (general,
//     trace) where the query rephrases concepts the file expresses
//     differently. R@5 +0.23 to +0.50 vs BM25-heavy.
//
// SearchModeAuto is the router default — caller passes intent and
// the client picks. Direct callers can override via SearchWithMode.
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

// SearchWithMode is the mode-explicit variant of Search. The router's
// intent-classification stage uses this to route paraphrase-heavy
// intents through "semantic" and identifier-heavy intents through
// "hybrid" / "bm25".
//
// An empty mode string falls back to "hybrid" — same behaviour as
// the legacy Search method, kept so callers that don't have an intent
// don't have to special-case the mode string themselves.
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
		"query":  query,
		"limit":  limit,
		"mode":   mode,
		"enrich": true,
		// Inline the matched source so downstream callers (router →
		// ToSnippets → DevPrompt) get function bodies in one round
		// trip. The codegraph API returns symbol slices for
		// Function/Method/Class/Interface and file heads (capped at
		// 64 KB) for file-level FTS hits — always cheap and bounded.
		"include_source": true,
	}
	if repo != "" {
		body["repo"] = repo
	}

	payload, _ := json.Marshal(body)
	resp, err := c.httpClient.Post(c.BaseURL+"/api/search", "application/json", bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("codegraph search: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("codegraph search %d: %s", resp.StatusCode, raw)
	}

	var sr searchResponse
	if err := json.NewDecoder(resp.Body).Decode(&sr); err != nil {
		return nil, fmt.Errorf("codegraph search decode: %w", err)
	}
	if sr.Error != "" {
		return nil, fmt.Errorf("codegraph search: %s", sr.Error)
	}
	return sr.Results, nil
}

// SearchModeForIntent maps a query intent (as classified by the
// router) to the codegraph search mode that benchmarks best for it.
//
// Calibrated on the goserving 30-question bench (results dated
// 2026-05-14). When a new intent is added the router falls back to
// hybrid, which is the safest default — never the worst, never the
// best.
//
//	intent     | best mode  | bench R@5 vs hybrid
//	-----------|------------|---------------------
//	debug      | hybrid     | tied
//	explore    | hybrid     | tied
//	general    | semantic   | +0.500 (was 0.200, now ≈0.700)
//	refactor   | bm25       | +0.000 to +0.05 (already 1.000 on hybrid)
//	trace      | semantic   | +0.229 (was 0.521, now ≈0.750)
func SearchModeForIntent(intent string) string {
	switch intent {
	case "general", "trace":
		return SearchModeSemantic
	case "refactor":
		return SearchModeBM25
	case "debug", "explore":
		return SearchModeHybrid
	default:
		return SearchModeHybrid
	}
}

// ---------------------------------------------------------------------------
// POST /api/query — raw Cypher for impact / callers / callees
// ---------------------------------------------------------------------------

type cypherResponse struct {
	Result []map[string]any `json:"result"`
	Error  string           `json:"error,omitempty"`
}

// Cypher executes a raw Cypher query against the repo's LadybugDB graph.
func (c *Client) Cypher(cypher string, repo string) ([]map[string]any, error) {
	body := map[string]any{"cypher": cypher}
	if repo != "" {
		body["repo"] = repo
	}
	payload, _ := json.Marshal(body)
	resp, err := c.httpClient.Post(c.BaseURL+"/api/query", "application/json", bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("codegraph cypher: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("codegraph cypher %d: %s", resp.StatusCode, raw)
	}

	var cr cypherResponse
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
		return nil, fmt.Errorf("codegraph cypher decode: %w", err)
	}
	if cr.Error != "" {
		return nil, fmt.Errorf("codegraph cypher: %s", cr.Error)
	}
	return cr.Result, nil
}

// Callers returns d=1 upstream callers of a symbol by name using Cypher.
func (c *Client) Callers(symbolName string, repo string) ([]string, error) {
	cypher := fmt.Sprintf(
		`MATCH (caller)-[r:CodeRelation {type: "CALLS"}]->(target)
		 WHERE target.name = "%s"
		 RETURN DISTINCT caller.name AS name
		 LIMIT 20`,
		escapeCypher(symbolName),
	)
	rows, err := c.Cypher(cypher, repo)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, row := range rows {
		if n, ok := row["name"].(string); ok && n != "" {
			names = append(names, n)
		}
	}
	return names, nil
}

// Callees returns d=1 downstream symbols called by a symbol.
func (c *Client) Callees(symbolName string, repo string) ([]string, error) {
	cypher := fmt.Sprintf(
		`MATCH (source)-[r:CodeRelation {type: "CALLS"}]->(callee)
		 WHERE source.name = "%s"
		 RETURN DISTINCT callee.name AS name
		 LIMIT 20`,
		escapeCypher(symbolName),
	)
	rows, err := c.Cypher(cypher, repo)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, row := range rows {
		if n, ok := row["name"].(string); ok && n != "" {
			names = append(names, n)
		}
	}
	return names, nil
}

// CallEdge is a caller->callee relationship with file location.
type CallEdge struct {
	From     string
	To       string
	FilePath string
}

// CallersWithPath returns d=1 upstream callers with file paths.
func (c *Client) CallersWithPath(symbolName string, repo string) ([]CallEdge, error) {
	cypher := fmt.Sprintf(
		`MATCH (caller)-[:CodeRelation {type: "CALLS"}]->(target)
		 WHERE target.name = "%s"
		 RETURN DISTINCT caller.name AS from, target.name AS to, caller.filePath AS file
		 LIMIT 15`,
		escapeCypher(symbolName),
	)
	return c.queryEdges(cypher, repo)
}

// CalleesWithPath returns d=1 downstream callees with file paths.
func (c *Client) CalleesWithPath(symbolName string, repo string) ([]CallEdge, error) {
	cypher := fmt.Sprintf(
		`MATCH (source)-[:CodeRelation {type: "CALLS"}]->(callee)
		 WHERE source.name = "%s"
		 RETURN DISTINCT source.name AS from, callee.name AS to, callee.filePath AS file
		 LIMIT 15`,
		escapeCypher(symbolName),
	)
	return c.queryEdges(cypher, repo)
}

// UpstreamChain traces callers 2 hops deep: grandparent -> parent -> target.
func (c *Client) UpstreamChain(symbolName string, repo string) ([]CallEdge, error) {
	cypher := fmt.Sprintf(
		`MATCH (gp)-[:CodeRelation {type: "CALLS"}]->(parent)-[:CodeRelation {type: "CALLS"}]->(target)
		 WHERE target.name = "%s"
		 RETURN DISTINCT gp.name AS from, parent.name AS to, gp.filePath AS file
		 LIMIT 10`,
		escapeCypher(symbolName),
	)
	return c.queryEdges(cypher, repo)
}

func (c *Client) queryEdges(cypher, repo string) ([]CallEdge, error) {
	rows, err := c.Cypher(cypher, repo)
	if err != nil {
		return nil, err
	}
	var edges []CallEdge
	for _, row := range rows {
		e := CallEdge{}
		if v, ok := row["from"].(string); ok {
			e.From = v
		}
		if v, ok := row["to"].(string); ok {
			e.To = v
		}
		if v, ok := row["file"].(string); ok {
			e.FilePath = v
		}
		if e.From != "" && e.To != "" {
			edges = append(edges, e)
		}
	}
	return edges, nil
}

// ---------------------------------------------------------------------------
// Graph relationship queries: IMPORTS, EXTENDS, HAS_METHOD, CONTAINS
// ---------------------------------------------------------------------------

// Importers finds files that IMPORT packages containing the given symbol.
// This reveals factory/registry files that wire up a component.
func (c *Client) Importers(symbolName string, repo string) ([]CallEdge, error) {
	cypher := fmt.Sprintf(
		`MATCH (sym) WHERE sym.name = "%s"
		 WITH sym
		 MATCH (pkg)-[:CodeRelation {type: "DEFINES"}]->(sym)
		 WITH pkg
		 MATCH (importer)-[:CodeRelation {type: "IMPORTS"}]->(pkg)
		 WHERE importer.name <> pkg.name
		 RETURN DISTINCT importer.name AS from, pkg.name AS to, importer.filePath AS file
		 LIMIT 15`,
		escapeCypher(symbolName),
	)
	return c.queryEdges(cypher, repo)
}

// ImportersByPackage finds files that import packages whose name contains the query word.
// Broader than Importers — catches factory files that import the package even if
// the exact symbol name doesn't match the DEFINES relationship.
func (c *Client) ImportersByPackage(pkgWord string, repo string) ([]CallEdge, error) {
	cypher := fmt.Sprintf(
		`MATCH (importer)-[:CodeRelation {type: "IMPORTS"}]->(pkg)
		 WHERE toLower(pkg.name) CONTAINS "%s"
		 AND NOT toLower(importer.name) CONTAINS "%s"
		 RETURN DISTINCT importer.name AS from, pkg.name AS to, importer.filePath AS file
		 LIMIT 30`,
		escapeCypher(strings.ToLower(pkgWord)),
		escapeCypher(strings.ToLower(pkgWord)),
	)
	return c.queryEdges(cypher, repo)
}

// Extends finds EXTENDS relationships involving the symbol (both directions).
// Catches struct embedding and interface implementations.
func (c *Client) Extends(symbolName string, repo string) ([]CallEdge, error) {
	cypher := fmt.Sprintf(
		`MATCH (child)-[:CodeRelation {type: "EXTENDS"}]->(parent)
		 WHERE child.name = "%s" OR parent.name = "%s"
		 RETURN DISTINCT child.name AS from, parent.name AS to, child.filePath AS file
		 LIMIT 15`,
		escapeCypher(symbolName),
		escapeCypher(symbolName),
	)
	return c.queryEdges(cypher, repo)
}

// CrossWireCallees returns 1-hop "wire transitions" — outbound HTTP /
// RPC calls from `symbolName` that land on a known route handler.
//
// Codegraph emits two relations that, joined together, encode a
// service-to-service call through an HTTP API:
//
//	(caller:Function) --FETCHES--> (route:Route) <--HANDLES_ROUTE-- (handler:Function)
//
// Walking just CALLS will never cross a process boundary because
// every call chain dead-ends at the HTTP client invocation. Walking
// FETCHES + HANDLES_ROUTE recovers the wire-side transition: from
// `caller` (e.g. `EnforceRefresh` in weaver) to `handler` (e.g.
// `GetCandidatesController` in kosmos) with the route URL as the
// hop reason.
//
// We require the handler to be function-level (`startLine IS NOT
// NULL`) so the BFS continues into the handler's downstream callees
// rather than getting stuck at a File-level node. File-level
// HANDLES_ROUTE remains in the graph as a fallback for visualisation
// but is not promoted to a callee here.
//
// CallEdge.From = caller name; CallEdge.To = handler name;
// CallEdge.FilePath = handler file (so the dashboard can place it
// in the same column band as a regular callee). The route URL
// itself is dropped on the floor — callers that need it should use
// the underlying Cypher.
func (c *Client) CrossWireCallees(symbolName string, repo string) ([]CallEdge, error) {
	cypher := fmt.Sprintf(
		`MATCH (caller)-[:CodeRelation {type: "FETCHES"}]->(route)<-[:CodeRelation {type: "HANDLES_ROUTE"}]-(handler)
		 WHERE caller.name = "%s" AND handler.startLine IS NOT NULL
		 RETURN DISTINCT caller.name AS from, handler.name AS to, handler.filePath AS file
		 LIMIT 15`,
		escapeCypher(symbolName),
	)
	return c.queryEdges(cypher, repo)
}

// CrossWireCallers is the upstream mirror of CrossWireCallees — for a
// handler symbol, recover all function-level callers that fetch the
// route the handler serves. Used when the seed lands on the server
// side and we want to surface every client (across services) that
// hits this endpoint, the same way upstream CALLS BFS surfaces
// in-process callers.
func (c *Client) CrossWireCallers(symbolName string, repo string) ([]CallEdge, error) {
	cypher := fmt.Sprintf(
		`MATCH (caller)-[:CodeRelation {type: "FETCHES"}]->(route)<-[:CodeRelation {type: "HANDLES_ROUTE"}]-(handler)
		 WHERE handler.name = "%s" AND caller.startLine IS NOT NULL
		 RETURN DISTINCT caller.name AS from, handler.name AS to, caller.filePath AS file
		 LIMIT 15`,
		escapeCypher(symbolName),
	)
	return c.queryEdges(cypher, repo)
}

// Methods finds HAS_METHOD relationships for a struct/interface.
func (c *Client) Methods(symbolName string, repo string) ([]CallEdge, error) {
	cypher := fmt.Sprintf(
		`MATCH (s)-[:CodeRelation {type: "HAS_METHOD"}]->(m)
		 WHERE s.name = "%s"
		 RETURN DISTINCT s.name AS from, m.name AS to, m.filePath AS file
		 LIMIT 15`,
		escapeCypher(symbolName),
	)
	return c.queryEdges(cypher, repo)
}

// Siblings finds other files in the same directory as the matched symbol's file.
// Reveals related files (BO, config, click handler) in the same package.
func (c *Client) Siblings(filePath string, repo string) ([]string, error) {
	parts := strings.Split(filePath, "/")
	if len(parts) < 2 {
		return nil, nil
	}
	dir := strings.Join(parts[:len(parts)-1], "/")

	cypher := fmt.Sprintf(
		`MATCH (folder)-[:CodeRelation {type: "CONTAINS"}]->(child)
		 WHERE folder.filePath = "%s"
		 RETURN DISTINCT child.name AS name, child.filePath AS path
		 LIMIT 20`,
		escapeCypher(dir),
	)
	rows, err := c.Cypher(cypher, repo)
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, row := range rows {
		if p, ok := row["path"].(string); ok && p != "" {
			paths = append(paths, p)
		} else if n, ok := row["name"].(string); ok && n != "" {
			paths = append(paths, dir+"/"+n)
		}
	}
	return paths, nil
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
// Type matches the codegraph CodeRelation.type the edge was derived from
// (mostly "CALLS", with "IMPORTS" / "EXTENDS" / "HAS_METHOD" for the
// supplementary structural relations, plus "INVOKES" for synthetic
// wire-cross edges that fold a (FETCHES → Route ← HANDLES_ROUTE)
// path into a single client-to-handler hop).
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
	// than the per-relation LIMIT in the underlying cypher (currently 15).
	// Dashboard surfaces this as a "+N more" hint so users know they're
	// looking at a clipped view, not the full graph.
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
	// Wire-cross step: at each frontier node we also probe FETCHES +
	// HANDLES_ROUTE via CrossWireCallees so the BFS jumps across the
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
	//
	// Wire-cross step: when the seed is server-side (a handler), we
	// also surface every function-level client that hits the same
	// route via CrossWireCallers. The client enters the frontier as
	// a "caller" so upstream CALLS BFS continues from it.
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
		// dot ("health.ServeRequest" → "ServeRequest"). The tail is
		// also registered as a seed so it sits in the same band as
		// the qualified form in the UI.
		//
		// Reject common short tails (Init, Run, New, Get, Set, …) and
		// any tail that resolves to many symbols across the repo —
		// otherwise a single qualified seed that didn't resolve pulls
		// in the union of every Init() in the codebase, drowning the
		// flow graph in unrelated callers/callees.
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
	// through Redis byte-stable, which matters for idempotent re-saves
	// (no spurious "subgraph changed" diffs on the same source data).
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
// before suppressing the qualified-name fallback. The ceiling is a
// conservative anti-noise threshold: real domain symbols rarely have
// more than ~30 namesakes in a single repo, while generic verbs like
// Init/Run/New routinely hit 200–1000. Bumped high enough to not block
// legitimate "MyType.Process" → "Process" (where Process is a domain
// concept), low enough to block "X.Init" → "Init".
const bareTailRarityCeiling = 30

// commonShortSymbols are method names so generic that any bare-tail
// fallback to them would always swamp the snapshot. Hardcoded because
// they're cheaper than a Cypher round-trip and unambiguous in any Go
// codebase. Add to this list when you see a new one anchoring noise.
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
// The 3-char minimum filters out trivial false positives like ".do",
// ".go", ".js" that would otherwise match a huge fraction of symbols.
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
// roles trail. The dashboard relies on this ordering to position columns
// (callers on the left, seeds in the middle, callees on the right) by
// walking the sorted Nodes slice once.
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

// RelatedFiles finds all files/directories whose path contains the keyword.
// Returns deduplicated paths — both leaf files and their parent directories.
func (c *Client) RelatedFiles(keyword string, repo string, limit int) ([]string, error) {
	if limit <= 0 {
		limit = 100
	}
	cypher := fmt.Sprintf(
		`MATCH (n) WHERE toLower(n.filePath) CONTAINS "%s" AND n.filePath IS NOT NULL
		 RETURN DISTINCT n.filePath AS path
		 LIMIT %d`,
		escapeCypher(strings.ToLower(keyword)), limit,
	)
	rows, err := c.Cypher(cypher, repo)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool)
	var paths []string
	for _, row := range rows {
		if p, ok := row["path"].(string); ok && p != "" && !seen[p] {
			seen[p] = true
			paths = append(paths, p)
		}
	}
	return paths, nil
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
	resp, err := c.httpClient.Get(u)
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
// that appear under the repo root (e.g. "oscar", "weaver", "lib",
// "cmpkg" for a Go monorepo).
//
// We resolve the repo's filesystem path via /api/repos and then
// read the directory directly. Two reasons we don't use Cypher:
//   - LadybugDB (the embedded graph store) doesn't ship a SPLIT
//     function, so `split(n.filePath, "/")[0]` errors.
//   - Even with SPLIT available, returning DISTINCT first segments
//     from an 11k-file monorepo requires shipping all rows over
//     HTTP (the engine doesn't push the dedup down). One readdir
//     is O(top-level dir count), bounded at a few hundred.
//
// Used by the router to gate service-aware anchor injection: only
// query tokens that match a known top-level dir are treated as
// service tokens, which keeps the canonical-path probe fan-out
// small. The result is small (≤ ~100 entries even for huge
// monorepos) and stable across queries, so callers should cache.
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
	resp, err := c.httpClient.Get(c.BaseURL + "/api/repos")
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

// SearchByFilePath finds all symbols defined in a file matching the given path.
// Tries exact match first, then falls back to CONTAINS for partial paths
// (e.g. "kosmos/matchengine/rule.go" matches "cmpkg/kosmos/matchengine/rule.go").
func (c *Client) SearchByFilePath(filePath string, repo string, limit int) ([]SearchResult, error) {
	if limit <= 0 {
		limit = 20
	}

	// Try exact match first, then CONTAINS for partial paths
	for _, op := range []string{"=", "ENDS WITH"} {
		cypher := fmt.Sprintf(
			`MATCH (n) WHERE n.filePath %s "%s" AND n.startLine IS NOT NULL
			 RETURN n.id AS id, n.name AS name, n.filePath AS filePath,
			        n.startLine AS startLine, n.endLine AS endLine,
			        n.content AS content
			 ORDER BY n.startLine
			 LIMIT %d`,
			op, escapeCypher(filePath), limit,
		)
		rows, err := c.Cypher(cypher, repo)
		if err != nil {
			continue
		}
		if len(rows) > 0 {
			results := make([]SearchResult, 0, len(rows))
			for _, row := range rows {
				results = append(results, parseSearchRow(row))
			}
			return results, nil
		}
	}

	// Last resort: CONTAINS match for deeply partial paths
	cypher := fmt.Sprintf(
		`MATCH (n) WHERE n.filePath CONTAINS "%s" AND n.startLine IS NOT NULL
		 RETURN n.id AS id, n.name AS name, n.filePath AS filePath,
		        n.startLine AS startLine, n.endLine AS endLine,
		        n.content AS content
		 ORDER BY n.startLine
		 LIMIT %d`,
		escapeCypher(filePath), limit,
	)
	rows, err := c.Cypher(cypher, repo)
	if err != nil {
		return nil, err
	}
	results := make([]SearchResult, 0, len(rows))
	for _, row := range rows {
		results = append(results, parseSearchRow(row))
	}
	return results, nil
}

// ExtractFilePath detects if a query contains an explicit file path (e.g. "kosmos/matchengine/rule.go")
// and returns it. Returns empty string if no file path found.
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
// Cypher-based symbol search (fallback when FTS index is absent)
// ---------------------------------------------------------------------------

// SearchOpts is an optional bundle of plan-driven retrieval signals for
// SearchByNameWithOpts. All fields are advisory; an empty SearchOpts
// produces identical behaviour to the legacy SearchByName.
type SearchOpts struct {
	// MustTerms acts as a file-level filter. After candidate retrieval,
	// results are kept only if their file appears in the union of files
	// where ANY must term hits name/path/content. Empty = no filter.
	MustTerms []string
	// ExcludeTerms drop matches via convention-based rules (test/mock):
	// filePath ending "_<term>.go" or containing "/<term>/" is dropped;
	// symbol name with title-case prefix "<Term>" is dropped.
	ExcludeTerms []string
	// ContextHints multiply the score of results whose filePath contains
	// any hint (case-insensitive). Default boost = 2.0 per matching hint
	// (capped). NEVER a hard filter.
	ContextHints []string
}

// SearchByName preserves the legacy zero-config behaviour. New callers
// should prefer SearchByNameWithOpts.
func (c *Client) SearchByName(query string, repo string, limit int) ([]SearchResult, error) {
	return c.SearchByNameWithOpts(query, repo, limit, SearchOpts{})
}

// SearchByNameWithOpts finds functions/methods/classes matching query
// words via Cypher, with optional plan-driven filtering and boosting.
//
// Strategy:
//  1. For each normalized term, run "name CONTAINS" and record per-term
//     hit count.
//  2. If hits < limit, run "content CONTAINS" for the RAREST term as a
//     fallback surface (catches code where the token lives in the body).
//  3. Apply ExcludeTerms drops (test/mock convention rules).
//  4. Apply MustTerms file-level filter (lazily resolves the file set).
//  5. Score each surviving node with sum_t( idf(t) * surfaceWeight ).
//  6. Multiply score by context-hint boost where filePath matches.
//  7. Sort, take top N.
func (c *Client) SearchByNameWithOpts(query string, repo string, limit int, opts SearchOpts) ([]SearchResult, error) {
	if limit <= 0 {
		limit = 10
	}

	words := SplitQueryWords(query)
	if len(words) == 0 {
		return nil, nil
	}

	type nodeHit struct {
		result   SearchResult
		surfaces map[string]string // term -> best surface ("name" beats "content")
	}
	hits := make(map[string]*nodeHit)
	hitCounts := make(map[string]int)

	collect := func(rows []map[string]any, term, surface string) {
		for _, row := range rows {
			r := parseSearchRow(row)
			if r.Name == "" || r.FilePath == "" {
				continue
			}
			key := r.Name + "|" + r.FilePath
			h, ok := hits[key]
			if !ok {
				h = &nodeHit{result: r, surfaces: map[string]string{}}
				hits[key] = h
			}
			if cur, seen := h.surfaces[term]; !seen || surfaceWeight(surface) > surfaceWeight(cur) {
				h.surfaces[term] = surface
			}
			hitCounts[term]++
		}
	}

	// 1) name search per term
	for _, w := range words {
		cypher := fmt.Sprintf(
			`MATCH (n) WHERE toLower(n.name) CONTAINS "%s" AND n.startLine IS NOT NULL
			 RETURN n.id AS id, n.name AS name, n.filePath AS filePath,
			        n.startLine AS startLine, n.endLine AS endLine,
			        n.content AS content
			 LIMIT %d`,
			escapeCypher(w), limit*3,
		)
		rows, err := c.Cypher(cypher, repo)
		if err != nil {
			continue
		}
		collect(rows, w, "name")
	}

	// 2) content fallback for the rarest term when name results are weak
	if len(hits) < limit {
		if rarest := rarestSeenTerm(words, hitCounts); rarest != "" {
			cypher := fmt.Sprintf(
				`MATCH (n) WHERE toLower(n.content) CONTAINS "%s" AND n.startLine IS NOT NULL
				 RETURN n.id AS id, n.name AS name, n.filePath AS filePath,
				        n.startLine AS startLine, n.endLine AS endLine,
				        n.content AS content
				 LIMIT %d`,
				escapeCypher(rarest), limit*2,
			)
			if rows, err := c.Cypher(cypher, repo); err == nil {
				collect(rows, rarest, "content")
			}
		}
	}

	// 3) drop excluded matches (test/mock convention rules)
	for key, h := range hits {
		if shouldExclude(h.result, opts.ExcludeTerms) {
			delete(hits, key)
		}
	}

	// 4) must-terms file-level filter (lazily compute file set)
	if len(opts.MustTerms) > 0 {
		mustFiles := c.filesMatchingAnyTerm(opts.MustTerms, repo)
		if len(mustFiles) > 0 {
			for key, h := range hits {
				if !mustFiles[h.result.FilePath] {
					delete(hits, key)
				}
			}
		}
		// If mustFiles came back empty (e.g. cypher errors), fall through
		// rather than erase all results — the must-terms are a soft anchor,
		// not a hard fail.
	}

	// 5) score with IDF * surface weight, then context-hint boost
	type scored struct {
		result SearchResult
		score  float64
	}
	ranked := make([]scored, 0, len(hits))
	for _, h := range hits {
		var s float64
		for term, surface := range h.surfaces {
			s += termIDF(hitCounts[term]) * surfaceWeight(surface)
		}
		s *= contextBoost(h.result.FilePath, opts.ContextHints)
		ranked = append(ranked, scored{result: h.result, score: s})
	}
	sort.Slice(ranked, func(i, j int) bool { return ranked[i].score > ranked[j].score })

	results := make([]SearchResult, 0, limit)
	for i, s := range ranked {
		if i >= limit {
			break
		}
		results = append(results, s.result)
	}
	return results, nil
}

// shouldExclude returns true when result matches a known noise convention
// for one of the exclude terms. We use targeted rules rather than substring
// CONTAINS so excluding "test" doesn't accidentally drop "requestSettings".
//
// Rules per term:
//
//	filePath endswith "_<term>.go"           e.g. "_test.go", "_mock.go"
//	filePath contains "/<term>/" or "/<term>s/"  e.g. "/test/", "/mocks/"
//	symbol name starts with "<Term>" word-boundary (next char uppercase
//	or end of string), e.g. "TestFoo", "BenchmarkBar", "MockClient".
func shouldExclude(r SearchResult, excludes []string) bool {
	if len(excludes) == 0 {
		return false
	}
	pathLower := strings.ToLower(r.FilePath)
	nameLower := strings.ToLower(r.Name)
	for _, ex := range excludes {
		if ex == "" {
			continue
		}
		exLower := strings.ToLower(ex)
		if strings.HasSuffix(pathLower, "_"+exLower+".go") {
			return true
		}
		if strings.Contains(pathLower, "/"+exLower+"/") ||
			strings.Contains(pathLower, "/"+exLower+"s/") {
			return true
		}
		if strings.HasPrefix(nameLower, exLower) {
			rest := r.Name[len(exLower):]
			if rest == "" {
				return true
			}
			first := rest[0]
			if first >= 'A' && first <= 'Z' {
				return true
			}
		}
	}
	return false
}

// contextBoost multiplies a result's score when its filePath matches one
// or more context hints (case-insensitive substring). Capped to avoid a
// hint hijacking ranking when the model emits something too generic.
func contextBoost(filePath string, hints []string) float64 {
	if len(hints) == 0 || filePath == "" {
		return 1.0
	}
	pathLower := strings.ToLower(filePath)
	matches := 0
	for _, h := range hints {
		if h == "" {
			continue
		}
		if strings.Contains(pathLower, strings.ToLower(h)) {
			matches++
		}
	}
	switch matches {
	case 0:
		return 1.0
	case 1:
		return 2.0
	default:
		return 3.0
	}
}

// filesMatchingAnyTerm returns the set of files where ANY term hits in
// name, content, or filePath. Used to enforce the must-terms file-level
// filter. One Cypher per term; failures are logged and skipped so a
// partial set is still useful.
func (c *Client) filesMatchingAnyTerm(terms []string, repo string) map[string]bool {
	out := make(map[string]bool)
	for _, t := range terms {
		if t == "" {
			continue
		}
		cypher := fmt.Sprintf(
			`MATCH (n)
			 WHERE (toLower(n.filePath) CONTAINS "%s"
			        OR toLower(n.name) CONTAINS "%s"
			        OR toLower(n.content) CONTAINS "%s")
			   AND n.filePath IS NOT NULL
			 RETURN DISTINCT n.filePath AS filePath
			 LIMIT 500`,
			escapeCypher(t), escapeCypher(t), escapeCypher(t),
		)
		rows, err := c.Cypher(cypher, repo)
		if err != nil {
			continue
		}
		for _, row := range rows {
			if v, ok := row["filePath"].(string); ok && v != "" {
				out[v] = true
			}
		}
	}
	return out
}

// NameHitCount returns the number of nodes whose name CONTAINS term in
// the given repo. Used by the router for cheap rarity estimation when
// auto-promoting a query token to a must-anchor.
func (c *Client) NameHitCount(term, repo string) (int, error) {
	if term == "" {
		return 0, nil
	}
	cypher := fmt.Sprintf(
		`MATCH (n) WHERE toLower(n.name) CONTAINS "%s" AND n.startLine IS NOT NULL
		 RETURN count(n) AS c`,
		escapeCypher(strings.ToLower(term)),
	)
	rows, err := c.Cypher(cypher, repo)
	if err != nil {
		return 0, err
	}
	if len(rows) == 0 {
		return 0, nil
	}
	switch v := rows[0]["c"].(type) {
	case float64:
		return int(v), nil
	case int:
		return v, nil
	case int64:
		return int(v), nil
	}
	return 0, nil
}

// surfaceWeight reflects how strong a hit on each surface is for ranking.
// Symbol names are the strongest signal; file paths are decent for package
// hits; content matches are noisy but still useful when nothing else matches.
func surfaceWeight(s string) float64 {
	switch s {
	case "name":
		return 1.0
	case "filePath":
		return 0.7
	case "content":
		return 0.4
	}
	return 0
}

// termIDF approximates inverse document frequency from the per-term hit count.
// Bounded to keep extremely rare or extremely common terms from dominating.
// Uses a notional corpus size of 1000 — the absolute number doesn't matter,
// only the relative ordering across terms.
func termIDF(hitCount int) float64 {
	if hitCount <= 0 {
		return 0
	}
	v := math.Log(1.0 + 1000.0/float64(hitCount+1))
	if v > 8 {
		v = 8
	}
	return v
}

// rarestSeenTerm returns the term (from the original word list) with the
// smallest non-zero hit count. Used to pick a high-signal candidate for the
// content-search fallback so we don't blow up on common words like "error".
func rarestSeenTerm(words []string, hitCounts map[string]int) string {
	best := ""
	bestCnt := 0
	for _, w := range words {
		cnt := hitCounts[w]
		if cnt == 0 {
			continue
		}
		if best == "" || cnt < bestCnt {
			best = w
			bestCnt = cnt
		}
	}
	return best
}

func indexOf(s string, c byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return i
		}
	}
	return -1
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
// monopolise the snippet budget at the expense of other on-topic
// files. Codegraph returns symbol-level results, so a query like
// "rate limiter" can return 5 hits all in `ratelimit.go` (the
// constructor, the check method, the config struct, etc.). Without
// dedup, snippetCap=10 would yield only ~5 unique files; with dedup
// we keep the highest-ranked symbol per file and let the cap fill
// with the next 5 distinct files. Bench-validated on goserving:
// closes the codegraph→devrouter R@5 gap that a path-collision in
// the snippet stream had been causing.
func ToSnippets(results []SearchResult, max int) []prompt.Snippet {
	snippets := make([]prompt.Snippet, 0, max)
	seenFiles := make(map[string]bool, max)
	for _, r := range results {
		if len(snippets) >= max {
			break
		}
		// Path-dedup: keep the first (best-ranked) match per file.
		// Codegraph already ranks within a file by combined score, so
		// this is "drop the lower-ranked symbol from the same file"
		// — we don't lose any file we'd otherwise return.
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
		// Prefer Source (returned by codegraph when include_source is
		// set on the request) over Content (legacy/empty for most
		// hits). Source is the [startLine,endLine] slice for symbol
		// labels and the file head for FTS-only hits, so it always
		// gives the agent something to read.
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

func parseSearchRow(row map[string]any) SearchResult {
	r := SearchResult{}
	if v, ok := row["id"].(string); ok {
		r.ID = v
		r.NodeID = v
		if idx := indexOf(v, ':'); idx > 0 {
			r.Label = v[:idx]
		}
	}
	if v, ok := row["name"].(string); ok {
		r.Name = v
	}
	if v, ok := row["filePath"].(string); ok {
		r.FilePath = v
	}
	if v, ok := row["content"].(string); ok {
		r.Content = v
	}
	if v, ok := row["startLine"].(float64); ok {
		r.StartLine = int(v)
	}
	if v, ok := row["endLine"].(float64); ok {
		r.EndLine = int(v)
	}
	return r
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

func escapeCypher(s string) string {
	var b bytes.Buffer
	for _, c := range s {
		if c == '"' || c == '\\' {
			b.WriteRune('\\')
		}
		b.WriteRune(c)
	}
	return b.String()
}
