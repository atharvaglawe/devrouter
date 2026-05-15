"""CLAUDE.md baseline adapter — the "do nothing" floor.

Many teams' "memory system" today is a hand-curated `CLAUDE.md` (or
`.cursor/rules/*.mdc`) file that the agent reads on every turn. This
adapter encodes what that buys you on a retrieval benchmark: nothing
adaptive, just whatever the human chose to write down.

Implementation: parse the repo's `CLAUDE.md` (and `.cursor/rules/*.mdc`),
extract every file path it mentions, score each path by how many query
tokens appear in the surrounding line. Files mentioned at all in the doc
beat files not mentioned, and files mentioned near query keywords beat
files mentioned in unrelated sections.

This is deliberately a low-effort baseline. A more sophisticated version
would chunk the doc and embed it, but at that point we're rebuilding mem0,
which is a separate adapter.
"""

from __future__ import annotations

import os
import re
import time
from collections import Counter
from pathlib import Path

from .base import Adapter, AdapterResult, approx_tokens, normalize_path, register

# Match anything that looks like a file path: at least one slash, ends in a
# common code/doc extension. We intentionally don't require an absolute
# path or specific prefix — CLAUDE.md authors write paths inconsistently.
_PATH_RE = re.compile(
    r"[\w./_\-]*[\w_\-]+\.(?:go|ts|tsx|js|jsx|py|rs|java|kt|cpp|c|h|hpp|"
    r"rb|php|swift|md|yaml|yml|json|toml|sql|sh|html|css)\b"
)

_TOKEN_RE = re.compile(r"[A-Za-z_][A-Za-z0-9_]{2,}")


@register
class ClaudeMdAdapter(Adapter):
    name = "claudemd"

    def __init__(self) -> None:
        self._repo_root: str = ""
        # one entry per (path, line_text) pair found in the rules docs
        self._mentions: list[tuple[str, str]] = []

    def setup(self, repo: str, repo_root: str) -> None:
        self._repo_root = repo_root
        self._mentions = []
        # Track the full byte size of every CLAUDE.md / rules doc we read.
        # In real use, the agent loads ALL of these into context on every
        # turn — that's the whole point of the CLAUDE.md mechanism — so
        # the token cost we charge per-query is the total doc size, not
        # just the matching lines.
        self._docs_total_chars = 0
        for doc_path in self._discover_docs(repo_root):
            try:
                text = Path(doc_path).read_text(errors="ignore")
            except OSError:
                continue
            self._docs_total_chars += len(text)
            for line in text.splitlines():
                for match in _PATH_RE.findall(line):
                    self._mentions.append((match, line))

    def query(self, q: str, repo: str, k: int) -> AdapterResult:
        if not self._mentions:
            return AdapterResult(
                files=[], symbols=[],
                error="no CLAUDE.md / .cursor/rules content found in repo",
            )
        start = time.perf_counter()
        q_tokens = {t.lower() for t in _TOKEN_RE.findall(q)}

        score: Counter[str] = Counter()
        for path, line in self._mentions:
            line_tokens = {t.lower() for t in _TOKEN_RE.findall(line)}
            overlap = len(q_tokens & line_tokens)
            score[path] += overlap + 1  # +1 baseline for "mentioned at all"

        ranked = [normalize_path(p, self._repo_root) for p, _ in score.most_common(max(k, 10))]
        ranked = [p for p in ranked if p]
        elapsed_ms = (time.perf_counter() - start) * 1000.0
        return AdapterResult(
            files=ranked[:k],
            symbols=[],
            latency_ms=elapsed_ms,
            # Charge the full CLAUDE.md / rules doc length — the agent
            # consumes the entire static file every turn, not just the
            # paths we extracted. This is the precise comparison
            # agentmemory uses to motivate "22,610 vs 3,142 tokens".
            tokens_returned=approx_tokens("x" * self._docs_total_chars),
            raw={"n_mentions": len(self._mentions), "n_distinct_paths": len(score)},
        )

    @staticmethod
    def _discover_docs(repo_root: str) -> list[str]:
        """Find CLAUDE.md, AGENTS.md, .cursor/rules/*.mdc within the repo.

        Searches at the repo root only (not recursive) for the top-level
        files; descends into `.cursor/rules/` and picks up every `.mdc` /
        `.md` so mono-repos with split rule files are still covered.
        """
        out: list[str] = []
        for name in ("CLAUDE.md", "AGENTS.md", "claude.md", "agents.md"):
            p = os.path.join(repo_root, name)
            if os.path.isfile(p):
                out.append(p)
        rules_dir = os.path.join(repo_root, ".cursor", "rules")
        if os.path.isdir(rules_dir):
            for name in sorted(os.listdir(rules_dir)):
                if name.endswith((".mdc", ".md")):
                    out.append(os.path.join(rules_dir, name))
        return out
