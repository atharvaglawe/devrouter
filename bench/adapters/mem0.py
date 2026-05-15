"""mem0 adapter — drives mem0ai's Memory.add + Memory.search.

Why this adapter exists
-----------------------
mem0 (53K ⭐ on GitHub) is the largest agent-memory framework in the
ecosystem and the most direct overlap with the *memory* half of
DevRouter (the half the original cross-language bench did not exercise
because it ran cold-start with zero saved memories).

The bench DevRouter previously ran answers "given a code question,
does the system find the right files?" via codegraph + graph + anchors.
This adapter exists to ask the orthogonal question: "given a
**seeded memory corpus** describing the repo, does the system find
the right files?" — which is what mem0 (and DevRouter's memory layer)
were actually built for.

How it works
------------
1. setup() loads `bench/memories/<repo>.jsonl` — hand-authored notes
   about specific files, one record per line: {"file_path": "...",
   "memory": "..."}. It spawns the long-lived `mem0_bridge.py`
   subprocess inside `bench/.mem0-venv/` (mem0 + its transitive deps
   are pinned there to keep them out of the harness venv) and sends
   an `ingest` op to load all memories into a fresh qdrant
   collection.
2. query() sends a `query` op with the question; the bridge runs
   mem0.search, extracts each hit's file_path from metadata, and
   returns the ranked list. Latency is reported by the bridge so
   it covers only mem0's own work, not the JSON marshalling.
3. teardown() sends `shutdown`.

Backend choice
--------------
- Vector store: **qdrant** (Docker container on :6333). mem0's
  score_and_rank assumes vector-store score is cosine similarity
  (high=good). qdrant returns exactly that. The chroma and faiss
  backends in mem0 v2.0.2 pass raw L2 distance through verbatim,
  which inverts the final ranking — verified empirically against a
  3-memory smoke test on 2026-05-14.
- Embedder: **ollama / nomic-embed-text**. Local, 768-dim, no API
  key. Same backend DevRouter uses, so we're not stacking the deck
  with OpenAI text-embedding-3-*.
- LLM: **ollama / qwen2.5:0.5b**. mem0's Memory class requires an
  LLM at construction time even when we pass `infer=False` to skip
  fact-extraction during add. The smallest local model satisfies
  this and is never actually invoked.

Fairness with DevRouter
-----------------------
- Both systems get the **same seeded memory corpus** (DevRouter via
  its `memory_save_file` MCP tool; mem0 via `Memory.add`).
- Same questions from `bench/questions/<repo>.jsonl`.
- Same expected-file ground truth.
- Both run locally — no API keys, no remote services.

What this adapter cannot test
-----------------------------
mem0 only knows about the memories you give it. If the seed corpus
covers 54% of the ground-truth answer space (as it does for mall),
mem0's ceiling on this bench is roughly that 54%. DevRouter, by
contrast, can fill the remaining 46% by falling back to codegraph
+ graph traversal. The whole point of the bench is to measure that
**blending uplift**.
"""

from __future__ import annotations

import json
import os
import subprocess
import sys
import time
from pathlib import Path
from typing import Any

from .base import Adapter, AdapterResult, approx_tokens, normalize_path, register


HERE = Path(__file__).resolve().parent
REPO_ROOT_OF_DEVROUTER = HERE.parent.parent  # bench/adapters → bench → repo
VENV_PYTHON = REPO_ROOT_OF_DEVROUTER / "bench" / ".mem0-venv" / "bin" / "python3"
BRIDGE_SCRIPT = HERE / "mem0_bridge.py"


class _Mem0Bridge:
    """Long-lived subprocess wrapper for the mem0_bridge.py NDJSON protocol."""

    def __init__(self) -> None:
        if not VENV_PYTHON.exists():
            raise RuntimeError(
                f"mem0 venv missing at {VENV_PYTHON}. Run:\n"
                f"  python3 -m venv bench/.mem0-venv\n"
                f"  bench/.mem0-venv/bin/pip install mem0ai chromadb qdrant-client "
                f"ollama redis redisvl nltk faiss-cpu requests"
            )
        # Inherit env so MEM0_QDRANT_HOST / MEM0_COLLECTION can be
        # overridden by the caller without us hard-coding them here.
        self.proc = subprocess.Popen(
            [str(VENV_PYTHON), str(BRIDGE_SCRIPT)],
            stdin=subprocess.PIPE,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
            bufsize=1,  # line-buffered: NDJSON contract
        )

    def call(self, op: str, **kwargs: Any) -> dict[str, Any]:
        if self.proc.stdin is None or self.proc.stdout is None:
            raise RuntimeError("mem0 bridge subprocess pipes closed")
        msg = json.dumps({"op": op, **kwargs}) + "\n"
        self.proc.stdin.write(msg)
        self.proc.stdin.flush()
        line = self.proc.stdout.readline()
        if not line:
            # Surface any startup stderr so debugging doesn't require
            # rerunning manually.
            stderr = ""
            if self.proc.stderr is not None:
                stderr = self.proc.stderr.read() or ""
            raise RuntimeError(
                f"mem0 bridge died before replying to op={op}. stderr:\n{stderr}"
            )
        return json.loads(line)

    def shutdown(self) -> None:
        try:
            self.call("shutdown")
        except Exception:
            pass
        try:
            self.proc.terminate()
            self.proc.wait(timeout=5)
        except Exception:
            self.proc.kill()


@register
class Mem0Adapter(Adapter):
    """mem0 driven through a venv-isolated bridge.

    Configurable user_id helps run multiple bench shards against the
    same qdrant instance without collision (e.g., one user_id per repo).
    """

    name = "mem0"

    def __init__(self) -> None:
        self.bridge: _Mem0Bridge | None = None
        self.user_id: str = "bench"
        self.n_memories: int = 0

    def setup(self, repo: str, repo_root: str) -> None:
        # Use a deterministic, repo-scoped qdrant collection so a re-run
        # wipes the old state cleanly. The bridge wipes-then-ingests on
        # every ingest op so this is idempotent.
        os.environ["MEM0_COLLECTION"] = f"bench_mem0_{repo}"
        self.user_id = f"bench-{repo}"

        memories_path = REPO_ROOT_OF_DEVROUTER / "bench" / "memories" / f"{repo}.jsonl"
        if not memories_path.exists():
            raise RuntimeError(
                f"mem0 adapter needs a seed corpus at {memories_path}. "
                f"Hand-author one note per line: "
                f'{{"file_path": "...", "memory": "..."}}'
            )

        memories: list[dict[str, str]] = []
        with memories_path.open() as f:
            for line in f:
                line = line.strip()
                if not line:
                    continue
                memories.append(json.loads(line))

        self.bridge = _Mem0Bridge()
        reply = self.bridge.call(
            "ingest",
            memories=memories,
            user_id=self.user_id,
        )
        if not reply.get("ok"):
            raise RuntimeError(
                f"mem0 ingest failed: {reply.get('error', 'unknown')}"
            )
        self.n_memories = int(reply.get("n", 0))
        print(
            f"[bench]   mem0 ingested {self.n_memories} memories into qdrant "
            f"collection {os.environ['MEM0_COLLECTION']}",
            file=sys.stderr,
        )

    def query(self, q: str, repo: str, k: int) -> AdapterResult:
        if self.bridge is None:
            return AdapterResult(error="mem0 bridge not initialised")

        try:
            reply = self.bridge.call(
                "query",
                query=q,
                user_id=self.user_id,
                k=k,
            )
        except Exception as e:
            return AdapterResult(error=f"mem0 bridge call failed: {e}")

        if not reply.get("ok"):
            return AdapterResult(error=reply.get("error", "mem0 search failed"))

        results = reply.get("results", [])
        # mem0 stored canonical repo-relative paths via the adapter's
        # ingest, but normalize anyway so an accidentally-absolute path
        # doesn't sink the scorer.
        # NOTE: We score files by path only — symbols aren't part of mem0's
        # contract, so symbols stays empty (correct: don't fake it).
        files: list[str] = []
        for hit in results:
            fp = hit.get("file_path")
            if not fp:
                continue
            files.append(fp)

        # Token cost — same convention as the agentmemory adapter:
        # count the bytes mem0 would hand back as memory text (those are
        # the agent's actual context budget if they read each result).
        tokens = approx_tokens("\n".join(hit.get("memory", "") for hit in results))

        return AdapterResult(
            files=files,
            symbols=[],
            latency_ms=float(reply.get("ms", 0.0)),
            tokens_returned=tokens,
            raw={"results": results, "n_memories_seeded": self.n_memories},
        )

    def teardown(self) -> None:
        if self.bridge is not None:
            self.bridge.shutdown()
            self.bridge = None
