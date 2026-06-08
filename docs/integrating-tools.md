# Integrating new retrieval tools

This guide explains how to add a new **external retrieval tool** to
devrouter — a documentation search, issue tracker, wiki, or any other
read-only source whose results ride alongside the native memory +
codegraph context in a `dev_context` response.

The design goal is "every tool is at the same level": memory, codegraph,
and every external tool satisfy the same `retrieval.Source` contract, and
the router holds them in one registry. **For an MCP or OpenAPI tool,
adding one is a single config entry pointing at its endpoint — no Go
code, no mapper.**

For the *runtime* knobs (env vars, the cmdocs sidecar lifecycle) see
[`configuration.md`](configuration.md). For the agent-facing tool surface
(`dev_context`, `dev_feedback`, …) see [`tools.md`](tools.md). For where
this sits in the request flow see [`architecture.md`](architecture.md).

---

## 1. How it fits together

```
dev_context query
   │
   ├─ memory   (native, inline)  ── produces Signals (recalled paths/symbols/terms)
   ├─ codegraph(native, inline)  ── symbols / snippets / call_chain / graph
   │
   └─ external Sources (parallel fan-out, each under a per-tool timeout)
        ├─ cmdocs   → []DocEntry ┐
        ├─ gitlab   → []DocEntry ├─→ DevPrompt.Documentation
        └─ <yours>  → []DocEntry ┘
```

Key properties:

- **Read-only.** Sources only *search*; they never mutate state.
- **Self-describing.** MCP tools are discovered via `tools/list`; OpenAPI
  tools are read from their spec. You point devrouter at the endpoint and
  it figures out the tool name + query argument.
- **Generic mapping.** A single shape-agnostic normalizer turns any
  MCP `structuredContent` / REST JSON into `DocEntry`s, so no per-tool
  mapper is needed.
- **Non-blocking.** Each source runs in its own goroutine under
  `DEVROUTER_SOURCE_TIMEOUT_MS` (default 8s). A slow or failing tool
  contributes nothing and never stalls the response.
- **Off by default.** A source is registered only when its config is
  present, so a default install fans out to nothing and pays nothing.
- **Self-tuning.** Each source learns its own result breadth over time
  (see [§5](#5-sources-learn-their-own-breadth)).
- **Aimed by memory.** The query handed to each tool is augmented with
  memory's recalled signal terms/paths (see [§6](#6-query-composition)).

The relevant code:

| Concern | File |
|---------|------|
| The `Source` / `Request` / `Result` contract | `internal/retrieval/retrieval.go` |
| The generic config-driven source | `internal/mcpsource/source.go` |
| Transports (http-json, mcp-http, mcp-stdio, openapi) | `internal/mcpsource/transport.go`, `internal/mcpsource/openapi.go` |
| Generic + named response mappers | `internal/mcpsource/mappers.go` |
| JSON tools-config parser | `internal/mcpsource/config.go` |
| Registration from env | `buildExternalSources()` in `internal/router/router.go` |
| Parallel fan-out + breadth | `fetchDocSources()` in `internal/router/source_adapters.go` |
| Per-source breadth bandit | `internal/heuristics/sourcebandit.go` |

---

## 2. The fast path — `DEVROUTER_TOOLS_CONFIG` (no code)

Point `DEVROUTER_TOOLS_CONFIG` at a JSON file holding an array of tool
configs. Each entry is loaded at startup; a bad entry is logged and
skipped, never fatal. For an MCP or OpenAPI tool the common case is a
single line — a name, a transport, and an endpoint:

```json
[
  { "name": "clickup", "transport": "mcp-stdio", "endpoint": "clickup-mcp" },

  { "name": "wiki", "transport": "mcp-http", "endpoint": "https://wiki.internal/mcp",
    "headers": { "Authorization": "Bearer $WIKI_TOKEN" } },

  { "name": "petstore", "transport": "openapi",
    "endpoint": "https://api.internal/openapi.json",
    "headers": { "Authorization": "Bearer $API_TOKEN" } },

  { "name": "docs", "transport": "http-json",
    "endpoint": "http://localhost:8099/search", "query_arg": "query" }
]
```

What happens for each transport when only `endpoint` is given:

- **`mcp-stdio` / `mcp-http`** — devrouter calls `tools/list`, picks the
  search-like tool (name/description matching `search|query|find|lookup|
  retrieve`, else the only tool), and infers the query argument from that
  tool's input schema (`query|q|search|term|text|keywords`, else the
  first required string property).
- **`openapi`** — devrouter loads the spec and picks the search-like
  `GET` operation (or the one whose `operationId` matches `tool_name` if
  you set it), binds the query to its query parameter, and calls it.
- **`http-json`** — a plain `POST` of the args as a JSON body. This one
  isn't self-describing, so set `query_arg` (and any `extra_args`).

In all cases the response is normalized by the **generic mapper** (see
[§4](#4-the-generic-mapper)), so no per-tool code is required.

### JSON fields

Mirrors `mcpsource.Config` with snake_case keys; only `name`,
`transport`, and `endpoint` are required.

| Field | Purpose |
|-------|---------|
| `name` | Stable label used in traces (`tool_stages`) and the `devrouter_retrieval_source_requests_total{source}` metric. |
| `transport` | `mcp-stdio` \| `mcp-http` \| `openapi` \| `http-json`. |
| `endpoint` | URL (http/openapi) or command line (stdio). |
| `headers` | Auth/other headers for http transports. |
| `env` | Extra `K=V` env entries for stdio transports. |
| `tool_name` | MCP tool / OpenAPI `operationId`. Auto-discovered when omitted. |
| `query_arg` | Argument carrying the query. Auto-discovered (or `query`). |
| `limit_arg` | Argument carrying the result cap (auto: `max_docs`/`limit`). |
| `extra_args` | Static args merged into every call. |
| `mapper` | Omit for the generic normalizer; set only to force a named mapper. |
| `max_docs` | Per-call `DocEntry` cap (default 5; also the breadth-bandit seed). |
| `timeout_ms` | Per-call timeout in ms. Omit to inherit `DEVROUTER_SOURCE_TIMEOUT_MS` (default 8000); set it to override the budget for this one slow/fast tool. |

When does an entry need more than the endpoint?

- **Ambiguous MCP server** (many tools, or a non-standard query arg
  name): set `tool_name` and/or `query_arg`.
- **Static parameters**: add `extra_args` (e.g. `{"space_id": "..."}`).
- **Caps**: `max_docs`.
- **Slow backends**: `timeout_ms` (see [§8](#8-limits--safety) — a tool
  slower than the budget is silently cut off).

---

## 3. The `Source` contract

Every retrieval tool implements:

```go
type Source interface {
    Name() string                                          // stable, low-cardinality (trace/metric label)
    Search(ctx context.Context, req Request) (Result, error)
}
```

You almost never implement this directly. `mcpsource.Source` is the
generic implementation covering any tool reachable over one of the
supported transports. You configure it; you don't rewrite it.

`mcpsource.New` validates config at startup — empty name/endpoint, an
unknown transport, OpenAPI specs with no usable search operation, etc.
return an error so misconfiguration fails loudly at boot.

### Transports

| Constant | Wire | Self-describing? |
|----------|------|------------------|
| `TransportMCPStdio` | MCP JSON-RPC 2.0 over a long-lived subprocess (newline-delimited stdin/stdout). `endpoint` is the command line. | Yes (`tools/list`). |
| `TransportMCPHTTP` | MCP JSON-RPC 2.0 over Streamable HTTP. Handles the `initialize` handshake, `Mcp-Session-Id`, and both `application/json` and SSE responses. | Yes (`tools/list`). |
| `TransportOpenAPI` | A single `GET` against a REST API described by an OpenAPI spec; the query is bound to a query parameter. | Yes (the spec). |
| `TransportHTTPJSON` | Plain HTTP `POST` of the args as a JSON body; raw response returned to the mapper. No framing. | No — set `query_arg`. |

For MCP/OpenAPI, the source consumes `structuredContent` when present
(MCP 2025-06-18), falling back to the concatenated text content blocks,
then to the raw body — all of which the generic mapper handles.

---

## 4. The generic mapper

A mapper turns the tool's raw text/JSON result into `DocEntry`s. The
default `"generic"` mapper is shape-agnostic and is used whenever
`mapper` is omitted, so **new tools need no mapper code.**

`prompt.DocEntry` (see `internal/prompt/types.go`):

```go
type DocEntry struct {
    Source     string // tool name (stamped automatically)
    ID         string // doc_id / issue iid / task id (optional)
    Title      string // human title (optional)
    Collection string // doc collection / project / list (optional)
    URL        string // canonical link when available (optional)
    Content    string // body — capped at maxDocContent = 4000 chars
}
```

The generic mapper handles, in order: a JSON array of objects, an object
wrapping an array (`items`/`results`/`data`/`documents`/`issues`/… keys),
a single object, and finally plain text (surfaced as one entry so nothing
is dropped). Per item it resolves field aliases:

| DocEntry field | Source keys (priority order) |
|----------------|------------------------------|
| `ID` | `id`, `iid`, `doc_id`, `key`, `number` |
| `Title` | `title`, `name`, `summary`, `heading`, `doc_name` |
| `URL` | `url`, `web_url`, `html_url`, `link`, `permalink` |
| `Collection` | `collection`, `project`, `list`, `repository`, `namespace`, `references.full` |
| `Content` | `content`, `description`, `body`, `text`, `snippet`, `excerpt`, or joined `sections[].content` |

**When a named mapper is still worth it:** only if a tool's payload is so
irregular that the generic normalizer mislabels `Title`/`URL`/`Content`.
In that case add a `func(text string) ([]prompt.DocEntry, error)` to
`mappers.go`, register it in the `mappers` map, and set `"mapper": "..."`
in the config. The `cmdocs`/`gitlab` mappers remain only for back-compat
and are not required for new tools.

---

## 5. Sources learn their own breadth

External sources participate in devrouter's repeat → expand → learn loop.
A per-source **breadth bandit** (`internal/heuristics/sourcebandit.go`)
tunes one integer per `(intent, repo, topic, source)` cell: how many docs
that source returns (`SourceDocsBounds` = `{2, 15}`, seeded from the
tool's `max_docs`). It reuses the codegraph bandit's ε-perturb /
K-sample-promote / 3-strike-rollback shape.

- **Credit assignment.** The query reward is a single scalar, so at most
  **one source explores** a perturbed breadth per query; the rest run at
  their learned value. The reward (from `dev_feedback` or implicit-repeat
  detection) routes to that one explored cell via the trace's
  `src_explore_*` fields.
- **Immediate widening.** On a repeat-exploration the request's `Expand`
  flag is set and every source's breadth is bumped (~+50%, clipped) for
  that call, on top of the slower bandit learning.
- **Gated.** The bandit only perturbs when enabled via
  `DEVROUTER_HEURISTICS_BANDIT=all` or `...=source_docs`; default installs
  serve each source's static `max_docs`.

This is automatic for every registered source — no per-tool wiring.

---

## 6. Query composition

Before calling a tool, the source augments the raw query with a few of
memory's recalled signals (`composeQuery` in `source.go`): up to **5**
signal terms and **3** recalled paths, deduplicated. This aims the
external search at the concepts memory surfaced, without drowning the
tool's own ranking.

When the breadth bandit (or a repeat) sets a per-call cap, it is passed
as the tool's `limit_arg` (default `max_docs`) so the tool returns more,
not just trims less.

---

## 7. The escape hatch — a bespoke transport

Implement `retrieval.Source` from scratch only if your tool needs a
protocol none of the transports cover (rare). Satisfy the contract's two
rules: `Name()` must be stable and low-cardinality (it's a metric label),
and `Search` must degrade gracefully — return an empty `Result` rather
than an error when the backend is simply empty, and respect `ctx`
cancellation. Register it in `buildExternalSources()`.

---

## 8. Limits & safety

- **Per-tool timeout:** `DEVROUTER_SOURCE_TIMEOUT_MS` (default 8000ms)
  bounds **both** the per-tool context deadline in the fan-out and the
  transport's HTTP/RPC client timeout — they're kept in sync so a tool
  can't be cut off by a stale 8s client cap. A tools-config `timeout_ms`
  overrides it for that one tool.
  > **Slow backends cut off silently.** A tool that takes longer than its
  > budget contributes nothing and logs a `context deadline exceeded`
  > warning on its `StageTrace` — the rest of the response is unaffected,
  > so it's easy to miss. cmdocs/PageIndex commonly takes ~9–12s, so raise
  > the budget (e.g. `DEVROUTER_SOURCE_TIMEOUT_MS=15000`, or a per-tool
  > `timeout_ms`) when wiring slow sources, and check `tool_stages` in a
  > `retrieve_debug` run to confirm docs actually returned.
- **Doc count cap:** `max_docs` (default 5), bandit-tunable within
  `{2, 15}`.
- **Content cap:** `maxDocContent` = 4000 chars per `DocEntry`.
- **Response read cap:** transports `io.LimitReader` the body (8 MB).
- **Failure isolation:** an error from one tool is recorded as a warning
  in its `StageTrace` and increments the `error` metric; other tools and
  the native paths are unaffected.

---

## 9. Checklist

- [ ] Tool added to `DEVROUTER_TOOLS_CONFIG` (or a named env block) with
      `name` + `transport` + `endpoint`.
- [ ] For non-self-describing tools (`http-json`): `query_arg` set.
- [ ] For slow tools: budget raised (`DEVROUTER_SOURCE_TIMEOUT_MS` or the
      entry's `timeout_ms`) above the tool's real latency.
- [ ] Only if the payload is irregular: a named mapper added + referenced.
- [ ] Env vars documented in `configuration.md` and `cmd/router/main.go`.
- [ ] `Makefile` sidecar lifecycle wired (only if the tool is a local
      service), optional and non-fatal.
- [ ] `make build` clean; source registers on startup
      (`[router] external retrieval source registered: <name>`) and
      results appear under `documentation` in a `dev_context` response.
```
