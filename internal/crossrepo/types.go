// Package crossrepo federates retrieval across multiple repos indexed
// by the same codegraph server.
//
// Background. devrouter's per-request `dev_context` surface is repo-scoped
// — every codegraph query carries `repo` and walks exactly one
// `.codegraph/` LadybugDB graph. That model is intentional (per-repo
// lifecycle, lock semantics, incremental analyze) but it leaves real
// service-to-service connections invisible:
//
//   - A concept like "scrr module management" can be split across a
//     Go backend repo (handlers, registry, dispatch tables) and a
//     PHP frontend repo (renderer factory, module map). A single
//     repo-scoped search misses half the picture.
//   - An HTTP/RPC call from goserving's `httpclient.GetClient("cmadserving")`
//     never produces a FETCHES edge inside goserving's graph because
//     the target Route node lives in the cmadserving graph.
//
// This package layers a federation surface on top of codegraph WITHOUT
// modifying codegraph itself. Two primitives:
//
//  1. RepoLinker.Search — fan out a search query to N indexed repos in
//     parallel and merge the results, stamped with the source repo so
//     downstream callers can group / cite per-repo. Backs the
//     `devrouter cross-context` CLI and the "shared concept" case in
//     dev_context.
//
//  2. RepoLinker.LinkTag — given a provider tag observed in repo A
//     (e.g. `"cmadserving"` extracted from a config-driven HTTP
//     client), resolve it to a set of target repos using the global
//     registry and each repo's surface signals (name, top-level dirs,
//     declared service-dir hints). Backs the API-call cross-repo
//     join — the eventual replacement for the missing in-graph
//     FETCHES edge across service boundaries.
//
// What this package does NOT do:
//
//   - It does not parse source code. Discovery is by registry +
//     directory layout + the existing codegraph HTTP surface. Heavier
//     extraction (the actual ClientCall/Route join, e.g. Option C from
//     docs/cross-repo design) belongs in codegraph proper if/when we
//     decide to make cross-repo a first-class graph concept.
//
//   - It does not mutate the registry. The on-disk registry is the
//     source of truth; this package reads it (with caching) and never
//     writes back.
//
// Concurrency: a single RepoLinker is safe to share across goroutines.
// Per-repo caches use mutexes; codegraph HTTP fan-out uses a bounded
// worker pool so a query to 30 indexed repos doesn't open 30 sockets
// at once.
package crossrepo

import (
	"time"

	"github.com/atharva-ag/devrouter/internal/codegraph"
)

// CrossRepoHit is a single search result enriched with the source repo
// it came from. It wraps codegraph.SearchResult so existing snippet /
// ranking code (ToSnippets, FilePaths, etc.) can consume the embedded
// result with zero changes — only callers that care about the repo
// label need to look at the Repo field.
//
// Score in the embedded SearchResult is the per-repo score as reported
// by codegraph. The federated ranker (see linker.go: rankAndTrim) may
// rewrite this with a federated score that's comparable across repos.
type CrossRepoHit struct {
	Repo string `json:"repo"`
	codegraph.SearchResult
}

// CrossRepoResponse is the federated result envelope returned by
// RepoLinker.Search. Hits are pre-ranked across all source repos so
// the caller can iterate them in priority order without re-sorting.
//
// PerRepo gives the raw counts before merge/dedup so the dashboard
// (and CLI verbose mode) can show "goserving: 8 hits, cmadserving: 3
// hits, mall: 0 hits". A repo that returned an error appears in
// Errors with the codegraph error message so transient failures don't
// silently shrink the result set.
type CrossRepoResponse struct {
	Query    string              `json:"query"`
	Hits     []CrossRepoHit      `json:"hits"`
	PerRepo  map[string]int      `json:"perRepo"`
	Errors   map[string]string   `json:"errors,omitempty"`
	Repos    []string            `json:"repos"`
	Duration time.Duration       `json:"-"` // populated by Search; not serialized
}

// RepoLink is one (source repo, provider tag, target repo) edge resolved
// by LinkTag. Used to back the API-call cross-repo join: the source
// observes a tag-only HTTP client (`httpclient.GetClient("X")`), the
// link resolver maps "X" to the target repo whose name / top-level
// dirs / declared service-dir hints match, and the caller can then
// fan out a follow-up Route search into the target.
//
// Confidence is a 0..1 score combining match strength (exact name >
// substring > token-overlap) and source quality (registry name >
// top-level dir > config-file hint). A consumer that wants only the
// strongest links should filter at >= 0.7.
//
// Reason is a short human-readable provenance string ("name:exact",
// "topdir:cmadserving", "configtag:scrr@services.cmad.url") suitable
// for inclusion in dev_context responses so the agent can explain
// why a cross-repo hop was suggested.
type RepoLink struct {
	SourceRepo  string  `json:"sourceRepo"`
	Tag         string  `json:"tag"`
	TargetRepo  string  `json:"targetRepo"`
	Confidence  float64 `json:"confidence"`
	Reason      string  `json:"reason"`
}

// SearchOptions configures one federated search call. Zero-value is
// valid and produces sensible defaults (10 hits per repo, hybrid
// search mode, no exclusions). Mirrors codegraph.SearchOpts shape on
// purpose so the call sites that already build SearchOpts can pass it
// straight through.
type SearchOptions struct {
	// Repos restricts the fan-out to the listed names. Empty = every
	// repo in the registry (modulo MaxRepos). Names are matched
	// case-sensitively against the registry's `name` field.
	Repos []string
	// LimitPerRepo caps results requested from each individual repo
	// before federation-side ranking trims further. Default 10.
	LimitPerRepo int
	// TotalLimit caps the final merged result count after federated
	// ranking. Default 30. Set 0 to mean "no extra cap beyond
	// LimitPerRepo * |Repos|".
	TotalLimit int
	// Mode selects the codegraph search mode (hybrid / bm25 /
	// semantic). Empty defaults to hybrid — same as codegraph.Search.
	Mode string
	// MaxRepos bounds the fan-out when Repos is empty. Default 8.
	// Keeps a 30-repo registry from blowing up a single request.
	MaxRepos int
	// Concurrency bounds the in-flight HTTP requests to codegraph.
	// Default 4. The codegraph server is single-LadybugDB per repo
	// but multiplexes across repos cheaply, so 4-8 is a sweet spot
	// even on a laptop.
	Concurrency int
}

// applyDefaults fills in zero-valued fields with their package defaults.
// Returns a copy so callers can keep their SearchOptions immutable.
func (o SearchOptions) applyDefaults() SearchOptions {
	if o.LimitPerRepo <= 0 {
		o.LimitPerRepo = 10
	}
	if o.TotalLimit <= 0 {
		o.TotalLimit = 30
	}
	if o.MaxRepos <= 0 {
		o.MaxRepos = 8
	}
	if o.Concurrency <= 0 {
		o.Concurrency = 4
	}
	return o
}
