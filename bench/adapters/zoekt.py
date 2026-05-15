"""Zoekt adapter — Sourcegraph's trigram code-search engine.

Zoekt is the canonical "best non-LLM code search" baseline: it powers
Sourcegraph's code search at scale, builds in O(seconds), and answers
in <50ms with a structured ranked list. It's a stronger floor than
`grep` because:

* it indexes once, then queries are O(ms) instead of O(seconds)
* it's symbol-aware (ctags), not just literal-text-aware
* it ranks results properly instead of by raw hit count

What it *doesn't* do:
* No semantic search (no vectors). Like all the BM25-style adapters,
  it has zero understanding of "client IP extraction" ≈ "clientip".
* No graph/structural context. Just text + symbol matches.

So this adapter establishes the ceiling for what pure code-search can
buy you on this benchmark, separate from anything DevRouter, codegraph,
or agentmemory's hybrid retrieval contributes.

Privacy: zoekt is a single Go binary, talks to nothing over the
network, and writes its index to a local cache directory under
`bench/.zoekt-cache/`. No code or queries leave the machine, ever.

Setup: we shell out to `zoekt-index` once per repo to build the
shards, then `zoekt` to query. Both binaries are produced by
`go install github.com/sourcegraph/zoekt/cmd/{zoekt-index,zoekt}@latest`.
The adapter searches PATH and the standard Go bin locations to find
them; if neither binary is present we fail setup with a clear message.
"""

from __future__ import annotations

import base64
import json
import os
import shutil
import subprocess
import time

from .base import Adapter, AdapterResult, approx_tokens, normalize_path, register

# Same stopword + token-extraction rule as the grep adapter so the two
# baselines are using comparable query rewrites. Zoekt's own query DSL
# treats bare words as substring matches, which is what we want.
_STOPWORDS = {
    "the", "a", "an", "is", "are", "was", "were", "be", "been", "being",
    "of", "in", "on", "at", "by", "for", "to", "from", "and", "or", "not",
    "where", "what", "when", "why", "how", "which", "who", "does", "do",
    "did", "this", "that", "with", "as", "it", "its",
}

# Directories zoekt should ignore on top of its default `.git,.hg,.svn`.
# `.idea` showed up in the smoke test as a noise source — IntelliJ stores
# "shelved" patch files that contain entire copies of source under
# .idea/shelf/*/shelved.patch. Those count as legitimate hits but are
# never the answer to a code-retrieval question, so we exclude them.
_IGNORE_DIRS = ".git,.hg,.svn,.idea,node_modules,vendor,dist,build,.codegraph"

# Cache dir for built indexes, one subdir per repo.
_CACHE_DIR = os.path.join(
    os.path.dirname(os.path.dirname(os.path.abspath(__file__))),
    ".zoekt-cache",
)


def _tokens(query: str) -> list[str]:
    # We split on hyphens (not just whitespace + punctuation) because Go
    # / TS code rarely uses hyphenated identifiers — "budget-throttle"
    # in a question almost always maps to "budgetthrottle" or
    # "budget_throttle" in code. Keeping the hyphen turned the literal
    # "budget-throttle" into a trigram zoekt couldn't match anywhere,
    # which forced the AND query to fall back to OR and tank ranking.
    # We also keep underscore tokens whole — those *are* common in code.
    raw = "".join(c if c.isalnum() or c == "_" else " " for c in query.lower())
    return [t for t in raw.split() if len(t) >= 3 and t not in _STOPWORDS]


def _find_binary(name: str) -> str | None:
    """Locate a zoekt binary: PATH first, then `go env GOPATH/bin`."""
    hit = shutil.which(name)
    if hit:
        return hit
    try:
        gopath = subprocess.check_output(
            ["go", "env", "GOPATH"], text=True, timeout=5
        ).strip()
    except (OSError, subprocess.SubprocessError):
        return None
    candidate = os.path.join(gopath, "bin", name)
    return candidate if os.path.exists(candidate) else None


@register
class ZoektAdapter(Adapter):
    name = "zoekt"

    def __init__(self) -> None:
        self._repo_root: str = ""
        self._index_dir: str = ""
        self._zoekt_bin: str = ""
        self._zoekt_index_bin: str = ""

    def setup(self, repo: str, repo_root: str) -> None:
        self._repo_root = repo_root

        zoekt = _find_binary("zoekt")
        zoekt_index = _find_binary("zoekt-index")
        if not zoekt or not zoekt_index:
            raise RuntimeError(
                "zoekt binaries not found. Install with: "
                "go install github.com/sourcegraph/zoekt/cmd/zoekt-index@latest "
                "&& go install github.com/sourcegraph/zoekt/cmd/zoekt@latest"
            )
        self._zoekt_bin = zoekt
        self._zoekt_index_bin = zoekt_index

        # Per-repo cache dir. Re-use across runs if a shard already exists —
        # zoekt-index is fast (~3s on goserving) but not free, and we don't
        # want to pay it on every bench invocation. The shard filename
        # contains a version suffix, so an upgrade of zoekt will produce a
        # new file and the old one stays inert.
        self._index_dir = os.path.join(_CACHE_DIR, repo)
        os.makedirs(self._index_dir, exist_ok=True)

        has_shards = any(
            f.endswith(".zoekt") for f in os.listdir(self._index_dir)
        )
        if has_shards:
            return

        # Build the shard.
        cmd = [
            self._zoekt_index_bin,
            "-index", self._index_dir,
            "-ignore_dirs", _IGNORE_DIRS,
            repo_root,
        ]
        proc = subprocess.run(
            cmd, capture_output=True, text=True, timeout=300, check=False,
        )
        if proc.returncode != 0:
            raise RuntimeError(
                f"zoekt-index failed (rc={proc.returncode}): "
                f"{(proc.stderr or proc.stdout)[-400:]}"
            )

    def query(self, q: str, repo: str, k: int) -> AdapterResult:
        toks = _tokens(q)
        if not toks:
            return AdapterResult(error="empty token set after stopword filter")
        if not self._zoekt_bin:
            return AdapterResult(error="adapter not set up")

        # Zoekt scoring is roughly "more trigram hits → higher rank", which
        # is *not* an IDF-aware model. So if we pass the full filtered
        # token set as OR, a common word like "block"/"types"/"use" wins
        # over the rare-but-relevant token like "advertiserblocker" and
        # the actual answer gets buried under every `types.go` in the
        # repo. The empirical fix on goserving: keep the 3 longest tokens
        # (length is our cheap rare-ness proxy — long identifiers like
        # `advertiserblocker` or `budgetthrottle` are almost always the
        # distinctive bit of the question) and AND them. AND is fine here
        # because we deliberately picked 3 tokens that the answering file
        # almost certainly contains together; if AND returns nothing we
        # fall back to OR-of-the-three so we never starve.
        distinctive = sorted(toks, key=len, reverse=True)[:3]
        if len(distinctive) <= 1:
            zoekt_query = distinctive[0] if distinctive else toks[0]
        else:
            zoekt_query = " ".join(distinctive)  # space = AND in zoekt

        # `-jsonl` returns one JSON object per *file*, not per match; each
        # file's record has its own Score derived from the matched lines.
        # We over-request a bit to give us room to dedup / filter noise.
        start = time.perf_counter()
        proc = self._run_zoekt(zoekt_query)
        # If AND of the 3 distinctive tokens returned no hits (zoekt exits
        # 0 with empty stdout in that case), fall back to OR-of-the-three.
        # We deliberately do NOT fall further back to "OR of all 7 query
        # tokens" because that's exactly the noise-explosion failure mode
        # the AND-of-distinctive rewrite is meant to fix.
        if proc is not None and proc.returncode == 0 and not proc.stdout.strip():
            if len(distinctive) > 1:
                fallback = " or ".join(f"({t})" for t in distinctive)
                proc = self._run_zoekt(fallback)
        elapsed_ms = (time.perf_counter() - start) * 1000.0

        if proc is None:
            return AdapterResult(latency_ms=elapsed_ms, error="zoekt query timeout (30s)")
        if proc.returncode != 0:
            # zoekt exits non-zero when the query is syntactically invalid;
            # propagate the message but don't crash the whole bench.
            return AdapterResult(
                latency_ms=elapsed_ms,
                error=f"zoekt rc={proc.returncode}: {(proc.stderr or '')[:200]}",
            )

        files: list[str] = []
        # We charge tokens against (path + matched-line bytes) for every
        # result we keep, mirroring how Sourcegraph would surface them
        # in a code-search UI — paths + a snippet, never the full file.
        token_chars = 0
        seen: set[str] = set()
        for line in proc.stdout.splitlines():
            line = line.strip()
            if not line:
                continue
            try:
                rec = json.loads(line)
            except json.JSONDecodeError:
                continue
            fn = rec.get("FileName") or ""
            if not fn or fn in seen:
                continue
            seen.add(fn)
            # zoekt indexes the absolute path it was given but stores the
            # repo-relative path inside the shard; the FileName is already
            # relative. normalize_path is defensive.
            rel = normalize_path(fn, self._repo_root)
            if not rel:
                continue
            files.append(rel)
            # Token accounting: path + decoded length of every LineMatch.
            # `Line` is base64 so its raw length is ~4/3 of the original.
            token_chars += len(rel) + 8
            for lm in (rec.get("LineMatches") or [])[:5]:
                raw = lm.get("Line") or ""
                try:
                    token_chars += len(base64.b64decode(raw))
                except (ValueError, TypeError):
                    token_chars += len(raw)
            if len(files) >= max(k, 10):
                break

        return AdapterResult(
            files=files[:k],
            symbols=[],
            latency_ms=elapsed_ms,
            tokens_returned=approx_tokens("x" * token_chars),
            raw={
                "tokens": toks,
                "distinctive": distinctive,
                "query": zoekt_query,
                "n_files_seen": len(files),
            },
        )

    def _run_zoekt(self, zoekt_query: str) -> subprocess.CompletedProcess | None:
        cmd = [
            self._zoekt_bin,
            "-index_dir", self._index_dir,
            "-jsonl",
            zoekt_query,
        ]
        try:
            return subprocess.run(
                cmd, capture_output=True, text=True, timeout=30, check=False,
            )
        except subprocess.TimeoutExpired:
            return None
