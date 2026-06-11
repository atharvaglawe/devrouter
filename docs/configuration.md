# devrouter configuration

Every devrouter knob is controlled by an environment variable. The
defaults are sane for a local Redis + bundled ONNX embedder setup;
override only what differs.

## Variable reference

| Variable | Default | Description |
|----------|---------|-------------|
| `DEVROUTER_REDIS` | `localhost:6379` | Redis Stack address (`host:port`). Doesn't have to be local — any reachable Redis Stack (Redis Cloud, in-cluster, shared box) works as long as the **RediSearch** module is loaded. |
| `DEVROUTER_EMBEDDING_URL` | `http://localhost:11435/api/embed` | Embedding endpoint. Must speak the canonical `/api/embed` wire shape (request: `{model, input}`, response: `{embeddings: [[...]]}`). Default points at the bundled Dockerized ONNX embedder (`make embedder-up`). Swap in any compatible service — hosted, in-cluster, or a custom worker — without code changes. |
| `DEVROUTER_EMBEDDING_MODEL` | `nomic-embed-text-v1.5` | Model name passed in the `model` field of every embed request. The bundled embedder ignores this (it serves exactly one model per container), but the value is preserved in log lines for diagnosability. **Must produce 768-dim vectors** — the Redis index is built for that dim and the wrapper rejects mismatches with a clear error. |
| `DEVROUTER_MEMORY_MAX_DISTANCE` | `0.60` | Cosine-distance ceiling for vector recall. Lower = stricter relevance. Distance bands for `nomic-embed-text-v1.5`: `<0.2` paraphrase, `0.2–0.4` same topic, `0.4–0.6` weak topical, `>0.6` incidental. See [`retrieval-rules.md`](retrieval-rules.md) Section 6.1. |
| `DEVROUTER_HEURISTICS_FROZEN` | `false` | When `true`, disables all bandit profile mutations (telemetry still collected). Used during incident recovery and isolated experiments. |
| `DEVROUTER_HEURISTICS_BANDIT` | _(unset)_ | Comma-separated knob names to perturb (`max_trace`, `caller_hops`, …) or `all`. Empty = no perturbation. Include `source_docs` (or `all`) to enable the per-source breadth bandit, which learns how many docs each external source returns per `(intent, repo, topic, source)` cell. See [`integrating-tools.md`](integrating-tools.md) §5. |
| `DEVROUTER_HEURISTICS_TOPICS` | `true` | Master switch for per-(intent, repo, topic) heuristic buckets. When off, every query uses the intent-global profile (today's pre-topic behaviour). Opt-out values: `off`, `none`, `disabled`, `false`, `0`. See [`heuristics.md`](heuristics.md) for the two-tier model. |
| `DEVROUTER_HEURISTICS_MAX_TOPICS` | `32` | Maximum number of topic centroids stored per (intent, repo) bucket. New queries beyond the cap LRU-evict the least-recently-seen centroid. Range `1..256`. Higher = finer-grained tuning at the cost of more bandit fan-out; lower = coarser buckets but faster warm-up. |
| `DEVROUTER_HEURISTICS_NEW_TOPIC_SIM` | `0.65` | Cosine-similarity floor below which a new query spawns a new topic centroid instead of being absorbed into the nearest existing one. Range `0..1`. Lower = coarser clusters (fewer topics), higher = finer clusters (more topics). Tuned for `nomic-embed-text-v1.5`. |
| `DEVROUTER_HEURISTICS_TOPIC_SAMPLE_FLOOR` | `20` | Per-bucket centroid-sample count required before the bandit will perturb a topic-specific profile. Below this floor, queries inherit the intent-global profile so cold buckets never get noisy tuning. Range `0..10000`. Set to `0` to perturb buckets from the first query (not recommended). |
| `DEVROUTER_DASHBOARD_ADDR` | `127.0.0.1:8088` | Bind address for the bundled read-only HTTP dashboard. Localhost-only by default. Override with any `host:port` (e.g. `:9090`). Opt-out values: `off`, `none`, `disabled`, `false`, `0`. See the [Dashboard](#dashboard) section below. |
| `DEVROUTER_METRICS_ADDR` | _(unset = mount on dashboard)_ | Controls the Prometheus `/metrics` endpoint mounted on the dashboard mux. When unset (default), `/metrics` is served alongside the dashboard at the same bind address. Opt-out values: `off`, `none`, `disabled`, `false`, `0`. See the [Telemetry](#telemetry) section below. |
| `DEVROUTER_LOG_FORMAT` | _(empty = text)_ | Slog output format. Set to `json` to emit one JSON object per record on stderr and route legacy `log.Printf` calls through the same handler so every log line is structured. Any other value falls back to the historical text format with the `[devrouter] ` prefix. |
| `DEVROUTER_LOG_LEVEL` | `info` | Minimum slog level. Accepts `debug`, `info`, `warn`, `error` (case-insensitive). Affects both new structured records and the stdlib-bridged `log.Printf` calls when `DEVROUTER_LOG_FORMAT=json`. |
| `DEVROUTER_RETRIEVAL_TRACE` | `on` | Whether the verbose `retrieval_trace` block is embedded in the `dev_context` response. Opt-out values: `off`, `none`, `disabled`, `false`, `0` — these omit it from the payload to save tokens. **Only the response is affected**: the trace is still built, still persisted to Redis for the `dev_feedback` join, the top-level `query_id` still ships (so `dev_feedback` keeps working), and the `retrieve_debug` tool still returns the full trace. |
| `DEVROUTER_RELEASE_BRANCH` | `origin/release` | Git ref the save-time scope detector diffs against to decide whether a memory should be `scope=global` (file matches the team's "shipped truth") or `scope=<branch>` (file diverges). Set to `origin/main` / `origin/master` / `upstream/trunk` / a local `main` if your repo's shipped branch isn't `origin/release`. Accepts any ref `git diff --quiet <ref> -- <file>` understands; if the value is `<remote>/<branch>`, devrouter also runs a best-effort `git fetch <remote> <branch>` before diffing so the baseline is fresh. See [`staleness.md`](staleness.md). |
| `CODEGRAPH_URL` | `http://localhost:4747` | Override only if devrouter's per-repo indexer is running on a non-default port or remote host. (`GITNEXUS_URL` is an accepted legacy alias.) |
| `DEVROUTER_CMDOCS_URL` | _(unset = cmdocs off)_ | cmdocs PageIndex sidecar `/search` URL (e.g. `http://127.0.0.1:8099/search`). When set, registers the `cmdocs` documentation source. See [External retrieval tools](#external-retrieval-tools). |
| `DEVROUTER_CMDOCS_CMD` | _(unset)_ | Alternative cmdocs transport: a stdio MCP command line (e.g. `python3 /path/to/cmdocs/pageindex_mcp.py`). Used only when `DEVROUTER_CMDOCS_URL` is unset. |
| `DEVROUTER_CMDOCS_MAX_DOCS` | `3` | Maximum documents the cmdocs source returns per query. |
| `DEVROUTER_GITLAB_MCP_URL` | _(unset = gitlab off)_ | GitLab MCP Streamable-HTTP endpoint (a PAT-based community server such as `zereight/gitlab-mcp`). When set, registers the `gitlab` source. |
| `DEVROUTER_GITLAB_TOKEN` | _(unset)_ | GitLab Personal Access Token, sent as both `Authorization: Bearer <tok>` and `PRIVATE-TOKEN: <tok>` headers on every GitLab MCP call. |
| `DEVROUTER_GITLAB_TOOL` | `search_issues` | The GitLab MCP tool name DevRouter invokes (read-only search). |
| `DEVROUTER_TOOLS_CONFIG` | _(unset)_ | Path to a JSON file holding an array of tool configs (`mcp-stdio`, `mcp-http`, `openapi`, `http-json`). MCP and OpenAPI tools self-describe (`tools/list` / the spec), so a typical entry is just `{name, transport, endpoint}` — no Go code or mapper. A bad entry is logged and skipped, never fatal. See [`integrating-tools.md`](integrating-tools.md). |
| `DEVROUTER_SOURCE_TIMEOUT_MS` | `8000` | Per-external-tool call timeout in milliseconds. Every registered tool runs in parallel under this bound (both the context deadline **and** the transport HTTP/RPC client timeout), so one slow/hung server can never stall a `dev_context` response. Raise it for slow backends (e.g. cmdocs/PageIndex can take ~9–12s). A `DEVROUTER_TOOLS_CONFIG` entry's explicit `timeout_ms` overrides this for that tool. |

> **There is no in-process query planner.** Query planning is the MCP
> caller's responsibility — the agent fills in the optional `plan` field
> on `dev_context` with structured retrieval terms. devrouter ships no
> LLM, no Ollama dependency, and no `DEVROUTER_PLANNER*` env vars. See
> [`agent-rules.md`](agent-rules.md) for the schema and worked examples.

## MCP-host config example

Minimal local setup. For Cursor, add this to `.cursor/mcp.json`
(project-level) or `~/.cursor/mcp.json` (global). Same shape works for
Claude Code (`~/.claude/mcp.json`), Codex CLI, and other MCP hosts:

```json
{
  "mcpServers": {
    "devrouter": {
      "command": "/path/to/devrouter",
      "args": []
    }
  }
}
```

That's the whole config for the default path — devrouter dials
`localhost:6379` for Redis and `localhost:11435` for the bundled
embedder out of the box. Add an `env` block only when you need to
override:

```json
"env": {
  "DEVROUTER_REDIS": "my-cluster.cache.redis:6379",
  "DEVROUTER_EMBEDDING_URL": "https://your-embedder/api/embed",
  "DEVROUTER_HEURISTICS_FROZEN": "true"
}
```

devrouter just makes HTTP/Redis calls, so any reachable Redis Stack and
any service speaking the `/api/embed` wire shape will work.
`DEVROUTER_HEURISTICS_FROZEN=true` is useful during an incident so the
bandit doesn't mutate trim/budget knobs underneath you.

## Operational notes

- **Stale binary trap:** after rebuilding `./devrouter`, drop the running
  processes (`pkill -f '/devrouter$'`) so your MCP host respawns the new
  binary. Otherwise the client stays bound to the old stdio handler and
  silently uses old code. See [`troubleshooting.md`](troubleshooting.md)
  for the full diagnosis pattern.
- **Custom embedder shape:** if you build your own embedder and point
  `DEVROUTER_EMBEDDING_URL` at it, it must return JSON of the form
  `{"embeddings": [[<768 floats>]]}` for a
  `POST {"model": "<m>", "input": "<text>"}` request. Status non-200 or
  a vector of any dim other than 768 is rejected with a clear error
  (devrouter would rather fail loud than corrupt the Redis index).
- **Bundled ONNX embedder:** the repo ships one at
  [`embedder/`](../embedder/README.md) — single static-ish Go binary on
  top of the canonical Rust tokenizer + Microsoft ONNX Runtime, no
  Python. Produces canonical `nomic-embed-text-v1.5` vectors. Brought
  up automatically by `make up`; swap in a hosted endpoint instead by
  skipping `make embedder-up` and setting `DEVROUTER_EMBEDDING_URL` to
  whatever you want devrouter to call. Switching embedding models
  requires re-indexing Redis (`make flush-memories` + repopulate) —
  vectors from different model spaces are not comparable.

## Dashboard

DevRouter ships with a read-only HTTP dashboard for inspecting what the
MCP server is doing in real time. It is **on by default**, bound to
`127.0.0.1:8088` (localhost only). Just open
[http://127.0.0.1:8088](http://127.0.0.1:8088) in any browser once a
devrouter process is running.

To bind elsewhere, set `DEVROUTER_DASHBOARD_ADDR` to any `host:port`:

```bash
DEVROUTER_DASHBOARD_ADDR=:9090 ./devrouter           # all interfaces, port 9090
DEVROUTER_DASHBOARD_ADDR=127.0.0.1:0 ./devrouter     # auto-pick a free port
```

To disable it entirely, set any of these sentinel values:

```bash
DEVROUTER_DASHBOARD_ADDR=off ./devrouter             # also: none, disabled, false, 0
```

Port already in use (common when your MCP host spawns multiple
devrouter processes — Cursor does this per project) is **non-fatal**:
the first instance binds the port, the rest log one line and keep
serving MCP traffic without the dashboard. So you only ever see the UI
for one devrouter per port, never a startup crash.

Five tabs:

- **Live Queries** — every `dev_context` call as it lands. Click a row
  to expand the full retrieval trace (intent, profile, latency, tokens,
  returned memory keys). The **Topic** column shows which per-(intent,
  repo, topic) bucket served the query (`global` means the cold-bucket
  fallback was used). Reward badge fills in once `dev_feedback` joins
  the trace.
- **Heuristics** — per-intent current vs frozen-default profile, every
  knob's delta (so you can see exactly what the bandit has shifted),
  reward distribution over the last 7 days, and the recent
  promote / discard / rollback history. Each intent card now nests the
  per-(repo, topic) buckets the bandit has spun up, marked **hot** once
  they cross the sample floor or **cold N/floor** while warming up.
- **Topics** — flat browser over every centroid the topic-clustering
  layer has created. Filter by intent or repo. Centroid samples and
  hot/cold status are shown id-only (`t-0`, `t-3`, …) per design —
  cluster auto-naming is deliberately out of scope.
- **Decisions** — `decision_save` records grouped per repo, rendered as
  a supersession tree (older decisions appear under the newer decisions
  that replaced them).
- **Flows** — saved flow memories per repo, with the entry-point →
  files mapping rendered as a small SVG graph.

The page auto-refreshes every 3 seconds (toggle in the header).

Implementation notes:

- Strictly read-only — the dashboard never writes to Redis, never
  participates in the MCP request path, and never touches the bandit.
- Data source is Redis itself, so the dashboard reflects whatever your
  agents have actually done. The `feedback:trace:index` ZSET is bounded
  (last 500 traces) so dashboard polls are O(log N) regardless of repo
  activity.
- The HTML / CSS / JS are embedded directly into the `devrouter`
  binary via `go:embed` — no separate static-asset deploy, no Node
  build step for the UI.
- **Bind to `127.0.0.1` in production** unless you front it with auth.
  There is no built-in authentication; the dashboard exposes query
  text and saved-memory contents, which can contain proprietary code.

## External retrieval tools

Alongside the two native sources — **memory** (Redis vector recall) and
**codegraph** (graph traversal + code search) — DevRouter can fan out to
external tools that implement the same `retrieval.Source` contract.
Today that's **cmdocs** (PageIndex documentation search) and **GitLab**
(issues/MRs); both are off until configured.

Execution model (see [`architecture.md`](architecture.md) for the full
picture):

1. **Memory runs first** as a cheap signal pre-step. Its recalled file
   paths / symbols / terms are folded into a shared request as `Signals`.
2. **Every other tool then runs in one parallel fan-out** — memory's
   full search, codegraph, cmdocs, GitLab — each under
   `DEVROUTER_SOURCE_TIMEOUT_MS`, with **no gating**: a configured tool
   runs on every query. Relevance is handled downstream by per-source
   caps, not by skipping tools.
3. External results land in `DevPrompt.documentation` (grouped by
   `source`); per-tool latency/errors land in `retrieval_trace.tool_stages`.

Adding a new external MCP tool is **one config row** (and, if its JSON
shape is new, one result mapper) — no pipeline changes. cmdocs, GitLab,
and the deferred ClickUp all share one generic MCP-client source that
differs only by transport, endpoint, auth headers, tool name, and mapper.

### cmdocs (documentation)

cmdocs is a PageIndex MCP server. The preferred transport is its FastAPI
sidecar (a long-lived process that loads the corpus once):

```bash
# in the cmdocs repo
pip install -r requirements-sidecar.txt -r PageIndex/requirements.txt
uvicorn server:app --host 127.0.0.1 --port 8099

# point devrouter at it
export DEVROUTER_CMDOCS_URL=http://127.0.0.1:8099/search
```

Alternatively, run cmdocs as a stdio MCP subprocess (no sidecar):

```bash
export DEVROUTER_CMDOCS_CMD="python3 /abs/path/to/cmdocs/pageindex_mcp.py"
```

> `pageindex_search` calls an LLM (LiteLLM) per query — keep
> `DEVROUTER_CMDOCS_MAX_DOCS` small and rely on the per-tool timeout.

### GitLab (issues / merge requests)

The official GitLab MCP server uses interactive OAuth, which is awkward
for a headless backend. Use a **PAT-based** Streamable-HTTP MCP server
(e.g. a community server like `zereight/gitlab-mcp`) and pass the token
via env:

```bash
export DEVROUTER_GITLAB_MCP_URL=https://your-gitlab-mcp.example/mcp
export DEVROUTER_GITLAB_TOKEN=glpat-xxxxxxxxxxxxxxxxxxxx
export DEVROUTER_GITLAB_TOOL=search_issues   # default; override per your server's tool name
```

The token is sent as both `Authorization: Bearer <tok>` and
`PRIVATE-TOKEN: <tok>` so it works with either header convention. Only
the read-only search tool is ever invoked.

> **ClickUp** is deferred. Because all external tools share one generic
> source, adding it later is one config block + a `search_tasks` result
> mapper.

## Telemetry

devrouter publishes a Prometheus exposition at `/metrics` on the same
mux as the dashboard (so a single bind address gives you both UIs).
Scrape it with any Prometheus-compatible agent:

```bash
curl -s http://127.0.0.1:8088/metrics | head
```

The metric surface is intentionally narrow and bounded:

| Subsystem | Sample metrics | Useful for |
|-----------|----------------|------------|
| `devrouter_mcp_*` | `requests_total{method,tool,status}`, `request_duration_seconds`, `sessions_total`, `sessions_active`, `session_duration_seconds` | Per-tool RED + session lifecycle. One process is one session; `sessions_active` should always be 1 for a healthy stdio subprocess. |
| `devrouter_query_*` | `total{intent,plan_source}`, `duration_seconds{intent}`, `stage_duration_seconds{stage,intent}`, `prompt_tokens`, `budget_used_fraction`, `files_returned`, `trimmed_files`, `relevance_gate_drops_total{stage}`, `auto_anchor_total{result}`, `repeat_total{intent}`, `sanitize_plan_drops_total{field}` | Quality SLIs derived from the same `RetrievalTrace` Redis writes the dashboard reads, but aggregated into histograms you can alert on. The plan_source label is the single highest-signal proxy for "agents stopped supplying plans". |
| `devrouter_feedback_*` | `total{intent,joined_via}`, `raw_reward`, `adjusted_reward`, `additional_files`, `fp_attributions_total` | Whether the loops are closing — `joined_via="dropped"` rising means agents are forgetting `query_id`. |
| `devrouter_codegraph_*` | `requests_total{endpoint,status}`, `request_duration_seconds{endpoint}`, `requests_inflight{endpoint}` | RED on every codegraph HTTP call. The `endpoint` label is the static set `search` / `graph` / `file` / `repos` (all `/api/graph/*` traversals collapse to `graph`), so cardinality stays trivial. |
| `devrouter_retrieval_*` | `source_requests_total{source,status}` | Outcome counter for each external retrieval tool (`cmdocs`, `gitlab`, …) in the parallel fan-out. The `source` label is a small fixed set; `status` is `ok`/`error`. Pair with `query_stage_duration_seconds{stage="docs"}` for fan-out latency. |
| `devrouter_embedder_*` | `requests_total{status}`, `request_duration_seconds`, `dim_mismatch_total` | Embedder health. `dim_mismatch_total` should always be zero in production — non-zero means a misconfigured model is silently corrupting Redis. |
| `devrouter_redis_*` | `commands_total{op,status}`, `command_duration_seconds{op}` | Per-command Redis instrumentation via the go-redis Hook interface. Includes pipeline commands (each command in a pipeline gets one observation). |
| `devrouter_heuristics_*` | `promotions_total{intent}`, `rollbacks_total{intent}`, `discards_total{intent}`, `reward_samples_total{intent,source}`, `frozen` | Bandit health. Rate of promotions vs rollbacks tells you whether the bandit is converging or thrashing. `frozen=1` when `DEVROUTER_HEURISTICS_FROZEN=true`. |
| `devrouter_anchor_*` | `explorations_total`, `observations_total{outcome}`, `probe_failures_total` | Anchor-learner activity: ε-greedy explore rate, reward attribution outcomes, and codegraph probe error rate. |
| `devrouter_build_info` | `version`, `codegraph_url`, `redis_addr` | Static gauge always at 1, labels expose where this process is pointing. Useful for fleet inventory. |

Labels are deliberately constrained to bounded sets (`intent`,
`plan_source`, `tool`, `stage`, `endpoint`, Redis command name). High-
cardinality identifiers like `query_id` and `heuristic_profile_id` are
emitted as **slog fields** and persisted on the per-query
`feedback:trace:{id}` Redis hash — never as Prometheus labels.

To turn the endpoint off:

```bash
DEVROUTER_METRICS_ADDR=off ./devrouter      # also: none, disabled, false, 0
```

### Structured logging

Set `DEVROUTER_LOG_FORMAT=json` to switch the slog handler to JSON.
This also bridges the stdlib `log` package through slog, so every
existing `log.Printf("[router] …", …)` call becomes a structured
record with a `time`, `level`, and `msg` field. Combine with
`DEVROUTER_LOG_LEVEL=debug` to see the funnel-debug lines without
having to set `DEVROUTER_FUNNEL_LOG=1` separately.

### Operational tips

- **One devrouter per MCP host spawn.** `devrouter_mcp_sessions_total`
  is incremented every `Run()` and `sessions_active` is decremented at
  EOF. A leak (active drifting up) means the host is leaving zombie
  stdio subprocesses behind.
- **Alert on `auto_anchor_total{result="none"}` rate.** When this rises,
  agents are calling `dev_context` without a `plan` and devrouter can't
  even find an anchor token. The retrieval still returns something, but
  recall degrades sharply.
- **Watch `embedder_dim_mismatch_total`.** Should always be zero. A
  non-zero value means someone swapped `DEVROUTER_EMBEDDING_MODEL` to a
  model that doesn't produce 768-dim vectors — the wrapper rejects the
  call cleanly, but every downstream memory write fails until it's
  reverted.
- **`codegraph_requests_total{status="5xx"}` is the first signal codegraph
  is down.** Cross-reference with `codegraph_requests_inflight` to tell
  "codegraph is slow" from "codegraph is dead".
