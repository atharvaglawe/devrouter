# codegraph (devrouter slim graph engine)

This directory is a **vendored, slimmed-down fork** of the upstream
[gitnexus](https://github.com/abhigyanpatwari/GitNexus) project, embedded
inside devrouter and renamed to **codegraph** to reflect the much smaller
scope. devrouter and codegraph are released as a single repo; the two no
longer need to be installed separately.

What was kept:

- Tree-sitter ingestion pipeline (`src/core/ingestion/`)
- LadybugDB graph storage and adapter (`src/core/lbug/`)
- Hybrid / BM25 / semantic search (`src/core/search/`, `src/core/embeddings/`)
- Storage / repo registry (`src/storage/`)
- A trimmed CLI (`analyze`, `index`, `serve`, `list`, `status`, `clean`)
- A trimmed HTTP server (`src/server/api.ts`) exposing the four endpoints
  devrouter actually consumes plus the `/api/analyze/*` job endpoints:

  ```
  GET  /api/heartbeat              SSE liveness
  GET  /api/info                   server metadata
  GET  /api/repos                  list registered repos
  POST /api/query                  read-only Cypher
  POST /api/search                 hybrid / bm25 / semantic search
  GET  /api/file                   read source file
  POST /api/analyze                start analysis job
  GET  /api/analyze/:jobId         poll analysis job
  GET  /api/analyze/:jobId/progress SSE progress
  DELETE /api/analyze/:jobId       cancel analysis job
  ```

What was removed:

- `gitnexus-web` (React/Vite SPA), `gitnexus-claude-plugin`,
  `gitnexus-cursor-integration`, `eval/` (Python harness)
- `core/wiki/`, `core/group/` (multi-repo), `core/augmentation/`
- The MCP layer (`mcp/`, `mcp-http.ts`, the `gitnexus mcp` CLI command) —
  devrouter is itself the MCP server
- `setup` (would write an MCP config pointing at `gitnexus mcp`, which
  no longer exists in this fork)
- `query`, `context`, `impact`, `cypher`, `wiki`, `augment`, `eval-server`
  CLI commands and the `LocalBackend`-driven `/api/processes`,
  `/api/clusters`, `/api/grep`, `/api/graph`, `/api/embed`, `/api/repo`
  HTTP endpoints — all UI-facing
- `gitnexus-shared` is folded in here as `src/_shared/`

## Use

From the devrouter repo root:

```bash
make codegraph-install                  # one-time npm install
make codegraph-build                    # compile TS -> dist/
make codegraph-serve                    # serve on :4747
make codegraph-analyze REPO=/abs/path   # ingest a repo into the local LadybugDB
```

Or directly:

```bash
cd codegraph
node dist/cli/index.js --help
```

The same commands are also available through the devrouter binary:

```bash
devrouter analyze /abs/path
devrouter list
devrouter status
```

## Migrating from `gitnexus/`

If you were using the older layout where this directory was called
`gitnexus/` and per-repo data lived under `.gitnexus/`, run:

```bash
make codegraph-migrate
```

That script:

- Renames `~/.gitnexus/` to `~/.codegraph/` (or honours `$GITNEXUS_HOME` /
  `$CODEGRAPH_HOME` if set).
- Walks every repo in the global registry and renames its `.gitnexus/`
  directory to `.codegraph/`.
- Rewrites `storagePath` fields in `registry.json` to point at the new
  location.
- Patches `.gitignore` files that pinned `.gitnexus` so the new name is
  also ignored.

It refuses to run while a server is listening on the codegraph port
(LadybugDB holds an exclusive file lock). Stop the server with
`make down` first.

Backwards compat: if for any reason you can't migrate immediately, the
runtime still recognises the legacy `.gitnexus/` directories and the
`GITNEXUS_*` env vars, so existing checkouts keep working.

## Backwards-compat env vars

| Preferred | Deprecated | Effect |
|-----------|------------|--------|
| `CODEGRAPH_HOME` | `GITNEXUS_HOME` | Override the global storage root |
| `CODEGRAPH_NO_GITIGNORE` | `GITNEXUS_NO_GITIGNORE` | Skip `.gitignore` parsing during analyze |
| `CODEGRAPH_VERBOSE` | `GITNEXUS_VERBOSE` | Verbose ingestion warnings |
| `CODEGRAPH_DEBUG` | `GITNEXUS_DEBUG` | Print extra diagnostics in the analyzer |
| `CODEGRAPH_EMBEDDING_URL` | `GITNEXUS_EMBEDDING_URL` | OpenAI-compatible embeddings endpoint |
| `CODEGRAPH_EMBEDDING_MODEL` | `GITNEXUS_EMBEDDING_MODEL` | Embedding model id |
| `CODEGRAPH_EMBEDDING_DIMS` | `GITNEXUS_EMBEDDING_DIMS` | Vector dimensions |
| `CODEGRAPH_EMBEDDING_API_KEY` | `GITNEXUS_EMBEDDING_API_KEY` | Embeddings API key |

Both forms work; the new ones win when both are set.
