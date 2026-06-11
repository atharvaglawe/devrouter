# codegraph engine migration: GitNexus (PolyForm-NC) -> colbymchenry/codegraph (MIT)

This directory used to vendor a fork of
[GitNexus](https://github.com/abhigyanpatwari/GitNexus), which is licensed
**PolyForm Noncommercial 1.0.0** and therefore cannot be deployed commercially.

It now hosts the MIT-licensed
[`colbymchenry/codegraph`](https://github.com/colbymchenry/codegraph)
engine **vendored in-tree** under `src/` (cloned, git history removed, kept to
the minimal build set: `src/`, `tsconfig.json`, `package.json`,
`package-lock.json`, `LICENSE`) plus a thin Node **sidecar** (`bin/` + `lib/`)
that wraps the compiled engine and re-exposes the small HTTP API that
devrouter's Go client (`internal/codegraph`) consumes on `:4747`. No GitNexus
source ships here anymore.

Vendoring (vs an npm dependency) keeps the engine MIT-licensed and in-tree, so
its extractors/resolvers can be patched directly if a coverage gap appears.

## Feasibility note (spike result: GO)

The new engine is a Node library/CLI/MCP over **SQLite + FTS5** — there is
no HTTP server and no Cypher. The sidecar bridges that gap.

Confirmed by an end-to-end smoke test (index a tiny repo, open it
read-only, run queries):

- `CodeGraph.init(root, { index: true })` indexes into
  `<root>/.codegraph/codegraph.db`.
- `getDatabasePath(root)` -> `DatabaseConnection.open(dbPath).getDb()` ->
  `new QueryBuilder(db)` exposes `searchNodes`, `getNodeById`,
  `getNodesByName`, `getNodesByFile`, `getOutgoingEdges(id, kinds)`,
  `getIncomingEdges(id, kinds)`, `getAllFiles`, etc.
- Raw SQL is available via `db.prepare(sql).all(...)` / `.get(...)`
  (node:sqlite), used for the route manifest and a few aggregates.

### Schema / vocabulary

- Node kinds: `file, module, class, struct, interface, trait, protocol,
  function, method, property, field, variable, constant, enum, enum_member,
  type_alias, namespace, parameter, import, export, route, component`.
- Edge kinds: `contains, calls, imports, exports, extends, implements,
  references, type_of, returns, instantiates, overrides, decorates`.

### Mapping of devrouter's graph queries

| devrouter (old, Cypher) | new engine |
|---|---|
| `Search` / `SearchWithMode` | `searchNodes(query, {limit, kinds, languages})` (FTS5 + LIKE + fuzzy + BM25) |
| `include_source` | slice file by `start_line`/`end_line` (bodies are not stored) |
| `CallersWithPath` | `getIncomingEdges(id, ['calls'])` |
| `CalleesWithPath` | `getOutgoingEdges(id, ['calls'])` |
| `Extends` (struct embed / iface impl) | `getIncomingEdges`/`getOutgoingEdges` over `['extends','implements']` |
| `Methods` (HAS_METHOD) | `getOutgoingEdges(id, ['contains'])` filtered to `method`/`function` |
| `Importers` / `ImportersByPackage` | `getIncomingEdges(id, ['imports','references','calls'])` / file-dependents |
| `CrossWire*` (FETCHES + HANDLES_ROUTE) | `route` nodes JOIN `edges kind IN ('references','calls')` -> handler |
| `SearchByFilePath` | `getNodesByFile(path)` + `file_path LIKE` fallback |
| `SearchByNameWithOpts` | `searchNodes` + name `LIKE`, must/exclude/context applied client-side |
| `NameHitCount` | `SELECT count(*) ... WHERE lower(name) LIKE ?` |
| `RelatedFiles` | `SELECT DISTINCT file_path ... WHERE lower(file_path) LIKE ?` |
| `ReadFile` | read file from disk (repo path) |
| `ListRepos`/`RepoPath`/`TopLevelDirs` | devrouter-owned registry + readdir |

### Graph-coverage validation (Go + PHP fixture)

Validated against an indexed mixed Go/PHP fixture:

- **Switch / type-switch call edges** — covered (`calls` edges captured for
  calls inside `switch` and Go type-switch bodies).
- **HAS_METHOD** — covered (`contains` -> method/function children).
- **Go interface -> implementation** — covered out of the box. The engine
  extracts interface method specs (`extractGoInterfaceMethods`) and synthesizes
  the implicit `implements` edge by structural method-set matching
  (`goImplementsEdges`, issue #584). Surfaced via `/api/graph/extends`.
- **Callers / callees** — covered.

No extractor patch was required; the vendored engine already handles the
cases the migration flagged as risky.

### Known gaps

- **No semantic / vector search** (FTS5 only). `SearchModeForIntent` remaps
  `semantic` to `hybrid`; an embeddings layer can be added to the sidecar later.
- **No global repo registry** in the engine (per-project `.codegraph/`), so
  the sidecar owns `registry.json` (same format devrouter's
  `internal/crossrepo` already reads).
- **Cross-wire / framework routes** are engine-framework-dependent. Route
  nodes appear only when the engine's framework resolvers (e.g. Laravel, Gin)
  recognise the project; a minimal fixture without the full framework layout
  produced none. `/api/graph/cross-wire` joins `route -> handler` whenever
  route nodes exist; the caller->route ("FETCHES") side is best-effort and
  should be re-validated on the real mega repo.
- Repos indexed by the old GitNexus engine must be **re-indexed** (LadybugDB
  -> SQLite store change).
