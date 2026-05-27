package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/atharva-ag/devrouter/internal/codegraph"
	"github.com/atharva-ag/devrouter/internal/dashboard"
	"github.com/atharva-ag/devrouter/internal/mcp"
	"github.com/atharva-ag/devrouter/internal/memory"
	"github.com/atharva-ag/devrouter/internal/router"
	"github.com/atharva-ag/devrouter/internal/telemetry"
)

// Build info populated at link time via -ldflags="-X main.Version=...".
// When unset (e.g. `go run`) the value falls back to the dev sentinel so
// telemetry build_info still emits a non-empty label.
var Version = "dev"

func main() {
	log.SetOutput(os.Stderr)
	log.SetPrefix("[devrouter] ")
	// Microsecond-resolution timestamps make latency profiling
	// directly readable from stderr — the bench harness already
	// captures stderr, so this is free signal during profiling
	// runs and harmless in production.
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	// telemetry.Setup must run before any package emits metrics or
	// log records: it (1) materialises the Prometheus registry the
	// dashboard /metrics handler reads, and (2) optionally redirects
	// stdlib log through slog when DEVROUTER_LOG_FORMAT=json. Setup
	// is idempotent so subcommand passthroughs below can safely
	// re-enter it via init paths.
	telemetry.Setup()

	// Subcommand passthrough to the in-tree codegraph Node CLI. Three forms:
	//
	//   devrouter analyze /path            — direct (any codegraph subcommand)
	//   devrouter codegraph analyze /path  — explicit, useful if a future
	//                                        devrouter-native subcommand ever
	//                                        collides with a codegraph name.
	//   devrouter gitnexus analyze /path   — legacy alias (the engine used to
	//                                        be called gitnexus). Prints a
	//                                        deprecation hint to stderr.
	//
	// With no args, fall through to the MCP server (default behaviour).
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "codegraph":
			if err := runCodegraphPassthrough(os.Args[2:]); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			return
		case "gitnexus":
			fmt.Fprintln(os.Stderr,
				"[devrouter] note: `devrouter gitnexus ...` is deprecated; use `devrouter codegraph ...`")
			if err := runCodegraphPassthrough(os.Args[2:]); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			return
		case "analyze", "index", "serve", "list", "status", "clean", "help":
			if err := runCodegraphPassthrough(os.Args[1:]); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			return
		case "-h", "--help":
			printDevrouterHelp()
			return
		}
	}

	// Resolve codegraph URL. Prefer CODEGRAPH_URL; fall back to legacy
	// GITNEXUS_URL with a one-line deprecation hint so existing env exports
	// keep working until users migrate.
	cgURL := os.Getenv("CODEGRAPH_URL")
	if cgURL == "" {
		if legacy := os.Getenv("GITNEXUS_URL"); legacy != "" {
			fmt.Fprintln(os.Stderr,
				"[devrouter] note: GITNEXUS_URL is deprecated; export CODEGRAPH_URL instead")
			cgURL = legacy
		} else {
			cgURL = "http://localhost:4747"
		}
	}

	redisAddr := os.Getenv("DEVROUTER_REDIS")
	if redisAddr == "" {
		redisAddr = "localhost:6379"
	}

	graph := codegraph.NewClient(cgURL)

	mem, err := memory.NewStore(redisAddr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to connect to Redis at %s: %v\n", redisAddr, err)
		os.Exit(1)
	}

	r := router.New(graph, mem)
	if r.Heuristics != nil {
		log.Printf("heuristics: picker initialized (frozen=%v)", r.Heuristics.Frozen())
	}

	// build_info is a static gauge always set to 1; the label vector
	// surfaces version + the external dependencies this process talks
	// to. Lets a dashboard answer "which devrouter binaries are out
	// there, and where are they pointing?" without scraping logs.
	telemetry.BuildInfo.WithLabelValues(Version, cgURL, redisAddr).Set(1)

	// Read-only observability dashboard. Enabled by default on
	// 127.0.0.1:8088 (localhost-only, no auth required). Surfaces live
	// queries, heuristic profile evolution, decision lineage, and
	// saved flows. Failures are logged but never block the MCP server.
	//
	// Opt-out: DEVROUTER_DASHBOARD_ADDR=off (or "none" / "disabled").
	// Bind elsewhere: DEVROUTER_DASHBOARD_ADDR=:9090 (or any host:port).
	//
	// Port conflicts (common when an MCP host spawns multiple devrouter
	// processes — Cursor does this per project) are non-fatal: only
	// the first instance binds, the rest log a one-line warning and
	// keep serving MCP traffic.
	if addr := dashboardAddr(); addr != "" {
		dashboard.Start(dashboard.Config{
			Addr:       addr,
			Memory:     mem,
			Heuristics: r.Heuristics,
		})
	}

	// Query planning is the caller's responsibility: the MCP `dev_context`
	// tool accepts a structured `plan` argument that the agent (Claude,
	// Cursor, etc.) fills in. devrouter no longer runs any in-process
	// planner LLM. Callers that don't supply a plan still get useful
	// retrieval via deterministic auto-anchoring.

	srv := mcp.NewServer(r)

	log.Printf("MCP server starting on stdio (Redis: %s, codegraph: %s)", redisAddr, cgURL)
	if err := srv.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "server error: %v\n", err)
		os.Exit(1)
	}
}

// dashboardAddr resolves the bind address for the bundled observability
// dashboard, honouring the opt-out sentinels documented in
// docs/configuration.md. Default (unset env var) returns the
// localhost-only address so a fresh install gets the UI for free.
func dashboardAddr() string {
	v := strings.TrimSpace(os.Getenv("DEVROUTER_DASHBOARD_ADDR"))
	if v == "" {
		return "127.0.0.1:8088"
	}
	switch strings.ToLower(v) {
	case "off", "none", "disabled", "false", "0":
		return ""
	}
	return v
}

func printDevrouterHelp() {
	fmt.Println(`devrouter — MCP server + vendored codegraph CLI

Usage:
  devrouter                          Start the MCP server on stdio (default)
  devrouter <codegraph-subcommand>   Run a codegraph CLI command
  devrouter codegraph <args...>      Same, with explicit "codegraph" prefix
  devrouter gitnexus <args...>       Legacy alias (deprecated)
  devrouter --help                   Show this message

Codegraph subcommands (forwarded to the in-tree Node CLI):
  analyze   Index a repository
  index     Register an existing .codegraph/ folder
  serve     Start the codegraph HTTP server (port 4747)
  list      List indexed repositories
  status    Show index status for current repo
  clean     Delete the codegraph index for current repo
  help      Show codegraph CLI help

Environment:
  DEVROUTER_REDIS                Redis address (default localhost:6379)
  DEVROUTER_EMBEDDING_URL        Embedding endpoint (default http://localhost:11435/api/embed — bundled ONNX embedder)
  DEVROUTER_EMBEDDING_MODEL      Model name sent in /api/embed requests (default nomic-embed-text-v1.5; advisory only on the ONNX embedder)
  DEVROUTER_RELEASE_BRANCH       Git ref the save-time scope detector diffs against (default origin/release; try origin/main / origin/master)
  DEVROUTER_DASHBOARD_ADDR       Bind address for the read-only dashboard (default 127.0.0.1:8088; set off/none/disabled to disable)
  DEVROUTER_METRICS_ADDR         Prometheus /metrics control (default: mounted on dashboard; set off to disable)
  DEVROUTER_LOG_FORMAT           Log output format (default text; set json for structured slog records)
  DEVROUTER_LOG_LEVEL            Minimum log level (default info; debug/info/warn/error)
  CODEGRAPH_URL                  Codegraph HTTP base URL (default http://localhost:4747)
  GITNEXUS_URL                   Legacy alias for CODEGRAPH_URL (deprecated)
  DEVROUTER_CODEGRAPH_CLI        Override path to codegraph dist/cli/index.js
  DEVROUTER_GITNEXUS_CLI         Legacy alias for DEVROUTER_CODEGRAPH_CLI (deprecated)`)
}

// runCodegraphPassthrough execs `node <cli> <args...>`, inheriting stdio and
// propagating the child's exit code. The CLI path is resolved as:
//
//  1. $DEVROUTER_CODEGRAPH_CLI       — explicit override
//  2. $DEVROUTER_GITNEXUS_CLI        — legacy override (deprecated)
//  3. <dir of this binary>/codegraph/dist/cli/index.js
//  4. <cwd>/codegraph/dist/cli/index.js
//
// If none exists, the user is pointed at `make codegraph-build`.
func runCodegraphPassthrough(args []string) error {
	cliPath, err := resolveCodegraphCLI()
	if err != nil {
		return err
	}

	// Prepend the CLI path so node receives `node <cli> <args...>`.
	nodeArgs := append([]string{cliPath}, args...)
	cmd := exec.Command("node", nodeArgs...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		// Surface the child's exit code so shell scripts can react to it.
		if ee, ok := err.(*exec.ExitError); ok {
			os.Exit(ee.ExitCode())
		}
		return fmt.Errorf("failed to invoke codegraph CLI (%s): %w", cliPath, err)
	}
	return nil
}

func resolveCodegraphCLI() (string, error) {
	// Explicit overrides win: prefer the new env var, fall back to the old one.
	for _, key := range []string{"DEVROUTER_CODEGRAPH_CLI", "DEVROUTER_GITNEXUS_CLI"} {
		if p := os.Getenv(key); p != "" {
			if _, err := os.Stat(p); err == nil {
				if key == "DEVROUTER_GITNEXUS_CLI" {
					fmt.Fprintln(os.Stderr,
						"[devrouter] note: DEVROUTER_GITNEXUS_CLI is deprecated; use DEVROUTER_CODEGRAPH_CLI")
				}
				return p, nil
			}
			return "", fmt.Errorf("%s=%s does not exist", key, p)
		}
	}

	candidates := []string{}

	if exe, err := os.Executable(); err == nil {
		// Resolve symlinks so a `devrouter` symlink in $PATH still finds the
		// repo-relative dist next to the real binary.
		if real, err := filepath.EvalSymlinks(exe); err == nil {
			exe = real
		}
		base := filepath.Dir(exe)
		candidates = append(candidates,
			filepath.Join(base, "codegraph", "dist", "cli", "index.js"),
			// Legacy path — useful while users still have unmigrated checkouts.
			filepath.Join(base, "gitnexus", "dist", "cli", "index.js"),
		)
	}
	if cwd, err := os.Getwd(); err == nil {
		candidates = append(candidates,
			filepath.Join(cwd, "codegraph", "dist", "cli", "index.js"),
			filepath.Join(cwd, "gitnexus", "dist", "cli", "index.js"),
		)
	}

	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}

	return "", fmt.Errorf(
		"codegraph CLI not found (looked in: %v).\n"+
			"Build it with: make codegraph-build\n"+
			"Or override the path: DEVROUTER_CODEGRAPH_CLI=/abs/path/to/dist/cli/index.js",
		candidates,
	)
}
