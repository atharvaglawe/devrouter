package crossrepo

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/atharva-ag/devrouter/internal/codegraph"
)

// RepoLinker is the federated retrieval surface. One process should
// share a single instance: the underlying codegraph client and
// registry loader both maintain caches that are wasted if duplicated.
//
// Construction is via NewRepoLinker; the zero value is not usable.
type RepoLinker struct {
	cg       *codegraph.Client
	registry *RegistryLoader
}

// NewRepoLinker constructs a linker wrapping the given codegraph
// client. The registry loader is created internally with default TTL;
// override via SetRegistryLoader for tests that want a stub registry.
func NewRepoLinker(cg *codegraph.Client) *RepoLinker {
	if cg == nil {
		return nil
	}
	return &RepoLinker{
		cg:       cg,
		registry: NewRegistryLoader(),
	}
}

// SetRegistryLoader swaps the registry loader. Intended for tests that
// inject a deterministic registry without touching disk.
func (l *RepoLinker) SetRegistryLoader(rl *RegistryLoader) {
	if l == nil || rl == nil {
		return
	}
	l.registry = rl
}

// Registry returns the cached registry snapshot. Convenience wrapper
// so callers that want to enumerate repos for their own purposes
// (e.g. "tell me every repo I can search") don't have to instantiate
// a separate loader.
func (l *RepoLinker) Registry() (*Registry, error) {
	if l == nil || l.registry == nil {
		return nil, fmt.Errorf("crossrepo: linker not initialized")
	}
	return l.registry.Load()
}

// Search fans out the query to every selected repo in parallel,
// collects results, and returns a merged response. See SearchOptions
// for the knobs. Errors per-repo are recorded in the response's
// Errors map; a top-level error is returned only for failures that
// affect the whole request (missing registry, unresolvable repo
// selection, etc.).
//
// Determinism: with a fixed query / options / registry, output ordering
// is stable across runs. The federated rank is (Score desc, Repo asc,
// FilePath asc, Name asc) so two repos returning the same Score don't
// flip-flop on consecutive calls.
func (l *RepoLinker) Search(query string, opts SearchOptions) (*CrossRepoResponse, error) {
	if l == nil || l.cg == nil {
		return nil, fmt.Errorf("crossrepo: linker not initialized")
	}
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, fmt.Errorf("crossrepo: empty query")
	}
	opts = opts.applyDefaults()

	repos, err := l.resolveRepos(opts)
	if err != nil {
		return nil, err
	}
	if len(repos) == 0 {
		return &CrossRepoResponse{
			Query:   query,
			Hits:    nil,
			PerRepo: map[string]int{},
			Repos:   nil,
		}, nil
	}

	mode := opts.Mode
	if mode == "" {
		mode = codegraph.SearchModeHybrid
	}

	start := time.Now()
	type repoResult struct {
		repo    string
		results []codegraph.SearchResult
		err     error
	}

	// Bounded fan-out: sem caps the number of in-flight codegraph
	// requests so a 30-repo registry doesn't open 30 sockets at once.
	// Per-repo response shape is small so a buffered results channel
	// with one slot per repo never blocks the workers.
	sem := make(chan struct{}, opts.Concurrency)
	resultsCh := make(chan repoResult, len(repos))
	var wg sync.WaitGroup
	for _, repo := range repos {
		wg.Add(1)
		go func(repo string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			res, err := l.cg.SearchWithMode(query, repo, opts.LimitPerRepo, mode)
			resultsCh <- repoResult{repo: repo, results: res, err: err}
		}(repo)
	}
	wg.Wait()
	close(resultsCh)

	resp := &CrossRepoResponse{
		Query:    query,
		PerRepo:  make(map[string]int, len(repos)),
		Errors:   nil,
		Repos:    repos,
		Duration: time.Since(start),
	}
	for r := range resultsCh {
		if r.err != nil {
			if resp.Errors == nil {
				resp.Errors = make(map[string]string)
			}
			resp.Errors[r.repo] = r.err.Error()
			continue
		}
		resp.PerRepo[r.repo] = len(r.results)
		for _, hit := range r.results {
			resp.Hits = append(resp.Hits, CrossRepoHit{
				Repo:         r.repo,
				SearchResult: hit,
			})
		}
	}

	rankAndTrim(resp.Hits, opts.TotalLimit, &resp.Hits)
	return resp, nil
}

// LinkedRoutes is the API-call-side primitive. Given a tag observed in
// `sourceRepo` (e.g. extracted from `httpclient.GetClient("X")`),
// resolve the tag to candidate target repos via LinkTag, then run a
// Route-shaped search in each target. The result is the set of
// (target repo, route handler) pairs the source call could be hitting.
//
// Returns links (the tag→repo resolutions used) and hits (the route
// matches in each target). Both are sorted highest-confidence first.
//
// This is the cross-repo replacement for the in-graph FETCHES edge
// that codegraph can only emit when caller + handler live in the same
// repo. It's strictly heuristic — we never claim the source actually
// hit a specific route, only that it's a plausible target the agent
// should consider when tracing the call chain.
func (l *RepoLinker) LinkedRoutes(sourceRepo, tag string, limitPerRepo int) ([]RepoLink, []CrossRepoHit, error) {
	if l == nil || l.cg == nil {
		return nil, nil, fmt.Errorf("crossrepo: linker not initialized")
	}
	links := l.LinkTag(sourceRepo, tag)
	if len(links) == 0 {
		return nil, nil, nil
	}
	if limitPerRepo <= 0 {
		limitPerRepo = 5
	}

	// Build the route-search query — anything that biases the search
	// toward Route nodes. Codegraph search is hybrid, so a query of
	// just the tag tends to surface route handlers + config + tests,
	// which is exactly what we want the agent to see. If the tag is
	// non-discriminating (very short), we additionally search for the
	// literal "route" / "handler" tokens to lean into Route nodes.
	q := tag
	if len(tag) < 4 {
		q = tag + " route handler"
	}

	type repoResult struct {
		repo    string
		results []codegraph.SearchResult
		err     error
	}

	resultsCh := make(chan repoResult, len(links))
	var wg sync.WaitGroup
	for _, link := range links {
		wg.Add(1)
		go func(link RepoLink) {
			defer wg.Done()
			res, err := l.cg.SearchWithMode(q, link.TargetRepo, limitPerRepo, codegraph.SearchModeHybrid)
			resultsCh <- repoResult{repo: link.TargetRepo, results: res, err: err}
		}(link)
	}
	wg.Wait()
	close(resultsCh)

	var hits []CrossRepoHit
	for r := range resultsCh {
		if r.err != nil {
			// Best-effort: a single repo failing doesn't kill the
			// whole linked-route response. The caller can re-derive
			// missing repos from the links slice if it cares.
			continue
		}
		for _, h := range r.results {
			hits = append(hits, CrossRepoHit{
				Repo:         r.repo,
				SearchResult: h,
			})
		}
	}
	rankAndTrim(hits, limitPerRepo*len(links), &hits)
	return links, hits, nil
}

// resolveRepos picks the set of repos to fan out to based on opts.Repos
// and the on-disk registry. Selection rules:
//
//   - opts.Repos == nil or empty → every registry entry, capped at
//     opts.MaxRepos in registry order. A missing registry returns an
//     empty slice (not an error) so downstream callers can degrade
//     gracefully to "no cross-repo signal available".
//
//   - opts.Repos non-empty → only entries whose Name exactly matches
//     one of the requested names, preserving the caller's order so
//     two callers with different priorities (e.g. "goserving first,
//     cmadserving second" vs the reverse) produce different result
//     orderings if scores tie.
//
// Unknown names in opts.Repos are dropped silently — they show up as
// missing from CrossRepoResponse.Repos so the caller can detect the
// drop. We don't error out because the registry can legitimately
// change between calls and we'd rather degrade than fail.
func (l *RepoLinker) resolveRepos(opts SearchOptions) ([]string, error) {
	reg, err := l.registry.Load()
	if err != nil {
		return nil, err
	}
	if reg == nil {
		return nil, nil
	}

	if len(opts.Repos) == 0 {
		names := reg.Names()
		if len(names) > opts.MaxRepos {
			names = names[:opts.MaxRepos]
		}
		return names, nil
	}

	known := make(map[string]struct{}, len(reg.Entries))
	for _, e := range reg.Entries {
		known[e.Name] = struct{}{}
	}
	out := make([]string, 0, len(opts.Repos))
	seen := make(map[string]struct{}, len(opts.Repos))
	for _, name := range opts.Repos {
		if _, dup := seen[name]; dup {
			continue
		}
		if _, ok := known[name]; !ok {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	return out, nil
}

// rankAndTrim sorts hits by (Score desc, Repo asc, FilePath asc, Name
// asc) then truncates to totalLimit. Writes the result back through
// the *out pointer so callers can pass &resp.Hits without allocating
// a second slice.
//
// The federated rank intentionally does NOT renormalise per-repo
// scores. Codegraph's hybrid score is already calibrated across repos
// (same scoring formula, same query) so a raw cross-repo sort is
// honest. If we wanted to bias toward small repos (so a 20-result
// PHP repo doesn't get drowned by a 50-result Go monorepo) we'd add
// a Repo-size penalty here; deferred until benches show it's needed.
func rankAndTrim(hits []CrossRepoHit, totalLimit int, out *[]CrossRepoHit) {
	if len(hits) == 0 {
		*out = nil
		return
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].Score != hits[j].Score {
			return hits[i].Score > hits[j].Score
		}
		if hits[i].Repo != hits[j].Repo {
			return hits[i].Repo < hits[j].Repo
		}
		if hits[i].FilePath != hits[j].FilePath {
			return hits[i].FilePath < hits[j].FilePath
		}
		return hits[i].Name < hits[j].Name
	})
	if totalLimit > 0 && len(hits) > totalLimit {
		hits = hits[:totalLimit]
	}
	*out = hits
}
