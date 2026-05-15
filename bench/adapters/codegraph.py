"""codegraph-alone adapter — DevRouter ablation.

Calls the bundled codegraph HTTP server directly, bypassing devrouter's
memory layer, relevance gate, plan sanitizer, and rerank pipeline. The
delta between this adapter's score and `devrouter`'s score isolates how
much value the memory + gate stack adds on top of the raw code-graph
search — the most important internal sanity check in the whole benchmark.

Assumes codegraph is running on http://localhost:4747 (Makefile target
`make codegraph-up`). The adapter does NOT spawn it — that's a global
prerequisite documented in bench/README.md.
"""

from __future__ import annotations

import json
import time
import urllib.error
import urllib.request

from .base import Adapter, AdapterResult, approx_tokens, normalize_path, register


@register
class CodegraphAdapter(Adapter):
    name = "codegraph"

    DEFAULT_BASE = "http://localhost:4747"

    def __init__(self, base_url: str | None = None, mode: str = "hybrid") -> None:
        self.base_url = (base_url or self.DEFAULT_BASE).rstrip("/")
        # mode is "hybrid" (default) | "bm25" | "semantic". Hybrid is what
        # devrouter uses in production so it's the apples-to-apples ablation.
        self.mode = mode
        self._repo_root: str = ""

    def setup(self, repo: str, repo_root: str) -> None:
        self._repo_root = repo_root
        # Liveness probe via /api/info (a normal JSON endpoint). NOT
        # /api/heartbeat — that's an SSE stream that never closes, which
        # would block the probe forever.
        try:
            with urllib.request.urlopen(f"{self.base_url}/api/info", timeout=3) as r:
                if r.status != 200:
                    raise RuntimeError(f"codegraph /api/info returned {r.status}")
        except (urllib.error.URLError, OSError) as e:
            raise RuntimeError(
                f"codegraph not reachable at {self.base_url}: {e}. "
                f"Run `make codegraph-up` first."
            ) from e

    def query(self, q: str, repo: str, k: int) -> AdapterResult:
        # Cap at 100 (codegraph's hard limit per its api.ts) so requesting
        # k=200 doesn't 400. We over-request vs k so we can dedupe and
        # still have enough to fill top-K.
        body = {
            "query": q,
            "repo": repo,
            "limit": min(100, max(k * 2, 20)),
            "mode": self.mode,
            "enrich": True,
            # Ask codegraph to inline the matched source — the API
            # returns a [startLine,endLine] slice for symbol-shaped
            # hits and the file head (capped at 64 KB) for file-level
            # hits. This puts the response shape on par with eager-
            # content retrievers (agentmemory) for an honest token
            # comparison.
            "include_source": True,
        }
        payload = json.dumps(body).encode("utf-8")
        req = urllib.request.Request(
            f"{self.base_url}/api/search",
            data=payload,
            headers={"content-type": "application/json"},
            method="POST",
        )

        start = time.perf_counter()
        try:
            with urllib.request.urlopen(req, timeout=15) as r:
                raw = json.loads(r.read())
        except (urllib.error.URLError, OSError, json.JSONDecodeError) as e:
            return AdapterResult(error=f"codegraph request failed: {e}")
        elapsed_ms = (time.perf_counter() - start) * 1000.0

        results = raw.get("results", []) or []

        # Dedup by file path while preserving codegraph's ranking. Many
        # node hits from the same file should not crowd out other files
        # in the top-K — symbol granularity is the wrong level for a
        # file-level recall metric.
        seen: set[str] = set()
        files: list[str] = []
        symbols: list[str] = []
        # Token cost: with `include_source: true` codegraph inlines
        # the matched source for every row — symbol slices for
        # Function/Method/Class/Interface (just the [startLine,
        # endLine] body) and file heads (capped at 64 KB) for
        # file-level FTS hits. We charge what the API actually
        # returns, which is the same shape agentmemory uses.
        token_text_parts: list[str] = []
        for item in results:
            fp = normalize_path(item.get("filePath") or "", self._repo_root)
            name = item.get("name") or item.get("id")
            source = item.get("source") or ""
            if name:
                symbols.append(name)
            if not fp or fp in seen:
                continue
            seen.add(fp)
            files.append(fp)
            if len(files) <= k:
                token_text_parts.append(f"{fp}\n{name}\n{source}")
            if len(files) >= k:
                break

        return AdapterResult(
            files=files,
            symbols=symbols[: max(k * 2, 20)],
            latency_ms=elapsed_ms,
            tokens_returned=approx_tokens("\n".join(token_text_parts)),
            raw={
                "n_raw_results": len(results),
                "mode": self.mode,
                "accounting": "include_source",
            },
        )
