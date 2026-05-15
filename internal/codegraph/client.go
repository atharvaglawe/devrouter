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
