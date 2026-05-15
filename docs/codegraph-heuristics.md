# Codegraph heuristics — shaping raw index output into useful context

`codegraph` returns three kinds of raw output: a list of search hits
(BM25 / semantic / hybrid), a graph of symbols and edges, and a set of
file-content slices. None of that is useful to an agent on its own —
the search returns 50 hits where you want 10, the graph fans out into
hubs that dominate the response, and the file slices duplicate the
same file across every matching symbol.

This doc covers the heuristics DevRouter applies *on top of* codegraph
to turn raw output into a clean, ranked, bounded context window. These
are bench-validated against the
[goserving / mall / airflow](benchmarks.md) question sets — every
change here has a measured before/after.

> Looking for the runtime self-tuning system that moves the
> per-intent dials based on agent feedback? That's
> [`heuristics.md`](heuristics.md). This doc is about the
> retrieval-shaping rules that sit between codegraph and the prompt.

## 1. Intent-aware search mode routing

**Problem.** Codegraph exposes three search modes —
[`bm25`](codegraph.md#search), `semantic` (HNSW vector), and `hybrid`
(weighted combination). Hybrid is the safest default but it's not the
best for every query intent. `trace` queries want call-chain neighbours
that lexical BM25 misses; `refactor` queries want exact-symbol matches
that semantic over-smooths.

**Fix.** [`SearchModeForIntent`](../internal/codegraph/client.go) maps
each of the five DevRouter intents to its best-on-bench mode:

| Intent | Best mode | Why |
|---|---|---|
| `debug` | hybrid | Tied with semantic; hybrid is safer fallback. |
| `explore` | hybrid | Same. |
| `general` | semantic | +0.500 R@5 on goserving — README/architecture questions ride on whole-doc cosine, not term overlap. |
| `refactor` | bm25 | Already ~1.0 on hybrid; bm25 keeps it there and saves embedding cost. |
| `trace` | semantic | +0.229 R@5 — call-chain questions benefit from cross-file semantic matches BM25 can't see. |

Calibrated on the goserving 30-question bench (2026-05-14). Unknown
intents fall back to `hybrid` — never the worst, never the best.

## 2. File-path snippet deduplication

**Problem.** Codegraph returns symbol-level hits. A query like
*"rate limiter"* can match 8 symbols all inside `ratelimit.go` (the
constructor, the check method, the config struct, …). With a snippet
cap of 10, that's 8 slots gone to one file before the next on-topic
file even gets a slot.

**Fix.** [`ToSnippets`](../internal/codegraph/client.go#L1044) keeps
the *highest-ranked* symbol per file path and drops the rest. Codegraph
already ranks within a file by combined score, so this is "drop the
lower-ranked symbol from the same file" — no on-topic file ever
disappears.

**Impact.** Closed the codegraph→DevRouter R@5 gap on goserving where
multi-symbol files like `manager.go` were monopolising the snippet
stream. Validated on every subsequent bench.

## 3. Graph relevance gate

**Problem.** Codegraph's graph traversal is exhaustive — every seed
symbol pulls every caller, every callee, every importer that exists
in the graph. Hub files (`manager.go`, `main.go`, `getters.go`) end
up dominating with no relevance signal. In the early goserving bench,
this produced 35–43 KB of wrong-file p99 noise vs `agentmemory`'s
10 KB ceiling.

**Fix.** [`filterCallChainByPlan`](../internal/router/router.go#L1752)
applies the query's relevance plan to graph edges with two policies:

- **Call-chain edges** (1-hop from a search-certified seed) —
  adjacency trust. Exclude only. The search layer already validated
  this neighbourhood is on-topic; pruning hubs that genuinely
  called/were-called-by a seed is too aggressive.
- **Importer / sibling edges** (2-hop fan-out) — must-term substring
  filter against the edge's structural text. Seed-file bypass still
  applies for edges that live in a search-certified file.

Tests / mocks / fixtures are dropped from both buckets by the same
narrow conventions [`shouldExcludeMemory`](../internal/router/router.go)
uses, so we don't over-prune on incidental substrings.

## 4. Anchor injection (static + bandit-learned)

**Problem.** Some files are so universally relevant on certain query
shapes that they should be retrieved even when neither BM25 nor
semantic search puts them in the top K. Example: a question about
"how is JWT validated" in a Spring Boot repo almost always wants the
module's `application.yml` even though `.yml` rarely outranks `.java`
on either lexical or vector match.

**Fix.** Two-tier anchor injection in
[`injectQueryAnchors`](../internal/router/router.go#L2395):

- **Static cold-start portfolio** —
  [`anchorlearn/types.go`](../internal/anchorlearn/types.go) ships
  with universal Maven/Spring (`pom.xml`, `application.yml`,
  `Application.java`), Go (`main.go`, `internal/`), and Python
  (`__init__.py`, `pyproject.toml`) entry-point patterns.
- **Per-repo learned set** — a Thompson-sampling bandit
  ([`anchorlearn/learner.go`](../internal/anchorlearn/learner.go))
  promotes path suffixes the agent has rewarded via `memory_save_file`.
  Scoring blends a cross-repo prior, a per-repo posterior, and a
  keyword-affinity weight for this query's tokens.

The gate has two firing modes:
1. **Verb-gated** — query mentions a service-trace verb
   (*"start"*, *"listen"*, *"handle"*). Fires the full static +
   discovered portfolio.
2. **Discovered-only fallback** — query mentions just a service
   token (a top-level dir name) without a trace verb. Fires only the
   per-repo *learned* patterns. Skipped on cold-start to avoid
   over-firing static patterns on every "how does X work" question.

**Impact.** On mall (Java/Spring Boot), the discovered-only fallback
is what gives the bandit a path to learn from operational questions
that don't fit the original "entry-point exploration" mould. Closes
~18% of the R@5 deficit on the mall coldstart→postlearn delta.

## 5. Parallel codegraph fan-out

**Problem.** A single `dev_context` call needs to make ~5–15
independent HTTP queries to codegraph: one per traced symbol for
callers, one per import keyword for related files, one per sibling
keyword. Sequentially this is 5–15 × ~50–80 ms HTTP round-trips,
dominating the response time.

**Fix.** [`parallelDo`](../internal/router/parallel.go) bounded-fan-out
helper runs the symbol-tracing loop, importer queries, and related-files
queries concurrently. Used in three places in
[`router.go`](../internal/router/router.go) (~lines 683, 828, 844, 872).

**Impact.** Combined with the UpstreamChain replacement below,
DevRouter p50 latency on goserving fell from 2,495 ms to 785 ms — a
3.2× speedup with no recall trade-off.

## 6. UpstreamChain replacement

**Problem.** The original
[`UpstreamChain`](../internal/codegraph/client.go) was a single 2-hop
Cypher query: *"find all callers of caller of seed"*. LadybugDB
executed this as a serial join — ~380 ms per call, called once per
traced symbol.

**Fix.** Replaced with two parallel 1-hop `CallersWithPath` calls:
collect direct callers in batch 1, then run `CallersWithPath` against
each unique parent in batch 2. Both batches use
[`parallelDo`](../internal/router/parallel.go). See
[router.go ~lines 746–795](../internal/router/router.go#L746).

**Impact.** ~380 ms → ~7 ms per call. The single biggest contributor
to the p50 latency drop.

## 7. `include_source` for token honesty

**Problem.** Codegraph's `/api/search` historically returned only
metadata (file path, symbol name, line range). DevRouter then had to
do a follow-up `/api/file` round-trip per result to get the actual
content. For benchmarking this also made codegraph's token cost
artificially low — "115 tokens at R@5" was metadata-only, not
comparable to `agentmemory`'s full-file payload.

**Fix.** New `include_source: true` request parameter on
`/api/search`. When set, codegraph inlines the
`[startLine, endLine]` slice for each hit. DevRouter's adapter sets
this flag by default
([`internal/codegraph/client.go`](../internal/codegraph/client.go)).

**Impact.** Closes the 5–10× round-trip cost on the snippet path
*and* gives the bench a fair `tokens_uniform` metric — every adapter
is now charged for *content actually opened*, not just *paths
returned*.

## 8. Vector-index reliability (codegraph side)

**Problem.** Two silent-failure paths in
[`codegraph/src/core/lbug/lbug-adapter.ts`](../codegraph/src/core/lbug/lbug-adapter.ts)
and
[`codegraph/src/core/embeddings/embedding-pipeline.ts`](../codegraph/src/core/embeddings/embedding-pipeline.ts):

1. `loadVectorExtension` previously `console.error`'d on a genuine
   `LOAD EXTENSION VECTOR` failure and fell through without throwing,
   leaving every subsequent `CREATE_VECTOR_INDEX` and
   `QUERY_VECTOR_INDEX` to silently no-op.
2. The post-create verification probed `row['index name']` (with a
   space) and `row.tableName` (camelCase). LadybugDB actually
   returns `row.index_name` and `row.table_name` (snake_case). The
   verify always failed to find the index — even when the index had
   been created successfully — and the analyzer would shoot itself
   with `"CREATE_VECTOR_INDEX returned without error but the index
   was not registered"` on every run.

**Fix.** `loadVectorExtension` now throws on non-benign load
failures. The verify probes the correct snake_case column names with
the older variants kept as fallbacks. Both branches preserve the
"already loaded / already exists" benign path so re-indexes don't
trip them.

**Impact.** Without this, the airflow benchmark could not be run at
all — every analyze attempt with `--embeddings` would fail. With it,
mall, goserving, and airflow all index cleanly and the live
[`SHOW_INDEXES()` snapshot](../bench/results/airflow-postfix-n30/report.md)
shows the HNSW index registered on every repo.

## Putting it together — the retrieval pipeline

A single `dev_context` call now flows through these stages, each
shaping the previous stage's output:

```
agent query
   │
   ▼  intent classifier (5 buckets)
   │
   ▼  SearchModeForIntent  ─►  codegraph /api/search?mode=…
   │
   ▼  ToSnippets dedup  (file-path level)
   │
   ▼  graph fan-out via parallelDo:
   │     ├─ traced symbols × CallersWithPath  (2× parallel batches)
   │     ├─ importer keywords × RelatedFiles  (parallel)
   │     └─ sibling keywords × RelatedFiles   (parallel)
   │
   ▼  filterCallChainByPlan  (relevance gate)
   │
   ▼  injectQueryAnchors  (static + bandit-learned)
   │
   ▼  prompt budget clipping (per-intent dial profile)
   │
   ▼  DevPrompt → agent
```

Steps 2 (`SearchModeForIntent`), 3 (`ToSnippets`), 5 (`filterCallChainByPlan`),
6 (`injectQueryAnchors`), and the `parallelDo` plumbing in step 4 are
the heuristic layer. Steps 1 and 7 are the runtime self-tuning system
covered in [`heuristics.md`](heuristics.md). The codegraph-side fixes
(7 and 8 above) are infrastructure beneath all of this — without
them the search calls would either return half-broken results or
hit silent no-ops.

## How to verify any of this in your repo

Every claim in this doc is reproducible against your own indexed
repo:

```bash
# 1. Anchor injection — see what fires for a query
curl -sS http://localhost:8765/api/route \
  -H 'Content-Type: application/json' \
  -d '{"query":"how does the scheduler enqueue tasks","repo":"airflow-core"}' \
  | jq '.primary_context[] | select(.type=="anchor")'

# 2. Search mode — see which mode the router picked
# (logged at INFO level when DEVROUTER_LOG_LEVEL=debug)

# 3. Snippet dedup — see file-path uniqueness in the response
curl … | jq '.snippets[].file' | sort -u | wc -l

# 4. Graph filter — see what got pruned
# (set DEVROUTER_LOG_LEVEL=debug; "filterCallChainByPlan dropped N edges"
# is logged per query)

# 5. Bench it
python3 bench/runner.py --repo <your-repo> --adapters devrouter
```

Full reproduction commands and the published numbers are in
[`benchmarks.md`](benchmarks.md).
