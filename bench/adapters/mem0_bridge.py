"""Long-lived Python bridge for the mem0 adapter.

Why a bridge: mem0 + chromadb + qdrant-client + ollama-client pull in
~300 MB of dependencies (faiss, redisvl, nltk, sentence-transformers as
transitive). We pin them into a dedicated venv at `bench/.mem0-venv/`
so the main harness Python stays slim. This bridge runs *inside* that
venv via subprocess; the parent adapter talks to it over stdin/stdout
NDJSON, identical pattern to `bench/adapters/agentmemory_bridge.mjs`.

Protocol (NDJSON over stdin → stdout, one line each):

  parent → bridge:
    {"op": "ingest", "memories": [{"file_path": "...", "memory": "..."}, ...],
     "user_id": "bench-mall"}
    {"op": "query",  "query": "...", "user_id": "bench-mall", "k": 10}
    {"op": "reset"}    # wipe the qdrant collection
    {"op": "shutdown"}

  bridge → parent:
    {"ok": true, "n": 30}                    # ingest reply
    {"ok": true, "results": [{"file_path":"...", "score": 0.74,
                              "memory": "..."}], "ms": 18.3}
    {"ok": true}                             # reset/shutdown reply
    {"ok": false, "error": "..."}            # on any failure

Why qdrant (not chroma / faiss): mem0's score_and_rank assumes the
vector store returns *cosine similarity* (high=good). Chroma and
faiss adapters in mem0 v2.0.2 pass through raw L2 *distance* (low=good)
verbatim, which inverts the final ranking — the correct answer ends
up last. Qdrant returns cosine similarity natively, so mem0's scoring
works as designed. (Verified 2026-05-14 against a 3-memory smoke
test: qdrant ranks scheduler_job_runner.py first for "how does the
scheduler enqueue tasks"; chroma + faiss rank it third.)
"""

from __future__ import annotations

import json
import os
import sys
import time
import uuid

# When this file is invoked as a script (subprocess.Popen),
# Python prepends `bench/adapters/` to sys.path. That directory
# also contains `mem0.py` (our *adapter* file, not the package),
# which would shadow the real mem0ai package. Drop the script-dir
# entry so `import mem0` resolves to the venv's site-packages.
_HERE = os.path.dirname(os.path.abspath(__file__))
sys.path = [p for p in sys.path if os.path.abspath(p) != _HERE]

# This file runs inside .mem0-venv. The next import lives in that venv.
from mem0 import Memory  # type: ignore
import requests  # type: ignore


QDRANT_HOST = os.environ.get("MEM0_QDRANT_HOST", "localhost")
QDRANT_PORT = int(os.environ.get("MEM0_QDRANT_PORT", "6333"))
OLLAMA_URL = os.environ.get("MEM0_OLLAMA_URL", "http://localhost:11434")
COLLECTION = os.environ.get("MEM0_COLLECTION", f"bench_{uuid.uuid4().hex[:8]}")


def _build_config() -> dict:
    return {
        # Qdrant — see module docstring for why not chroma/faiss.
        "vector_store": {
            "provider": "qdrant",
            "config": {
                "collection_name": COLLECTION,
                "embedding_model_dims": 768,
                "host": QDRANT_HOST,
                "port": QDRANT_PORT,
            },
        },
        # Local Ollama — same backend devrouter uses, so we're not
        # comparing against an OpenAI text-embedding-3-* that DevRouter
        # doesn't get to use.
        "embedder": {
            "provider": "ollama",
            "config": {
                "model": "nomic-embed-text",
                "ollama_base_url": OLLAMA_URL,
                "embedding_dims": 768,
            },
        },
        # mem0's Memory class requires an LLM provider even when
        # infer=False. We point it at the smallest local model so
        # instantiation succeeds; we never actually call it.
        "llm": {
            "provider": "ollama",
            "config": {
                "model": "qwen2.5:0.5b",
                "ollama_base_url": OLLAMA_URL,
            },
        },
    }


def _wipe_collection() -> None:
    """Drop the qdrant collection if it exists. Idempotent."""
    try:
        requests.delete(
            f"http://{QDRANT_HOST}:{QDRANT_PORT}/collections/{COLLECTION}",
            timeout=5,
        )
    except Exception:
        # Pre-init: collection may not exist yet, that's fine.
        pass


def _ingest(mem: Memory, memories: list[dict], user_id: str) -> int:
    """Insert memories with infer=False so mem0 doesn't run an LLM
    fact-extraction pass over our hand-authored notes — they're
    already the canonical form, and the LLM step would (a) cost
    seconds per add and (b) potentially rewrite the note text in a
    way that loses the file_path tie."""
    n = 0
    for m in memories:
        mem.add(
            m["memory"],
            user_id=user_id,
            infer=False,
            metadata={"file_path": m["file_path"]},
        )
        n += 1
    return n


def _extract_file_path(hit: dict) -> str | None:
    """mem0 hits can carry metadata under either key depending on
    vector-store backend. Check both."""
    md = hit.get("metadata") or hit.get("payload") or {}
    if isinstance(md, dict):
        return md.get("file_path")
    return None


def _query(mem: Memory, query: str, user_id: str, k: int) -> tuple[list[dict], float]:
    t0 = time.perf_counter()
    rr = mem.search(query, filters={"user_id": user_id}, limit=k)
    elapsed = (time.perf_counter() - t0) * 1000.0
    results = rr.get("results", []) if isinstance(rr, dict) else rr
    out: list[dict] = []
    for hit in results[:k]:
        fp = _extract_file_path(hit)
        if not fp:
            # Skip memories without a file_path — they wouldn't help the
            # bench scorer anyway since ground truth is file-path based.
            continue
        out.append(
            {
                "file_path": fp,
                "score": float(hit.get("score") or 0.0),
                "memory": hit.get("memory", ""),
            }
        )
    return out, elapsed


def _reply(payload: dict) -> None:
    sys.stdout.write(json.dumps(payload) + "\n")
    sys.stdout.flush()


def main() -> int:
    mem: Memory | None = None
    user_id_default = "bench"
    for raw in sys.stdin:
        raw = raw.strip()
        if not raw:
            continue
        try:
            req = json.loads(raw)
        except json.JSONDecodeError as e:
            _reply({"ok": False, "error": f"bad-json: {e}"})
            continue
        op = req.get("op")
        try:
            if op == "ingest":
                # New collection per ingest; tolerate re-entrant ingest
                # calls by wiping first.
                _wipe_collection()
                mem = Memory.from_config(_build_config())
                user_id = req.get("user_id") or user_id_default
                n = _ingest(mem, req.get("memories", []), user_id)
                _reply({"ok": True, "n": n})
            elif op == "query":
                if mem is None:
                    _reply({"ok": False, "error": "not-ingested"})
                    continue
                user_id = req.get("user_id") or user_id_default
                k = int(req.get("k", 10))
                results, ms = _query(mem, req["query"], user_id, k)
                _reply({"ok": True, "results": results, "ms": ms})
            elif op == "reset":
                _wipe_collection()
                mem = None
                _reply({"ok": True})
            elif op == "shutdown":
                _reply({"ok": True})
                return 0
            else:
                _reply({"ok": False, "error": f"unknown-op: {op}"})
        except Exception as e:  # pragma: no cover — surface any mem0 internal failure
            _reply({"ok": False, "error": f"{type(e).__name__}: {e}"})
    return 0


if __name__ == "__main__":
    sys.exit(main())
