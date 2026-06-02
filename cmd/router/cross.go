package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/atharva-ag/devrouter/internal/codegraph"
	"github.com/atharva-ag/devrouter/internal/crossrepo"
)

// runCrossContext implements the `devrouter cross-context "<query>"`
// subcommand. It exists as a standalone CLI surface (not just an MCP
// tool) so the cross-repo federation can be exercised from shell
// scripts and benches without spinning up an MCP host.
//
// Usage:
//
//	devrouter cross-context [flags] "<query>"
//
// Flags:
//
//	--repos a,b,c       restrict to listed repos (default: all)
//	--limit-per-repo N  hits requested per repo before merge (default 10)
//	--total-limit N     final merged result count (default 30)
//	--mode M            search mode: hybrid|bm25|semantic (default hybrid)
//	--link-tag TAG      additionally resolve TAG → repo(s) and list
//	                    candidate routes in each target repo
//	--json              emit raw JSON instead of the human pretty view
//	--codegraph-url URL override CODEGRAPH_URL just for this invocation
//
// Exit codes:
//
//	0  request completed (even if zero hits)
//	1  invalid arguments or registry / codegraph error
//	2  query returned zero hits across the selected repos (useful for
//	   bench scripts that want to fail fast on regressions)
func runCrossContext(args []string) error {
	fs := flag.NewFlagSet("cross-context", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	var (
		reposCSV     string
		limitPerRepo int
		totalLimit   int
		mode         string
		linkTag      string
		jsonOut      bool
		cgURL        string
	)
	fs.StringVar(&reposCSV, "repos", "", "comma-separated repos (default: all)")
	fs.IntVar(&limitPerRepo, "limit-per-repo", 10, "hits requested per repo before merge")
	fs.IntVar(&totalLimit, "total-limit", 30, "final merged result count")
	fs.StringVar(&mode, "mode", "hybrid", "search mode: hybrid|bm25|semantic")
	fs.StringVar(&linkTag, "link-tag", "", "additionally resolve TAG to candidate repos / routes")
	fs.BoolVar(&jsonOut, "json", false, "emit JSON instead of the human view")
	fs.StringVar(&cgURL, "codegraph-url", "", "override CODEGRAPH_URL for this call")

	if err := fs.Parse(args); err != nil {
		printCrossContextHelp()
		// flag.ErrHelp is the user explicitly asking for --help / -h;
		// surface it as a clean exit, not a request error.
		if err == flag.ErrHelp {
			return nil
		}
		return err
	}
	rest := fs.Args()
	if len(rest) == 0 {
		printCrossContextHelp()
		return fmt.Errorf("cross-context: query is required")
	}
	query := strings.TrimSpace(strings.Join(rest, " "))
	if query == "" {
		return fmt.Errorf("cross-context: query is empty after trim")
	}

	url := cgURL
	if url == "" {
		url = os.Getenv("CODEGRAPH_URL")
	}
	if url == "" {
		url = "http://localhost:4747"
	}
	cg := codegraph.NewClient(url)
	linker := crossrepo.NewRepoLinker(cg)
	if linker == nil {
		return fmt.Errorf("cross-context: failed to construct linker (codegraph url %q)", url)
	}

	opts := crossrepo.SearchOptions{
		Repos:        splitCSV(reposCSV),
		LimitPerRepo: limitPerRepo,
		TotalLimit:   totalLimit,
		Mode:         mode,
	}
	resp, err := linker.Search(query, opts)
	if err != nil {
		return fmt.Errorf("cross-context: %w", err)
	}

	// Optional tag-link probe. We run it after the main search so the
	// human view can render search hits first, then the cross-repo
	// tag link block below — that mirrors how a real dev_context
	// response would surface "and by the way, this query mentions
	// tag X which resolves to repo Y".
	var links []crossrepo.RepoLink
	var routeHits []crossrepo.CrossRepoHit
	if linkTag != "" {
		sourceRepo := ""
		if len(opts.Repos) > 0 {
			sourceRepo = opts.Repos[0]
		}
		links, routeHits, err = linker.LinkedRoutes(sourceRepo, linkTag, 5)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[cross-context] link-tag probe failed: %v\n", err)
		}
	}

	if jsonOut {
		envelope := map[string]any{
			"response": resp,
		}
		if linkTag != "" {
			envelope["linkTag"] = linkTag
			envelope["links"] = links
			envelope["routeHits"] = routeHits
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(envelope); err != nil {
			return fmt.Errorf("cross-context: encode json: %w", err)
		}
	} else {
		printHumanResponse(os.Stdout, resp, linkTag, links, routeHits)
	}

	if len(resp.Hits) == 0 {
		return errExitNoHits
	}
	return nil
}

// errExitNoHits is a sentinel returned by runCrossContext when the
// query produced zero hits across the selected repos. The caller in
// main() maps it to exit code 2 so bench scripts can detect this case
// distinctly from "real error" (exit 1) without parsing stderr.
var errExitNoHits = fmt.Errorf("cross-context: no hits across selected repos")

// printCrossContextHelp prints the subcommand usage to stderr.
func printCrossContextHelp() {
	fmt.Fprintln(os.Stderr, `devrouter cross-context — federated search across indexed repos

Usage:
  devrouter cross-context [flags] "<query>"

Flags:
  --repos a,b,c        restrict to listed repos (default: all indexed)
  --limit-per-repo N   hits requested per repo before merge (default 10)
  --total-limit N      final merged result count (default 30)
  --mode M             search mode: hybrid|bm25|semantic (default hybrid)
  --link-tag TAG       also resolve TAG to candidate repos + routes
  --json               emit raw JSON instead of the human pretty view
  --codegraph-url URL  override CODEGRAPH_URL for this call

Examples:
  devrouter cross-context "how does scrr module management work?"
  devrouter cross-context --repos goserving,cmadserving "scrr module registry"
  devrouter cross-context --link-tag cmadserving "client call from kosmos"

Exit codes:
  0  hits returned
  1  invalid args / codegraph or registry error
  2  zero hits across selected repos`)
}

// printHumanResponse renders the federated search response in a shell-
// friendly format: per-repo summary, then a flat ranked list of hits
// with repo, file, lines, and a one-line content preview.
//
// Output is mildly opinionated — we colour-skip and stay pure ASCII so
// the bench / CI logs don't get cluttered with escape codes, and we
// truncate long content previews at 120 chars so each hit fits on
// one line in a standard terminal.
func printHumanResponse(
	w io.Writer,
	resp *crossrepo.CrossRepoResponse,
	linkTag string,
	links []crossrepo.RepoLink,
	routeHits []crossrepo.CrossRepoHit,
) {
	fmt.Fprintf(w, "query:    %s\n", resp.Query)
	fmt.Fprintf(w, "repos:    %s\n", strings.Join(resp.Repos, ", "))
	fmt.Fprintf(w, "duration: %s\n", resp.Duration)

	if len(resp.PerRepo) > 0 {
		fmt.Fprintln(w, "per-repo hits:")
		for _, r := range resp.Repos {
			fmt.Fprintf(w, "  %-30s %d\n", r, resp.PerRepo[r])
		}
	}
	if len(resp.Errors) > 0 {
		fmt.Fprintln(w, "errors:")
		for repo, msg := range resp.Errors {
			fmt.Fprintf(w, "  %-30s %s\n", repo, msg)
		}
	}

	fmt.Fprintln(w, "")
	fmt.Fprintf(w, "%d merged hits (ranked):\n", len(resp.Hits))
	for i, h := range resp.Hits {
		preview := singleLine(firstNonEmpty(h.Source, h.Content), 120)
		lineRange := ""
		if h.StartLine > 0 {
			lineRange = fmt.Sprintf(" L%d-%d", h.StartLine, h.EndLine)
		}
		fmt.Fprintf(w, "  %2d. [%s] %s %s%s  (score %.3f)\n",
			i+1, h.Repo, h.Label, h.Name, lineRange, h.Score)
		fmt.Fprintf(w, "      %s\n", h.FilePath)
		if preview != "" {
			fmt.Fprintf(w, "      %s\n", preview)
		}
	}

	if linkTag != "" {
		fmt.Fprintln(w, "")
		fmt.Fprintf(w, "link-tag probe: %q\n", linkTag)
		if len(links) == 0 {
			fmt.Fprintln(w, "  (no candidate target repos)")
		} else {
			for _, l := range links {
				fmt.Fprintf(w, "  -> %-25s (conf=%.2f, %s)\n", l.TargetRepo, l.Confidence, l.Reason)
			}
		}
		if len(routeHits) > 0 {
			fmt.Fprintf(w, "candidate routes / handlers (%d):\n", len(routeHits))
			for i, h := range routeHits {
				lineRange := ""
				if h.StartLine > 0 {
					lineRange = fmt.Sprintf(" L%d-%d", h.StartLine, h.EndLine)
				}
				fmt.Fprintf(w, "  %2d. [%s] %s %s%s\n", i+1, h.Repo, h.Label, h.Name, lineRange)
				fmt.Fprintf(w, "      %s\n", h.FilePath)
			}
		}
	}
}

func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := parts[:0]
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// singleLine collapses runs of whitespace to a single space and truncates
// to max runes. Used to render multi-line source previews on a single
// terminal row without clipping mid-word.
func singleLine(s string, max int) string {
	if s == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(s))
	inSpace := false
	for _, r := range s {
		if r == '\n' || r == '\t' || r == '\r' || r == ' ' {
			if !inSpace && b.Len() > 0 {
				b.WriteByte(' ')
				inSpace = true
			}
			continue
		}
		inSpace = false
		b.WriteRune(r)
	}
	out := strings.TrimSpace(b.String())
	if len(out) > max {
		out = out[:max-1] + "\u2026"
	}
	return out
}
