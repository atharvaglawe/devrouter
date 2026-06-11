# codegraph (devrouter sidecar)

This directory is a thin **HTTP sidecar** that wraps the MIT-licensed
[`@colbymchenry/codegraph`](https://github.com/colbymchenry/codegraph)
engine and re-exposes the small API devrouter's Go client
(`internal/codegraph`) consumes on `:4747`.

It replaces the previous vendored fork of
[GitNexus](https://github.com/abhigyanpatwari/GitNexus), which was licensed
**PolyForm Noncommercial 1.0.0** and could not be deployed commercially.

The MIT engine source is **vendored in-tree** under `src/` (cloned from
`colbymchenry/codegraph`, git history stripped) so it can be patched if a
coverage gap ever shows up. It compiles to `dist/` via `npm run build`
(`tsc` + WASM/schema copy); the sidecar (`bin/` + `lib/`) imports the
compiled engine from `dist/`.

See [`MIGRATION.md`](MIGRATION.md) for the data-model mapping, spike findings,
and graph-coverage validation.

## Layout

```
src/                       vendored MIT engine (TypeScript) — compiles to dist/
LICENSE                    the engine's MIT license (retained)
tsconfig.json              engine build config (tsc: src/ -> dist/)
bin/codegraph-sidecar.js   sidecar CLI: serve | index | repos
lib/server.js              HTTP server + route table (Node built-in http)
lib/handlers.js            endpoint logic (QueryBuilder + raw SQL)
lib/pool.js                per-repo DB connection cache (read-only)
lib/registry.js            repo registry (~/.codegraph/registry.json)
lib/indexer.js             index/refresh a repo + register it
```

## Use

You almost never call this directly — `make` manages it. Manually:

```bash
npm install        # engine deps (web-tree-sitter, tree-sitter-wasms, …)
npm run build      # compile the vendored engine: src/ -> dist/
node bin/codegraph-sidecar.js index /abs/path/to/repo --name myrepo
node bin/codegraph-sidecar.js serve --port 4747
```

`index` builds (or refreshes) the repo's `<repo>/.codegraph/codegraph.db`
via the engine and records it in the registry so the sidecar and devrouter's
cross-repo loader can resolve it by name. `serve` starts the HTTP API.

## HTTP API (consumed by `internal/codegraph`)

- `GET  /api/repos` — registered repos `[{name, path, indexedAt}]`
- `GET  /api/file?path=&repo=` — file content
- `POST /api/search` — FTS/hybrid/bm25 search (`{query, repo, limit, include_source}`)
- `POST /api/search-by-name` — plan-aware name search (must/exclude/context)
- `POST /api/search-by-path` — symbols defined in a file
- `POST /api/graph/{callers,callees,upstream}` — call graph
- `POST /api/graph/{importers,importers-by-package,extends,methods}` — structural edges
- `POST /api/graph/cross-wire` — route -> handler (best-effort)
- `POST /api/graph/{siblings,related-files,name-hits}` — file/aggregate helpers

## Requirements

- Node `>=20 <25` (the engine bundles `web-tree-sitter` WASM grammars; the
  sidecar uses Node's built-in `node:sqlite`).

## Backwards-compat

`CODEGRAPH_HOME` / `GITNEXUS_HOME` still resolve the registry root (same
precedence as `internal/crossrepo`). Repos indexed by the old GitNexus engine
must be **re-indexed** with `codegraph-sidecar index` — the on-disk store
changed from LadybugDB to SQLite.
