"""ripgrep / grep adapter — the "no system" floor.

Establishes the lower bound for any retrieval system: if a memory engine
can't beat blind text search, it's not earning its keep. Uses `rg` if
present, falls back to BSD/GNU `grep -r` so the harness runs out of the
box on macOS without extra installs.

Ranking: files are ordered by hit count (most matches first). For multi-
word queries we run one search per "interesting" token (length ≥ 3, not in
a tiny stopword list) and sum hits per file. This is intentionally cheap
and dumb — the whole point of this adapter is to be the floor.
"""

from __future__ import annotations

import os
import shutil
import subprocess
import time
from collections import Counter

from .base import Adapter, AdapterResult, approx_tokens, normalize_path, register

_STOPWORDS = {
    "the", "a", "an", "is", "are", "was", "were", "be", "been", "being",
    "of", "in", "on", "at", "by", "for", "to", "from", "and", "or", "not",
    "where", "what", "when", "why", "how", "which", "who", "does", "do",
    "did", "this", "that", "with", "as", "it", "its",
}


def _tokens(query: str) -> list[str]:
    raw = "".join(c if c.isalnum() or c in "_-." else " " for c in query.lower())
    return [t for t in raw.split() if len(t) >= 3 and t not in _STOPWORDS]


@register
class GrepAdapter(Adapter):
    name = "grep"

    def __init__(self) -> None:
        self._tool: str | None = None  # "rg" or "grep"
        self._repo_root: str = ""

    def setup(self, repo: str, repo_root: str) -> None:
        self._repo_root = repo_root
        if shutil.which("rg"):
            self._tool = "rg"
        elif shutil.which("grep"):
            self._tool = "grep"
        else:
            raise RuntimeError("neither rg nor grep is on PATH")

    def query(self, q: str, repo: str, k: int) -> AdapterResult:
        toks = _tokens(q)
        if not toks:
            return AdapterResult(error="empty token set after stopword filter")
        if not self._tool:
            return AdapterResult(error="adapter not set up")

        start = time.perf_counter()
        hits: Counter[str] = Counter()
        for tok in toks:
            for path in self._search_paths(tok):
                hits[path] += 1
        elapsed_ms = (time.perf_counter() - start) * 1000.0

        ranked = [
            normalize_path(p, self._repo_root)
            for p, _ in hits.most_common(max(k, 10))
        ]
        ranked = [p for p in ranked if p]
        top = ranked[:k]

        # Token budget for grep is "the agent has to read each matched
        # file to figure out which is relevant" — so we charge the full
        # file content for every top-K file. This matches how Cursor /
        # Claude Code would consume a list of files-with-matches.
        # Capped at MAX_BYTES_PER_FILE so a 10 MB generated file doesn't
        # explode the estimate. 64 KB matches what every other "eager
        # content" adapter uses (agentmemory, codegraph file-head
        # fallback, score.uniform_tokens_for_files), so the native
        # token column is symmetric across systems.
        MAX_BYTES_PER_FILE = 64 * 1024
        total_chars = 0
        for rel in top:
            full = os.path.join(self._repo_root, rel)
            try:
                sz = min(os.path.getsize(full), MAX_BYTES_PER_FILE)
            except OSError:
                continue
            total_chars += sz
        tokens = approx_tokens("x" * total_chars)

        return AdapterResult(
            files=top,
            symbols=[],
            latency_ms=elapsed_ms,
            tokens_returned=tokens,
            raw={"tokens": toks, "hits": dict(hits.most_common(20))},
        )

    def _search_paths(self, token: str) -> list[str]:
        """Return the list of file paths that contain `token` at least once.

        Uses `--files-with-matches` style flags so we get one path per file
        regardless of in-file hit count — the adapter's outer Counter then
        accumulates "how many tokens of the query hit this file" as the
        ranking signal.
        """
        if self._tool == "rg":
            cmd = ["rg", "--files-with-matches", "--no-messages",
                   "--hidden", "--glob", "!.git", "--", token, self._repo_root]
        else:
            cmd = ["grep", "-rIl",
                   "--exclude-dir=.git", "--exclude-dir=node_modules",
                   "--exclude-dir=vendor", "--exclude-dir=dist",
                   "--", token, self._repo_root]
        try:
            proc = subprocess.run(
                cmd, capture_output=True, text=True, timeout=15,
                check=False,
                env={**os.environ, "LC_ALL": "C"},
            )
        except subprocess.TimeoutExpired:
            return []
        return [line for line in proc.stdout.splitlines() if line.strip()]
