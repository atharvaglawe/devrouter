"""agentmemory adapter — drives their SearchIndex / HybridSearch primitives.

Why this adapter exists
-----------------------
agentmemory is the named challenger in our README's Phase-2 plan. Their
LongMemEval-S benchmark reports 95.2% R@5 on chat memory retrieval. The
question we want to answer is: when you put the *same retrieval engine* on
the haystack DevRouter was built for — a real codebase, not chat sessions —
what does it score? That tells us where agentmemory genuinely competes with
DevRouter and where the categories diverge.

How it works
------------
1. setup() walks the target repo, builds a list of {path, content} docs, and
   hands them to a Node bridge process. The bridge owns agentmemory's
   SearchIndex (and VectorIndex for hybrid mode) and keeps it warm for the
   duration of the run.
2. query() sends the question to the bridge and reads back a ranked list of
   file paths. Latency timing covers only the round-trip — setup cost is
   amortized into setup() to match the codegraph / devrouter behavior.
3. teardown() asks the bridge to shut down cleanly so the next adapter can
   own the process tree.

Modes
-----
- bm25:    SearchIndex only. Matches their "BM25-only fallback" cell
           (86.2% R@5 on LongMemEval).
- hybrid:  BM25 + Vector + Xenova/all-MiniLM-L6-v2 embeddings via
           HybridSearch. Matches their headline "BM25+Vector" cell
           (95.2% R@5). Setup is slow on first run (~30s model download +
           ~1ms/file embedding) but query is sub-millisecond after.

We register both as separate adapters (`agentmemory-bm25`, `agentmemory-hybrid`)
so the report shows both points on the same axis — same ablation choice
agentmemory's own benchmark publishes.

File-set fairness
-----------------
Code retrieval needs sensible content. We send every text file whose
extension is in CODE_EXTS, capped at MAX_BYTES per file. Binary, vendored,
and oversized files are skipped — same defaults you'd want for any
"ingest-a-repo" tool. The cap matters: their VectorIndex uses 384-dim
embeddings on the first 512 chars of each doc, so a 2 MB generated file
contributes nothing extra after truncation but does bloat memory.
"""

from __future__ import annotations

import json
import os
import shutil
import subprocess
import time
from pathlib import Path
from typing import Any

from .base import Adapter, AdapterResult, approx_tokens, normalize_path, register


# Extensions worth ingesting. Aggressively pruned — agentmemory's index isn't
# trying to match grep, so feeding it lock files or minified bundles wastes
# index space and gives it false negatives.
CODE_EXTS = {
    ".go", ".rs", ".py", ".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs",
    ".java", ".kt", ".scala", ".rb", ".php", ".cs", ".swift", ".m",
    ".c", ".h", ".cc", ".cpp", ".hpp",
    ".md", ".rst", ".txt",
    ".yaml", ".yml", ".toml", ".ini", ".conf",
    ".sql", ".proto", ".graphql", ".html",
}

# Cap per-file content. agentmemory's vector index uses the first 512 chars
# only, BM25 indexes everything. 64 KB is plenty for any hand-written source
# file and cuts off generated megabytes-of-strings dumps.
MAX_BYTES = 64 * 1024

# Skip these directory names anywhere in the path. Standard "stuff that's
# not source you wrote" list.
SKIP_DIRS = {
    "node_modules", "vendor", "dist", "build", "target", ".git", ".idea",
    ".vscode", "__pycache__", ".pytest_cache", ".next", ".turbo", ".cache",
    "coverage", ".venv", "venv", "env",
}


class _AgentMemoryBase(Adapter):
    """Shared Python<->Node bridge plumbing for both bm25 and hybrid modes."""

    # Override in subclasses.
    mode: str = "bm25"

    def __init__(self) -> None:
        here = os.path.dirname(os.path.abspath(__file__))
        self.bridge_script = os.path.join(here, "agentmemory_bridge.mjs")
        self.bridge_cwd = os.path.join(here, "agentmemory_src")
        self._proc: subprocess.Popen | None = None
        self._repo_root: str = ""

    # ------------------------------------------------------------------
    # Lifecycle
    # ------------------------------------------------------------------

    def setup(self, repo: str, repo_root: str) -> None:
        if not os.path.isdir(self.bridge_cwd):
            raise RuntimeError(
                f"agentmemory source not vendored at {self.bridge_cwd}. "
                f"Run `bench/scripts/setup_agentmemory.sh` first "
                f"(clones rohitg00/agentmemory and runs npm install)."
            )
        if not os.path.isfile(self.bridge_script):
            raise RuntimeError(f"bridge script missing: {self.bridge_script}")
        npx = shutil.which("npx")
        if npx is None:
            raise RuntimeError(
                "npx not found on PATH. Install Node.js >= 20 to run the "
                "agentmemory adapter."
            )
        self._repo_root = repo_root

        # Spawn the bridge via tsx so it can import .ts sources directly.
        # bufsize=1 makes stdout line-buffered for the request/response loop.
        self._proc = subprocess.Popen(  # noqa: S603
            [npx, "tsx", self.bridge_script],
            cwd=self.bridge_cwd,
            stdin=subprocess.PIPE,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
            bufsize=1,
        )

        docs = list(self._enumerate_docs(repo_root))
        if not docs:
            raise RuntimeError(
                f"no code files found under {repo_root}; check CODE_EXTS / SKIP_DIRS"
            )

        resp = self._call({"cmd": "setup", "mode": self.mode, "docs": docs}, timeout=600.0)
        if not resp.get("ok"):
            raise RuntimeError(f"bridge setup failed: {resp.get('error')}")
        print(
            f"[bench]   agentmemory({self.mode}) ingested {resp.get('n_docs')} docs "
            f"in {resp.get('ms')} ms",
            flush=True,
        )

    def teardown(self) -> None:
        if self._proc is None:
            return
        try:
            self._call({"cmd": "shutdown"}, timeout=5.0)
        except Exception:  # noqa: BLE001 - best effort
            pass
        try:
            self._proc.wait(timeout=5)
        except subprocess.TimeoutExpired:
            self._proc.kill()
        finally:
            self._proc = None

    # ------------------------------------------------------------------
    # Query
    # ------------------------------------------------------------------

    def query(self, q: str, repo: str, k: int) -> AdapterResult:
        if self._proc is None:
            return AdapterResult(error="adapter not set up")

        start = time.perf_counter()
        try:
            resp = self._call({"cmd": "query", "q": q, "k": k}, timeout=30.0)
        except Exception as e:  # noqa: BLE001
            return AdapterResult(error=f"agentmemory query failed: {e}")
        elapsed_ms = (time.perf_counter() - start) * 1000.0

        if not resp.get("ok"):
            return AdapterResult(
                error=f"bridge query error: {resp.get('error')}",
                latency_ms=elapsed_ms,
            )

        # Bridge already returns repo-relative paths (we ingested them that
        # way). normalize_path is defensive in case a future change ships
        # absolute paths.
        files = [normalize_path(p, self._repo_root) for p in resp.get("files", [])]
        files = [p for p in files if p][:k]

        # Token cost for agentmemory: the agent receives a list of
        # `HybridSearchResult.observation` records. In practice that's
        # an object per match with `.narrative` (the file content we
        # ingested, capped at MAX_BYTES per file in _enumerate_docs).
        # We charge the actual narrative + path bytes the agent would
        # see — same accounting as codegraph's snippet column.
        token_chars = 0
        for rel in files:
            full = os.path.join(self._repo_root, rel)
            try:
                token_chars += min(os.path.getsize(full), MAX_BYTES)
            except OSError:
                pass
            token_chars += len(rel) + 16  # path + metadata overhead

        return AdapterResult(
            files=files,
            latency_ms=elapsed_ms,
            tokens_returned=approx_tokens("x" * token_chars),
            raw={"bridge_ms": resp.get("ms"), "scores": resp.get("scores", [])[:k]},
        )

    # ------------------------------------------------------------------
    # Internals
    # ------------------------------------------------------------------

    def _enumerate_docs(self, repo_root: str) -> Any:
        """Walk the repo and yield {id, path, text} dicts the bridge accepts.

        We do all filtering on the Python side so the Node bridge stays
        dumb — no filesystem awareness, just an in-memory index.
        """
        for root, dirs, files in os.walk(repo_root):
            # Mutate dirs in place to prune the descent — standard `os.walk`
            # pattern. SKIP_DIRS skips by name, not by full path, so any
            # `vendor/` anywhere in the tree is pruned.
            dirs[:] = [d for d in dirs if d not in SKIP_DIRS and not d.startswith(".")]
            for name in files:
                ext = os.path.splitext(name)[1].lower()
                if ext not in CODE_EXTS:
                    continue
                full = os.path.join(root, name)
                try:
                    sz = os.path.getsize(full)
                except OSError:
                    continue
                if sz == 0 or sz > MAX_BYTES * 4:
                    # Skip empty and very-large files outright. MAX_BYTES * 4
                    # so that a 200KB hand-written file still has its head
                    # ingested via the truncation below, but a 10MB generated
                    # file is dropped entirely (the whole point is to keep
                    # the index lean).
                    continue
                try:
                    with open(full, "rb") as f:
                        raw = f.read(MAX_BYTES)
                except OSError:
                    continue
                try:
                    text = raw.decode("utf-8", errors="ignore")
                except Exception:  # noqa: BLE001
                    continue
                if not text.strip():
                    continue
                rel = os.path.relpath(full, repo_root)
                rel_posix = rel.replace(os.sep, "/")
                yield {"id": f"obs_{rel_posix}", "path": rel_posix, "text": text}

    def _call(self, msg: dict, timeout: float) -> dict:
        if self._proc is None or self._proc.stdin is None or self._proc.stdout is None:
            raise RuntimeError("bridge process not running")
        self._proc.stdin.write(json.dumps(msg) + "\n")
        self._proc.stdin.flush()

        deadline = time.monotonic() + timeout
        while True:
            if time.monotonic() > deadline:
                raise TimeoutError(f"bridge {msg.get('cmd')!r} timed out after {timeout}s")
            line = self._proc.stdout.readline()
            if not line:
                # Process died — surface its stderr for diagnosis.
                err = ""
                if self._proc.stderr:
                    try:
                        err = self._proc.stderr.read() or ""
                    except OSError:
                        pass
                raise RuntimeError(
                    f"bridge exited unexpectedly (rc={self._proc.poll()}): {err[:500]}"
                )
            line = line.strip()
            if not line:
                continue
            try:
                return json.loads(line)
            except json.JSONDecodeError:
                # Bridge should never emit non-JSON on stdout (logs go to
                # stderr), but if a future change leaks output we skip it.
                continue


@register
class AgentMemoryBm25Adapter(_AgentMemoryBase):
    """agentmemory's BM25-only path (their 'BM25-only fallback' configuration).

    Matches the 86.2% R@5 published cell from LongMemEval. Fast setup
    (no embedding model), sub-ms queries. Useful baseline for isolating
    the contribution of agentmemory's vector half vs BM25 half on
    code retrieval.
    """

    name = "agentmemory-bm25"
    mode = "bm25"


@register
class AgentMemoryHybridAdapter(_AgentMemoryBase):
    """agentmemory's hybrid BM25+Vector path (their headline configuration).

    Matches the 95.2% R@5 published cell from LongMemEval. First-run setup
    is slow (downloads ~25 MB Xenova/all-MiniLM-L6-v2 model and embeds
    every file's first 512 chars), but query latency stays sub-ms after.
    """

    name = "agentmemory-hybrid"
    mode = "hybrid"
