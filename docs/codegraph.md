# codegraph — devrouter's per-repo code indexer

devrouter answers questions about your code by combining three things:

1. **Memory** — agent-written notes (Redis, vector-searchable).
2. **Code structure** — symbols, call chains, importers, source snippets.
3. **Saved decisions** — architecture rationale, etc.

Item 2 is what `codegraph` provides. It walks your repo, parses every
supported source file, builds a graph of symbols and the edges between
them (calls, imports, inherits, has-method, …), and exposes a small
HTTP API that devrouter queries on every `dev_context` call.

You almost never interact with codegraph directly. devrouter ships
with the codegraph binary vendored under `codegraph/` and starts /
manages it for you. The only command you'll routinely run is
`./devrouter analyze /abs/path/to/your-repo` once per repo.

This doc explains what codegraph is, what it stores, what it exposes,
and the few knobs you might want to turn. For the full pipeline that
turns a query into a response, see
[`retrieval-rules.md`](retrieval-rules.md). For the
retrieval-shaping rules DevRouter applies on top of codegraph's raw
output (intent-aware search mode routing, snippet dedup, graph
relevance filtering, anchor injection, parallel fan-out), see
[`codegraph-heuristics.md`](codegraph-heuristics.md). For the
extractor and structural-edge work that landed on top of the initial
codegraph implementation (generic API-endpoint extraction across
Go / Java / Python, provider-tag and config-tag resolution,
structural IMPLEMENTS detection), see
[`codegraph-fixes.md`](codegraph-fixes.md). For how index drift,
memory git-hash drift, and the relevance-decay loop are detected
and handled across the system, see [`staleness.md`](staleness.md).

## Where it sits

```
   you  ─►  agent  ─►  devrouter (MCP)  ─►  codegraph  ─►  your repo
                              │                  │
                              │                  └─►  .codegraph/   (per-repo index)
                              │                  └─►  ~/.codegraph/ (global registry)
                              │
                              └─►  Redis           (memory + heuristics)
                              └─►  Embedder        (bundled ONNX, /api/embed)
```

devrouter speaks MCP to the agent and HTTP to codegraph. codegraph
speaks tree-sitter and LadybugDB to the filesystem. The agent and
your IDE never see codegraph.

## What gets indexed

codegraph parses each file with tree-sitter and produces a graph
with these node types: files, packages, symbols (functions, classes,
interfaces, methods, fields, types, …), and routes (HTTP /
middleware where it can detect them).

Edges between symbols include:

| Edge | Meaning |
|------|---------|
| `CALLS` | Function A invokes function B |
| `IMPORTS` | Package or symbol import |
| `EXTENDS` | Class / interface inheritance |
| `HAS_METHOD` | Type owns this method |
| `RETURNS` / `PARAM_TYPE` | Type-level relationships where the parser can resolve them |
| Route attachment | Handler ↔ HTTP route / middleware chain |

The pipeline runs in phases: structure → parsing → imports → calls →
heritage → MRO → communities → processes. Each phase is a separate
pass over the parsed ASTs, with progress reported to the analyze
job.

### Languages supported

C, C#, C++, Go, Java, JavaScript, PHP, Python, Ruby, Rust,
TypeScript (with `.tsx` / Vue SFC support).

Optional via tree-sitter: Dart, Kotlin, Swift, COBOL/JCL.

You can index a polyglot repo without flagging the languages — the
ingester picks the right parser per file based on extension.

## Where data lives

| Path | Contents |
|------|----------|
| `<your-repo>/.codegraph/` | Per-repo index: LadybugDB graph file, parsed metadata, `meta.json` with timestamps and stats. |
| `~/.codegraph/registry.json` | Global registry. Lists every indexed repo so the HTTP server can find them from any cwd. |
| `~/.codegraph/` (root) | Override with `CODEGRAPH_HOME=/some/path`. Useful for shared dev boxes. |

Storage cost: typical small-to-medium Go repo ≈ 50 – 200 MB on disk.
A 1M-LOC monorepo ≈ 1 – 3 GB. The bulk is the graph + AST cache;
embeddings (off by default) add another ~1.5× when enabled.

The graph backend is **LadybugDB** — a local file-based graph store
that holds an exclusive lock while open. That's why migration scripts
refuse to run when the server is up.

## The HTTP API

codegraph runs as `node dist/cli/index.js serve` on port `:4747` by
default. You can either let `make up` do this, or run it yourself.

devrouter consumes exactly **four** of these endpoints in steady
state:

| Endpoint | Used by | Purpose |
|----------|---------|---------|
| `POST /api/search` | `Client.Search` | Hybrid (BM25 + embedding) search across symbols |
| `POST /api/query` | `Client.Cypher` | Read-only Cypher for callers / callees / impact / siblings |
| `GET /api/file` | `Client.ReadFile` | Read source file content for snippet assembly |
| `GET /api/repos` | `Client.ListRepos` | List indexed repos (used by `dev_context` to validate `repo`) |

Plus the analyze lifecycle endpoints, which `./devrouter analyze`
uses under the hood:

| Endpoint | Purpose |
|----------|---------|
| `POST /api/analyze` | Start an analysis job |
| `GET /api/analyze/:jobId` | Poll job status |
| `GET /api/analyze/:jobId/progress` | SSE stream of progress events |
| `DELETE /api/analyze/:jobId` | Cancel a running job |
| `GET /api/heartbeat` | SSE liveness ping |
| `GET /api/info` | Server metadata |

Everything else (cypher write paths, web UI endpoints, MCP shim) was
removed from the upstream fork. See
[`codegraph/README.md`](../codegraph/README.md) for the full
"what was kept / what was removed" list — that file is the
maintainer-facing view.

## CLI

The codegraph CLI (`codegraph` after `npm install`, or `node
dist/cli/index.js` after build) has six commands. devrouter exposes
the most common ones via its own binary so you don't have to drop
into `cd codegraph`.

| codegraph CLI | devrouter equivalent | Purpose |
|---------------|----------------------|---------|
| `codegraph analyze [path]` | `./devrouter analyze [path]` | Index a repo (full analysis). Required once per repo. |
| `codegraph index [path...]` | (not exposed) | Register an existing `.codegraph/` folder into the global registry without re-analysing. Used when you copy a pre-built index across machines. |
| `codegraph serve` | `make up` (started for you) | Run the HTTP server on `:4747`. |
| `codegraph list` | `./devrouter list` | List all indexed repos. |
| `codegraph status` | `./devrouter status` | Index status + health for the current repo. |
| `codegraph clean` | (not exposed) | Delete the `.codegraph/` index for one repo, or `--all`. |

### Common `analyze` flags

```
--force               Re-index from scratch even if up to date
--embeddings          Generate symbol embeddings for semantic search (off by default)
--skills              Generate repo-specific skill files from detected communities
--skip-git            Index a folder that isn't a git repo
--skip-agents-md      Don't update the codegraph section of AGENTS.md / CLAUDE.md
--no-stats            Omit volatile counts from AGENTS.md / CLAUDE.md
-v, --verbose         Print ingestion warnings
```

Embeddings off by default because they cost time at index and disk
at rest, and devrouter's search cascade gets most of the benefit
from BM25 + structure alone. Turn them on with `--embeddings` when
you want true semantic-only fallback (rule 5 of `retrieval-rules.md`
covers when this kicks in).

## Settings

| Variable | Default | Effect |
|----------|---------|--------|
| `CODEGRAPH_URL` | `http://localhost:4747` | Where devrouter expects codegraph to be reachable. Override for hosted setups. (`GITNEXUS_URL` is an accepted legacy alias.) |
| `CODEGRAPH_HOME` | `~/.codegraph` | Global storage root. Move this when you want indices on a faster disk. |
| `CODEGRAPH_NO_GITIGNORE` | unset | When set, skip `.gitignore` during analyze. Still reads `.codegraphignore`. |
| `CODEGRAPH_VERBOSE` | unset | Verbose ingestion warnings. |
| `CODEGRAPH_DEBUG` | unset | Extra diagnostics in the analyzer. |
| `CODEGRAPH_EMBEDDING_URL` | unset | OpenAI-compatible embeddings endpoint. Required if `--embeddings` is on. |
| `CODEGRAPH_EMBEDDING_MODEL` | unset | Embedding model id (e.g. `text-embedding-3-small`). |
| `CODEGRAPH_EMBEDDING_DIMS` | unset | Vector dimensions, matched to the model. |
| `CODEGRAPH_EMBEDDING_API_KEY` | unset | Bearer token for the embeddings endpoint. |

Every `CODEGRAPH_*` var has a `GITNEXUS_*` legacy alias that still
works; the new name wins when both are set.

## Re-indexing

Re-run `./devrouter analyze /path/to/repo` after substantial repo
changes:

- New top-level packages or large refactors of existing ones.
- Imports / call edges shifting (e.g. you split a module).
- New language adoption (a TypeScript-only repo started shipping
  Python, etc.).

You don't need to re-index after every commit. devrouter falls back
gracefully when it queries a symbol that's been renamed or deleted —
it just returns nothing for that node and the relevance gate
(`retrieval-rules.md` Section 6) drops it. Stale `.codegraph/` from
a few weeks ago is usually fine; stale `.codegraph/` from before a
package rename is not.

`--force` is rarely needed — analyze is incremental by default,
keyed off the last commit hash recorded in `meta.json`.

## When something is wrong

`make status` shows the codegraph health line. If it says `DOWN`:

1. `tail /tmp/devrouter-codegraph.log` to see why it died.
2. `make codegraph` to restart just the indexer.
3. If LadybugDB complains about a lock file, kill any stale
   `node dist/cli/index.js serve` process and try again.

If `dev_context` returns no symbols for a repo you indexed:

1. `./devrouter list` — is the repo in the registry?
2. If yes, hit `curl localhost:4747/api/repos` directly and confirm
   the same.
3. If yes, run `curl -X POST localhost:4747/api/search -d '{"query":"<known
   symbol>","repo":"<your-repo>"}' -H 'content-type: application/json'`
   to bypass devrouter and see whether the index has it.
4. Empty result there → re-run `./devrouter analyze` with `-v` and
   read the warnings.

See [`troubleshooting.md`](troubleshooting.md) for the symptom-to-fix
table.

## Migrating from `gitnexus/`

If you were using devrouter before the gitnexus → codegraph rename,
the on-disk paths used to be `~/.gitnexus/` and `<repo>/.gitnexus/`.
Run once:

```bash
make codegraph-migrate
```

That script:

- Renames `~/.gitnexus/` → `~/.codegraph/` (or honours
  `$GITNEXUS_HOME` / `$CODEGRAPH_HOME`).
- Walks every repo in the global registry and renames its
  `.gitnexus/` directory to `.codegraph/`.
- Rewrites `storagePath` fields in `registry.json`.
- Patches `.gitignore` files that pinned `.gitnexus`.

Refuses to run while a server is listening on the codegraph port
(LadybugDB lock). Stop with `make down` first.

Backwards compat: if you can't migrate immediately, the runtime
still recognises legacy `.gitnexus/` directories and `GITNEXUS_*`
env vars, so existing checkouts keep working.

## Going deeper

- [`codegraph/README.md`](../codegraph/README.md) — maintainer view:
  what was forked from gitnexus, what was kept, what was removed,
  the full HTTP endpoint list (including analyze lifecycle).
- [`codegraph/CHANGELOG.md`](../codegraph/CHANGELOG.md) — release
  history of the slim fork.
- [`codegraph/src/`](../codegraph/src/) — TypeScript source. Start
  at `src/cli/index.ts` for command wiring and
  `src/core/ingestion/pipeline.ts` for the analyze pipeline.
- [`internal/codegraph/client.go`](../internal/codegraph/client.go)
  — devrouter's Go HTTP client. The four `Client.*` methods listed
  in the API table above are all defined here, with the exact
  request/response shapes devrouter uses.
- [`retrieval-rules.md`](retrieval-rules.md) Sections 5, 7 — when
  devrouter calls codegraph during a `dev_context` request and what
  it does with each response.
