# devrouter architecture

devrouter is an MCP server that returns structured, memory-augmented
context about a code repository so coding agents can answer questions
without re-reading the same files over and over.

This doc explains the **shape** of the system: what processes run,
what state lives where, what happens on a single request, and why
it's split this way. For the request pipeline rules in detail, see
[`retrieval-rules.md`](retrieval-rules.md).

## The live system

```
   ┌────────────────────────────────────────────────────────────────────┐
   │                     agent (Cursor / Claude / …)                     │
   │                                                                    │
   │   produces the structured `plan` ──┐                                │
   │   (must/should/exclude/phrases/    │  MCP over stdio:               │
   │    context_hints) from convo       │  dev_context(query, repo, plan)│
   │    context                         │  dev_feedback, memory_save_*,  │
   │                                    │  decision_*, …                 │
   └────────────────────────────────────┼───────────────────────────────┘
                                        ▼
   ┌────────────────────────────────────────────────────────────────────┐
   │                     devrouter MCP server  (Go)                      │
   │                                                                    │
   │   router.HandleQueryWithPlan ──►  sanitize plan ─► intent ─►        │
   │                                   search ─► assemble                │
   │   router.SubmitFeedback ─►  reward + bandit + FP-attribution        │
   │   heuristics.Picker     ─►  pick / score per-intent dial profile    │
   │                                                                    │
   │   in-memory: last-call LRU (query_id ↔ outcome, 16 entries)         │
   └─────┬────────────────────┬─────────────────┬──────────────────────┘
         │ HTTP :4747         │ Redis (TCP)     │ HTTP (configurable URL)
         ▼                    ▼                 ▼
   ┌──────────────┐    ┌────────────────┐    ┌──────────────────────┐
   │  codegraph   │    │  Redis Stack   │    │  Embedder            │
   │  (Node)      │    │  + RediSearch  │    │                      │
   │              │    │                │    │  /api/embed          │
   │  /api/search │    │  memory:*      │    │  bundled ONNX        │
   │  /api/graph/*│    │  mem:fp:*      │    │  nomic-embed-text-   │
   │  /api/file   │    │  heuristics:*  │    │  v1.5 on :11435      │
   │  /api/repos  │    │  feedback:*    │    │  (any /api/embed-    │
   │              │    │  recent_*      │    │   compatible service │
   │  per-repo    │    │                │    │   works)             │
   │  SQLite+FTS5 │    │                │    │                      │
   │  index       │    │                │    │                      │
   └──────┬───────┘    └────────────────┘    └──────────────────────┘
          │
          ▼
   .codegraph/  in each indexed repo
```

Four moving parts. Only one of them — the devrouter MCP server — is
something the agent talks to directly. The other three are
implementation detail that could in principle be swapped (Redis and
the embedder endpoint can be hosted anywhere; only codegraph has to
live where the repos live). Note: there is no separate planner
endpoint — query planning happens **inside the agent**, which fills in
the structured `plan` argument on `dev_context`.

## What each component does

| Component | Process | State | Purpose |
|-----------|---------|-------|---------|
| **devrouter MCP server** | Go binary, one per agent connection (stdio sub-process) | In-memory LRU only | Owns the request pipeline, the relevance gate, the bandit, the FP loop. Is the only thing the agent ever talks to. |
| **codegraph** | Long-running Node HTTP sidecar on `:4747`, one per machine (vendored MIT engine + thin wrapper) | Per-repo `.codegraph/` SQLite store (`codegraph.db`, FTS5) on disk | Parses your repo with tree-sitter, builds the symbol/call/import graph, answers `/api/search` (FTS5/BM25), purpose-built `/api/graph/*` traversal (callers/callees/importers/extends/…), `/api/file`, `/api/repos`. See [`codegraph.md`](codegraph.md). |
| **Redis Stack** | External, can be remote | All persistent shared state | Memory (vector-searchable), false-positive centroids, heuristic profiles, reward history, query traces, recent-query embeddings for repeat detection. |
| **Embedder endpoint** | External, can be remote | Embedding model on disk | A 768-dim embedding endpoint (default: bundled ONNX `nomic-embed-text-v1.5` at `http://localhost:11435/api/embed` — see [`embedder/`](../embedder/README.md)) used for query/memory/FP embeddings. The embedder URL and model name are configurable — see [`configuration.md`](configuration.md). |
| **agent's MCP host** | Cursor / Claude / etc. | n/a | Spawns the devrouter binary as a stdio subprocess per session. Produces the structured `plan` argument on every `dev_context` call. |

## How a `dev_context` request flows

A single call from the agent. Numbered to match the dotted lines in
[`retrieval-rules.md`](retrieval-rules.md).

```
  agent ─► devrouter ─► … ─► response
         (1) JSON-RPC 2.0 over stdio: {"method":"tools/call",
             "params":{"name":"dev_context",
                       "arguments":{"query":..., "repo":..., "plan":{...}}}}

  devrouter, in order:
    (2)  SanitizePlan(plan)                                [in-process]
            (lowercase, dedupe, drop garbage,
             cap per-field counts; no LLM call)
    (3)  tokenize query                                    [in-process]
    (4)  detect file/package paths                         [in-process]
    (5)  classify intent (keyword-only, microseconds)      [in-process]
    (6)  PICK profile  ─► Redis HGET heuristics:current:*  [Redis]
    (7)  parallel:
           ├─ /api/search hybrid                           [codegraph]
           ├─ memory KNN  (FT.SEARCH)                      [Redis]
           └─ embed(query) for repeat-detection + FP       [embedder endpoint]
    (8)  ensureMustAnchor (auto-anchor rarest token if     [codegraph]
           caller didn't supply must_terms)
    (9)  4-stage relevance gate on memory hits             [in-process]
    (10) compute graph budget from memCount + intent       [in-process]
    (11) per-symbol traversal: callers/callees/importers   [codegraph /api/graph/*]
    (12) related-files                                     [codegraph /api/graph/related-files]
    (13) trim, assemble, derive honest signals             [in-process]
    (14) HSET feedback:trace:{query_id}  (decision side)   [Redis]
    (15) ZADD recent_queries:{repo} {ts} {query_emb}       [Redis]

  devrouter ─► agent
         (16) JSON response with primary_context, graph, memories,
              query_plan (with source="agent" or "auto"),
              retrieval_trace, query_id
```

Steps (7), (11), (12) are the round-trips that dominate latency. (7)
runs three things in parallel — that's the biggest single perf win in
the pipeline. (2) is microseconds; the previous design's parallel
planner LLM call has been retired entirely.

## How `dev_feedback` closes the loops

The agent calls `dev_feedback({query_id, additional_files,
revisited_files, file_paths, success})` after acting on the response.

```
  agent ─► devrouter ─► … ─► ack
         (a) lookup query_id ─► last-call LRU OR
                                HGET feedback:trace:{query_id}      [Redis]
         (b) compute raw + adjusted reward                          [in-process]
                  ↑                ↑
                  │                └── HGETALL heuristics:reward:*  [Redis]
                  │                    (rolling mean for normalize)
                  │
                  └── inputs: additional_files, revisited_files,
                              prompt_tokens (from trace), trim_overlap
         (c) Bandit.Update(intent, profileID, adjusted, weight=1.0) [in-process]
                  └─► may promote / discard / rollback the candidate profile
                       and SET heuristics:current:{intent}:*        [Redis]
         (d) RPUSH heuristics:reward:{intent}:{yyyy-mm-dd}          [Redis]
         (e) HSET feedback:trace:{query_id} (feedback side)         [Redis]
         (f) attribute false-positives (for each returned memory):  [Redis]
                  ├─ HMGET memory keys to load their files
                  ├─ for each memory whose files don't overlap
                  │   the agent's read set:
                  │     - re-embed query                             [embedder endpoint]
                  │     - HSET mem:fp:{memKey} cent=new count++      [Redis]

  parallel side-channel: every dev_context call also runs implicit
  repeat detection (no agent cooperation), which feeds Bandit.Update
  with weight=0.5 when the current query is cosine-close to a
  recent one.
```

So a single `dev_context` + `dev_feedback` pair touches three
learning surfaces in Redis:

- `heuristics:current:{intent}:*` — the bandit may promote a new
  per-intent profile.
- `heuristics:reward:{intent}:{yyyy-mm-dd}` — rolling samples for
  variance reduction and dashboards.
- `mem:fp:{memKey}` — per-memory FP centroid, suppresses misfiring
  memories on the next similar query.

## Where state lives

Three stores, each holding a different slice. None of them is owned
by the devrouter binary itself — restart it any time, no state loss.

| Store | Held by | Lifetime | What's in it |
|-------|---------|----------|--------------|
| **Redis** | Operator (local or hosted) | Mostly persistent (some TTLs) | Memory hashes (`memory:*`), memory vector index (`memidx`), FP centroids (`mem:fp:*`, 14-day TTL), heuristic profiles (`heuristics:current/default/history/reward`), per-query span (`feedback:trace:*`, 30-day TTL), recent-query sorted set (`recent_queries:{repo}`, 30-min window). |
| **codegraph SQLite** | codegraph sidecar, on disk | Survives process restarts; rebuilt by `analyze` | Per-repo symbol graph in `codegraph.db`: nodes (files, packages, symbols, routes), edges (CALLS, IMPORTS, EXTENDS, HAS_METHOD, IMPLEMENTS, …), plus an FTS5 index for lexical search. Source snippets are sliced from disk on demand. |
| **devrouter in-process** | devrouter binary | Lives only as long as the MCP connection | Last-call LRU (`query_id` ↔ outcome, 16 entries, 10-min TTL — for `dev_feedback` fallback when the agent omits `query_id`). |

The split is deliberate: anything multi-developer (memory,
heuristics, FPs) lives in Redis so two engineers in the same repo
share the same brain. Anything per-machine (a symbol graph parsed
from local source) lives in codegraph's SQLite store. Anything strictly
per-session (the LRU) is in-process and disappears when the agent
closes the connection.

## Why it's split this way

A few non-obvious decisions, with the reasoning:

**MCP over stdio, not HTTP.** The MCP host already manages stdio
subprocesses per agent session. Running each agent's devrouter as
its own subprocess gives clean state isolation for the in-process
LRU and means there's nothing to deploy / scale / authenticate as a
shared service. The persistent state that *should* be shared
(memory, heuristics, FPs) is in Redis — which already is a shared
service.

**codegraph in a separate process.** The tree-sitter parser
ecosystem is mature in Node and immature in Go. Vendoring the
MIT-licensed `colbymchenry/codegraph` engine and keeping it in
TypeScript (behind a thin HTTP sidecar) was cheaper than porting
20+ language parsers, and the HTTP boundary lets us swap or upgrade
the indexer without recompiling devrouter. The price is a
process boundary on every search/graph/file call — measured in
milliseconds, dwarfed by the embedder and Redis round-trips.

**Memory in Redis, not in codegraph.** Memory is multi-developer
shared knowledge ("FMS swallows unmarshal errors"). codegraph's
SQLite store is a per-machine local index rebuilt from source. Mixing
the two would either force every developer's machine to hold every
other developer's notes, or fork the source-of-truth between
machines. Redis with a vector index is a much better fit.

**Two complementary feedback loops, not one.** The bandit (Section
9 of `retrieval-rules.md`) tunes how *much* to retrieve via
per-intent dial profiles. The FP loop (Section 10) tunes which
*individual memories* should be suppressed for queries semantically
near a previous false positive. They optimise different things —
merging them would mean every reward signal carries less attribution
about each axis. Keeping them separate means each can be debugged,
disabled, or replayed independently.

**Honest signals as a foundation.** Every signal in the response
(`context_confidence`, `semantic_similarity`, per-entry `confidence`)
is now derived from real cosine numbers instead of hardcoded
constants. Both learning loops above only work because the upstream
signals stopped lying — see `retrieval-rules.md` Section 11.

**Keyword-only intent classification.** An earlier version called a
small Ollama model on intent-keyword misses. It paid a 5-second
timeout on roughly 5–10% of queries while contributing little
signal that the per-intent profile actually needed. The general
defaults are fine when intent is genuinely ambiguous, so the
classifier is now strictly local + free.

**Query planning happens in the agent, not in devrouter.** Earlier
versions ran a small sidecar Ollama LLM to extract structured
must/should/exclude terms from the raw query. That made the planner the
slowest single stage on cache misses (~1-2s), introduced an Ollama
runtime dependency, and crucially gave the planner only the bare query
string — no conversation context. Pushing planning back to the MCP
caller turns this on its head: the agent (which has the full convo) now
fills in the `plan` argument on `dev_context`, devrouter just sanitizes
and applies it. Result: zero LLM dependency in devrouter, materially
better plans, no caching layer to manage.

## Failure modes

What happens when a component is down or slow.

| Component | If down | If slow |
|-----------|---------|---------|
| **codegraph** | `dev_context` returns memory + decisions but no symbols / call graph / snippets. Logged. Agent can still answer some questions from memory alone. | Linear hit on every request. `make status` shows the health line; restart with `make codegraph`. |
| **Redis** | `dev_context` falls back to a code-only response with no memory and no learning loops. `dev_feedback` becomes a no-op. Severe degradation; treat as outage. | Tail latency spikes show up directly in `dev_context` p95. |
| **Embedder endpoint** | New memory writes fail (can't embed). Existing memory still vector-searchable. Repeat detection and FP attribution stop working. | Memory KNN parallelises with code search, so some of this is hidden. |
| **Caller omits `plan` field** | Falls through to the auto-anchor (rarest token) — no quality cliff, recall narrows slightly, exclude rules don't fire. Surfaces as `plan.source = "auto"` in `retrieve_debug`. | n/a — there's no LLM in this path anymore; sanitization is microseconds. |
| **devrouter binary** | Agent's MCP host reports the tool as unavailable; the host typically restarts the subprocess on the next call. No state loss (LRU is rebuilt; everything else is in Redis). | n/a — this is the orchestrator, not a backend. |

The pattern: **codegraph and Redis are required**, **the embedder
endpoint is graceful-degrading** (and swappable via
`DEVROUTER_EMBEDDING_URL`), the agent-supplied `plan` is optional but
strongly recommended, and the devrouter binary itself holds no
durable state so it's freely restartable.

## Going deeper — pipeline rule index

For each pipeline stage in the diagram, the canonical rule lives in
[`retrieval-rules.md`](retrieval-rules.md):

| Section | Concern |
|---------|---------|
| Section 3 | Query planning (agent-supplied plan, auto-anchor fallback, sanitization caps) |
| Section 4 | Intent detection (keyword-only, no LLM round-trip) |
| Section 5 | Code search cascade (file/package/hybrid/graph-traversal/path-boost) |
| Section 6 | Vector recall → 4-stage relevance gate (floor / must / FP / rerank) |
| Section 7 | Graph expansion budget per intent |
| Section 8 | Trim caps per intent + memory shrink |
| Section 9 | Self-tuning per-intent dial profile (bandit, reward, freeze mode) |
| Section 10 | Per-memory FP learning (false-positive feedback loop) |
| Section 11 | Honest signals (cosine-derived confidence on every field) |

## Observability surfaces

devrouter exposes two complementary observability stories:

| Surface | Lives in | Best for |
|---------|----------|----------|
| **Per-query traces** | `feedback:trace:{query_id}` hashes in Redis, rendered by [`internal/dashboard/`](../internal/dashboard) at `http://127.0.0.1:8088` | Debugging a specific call: which memories were returned, what the bandit picked, what reward joined later. |
| **Process metrics** | Prometheus exposition at `http://127.0.0.1:8088/metrics`, served by [`internal/telemetry/`](../internal/telemetry) | Aggregate RED / SLIs across all calls: per-intent latency histograms, per-tool MCP RED, codegraph + Redis + embedder external-call instrumentation, bandit health (promotions / rollbacks / reward samples), build_info. |

The split is deliberate. Labels on the Prometheus surface are
bounded (`intent`, `plan_source`, `tool`, `stage`, `endpoint`,
canonical Redis command name) so an SRE can confidently scrape and
alert. High-cardinality identifiers like `query_id` and
`heuristic_profile_id` stay on the Redis trace + the slog stream,
where the dashboard can join them on demand.

See [`configuration.md` § Telemetry](configuration.md#telemetry) for
the full metric catalogue and the `DEVROUTER_METRICS_ADDR` /
`DEVROUTER_LOG_FORMAT` knobs.

## Related docs

- [`retrieval-rules.md`](retrieval-rules.md) — the canonical request
  pipeline, end to end
- [`heuristics.md`](heuristics.md) — what the self-tuning system
  actually does (dials, scoring, safety, freeze)
- [`codegraph.md`](codegraph.md) — the per-repo code indexer
  devrouter ships with (storage, HTTP API, CLI, languages)
- [`tools.md`](tools.md) — the MCP surface devrouter exposes to
  agents
- [`agent-rules.md`](agent-rules.md) — the call-order contract
  agents must follow for the closed loops in Sections 9 and 10 to
  actually close
- [`configuration.md`](configuration.md) — every env var that
  influences the pipeline (cosine floor, freeze mode, hosted-service
  overrides, telemetry exposition)
