"""Adapter interface and registry.

An adapter wraps a single retrieval system (devrouter, codegraph, grep,
agentmemory, …) and translates its native output into the harness's common
shape: a ranked list of repo-relative file paths plus optional symbols.

Adapters that need an HTTP client, an MCP child process, a vector DB, etc.
hold those resources in `setup()` and tear them down in `teardown()`.
The harness calls `setup()` once per repo and `teardown()` once at the end,
so per-adapter resource cost amortizes across the whole question set.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any


@dataclass
class AdapterResult:
    """Uniform retrieval result returned by every adapter.

    `files` is the ranked, top-K list of repo-relative POSIX paths. The
    scorer compares this directly against `expected_files` in the gold set
    via exact string match, so adapters MUST canonicalize paths (strip
    leading `./`, strip the repo root prefix, normalize separators).

    `symbols` is optional and only populated by adapters that expose
    symbol-level retrieval (devrouter via `Symbols`/`CodeSnippets`,
    codegraph via node names). Pure file-grep adapters leave it empty.

    `latency_ms` is wall-clock time spent inside `query()` only — setup
    cost is excluded so cold-start overhead doesn't pollute steady-state
    latency stats.

    `tokens_returned` is the approximate token count of *everything*
    the adapter wants the agent to consume — the prompt it would inject,
    not just file paths. This is the axis agentmemory leans on hardest
    in their README (3,142 vs 22,610 tokens). For grep that's the raw
    match output; for devrouter it's the serialized DevPrompt; for
    codegraph it's the {file, snippet, name} rows. We use a
    char-divide-by-4 approximation rather than tiktoken so the harness
    has no external runtime dep, and because every adapter pays the
    same approximation cost the inter-adapter ranking is unaffected.

    `error` is set when the call failed; the runner records the failure
    but continues to the next question so one broken adapter doesn't kill
    the whole run.
    """

    files: list[str] = field(default_factory=list)
    symbols: list[str] = field(default_factory=list)
    latency_ms: float = 0.0
    tokens_returned: int = 0
    raw: dict[str, Any] = field(default_factory=dict)
    error: str | None = None


def approx_tokens(text: str | None) -> int:
    """Coarse token estimate: 4 characters per token.

    Matches OpenAI's commonly cited rule of thumb (~4 chars/token for
    English code+prose). We don't ship tiktoken on the harness path
    because (a) every adapter pays the same approximation, so the
    ranking is unaffected, and (b) tiktoken's BPE tables are 5 MB and
    we'd rather not pull them into a benchmark dependency. Returns 0
    for None or empty input.
    """
    if not text:
        return 0
    return max(1, len(text) // 4)


class Adapter:
    """Base class. Subclasses override `query`; most also override `setup`."""

    name: str = "base"

    def setup(self, repo: str, repo_root: str) -> None:  # noqa: B027 - intentional no-op
        """One-time setup per (adapter, repo). Override to pre-warm clients,
        spawn subprocesses, ingest documents, etc."""

    def query(self, q: str, repo: str, k: int) -> AdapterResult:  # pragma: no cover
        raise NotImplementedError

    def teardown(self) -> None:  # noqa: B027 - intentional no-op
        """Release resources held by setup()."""


REGISTRY: dict[str, type[Adapter]] = {}


def register(cls: type[Adapter]) -> type[Adapter]:
    """Decorator: register an adapter class under its `name` attribute.

    Used by the runner to resolve `--adapters devrouter,grep` into instances.
    """
    if not getattr(cls, "name", None):
        raise ValueError(f"adapter {cls!r} has no .name")
    if cls.name in REGISTRY:
        raise ValueError(f"adapter {cls.name!r} already registered")
    REGISTRY[cls.name] = cls
    return cls


def normalize_path(path: str, repo_root: str) -> str:
    """Reduce a path to a repo-relative POSIX form for ground-truth matching.

    Strips:
      - the repo_root prefix (so absolute and relative paths compare equal)
      - a leading `./`
      - a leading `/`

    Does NOT touch case (filesystems are case-sensitive in CI / Linux).
    Returns "" for empty input — caller must filter those out before scoring.
    """
    if not path:
        return ""
    p = path.strip()
    if not p:
        return ""
    p = p.replace("\\", "/")
    if repo_root:
        root = repo_root.rstrip("/") + "/"
        if p.startswith(root):
            p = p[len(root):]
    while p.startswith("./"):
        p = p[2:]
    while p.startswith("/"):
        p = p[1:]
    return p
