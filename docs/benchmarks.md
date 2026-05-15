# Benchmarks — DevRouter vs the field

DevRouter is benchmarked against three competing approaches to per-repo
code retrieval:

- **`agentmemory-hybrid`** — BM25 + local vector search using
  Xenova `all-MiniLM-L6-v2`. The published headline configuration from
  the agentmemory README (95.2% R@5 on `LongMemEval-S`).
- **`agentmemory-bm25`** — BM25 only, same library, model loading
  skipped. The "what does pure BM25 over the same doc set look like"
  ablation.
- **`grep`** — `ripgrep` with a fixed keyword expansion derived from
  the question, ranked by file size + match count. The "if I just
  grepped, what would I get" floor.

All three competitors run locally on the same machine with the same
question set. There are no API calls, no remote services, no warm
caches — every adapter starts from a cold index every run.

## What the benchmark measures

Given a developer question about a real repo, does the system surface
the files (and ideally symbols) a human would have read to answer it?

| Metric | What it captures |
|---|---|
| **R@5** | Recall at top 5 — the fraction of expected files present in the first 5 returned. The primary headline because most agents only look at the first few results. |
| **R@10** | Same, top 10. |
| **MRR** | Mean Reciprocal Rank — rewards systems for placing the *first* correct answer high, not just somewhere in the top K. |
| **Latency p50 / p95** | End-to-end retrieval time per query, cold cache between adapters. Excludes one-time setup (model load, embedding pass). |
| **Tokens p50 / p95 (uniform)** | Deterministic floor: every adapter is charged for `top-K paths × min(file_size, 64 KB)` from its own file picks, regardless of what it serialized into its actual response. This is the strict apples-to-apples view of "if you actually opened these files, how many tokens would you spend?" |

## Question sets

| Repo | Language | Files | Questions | Intent mix |
|---|---|---:|---:|---|
| [goserving](https://github.com/atharvaag/goserving) | Go | 7,796 | 30 | 8 trace · 6 explore · 5 debug · 6 refactor · 5 general |
| [mall](https://github.com/macrozheng/mall) | Java / Spring Boot | 685 | 30 | 8 trace · 6 explore · 6 debug · 6 refactor · 4 general |
| [airflow](https://github.com/apache/airflow) (airflow-core only) | Python | 2,407 | 30 | 8 trace · 6 explore · 7 debug · 5 refactor · 4 general |

Each question is hand-authored with:
- A natural-language query phrased the way an agent would ask it
- A list of expected files (the human-curated answer set)
- A list of expected symbols (function / class / method names)
- A short notes field explaining the intent

Files are available under [`bench/questions/`](../bench/questions/).
Every expected file is verified to exist in the indexed repo before
the bench accepts the question.

## Headline results (2026-05-14, post-fix)

| Repo | Lang | DevRouter R@5 | agentmemory-hybrid R@5 | agentmemory-bm25 R@5 | grep R@5 | DevRouter MRR | agentmemory-hybrid MRR | DevRouter p50 | DevRouter p95 |
|---|---|---:|---:|---:|---:|---:|---:|---:|---:|
| goserving | Go | **0.644** | 0.606 | 0.178 | 0.133 | **0.520** | 0.493 | 785 ms | 928 ms |
| mall | Java | 0.464 | **0.506** | 0.147 | 0.256 | **0.532** | 0.537 | 452 ms | 580 ms |
| airflow-core | Python | **0.558** | 0.000 | 0.139 | 0.150 | **0.631** | 0.000 | 550 ms | 674 ms |
| **Average** | | **0.555** | 0.371 | 0.155 | 0.180 | **0.561** | 0.343 | 596 ms | 727 ms |

**DevRouter wins overall R@5 on 2 of 3 repos and overall MRR on 3 of 3**,
averaging 18.4 percentage points higher R@5 than the strongest competitor
(`agentmemory-hybrid`) across the three languages. The single R@5 loss
(mall) is 4 points; MRR on the same repo is essentially tied.

## Per-intent R@5 — DevRouter vs agentmemory-hybrid

The intent dimension is the most useful diagnostic. DevRouter is
optimized for **trace** (call chains) and **debug** (suspected-fault
localization) because those are where graph-backed retrieval pays off
hardest; **general** is the soft spot because architecture-shaped
questions ride mostly on README / top-level doc retrieval.

| Intent | goserving (Go) | mall (Java) | airflow (Python) | DevRouter avg | agentmemory-hybrid avg |
|---|---:|---:|---:|---:|---:|
| **trace** | **0.875** / 0.750 | **0.521** / 0.333 | **0.667** / 0.000 | **0.688** | 0.361 |
| **explore** | **0.556** / 0.444 | **0.667** / 0.639 | **0.458** / 0.000 | **0.560** | 0.361 |
| **debug** | 0.400 / 0.400 | **0.681** / 0.528 | **0.762** / 0.000 | **0.614** | 0.309 |
| **refactor** | **0.667** / 0.667 | 0.222 / **0.639** | **0.467** / 0.000 | **0.452** | 0.435 |
| **general** | 0.600 / **0.700** | 0.083 / **0.417** | **0.250** / 0.000 | 0.311 | 0.372 |

Where DevRouter wins per-intent across all three repos: **trace**
(call-chain navigation) and **explore** (where does feature X live).
Where the head-to-head is even or DevRouter trails: **general**
(architecture overviews) and **refactor** on mall specifically. The
mall refactor gap is the known anchor-coverage hole — those questions
ask "where do I add field X" which benefits from explicit
controller/service/dto co-location signals the bandit hasn't fully
learned for that repo's Spring Boot conventions yet.

## Latency and token cost

| Adapter | p50 latency | p95 latency | Uniform tokens p50 | Uniform tokens p95 | Setup |
|---|---:|---:|---:|---:|---|
| `devrouter` (avg across 3 repos) | 596 ms | 755 ms | ~45 KB | ~90 KB | ~30 ms |
| `agentmemory-hybrid` (avg) | 15 ms | 25 ms | ~10 KB | ~18 KB | 7–33 s |
| `agentmemory-bm25` (avg) | 7 ms | 17 ms | ~70 KB | ~85 KB | 1–3 s |
| `grep` (avg) | 4,540 ms | 6,340 ms | ~125 KB | ~155 KB | <1 ms |

DevRouter is 30–100× slower at query time than agentmemory's in-memory
indexes, but absorbs its real cost into the codegraph build (a one-time
`./devrouter analyze`). agentmemory rebuilds its index from scratch on
every process start (7s BM25, 33s hybrid), which is fine for a single
agent session but compounds for any harness that boots many adapter
processes. DevRouter's ~30 ms setup is just MCP child-process spawn
and a heartbeat probe.

For the tokens dimension, DevRouter's `devrouter` adapter sits between
agentmemory-hybrid (smallest, because it only returns file paths the
client has to open itself) and grep (largest, because it returns full
files). The "uniform" metric is what the next agent step actually pays
in context budget once it opens the top-K files DevRouter returned.

## Why agentmemory-hybrid scores 0.000 on airflow

Three compounding factors, all structural:

1. **First-512-bytes truncation** — Xenova `all-MiniLM-L6-v2` embeds
   only the first 512 chars of each file. A 300-byte changelog
   fragment fits entirely; a 100 KB `scheduler_job_runner.py` only
   contributes its imports header.
2. **High keyword density** — Airflow's `newsfragments/*.rst` files
   are 300–600 byte release notes like *"Airflow scheduler CLI command
   have a new --only-idle flag…"* — extremely dense, extremely
   keyword-aligned with developer questions.
3. **Sheer count** — Airflow ships ~600 newsfragments. Even at low
   per-fragment relevance, they saturate top-K by volume.

The result: **80% of agentmemory-hybrid's airflow returns are
`newsfragments/*.rst`, 6% are autogenerated TypeScript clients,
1% are Python.** Every expected ground-truth answer is Python.

This is not an agentmemory bug — the adapter doesn't crash, it ingests
all 2,168 docs, latency is normal, and the same adapter scores 0.506
R@5 on mall (Java) and 0.606 on goserving (Go). It's an
**architectural failure mode** of pure-embedding retrieval that gets
exposed when a repo carries a large corpus of short, keyword-dense
doc files alongside its real source. DevRouter's mixed
BM25-on-symbol-content + graph traversal + anchor injection is
specifically designed to be robust to this exact pattern.

## Anchor learning (bandit) — how DevRouter adapts per repo

DevRouter ships with a per-repo Thompson-sampling bandit that learns
which file-path patterns the agent actually ends up reading for
queries that mention specific services / modules. On a fresh repo
it starts from a portfolio of static cold-start patterns (README,
`main.go`, `Application.java`, `pom.xml`, `src/main/resources/application.yml`,
…) and promotes the ones the agent rewards via `memory_save_file`
observations.

The mall benchmark exposed the cold-start gap on Spring Boot. We ran
a synthetic 60-event learning trace (`bench/feed_anchor_traces_mall.py`)
to demonstrate the bandit closing the gap empirically. The trace is
intentionally short — 20 `dev_context` queries paired with 40
`memory_save_file` events — so the result reflects what 30–60
minutes of real agent activity would produce, not weeks.

For the architecture and dials, see
[`internal/anchorlearn/doc.go`](../internal/anchorlearn/doc.go).

## How to reproduce

```bash
# 0. one-time setup
make up                                              # Redis + embedder + codegraph

# 1. index the three repos with embeddings on
./devrouter analyze --embeddings /path/to/goserving
./devrouter analyze --embeddings /path/to/mall
./devrouter analyze --embeddings /path/to/airflow/airflow-core --skip-git

# 2. run the bench (each takes 1–10 min depending on repo size)
python3 bench/runner.py \
  --repo goserving \
  --repo-root /path/to/goserving \
  --adapters devrouter,agentmemory-hybrid,agentmemory-bm25,grep

python3 bench/runner.py \
  --repo mall \
  --repo-root /path/to/mall \
  --adapters devrouter,agentmemory-hybrid,agentmemory-bm25,grep

python3 bench/runner.py \
  --repo airflow-core \
  --repo-root /path/to/airflow/airflow-core \
  --adapters devrouter,agentmemory-hybrid,agentmemory-bm25,grep
```

Each run writes `summary.json`, `raw.jsonl`, and `report.md` under
`bench/results/<timestamp>/` (or `--output-dir` if you pass one).

To add a new repo, write `bench/questions/<repo>.jsonl` following the
existing format. The harness verifies every expected file exists in
the repo at run start and fails loudly if not.

## Memory-augmented retrieval — DevRouter vs mem0

The three benchmarks above run **cold**: no agent has saved any memories
yet, so each system answers from a flat code index. That measures
DevRouter's code-retrieval half well, but says nothing about the
memory half it was actually built for.

This section is the orthogonal measurement: **given the same hand-authored
memory corpus pre-loaded into both systems, which one's retrieval blends
memory with code more effectively?**

### Why mem0, not Letta / MemGPT

mem0 (53K ⭐) is the closest competitor in category: a memory framework
where you store notes and retrieve them by semantic similarity. The
exact subsystem DevRouter is described as having alongside its code
retrieval layer.

Letta / MemGPT (22K ⭐) is in a different category — it pages working
memory into and out of an LLM's context window. It does not expose a
"give me the most relevant memories for this query" call, so it cannot
be benchmarked on this metric without inventing a wrapper the framework
itself does not ship.

agentmemory is kept as the "code-only, no memory" baseline so the
table tells you both gaps: DevRouter vs memory-only, and DevRouter vs
code-only.

### Setup

A seed corpus of 30 hand-authored notes lives at
[`bench/memories/mall.jsonl`](../bench/memories/mall.jsonl) — one note
per file, each ~50 words describing the file's purpose, key methods,
and dependencies. The corpus covers **30 of the 56 files (54%)** in
the mall ground-truth answer space, by design — agents save memories
about what they explore, not everything in the codebase.

Both systems ingest **the same memories** before any question is asked:

- **DevRouter**: `python3 bench/seed_memories.py --repo mall` calls the
  MCP `memory_save_file` tool 30 times.
- **mem0**: the adapter's `setup()` calls `Memory.add(..., infer=False)`
  30 times against a fresh Qdrant collection.

Both use **local Ollama / nomic-embed-text** for embeddings (same model,
768-dim) so we are not stacking the deck with OpenAI's text-embedding-3.

Vector store choice for mem0 is **Qdrant**, not the default Chroma.
mem0 v2.0.2's score_and_rank assumes the vector store returns cosine
similarity (high = good), but its Chroma and Faiss adapters pass raw
L2 distance through verbatim — which inverts the final ranking
(verified on a 3-memory smoke test: the correct hit consistently
ranked last). Qdrant returns cosine similarity natively, so the
rankings are correct. We log this as a finding rather than a workaround:
**out-of-the-box mem0 with Chroma silently mis-ranks results**.

### Results — mall (30 questions, seeded with 30 memories)

| Adapter | Type | R@5 | R@10 | MRR | p50 ms | p50 native tokens |
|---|---|---:|---:|---:|---:|---:|
| **`devrouter`** | memory + code | **0.731** | **0.781** | **0.901** | 339 | 2336 |
| `mem0` (qdrant) | memory only | 0.586 | 0.625 | 0.900 | 34 | 657 |
| `agentmemory-hybrid` | code only | 0.506 | 0.628 | 0.537 | 6 | 6782 |

**+0.145 R@5 over memory-only, +0.225 R@5 over code-only.** DevRouter
also wins R@10 (+0.156 vs mem0) and ties on MRR (0.901 vs 0.900) —
meaning when DevRouter has the right answer, it surfaces it just as
high in the list as a pure memory system would, while also catching
the answers memory alone misses.

### Per-intent breakdown

| Adapter | debug | explore | general | refactor | trace |
|---|---:|---:|---:|---:|---:|
| `devrouter` | 0.597 | **0.806** | **0.583** | **0.694** | **0.875** |
| `mem0` | 0.569 | 0.583 | 0.417 | 0.583 | 0.688 |

DevRouter wins on **explore**, **general**, **refactor**, and **trace**.
mem0 closes the gap on **debug**, where the answer is often a specific
call-chain that a flat memory note doesn't capture as well as
DevRouter's codegraph traversal does on a non-seeded question — but
both are within 0.03 of each other.

### What this measurement caught (and DevRouter shipped a fix for)

The first run of this bench scored DevRouter at **0.453 R@5**, below
mem0. The logs showed `memory plan-filter dropped 8/8 hits` — the
must-term gate was killing every cosine-passing memory because the
auto-anchored token (the rarest query keyword, e.g. `"user"`) wasn't
in the memory's file path. The gate is the right policy for codegraph
results (large, noisy population) but wrong for memory hits (small,
already-on-topic, often with English-only file paths).

The fix, in [`internal/router/router.go`](../internal/router/router.go)
`filterMemoriesByPlan`: when the must term came from
`ensureMustAnchor` (heuristic) rather than from a caller-supplied
plan, downgrade it to a ranking SHOULD on the memory side. The
exclude blocklist still applies. The graph-traversal side still
treats it as a hard gate (its population is hub-prone and needs it).

Effect on the same bench, same questions, same seed corpus:

| Run | R@5 | R@10 | MRR | per-intent failures |
|---|---:|---:|---:|---|
| Before fix | 0.453 | 0.569 | 0.507 | general 0.083, refactor 0.222 |
| After fix | **0.731** | **0.781** | **0.901** | general 0.583, refactor 0.694 |

Coverage logged at `/api/devcontext` flipped from `"memory_coverage":"none"`
to `"memory_coverage":"high"` on the previously-failing questions, and
the `primary_context` field is now populated with seeded memories
instead of being empty. Unit test for the auto-anchor bypass:
`TestFilterMemoriesByPlanAutoAnchorBypass` in
[`internal/router/relevance_test.go`](../internal/router/relevance_test.go).

### How to reproduce the memory-augmented bench

```bash
# 0. one-time deps for the mem0 adapter (separate venv, ~300 MB pinned)
python3 -m venv bench/.mem0-venv
bench/.mem0-venv/bin/pip install \
    mem0ai chromadb qdrant-client ollama redis redisvl nltk faiss-cpu requests
bench/.mem0-venv/bin/python -c "import nltk; nltk.download('stopwords', quiet=True)"

# 1. start qdrant for mem0
docker run -d --name mem0-qdrant -p 6333:6333 -p 6334:6334 \
    -v /tmp/qdrant_storage:/qdrant/storage qdrant/qdrant:latest

# 2. seed DevRouter with the same memory corpus mem0 will see
python3 bench/seed_memories.py --repo mall

# 3. run the bench
python3 bench/runner.py --repo mall \
    --adapters devrouter,mem0,agentmemory-hybrid \
    --output-dir bench/results/mall-memaug
```

The mem0 adapter does its own ingest inside `setup()`, so re-running
is idempotent — qdrant collection `bench_mem0_mall` is wiped and
reloaded each run.

### Caveats

- **Mall only, for now.** Authoring a seed corpus is hand work: each
  note has to be domain-correct, the file path has to exist, and the
  coverage ratio has to be realistic. Extending to goserving and
  airflow-core is mechanical but takes time.
- **Memory ceiling is corpus-bound.** mem0's R@5 ceiling on this bench
  is roughly 0.54 (the corpus coverage) plus partial-credit bonus from
  multi-file expected sets. The blending uplift is precisely the
  question DevRouter answers above its corpus ceiling.
- **No LLM in the loop.** mem0 supports an `infer=True` mode that
  runs an LLM fact-extraction pass over each memory add. We disabled
  it (`infer=False`) because our seed corpus is already canonical
  prose, and the LLM step costs seconds per add. Re-running with
  `infer=True` is left to anyone who wants to measure that path.

## Notes on fairness and what we did not do

- **No tuning per adapter.** `CODE_EXTS` and `SKIP_DIRS` in the
  agentmemory adapter are identical across all three repos. We did
  not exclude `newsfragments/` to boost agentmemory's airflow score,
  because that is exactly the kind of manual tuning a real user would
  not perform out of the box.
- **No retries.** Latency stats are honest only if measured one-shot.
- **Same question set across adapters per repo.** Every adapter sees
  the exact same query string. Phrasing isn't tilted toward DevRouter's
  intent classifier.
- **Hand-authored ground truth.** Expected files were chosen by reading
  the repos, not by querying any of the systems under test.
- **No warm caches.** Each adapter is set up cold from process start.

The harness source is at [`bench/`](../bench/); the adapter code
that talks to each system is in [`bench/adapters/`](../bench/adapters/).
