# DevRouter Benchmark Harness

A retrieval-quality benchmark for DevRouter and competing memory/context
systems for AI coding agents.

## Why this exists

The agentmemory README cites 95.2% R@5 on **LongMemEval-S** — a *general
long-term chat memory* benchmark. DevRouter is not in that category: it
targets per-repo *code-aware retrieval*. Putting it on LongMemEval would be
unfair and uninformative.

This harness benchmarks DevRouter on the category it was actually built for:
**given a developer question about a real repo, does the system surface the
files (and ideally symbols) a human would have read to answer it?**

We measure that with R@5, R@10, and MRR over a hand-authored ground-truth
set (currently 30 goserving questions), plus latency p50/p95 and
**tokens-returned p50/p95** — the latter being the dimension agentmemory's
own README leans on hardest, and the one where DevRouter's `codegraph`
core actually shines (112 tokens p50 vs agentmemory-hybrid's 5,794).

## Systems compared

| Adapter               | Class             | Status              |
| --------------------- | ----------------- | ------------------- |
| `devrouter`           | full pipeline     | **live**            |
| `codegraph`           | DR ablation       | **live**            |
| `grep`                | "no system" floor | **live**            |
| `claudemd`            | "do nothing"      | **live**            |
| `agentmemory-bm25`    | competitor        | **live**            |
| `agentmemory-hybrid`  | competitor        | **live**            |
| `zoekt`               | code-search floor | **live**            |
| `aider`               | code-native       | attempted, abandoned (see below) |
| `mem0`                | general memory    | skipped (requires LLM for ingest) |
| `continue.dev`        | IDE-native        | skipped (no headless mode) |
| `cody`                | code-native       | skipped (requires Sourcegraph cloud) |

### Why some competitors aren't here

* **`aider`** — aider's `RepoMap` is the right comparison target on
  paper (code-native, on-device PageRank over a tree-sitter symbol
  graph). We got the venv set up and the offline-mode smoke test passed,
  but driving it as a subprocess turned out to be fragile: aider's
  `InputOutput` constructor writes "Detected dumb terminal…" to stdout
  before we can intercept it, which corrupts the JSON line protocol we
  use to talk to the worker. Fixable with deeper plumbing (custom fd,
  patching `aider.io` at import time) but for a benchmark harness it
  was buying complexity without changing the story — `aider`'s
  RepoMap is conceptually similar to `codegraph`'s symbol graph and
  would land in the same Pareto-frontier neighbourhood.
* **`mem0`** — mem0's retrieval path requires an LLM at ingest time
  to summarize observations into structured memories. Running it
  "locally" needs Ollama (~4 GB model) and even then the answers
  diverge from mem0's published numbers because the LLM is different.
  Skipped to keep the harness LLM-free.
* **`continue.dev`** — context retrieval is locked inside their VS
  Code extension; there's no headless library entry point. Running it
  would mean automating an Electron app, which is far beyond a bench
  harness budget.
* **`cody`** — Sourcegraph Cody's retrieval ranks against their cloud
  embeddings service. There's no local-only mode for code search;
  ingest pushes to their backend. Excluded on the privacy axis.

The two `agentmemory-*` adapters drive the same `SearchIndex` /
`VectorIndex` / `HybridSearch` primitives that produce agentmemory's
published 95.2% R@5 on LongMemEval-S. We vendor their source via
`bench/scripts/setup_agentmemory.sh` (gitignored under
`adapters/agentmemory_src/`) and drive it from a small Node bridge
([`adapters/agentmemory_bridge.mjs`](adapters/agentmemory_bridge.mjs))
over stdio.

## Layout

```
bench/
├── README.md            # this file
├── runner.py            # entrypoint: runs adapters × questions, scores, writes report
├── score.py             # R@K, MRR, latency stats
├── adapters/
│   ├── base.py          # Adapter interface
│   ├── devrouter.py     # MCP stdio call to dev_context
│   ├── codegraph.py     # HTTP to codegraph /api/search
│   ├── grep.py          # subprocess to rg/grep
│   ├── claudemd.py      # parse repo CLAUDE.md baseline
│   └── …                # competitor adapters (Phase 2)
├── questions/
│   ├── goserving.jsonl  # ground-truth Q&A for goserving
│   └── README.md        # question authoring guide
└── results/             # benchmark outputs (gitignored)
```

## Question format

One JSON object per line in `questions/<repo>.jsonl`:

```json
{
  "id": "goserving-001",
  "repo": "goserving",
  "intent": "trace",
  "query": "Where is the FmsController served from?",
  "expected_files": ["controllers/fms_controller.go", "routes/api.go"],
  "expected_symbols": ["FmsController.Get", "registerAPIRoutes"],
  "notes": "Optional human note about why these are the right answer."
}
```

* `expected_files` is the **gold set** for R@K scoring.
* `expected_symbols` is optional, used for symbol-level scoring when the
  adapter exposes symbol lists (devrouter does, grep does not).
* `intent` is one of `debug | trace | refactor | explore | general` — used
  for per-intent breakdown in the final report.

## How to run

```bash
# One-time: vendor agentmemory's source (skip if you don't need that adapter)
bench/scripts/setup_agentmemory.sh

# Run all Phase-1 adapters on all goserving questions
python3 bench/runner.py --repo goserving

# Subset: just devrouter and grep
python3 bench/runner.py --repo goserving --adapters devrouter,grep

# Full head-to-head with agentmemory
python3 bench/runner.py --repo goserving \
  --adapters devrouter,codegraph,grep,claudemd,agentmemory-bm25,agentmemory-hybrid

# K override (default 10)
python3 bench/runner.py --repo goserving --k 5

# Single question for debugging
python3 bench/runner.py --repo goserving --question-id goserving-001
```

`agentmemory-hybrid` is slow on first run (~160 s to embed 7,646
goserving files with `Xenova/all-MiniLM-L6-v2`). Subsequent runs hit
the local model cache and complete in the same time as `bm25`.

Results land in `results/<timestamp>/` with a markdown report and a JSONL
of per-question per-adapter outputs.

The current headline run is the 7-adapter sweep documented in
[`results/20260513-222734/FINDINGS.md`](results/20260513-222734/FINDINGS.md)
(30 questions, codegraph + devrouter + claudemd + agentmemory-bm25 +
agentmemory-hybrid + grep + zoekt). It establishes a 3-point Pareto
frontier — `codegraph` for lean context, `agentmemory-hybrid` for raw
recall, `zoekt` in the middle — with the other four adapters strictly
dominated.

Prior 6-adapter run (before zoekt) is at
[`results/20260513-215059/`](results/20260513-215059) with its own
[`FINDINGS.md`](results/20260513-215059/FINDINGS.md) covering the
token-efficiency column in detail.

### SCALE — degradation curves

[`results/20260513-224451-scale/SCALE.md`](results/20260513-224451-scale/SCALE.md)
and its
[`FINDINGS.md`](results/20260513-224451-scale/FINDINGS.md) chart how
each adapter behaves at 100 %, 50 %, and 25 % of the goserving corpus.
Three headline findings:

1. **Setup is linear, with a wide constant.** codegraph pays ~6 ms
   per file (47 s on 8k files); zoekt is ~20× cheaper.
2. **Recall *improves* as the corpus shrinks** — the smaller the
   pool, the less competition for the top-K slots. Naive adapters
   (grep, BM25) gain the most; codegraph gains the least (it already
   filters distractors aggressively).
3. **Token cost is essentially flat with scale**, because top-K is
   fixed. So codegraph's 112-tokens-per-answer headline holds at
   every repo size, and grep's 35k-token blow-up holds at every
   size too.

To re-run:

```bash
python3 bench/scale.py --repo goserving --scales 100,50,25 \
  --adapters codegraph,zoekt,grep,agentmemory-bm25
```

Earlier runs:
* [`results/20260513-212605/`](results/20260513-212605) — first
  6-adapter run on the original 12-question set
  ([`FINDINGS.md`](results/20260513-212605/FINDINGS.md)).
* [`results/20260513-205818/`](results/20260513-205818) — 4-adapter
  baseline after the codegraph FTS fix.
* [`results/20260512-190058/`](results/20260512-190058) — second 4-adapter
  run, [`FINDINGS.md`](results/20260512-190058/FINDINGS.md) explains how
  the codegraph FTS fix moved its R@5 from 0.000 → 0.375.
* [`results/20260512-170558/`](results/20260512-170558) — first run
  ever; codegraph at 0.000 because FTS wasn't loaded for `/api/search`.

## Adapter contract

Every adapter implements:

```python
class Adapter:
    name: str
    def setup(self, repo_root: str) -> None: ...   # one-time per repo
    def query(self, q: str, repo: str, k: int) -> AdapterResult: ...
    def teardown(self) -> None: ...
```

`AdapterResult` is:

```python
@dataclass
class AdapterResult:
    files: list[str]                  # ranked, top-K, repo-relative paths
    symbols: list[str] = ()           # optional, ranked
    latency_ms: float = 0.0
    tokens_returned: int = 0          # ~chars/4 of everything the agent would consume
    raw: dict = ()                    # opaque debug payload
    error: str | None = None
```

The `tokens_returned` accounting is per-adapter and tries to charge the
*real* prompt the agent would receive: file content for grep, symbol +
snippet rows for codegraph, the serialized DevPrompt for devrouter,
observation narratives for agentmemory, the full doc body for claudemd.
See [`adapters/base.py`](adapters/base.py) for the `approx_tokens`
rule-of-thumb (4 chars/token) and each adapter for what it counts.

Adapters MUST normalize `files` to **repo-relative POSIX paths**. The scorer
joins on exact string match against the gold set, so canonicalize aggressively
(strip leading `./`, leading repo root, trailing slash, etc.).

## Why Python (not Go)

The harness has to drive: an MCP server over stdio (devrouter), HTTP
(codegraph, khoj), Node-based npm packages (agentmemory, claude-mem),
Python packages with vector DB deps (mem0), and a heavyweight agent runtime
(letta). Python is the only language with first-class clients for all of
them, and the harness is not on a hot path.
