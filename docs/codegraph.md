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
[`codegraph-fixes.md`](codegraph-fixes.md).

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
parses with tree-sitter (web-tree-sitter WASM grammars) and stores
the graph in a per-repo SQLite database (with FTS5 for search). The
agent and your IDE never see codegraph.

> **Engine:** codegraph is the MIT-licensed
> [`colbymchenry/codegraph`](https://github.com/colbymchenry/codegraph)
> engine, vendored in-tree under `codegraph/src/` and fronted by a thin
> Node HTTP sidecar (`codegraph/bin` + `codegraph/lib`). It replaced the
> earlier GitNexus fork (PolyForm-Noncommercial). See
> [`codegraph/MIGRATION.md`](../codegraph/MIGRATION.md).

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

Indexing runs in two passes: **extraction** (tree-sitter parses each
file into nodes + intra-file edges) followed by **resolution** (cross-file
imports, calls, heritage, and structural synthesis — e.g. Go implicit
`implements` edges by method-set matching). The result is written to the
repo's SQLite store.

### Languages supported

C, C#, C++, Go, Java, JavaScript, PHP, Python, Ruby, Rust,
TypeScript (with `.tsx` / Vue SFC support).

Optional via tree-sitter: Dart, Kotlin, Swift, COBOL/JCL.

You can index a polyglot repo without flagging the languages — the
ingester picks the right parser per file based on extension.

## Where data lives

| Path | Contents |
|------|----------|
| `<your-repo>/.codegraph/` | Per-repo index: `codegraph.db` SQLite graph store (nodes, edges, FTS5 search index). |
| `~/.codegraph/registry.json` | Global registry. Lists every indexed repo so the HTTP server can find them from any cwd. |
| `~/.codegraph/` (root) | Override with `CODEGRAPH_HOME=/some/path`. Useful for shared dev boxes. |

Storage cost (SQLite `codegraph.db`): a mid-size repo is tens of MB; a
large Go monorepo (~7k files) is on the order of ~150 MB. There is no
embedding store — the bulk is nodes, edges, and the FTS5 index.

The graph backend is **SQLite** — one `codegraph.db` file per repo. The
sidecar opens each repo's DB **read-only** and caches the connection, so
serving and re-indexing don't fight over a lock the way the old
file-graph backend did.

## The HTTP API

The sidecar runs as `node bin/codegraph-sidecar.js serve` on port `:4747`
by default. You can either let `make up` do this, or run it yourself.

Because the engine has no Cypher, the old single `/api/query` endpoint is
replaced by **purpose-built endpoints** — one per graph question the Go
client asks. Steady-state endpoints:

| Endpoint | Used by | Purpose |
|----------|---------|---------|
| `POST /api/search` | `Client.Search` | Hybrid (FTS5/BM25) search across symbols; optional source slicing |
| `POST /api/graph/{callers,callees,upstream}` | `Client.Callers/Callees/Upstream` | Call-graph traversal |
| `POST /api/graph/{importers,importers-by-package}` | `Client.Importers` | Import edges |
| `POST /api/graph/{extends,methods,siblings}` | `Client.Extends/Methods/Siblings` | Heritage (incl. Go implicit impls), members, co-located symbols |
| `POST /api/graph/{cross-wire,related-files,name-hits}` | route/relevance helpers | Route↔handler join, related files, name lookups |
| `POST /api/{file,files,symbols}` | `Client.ReadFile`, memory auto-populate | Source content; file/symbol enumeration |
| `POST /api/repos` | `Client.ListRepos` | List indexed repos (used by `dev_context` to validate `repo`) |

Indexing is **not** an HTTP job anymore — it's the `index` CLI command
(`./devrouter analyze` shells out to it), which builds the SQLite store
synchronously and registers the repo. There are no analyze-job /
heartbeat / web-UI endpoints. See
[`codegraph/README.md`](../codegraph/README.md) and
[`codegraph/MIGRATION.md`](../codegraph/MIGRATION.md) for the full
endpoint list and the data-model mapping.

## CLI

The sidecar CLI is `node bin/codegraph-sidecar.js <serve|index|repos>`.
devrouter exposes the common operations via its own binary so you don't
have to drop into `cd codegraph`.

| sidecar CLI | devrouter equivalent | Purpose |
|-------------|----------------------|---------|
| `codegraph-sidecar index [path] --name <n>` | `./devrouter analyze [path]` | Index a repo into SQLite + register it. Required once per repo. |
| `codegraph-sidecar serve` | `make up` (started for you) | Run the HTTP sidecar on `:4747`. |
| `codegraph-sidecar repos` | `./devrouter list` | List all indexed repos. |

The vendored engine also ships its own full-featured CLI
(`node dist/bin/codegraph.js`, the upstream `codegraph` binary) for
direct use, but devrouter only relies on the sidecar wrapper above.

> **Search mode:** the engine is **FTS5/BM25 + structural** — there is no
> built-in vector/embedding search in this version, so `SearchModeForIntent`
> remaps a `semantic` intent to `hybrid`. The `CODEGRAPH_EMBEDDING_*`
> settings below are inert until an embeddings layer is added to the
> sidecar.

## Settings

| Variable | Default | Effect |
|----------|---------|--------|
| `CODEGRAPH_URL` | `http://localhost:4747` | Where devrouter expects the codegraph sidecar to be reachable. Override for hosted setups. (`GITNEXUS_URL` is an accepted legacy alias.) |
| `CODEGRAPH_HOME` | `~/.codegraph` | Global storage root holding `registry.json`. Move this when you want the registry on a faster disk or to isolate environments. (`GITNEXUS_HOME` legacy alias honoured.) |

`CODEGRAPH_URL` and `CODEGRAPH_HOME` are the only settings devrouter +
the sidecar consume. There are **no embedding settings** — the MIT
engine has no vector search (the old `CODEGRAPH_EMBEDDING_*` /
`--embeddings` knobs are gone). Indexing always reads `.gitignore`; the
old `CODEGRAPH_NO_GITIGNORE` / `CODEGRAPH_VERBOSE` / `CODEGRAPH_DEBUG`
analyze knobs were part of the previous engine and no longer apply.

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
2. `make codegraph` to restart just the sidecar.
3. If a repo won't open, kill any stale
   `node bin/codegraph-sidecar.js serve` process and try again. The DBs
   open read-only, so a crashed serve doesn't normally leave a lock.

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

## Migrating from the GitNexus engine

The graph engine was replaced (GitNexus/LadybugDB →
[`colbymchenry/codegraph`](https://github.com/colbymchenry/codegraph)/SQLite,
MIT). The **on-disk store format changed**, so any `.codegraph/` (or legacy
`.gitnexus/`) index built by the old engine must be rebuilt:

```bash
make codegraph-migrate   # prints what to do
./devrouter analyze /abs/path/to/your-repo   # re-index, once per repo
```

`make codegraph-migrate` no longer does an in-place data migration — there
is no automatic LadybugDB → SQLite converter; it just walks the registry
and tells you which repos need a re-index.

Backwards compat: `CODEGRAPH_HOME` still honours the legacy `GITNEXUS_HOME`
env var (and `CODEGRAPH_URL`/`GITNEXUS_URL`), and the sidecar still reads
the existing `~/.codegraph/registry.json` layout, so paths and config carry
over — only the per-repo graph data must be regenerated.

## Going deeper

- [`codegraph/README.md`](../codegraph/README.md) — maintainer view:
  sidecar layout, the full HTTP endpoint list, and build/run notes.
- [`codegraph/MIGRATION.md`](../codegraph/MIGRATION.md) — engine
  data-model mapping, spike findings, and graph-coverage validation.
- [`codegraph/src/`](../codegraph/src/) — vendored MIT engine
  (TypeScript). Extraction lives under `src/extraction/`, cross-file
  resolution under `src/resolution/`. The sidecar wrapper is
  `codegraph/bin/` + `codegraph/lib/`.
- [`internal/codegraph/client.go`](../internal/codegraph/client.go)
  — devrouter's Go HTTP client. The four `Client.*` methods listed
  in the API table above are all defined here, with the exact
  request/response shapes devrouter uses.
- [`retrieval-rules.md`](retrieval-rules.md) Sections 5, 7 — when
  devrouter calls codegraph during a `dev_context` request and what
  it does with each response.
