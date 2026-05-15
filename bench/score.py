"""Retrieval-quality scoring for the DevRouter benchmark harness.

Implements the standard IR triple: Recall@K, MRR, plus latency percentiles.

Why these metrics:

- **R@K** is the headline number — "of the K things the system surfaced,
  did it cover the gold answer?". It's what agentmemory's 95.2% claim is.
  We report R@5 and R@10 so we can compare against the dominant K used in
  the literature.

- **MRR** rewards systems that put the right answer high in the ranking,
  not just somewhere in the top K. Two systems can both hit R@10=1.0 but
  one ranks the gold file at position 1 (MRR=1.0) and the other at
  position 10 (MRR=0.1). The agent will read the higher-ranked one first,
  so MRR matters in practice.

- **Latency p50/p95** matters because dev_context is in the agent's hot
  path. A system that wins on R@5 but takes 2s per call is worse in
  practice than one that wins by less but returns in 200ms.

Scoring is on **file paths**, not symbols. Symbols are recorded but not
scored — they vary too much across systems (devrouter emits qualified
names, codegraph emits short names, grep emits nothing) and standardizing
is a separate research problem.
"""

from __future__ import annotations

import math
import os
import statistics
from dataclasses import dataclass


# Apples-to-apples cap for the uniform token model. 64 KB matches what
# agentmemory and codegraph (file-head fallback) already use, so the
# floor calculation lines up with their native accounting and only
# normalizes the *zoekt/grep/devrouter* asymmetries away.
UNIFORM_TOKENS_FILE_CAP_BYTES = 64 * 1024


@dataclass
class QuestionScore:
    """Per-(adapter, question) score row."""

    question_id: str
    adapter: str
    intent: str
    r_at_5: float
    r_at_10: float
    mrr: float
    hit_rank: int | None  # 1-based; None if no hit in top K
    latency_ms: float
    tokens_returned: int  # see AdapterResult.tokens_returned
    # Apples-to-apples token model: each adapter is charged for
    # (path + min(file_size, 64 KB)) for every file in its top-K,
    # regardless of what the adapter actually serialized. Strips
    # out per-tool design choices (line-ranges vs symbol slices vs
    # full files) and asks the simpler question — "if every system
    # had to return the same thing, how much would it cost the
    # agent given this system's file picks?". Pair with
    # tokens_returned to see realistic vs floor.
    tokens_uniform: int = 0
    n_returned: int = 0
    n_expected: int = 0
    error: str | None = None


def recall_at_k(returned: list[str], expected: list[str], k: int) -> float:
    """Fraction of expected items present in the first K returned items.

    Returns 0.0 if expected is empty (avoids division-by-zero; treats a
    question with no gold answer as impossible to score, which the runner
    surfaces separately).

    Case-sensitive exact match. Both sides must be canonicalized to the
    same form (the adapter `normalize_path()` enforces this).

    Note: This is the multi-relevant variant — if expected has 3 files and
    2 are in top-K, recall = 2/3. Common alternative is "hit@K" (1.0 if
    any expected is in top-K, else 0); we report MRR for that flavor.
    """
    if not expected:
        return 0.0
    top = set(returned[:k])
    hits = sum(1 for e in expected if e in top)
    return hits / len(expected)


def reciprocal_rank(returned: list[str], expected: list[str]) -> tuple[float, int | None]:
    """MRR contribution for a single query: 1/rank of the first expected hit.

    Returns (mrr, hit_rank) where hit_rank is 1-based, or (0.0, None) if no
    expected item appears anywhere in `returned`.

    Unlike R@K this scans the full returned list — when comparing systems
    that return different K, this is the more honest "where in the ranking
    does the right answer first appear" measurement.
    """
    if not expected:
        return 0.0, None
    expected_set = set(expected)
    for i, item in enumerate(returned, start=1):
        if item in expected_set:
            return 1.0 / i, i
    return 0.0, None


def percentiles(values: list[float], ps: list[int]) -> dict[int, float]:
    """Return {p: percentile_value} for each p in `ps` (0-100).

    Uses inclusive linear interpolation. Returns NaN for the empty input
    rather than raising — the runner can then render "n/a" in the report.
    """
    if not values:
        return {p: math.nan for p in ps}
    sorted_vals = sorted(values)
    n = len(sorted_vals)
    out: dict[int, float] = {}
    for p in ps:
        if n == 1:
            out[p] = sorted_vals[0]
            continue
        rank = (p / 100.0) * (n - 1)
        lo = int(math.floor(rank))
        hi = int(math.ceil(rank))
        if lo == hi:
            out[p] = sorted_vals[lo]
        else:
            frac = rank - lo
            out[p] = sorted_vals[lo] * (1 - frac) + sorted_vals[hi] * frac
    return out


@dataclass
class AdapterAggregate:
    """Aggregated metrics across all questions for one adapter."""

    adapter: str
    n_questions: int
    n_errors: int
    mean_r_at_5: float
    mean_r_at_10: float
    mean_mrr: float
    latency_p50_ms: float
    latency_p95_ms: float
    tokens_p50: float
    tokens_p95: float
    tokens_uniform_p50: float
    tokens_uniform_p95: float
    by_intent: dict[str, dict[str, float]]


def uniform_tokens_for_files(
    files: list[str], repo_root: str, k: int = 10
) -> int:
    """Deterministic per-(file_set) token cost.

    Walks the first `k` files in `files`, sums `min(getsize, 64 KB)` for
    each that exists on disk, divides by 4 (the same approx_tokens rule
    every adapter uses). Files outside the repo or missing on disk
    contribute zero — that's the right behavior, since the agent
    couldn't read them either.

    Why this matters for the bench: it strips out the per-tool decision
    of "what slice should I serialize?" and asks "given the file picks
    each system landed on, what would they cost the agent if they all
    serialized the same way?". The answer to "is this apples-to-apples"
    becomes yes-by-construction along this column.
    """
    if not files:
        return 0
    total_bytes = 0
    for rel in files[:k]:
        if not rel:
            continue
        full = os.path.join(repo_root, rel)
        try:
            sz = os.path.getsize(full)
        except OSError:
            continue
        total_bytes += min(sz, UNIFORM_TOKENS_FILE_CAP_BYTES)
        # Path string itself is a few dozen bytes — small but not zero
        # at p95. Charge it so a system that returns 10 deeply-nested
        # paths doesn't get a free lunch.
        total_bytes += len(rel)
    return total_bytes // 4


def aggregate(scores: list[QuestionScore]) -> list[AdapterAggregate]:
    """Roll per-question scores up to per-adapter summaries.

    Errors are counted but excluded from the metric averages — a system
    that crashes on 5/30 questions is reported as "26 questions, R@5=0.7"
    plus a separate "5 errors" column, not as "R@5=0.58 with errors silently
    counted as zero". The latter would conflate breakage with bad retrieval.

    Per-intent breakdown lets us spot e.g. "system X is great on `trace`
    but terrible on `refactor`", which is the more actionable finding than
    a single global average.
    """
    by_adapter: dict[str, list[QuestionScore]] = {}
    for s in scores:
        by_adapter.setdefault(s.adapter, []).append(s)

    out: list[AdapterAggregate] = []
    for adapter, rows in by_adapter.items():
        ok_rows = [r for r in rows if r.error is None]
        latencies = [r.latency_ms for r in ok_rows]
        lpcts = percentiles(latencies, [50, 95])
        # Token percentiles ignore errored rows for the same reason latency
        # percentiles do — a crashed call contributes no real measurement.
        tokens = [float(r.tokens_returned) for r in ok_rows]
        tpcts = percentiles(tokens, [50, 95])
        utokens = [float(r.tokens_uniform) for r in ok_rows]
        utpcts = percentiles(utokens, [50, 95])

        by_intent: dict[str, dict[str, float]] = {}
        intents = {r.intent for r in ok_rows if r.intent}
        for intent in intents:
            sub = [r for r in ok_rows if r.intent == intent]
            if not sub:
                continue
            by_intent[intent] = {
                "n": float(len(sub)),
                "r@5": statistics.fmean(r.r_at_5 for r in sub),
                "r@10": statistics.fmean(r.r_at_10 for r in sub),
                "mrr": statistics.fmean(r.mrr for r in sub),
            }

        out.append(
            AdapterAggregate(
                adapter=adapter,
                n_questions=len(rows),
                n_errors=len(rows) - len(ok_rows),
                mean_r_at_5=statistics.fmean(r.r_at_5 for r in ok_rows) if ok_rows else 0.0,
                mean_r_at_10=statistics.fmean(r.r_at_10 for r in ok_rows) if ok_rows else 0.0,
                mean_mrr=statistics.fmean(r.mrr for r in ok_rows) if ok_rows else 0.0,
                latency_p50_ms=lpcts[50],
                latency_p95_ms=lpcts[95],
                tokens_p50=tpcts[50],
                tokens_p95=tpcts[95],
                tokens_uniform_p50=utpcts[50],
                tokens_uniform_p95=utpcts[95],
                by_intent=by_intent,
            )
        )
    return out


def render_markdown_report(
    aggregates: list[AdapterAggregate],
    repo: str,
    n_questions: int,
    k: int,
) -> str:
    """Render an aggregate table + per-intent breakdown as Markdown.

    Mirrors the agentmemory README's comparison-table style so the output
    is directly comparable in shape to what the broader market publishes.
    """
    lines: list[str] = []
    lines.append(f"# DevRouter benchmark — {repo}")
    lines.append("")
    lines.append(f"- Question set: `bench/questions/{repo}.jsonl` ({n_questions} questions)")
    lines.append(f"- K (for R@K headline): {k}")
    lines.append("- Metric definitions: see [`bench/score.py`](../score.py)")
    lines.append("")
    lines.append("## Headline results")
    lines.append("")
    lines.append(
        "| Adapter | N | Errors | R@5 | R@10 | MRR | "
        "Latency p50 (ms) | Latency p95 (ms) | "
        "Tokens p50 (native) | Tokens p95 (native) | "
        "Tokens p50 (uniform) | Tokens p95 (uniform) |"
    )
    lines.append(
        "| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |"
        " ---: | ---: | ---: | ---: |"
    )
    ranked = sorted(aggregates, key=lambda a: a.mean_r_at_5, reverse=True)
    for agg in ranked:
        lines.append(
            f"| `{agg.adapter}` | {agg.n_questions} | {agg.n_errors} | "
            f"{agg.mean_r_at_5:.3f} | {agg.mean_r_at_10:.3f} | {agg.mean_mrr:.3f} | "
            f"{_fmt(agg.latency_p50_ms)} | {_fmt(agg.latency_p95_ms)} | "
            f"{_fmt(agg.tokens_p50)} | {_fmt(agg.tokens_p95)} | "
            f"{_fmt(agg.tokens_uniform_p50)} | {_fmt(agg.tokens_uniform_p95)} |"
        )
    lines.append("")
    lines.append(
        "*Tokens (native)* = what the adapter actually returns "
        "(symbol slices for codegraph, full files for agentmemory/grep, "
        "matched lines for zoekt, profile-clipped DevPrompt for "
        "devrouter). *Tokens (uniform)* = deterministic floor: every "
        "adapter is charged for `top-K paths × min(file_size, 64 KB)` "
        "from its own file picks, regardless of what it serialized — "
        "the strict apples-to-apples view."
    )
    lines.append("")

    if any(agg.by_intent for agg in aggregates):
        lines.append("## Per-intent R@5")
        lines.append("")
        all_intents = sorted({i for agg in aggregates for i in agg.by_intent})
        header = "| Adapter | " + " | ".join(all_intents) + " |"
        sep = "| --- | " + " | ".join(["---:"] * len(all_intents)) + " |"
        lines.append(header)
        lines.append(sep)
        for agg in ranked:
            cells = []
            for intent in all_intents:
                d = agg.by_intent.get(intent)
                cells.append(f"{d['r@5']:.3f} (n={int(d['n'])})" if d else "—")
            lines.append(f"| `{agg.adapter}` | " + " | ".join(cells) + " |")
        lines.append("")

    return "\n".join(lines)


def _fmt(v: float) -> str:
    if math.isnan(v):
        return "n/a"
    return f"{v:.0f}"
