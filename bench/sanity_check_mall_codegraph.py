"""Ground-truth sanity check on mall.

For every question in bench/questions/mall.jsonl, query codegraph's /api/search
endpoint directly in all three modes (hybrid, bm25, semantic), score the top-10
file list against the gold expected_files, and print:

    * per-question best-mode R@5 and the rank of the first gold hit
    * which questions ARE answerable by codegraph at all (any mode hits)
    * which questions are NOT answerable (no gold file appears in top-10 in any mode)

This is the "is the bench fair" sanity check: if codegraph itself can't surface
the gold file even with its native search, devrouter's wrapper-layer score is
bounded from above by what codegraph can return. A question where every mode
misses isn't a devrouter bug — it's a retrieval ceiling we're hitting.

Run:
    python3 bench/sanity_check_mall_codegraph.py
"""

from __future__ import annotations

import json
import sys
import urllib.request
from collections import defaultdict
from pathlib import Path

CG_URL = "http://localhost:4747"
REPO = "mall"
QUESTIONS = Path("/Users/atharva.ag/IdeaProjects/devrouter/bench/questions/mall.jsonl")
K = 10
MODES = ["hybrid", "bm25", "semantic"]


def search(query: str, mode: str, limit: int = 10) -> list[dict]:
    payload = json.dumps({
        "query": query,
        "repo": REPO,
        "limit": limit,
        "mode": mode,
        "enrich": False,
        "include_source": False,
    }).encode()
    req = urllib.request.Request(
        f"{CG_URL}/api/search",
        data=payload,
        headers={"Content-Type": "application/json"},
        method="POST",
    )
    try:
        with urllib.request.urlopen(req, timeout=20) as resp:
            data = json.loads(resp.read())
    except Exception as e:  # noqa: BLE001
        print(f"[error] {mode} '{query[:40]}' -> {e}", file=sys.stderr)
        return []
    return data.get("results", []) or []


def file_ranking(results: list[dict]) -> list[str]:
    """Flatten enriched results to a dedup'd file path list, preserving order."""
    seen: set[str] = set()
    out: list[str] = []
    for r in results:
        path = r.get("filePath") or ""
        if not path or path in seen:
            continue
        seen.add(path)
        out.append(path)
    return out


def first_hit_rank(ranking: list[str], expected: list[str]) -> int | None:
    """1-indexed rank of first gold hit, or None if none in top-len(ranking)."""
    expected_set = set(expected)
    for i, p in enumerate(ranking, 1):
        if p in expected_set:
            return i
    return None


def recall_at(k: int, ranking: list[str], expected: list[str]) -> float:
    if not expected:
        return 0.0
    top = set(ranking[:k])
    hits = sum(1 for e in expected if e in top)
    return hits / len(expected)


def main() -> None:
    questions = [json.loads(l) for l in QUESTIONS.read_text().splitlines() if l.strip()]
    print(f"=== Codegraph raw-search sanity check on {REPO} ===")
    print(f"questions={len(questions)}  K={K}  modes={MODES}")
    print()

    by_intent: dict[str, list[tuple[str, dict[str, dict]]]] = defaultdict(list)
    answerable, unanswerable = [], []

    header = f"{'id':<10} {'intent':<10} {'hybrid R@5':>10} {'bm25 R@5':>9} {'sem R@5':>8} {'best rank':>10}  query"
    print(header)
    print("-" * len(header))
    for q in questions:
        per_mode: dict[str, dict] = {}
        for mode in MODES:
            results = search(q["query"], mode, K)
            ranking = file_ranking(results)
            per_mode[mode] = {
                "ranking": ranking,
                "r@5": recall_at(5, ranking, q["expected_files"]),
                "r@10": recall_at(10, ranking, q["expected_files"]),
                "rank": first_hit_rank(ranking, q["expected_files"]),
            }
        best_rank = min(
            (m["rank"] for m in per_mode.values() if m["rank"] is not None),
            default=None,
        )
        ranks_str = f"#{best_rank}" if best_rank else "MISS"
        print(
            f"{q['id']:<10} {q['intent']:<10} "
            f"{per_mode['hybrid']['r@5']:>10.3f} "
            f"{per_mode['bm25']['r@5']:>9.3f} "
            f"{per_mode['semantic']['r@5']:>8.3f} "
            f"{ranks_str:>10}  {q['query'][:75]}"
        )
        by_intent[q["intent"]].append((q["id"], per_mode))
        if best_rank is None:
            unanswerable.append(q)
        else:
            answerable.append((q, best_rank))

    # ------------------------------------------------------------------
    # Aggregate
    # ------------------------------------------------------------------
    def aggregate(per_intent_modes: list[dict]) -> dict[str, float]:
        if not per_intent_modes:
            return {}
        out: dict[str, float] = {}
        for mode in MODES:
            out[f"{mode}_r@5"] = sum(m[mode]["r@5"] for m in per_intent_modes) / len(per_intent_modes)
            out[f"{mode}_r@10"] = sum(m[mode]["r@10"] for m in per_intent_modes) / len(per_intent_modes)
        # "best of three modes" — upper bound any router can hit with mode selection
        out["oracle_r@5"] = sum(
            max(m[mode]["r@5"] for mode in MODES) for m in per_intent_modes
        ) / len(per_intent_modes)
        out["oracle_r@10"] = sum(
            max(m[mode]["r@10"] for mode in MODES) for m in per_intent_modes
        ) / len(per_intent_modes)
        return out

    print()
    print("=== Aggregate by intent ===")
    print(
        f"{'intent':<10} {'n':>3} {'hybrid R@5':>10} {'bm25 R@5':>9} {'sem R@5':>8} "
        f"{'oracle R@5':>11} {'oracle R@10':>12}"
    )
    print("-" * 70)
    all_modes_list: list[dict] = []
    for intent in ["trace", "explore", "debug", "refactor", "general"]:
        rows = by_intent.get(intent, [])
        per = [r[1] for r in rows]
        all_modes_list.extend(per)
        agg = aggregate(per)
        if not agg:
            continue
        print(
            f"{intent:<10} {len(rows):>3} "
            f"{agg['hybrid_r@5']:>10.3f} {agg['bm25_r@5']:>9.3f} {agg['semantic_r@5']:>8.3f} "
            f"{agg['oracle_r@5']:>11.3f} {agg['oracle_r@10']:>12.3f}"
        )
    agg = aggregate(all_modes_list)
    print("-" * 70)
    print(
        f"{'OVERALL':<10} {len(all_modes_list):>3} "
        f"{agg['hybrid_r@5']:>10.3f} {agg['bm25_r@5']:>9.3f} {agg['semantic_r@5']:>8.3f} "
        f"{agg['oracle_r@5']:>11.3f} {agg['oracle_r@10']:>12.3f}"
    )

    # ------------------------------------------------------------------
    # Which questions are completely unanswerable by codegraph?
    # ------------------------------------------------------------------
    print()
    print(f"=== Unanswerable by codegraph (no gold file in top-{K} for ANY mode): "
          f"{len(unanswerable)} / {len(questions)} ===")
    for q in unanswerable:
        print(f"  {q['id']} ({q['intent']:<8}) {q['query'][:80]}")
        print(f"            expected: {q['expected_files'][0]}")

    # ------------------------------------------------------------------
    # Where the answer is buried (codegraph can find it but ranks it poorly)
    # ------------------------------------------------------------------
    deep = [(q, r) for (q, r) in answerable if r > 5]
    print()
    print(f"=== Answer ranked beyond top-5 (codegraph finds it, but devrouter "
          f"would need to surface it): {len(deep)} / {len(questions)} ===")
    for q, r in deep:
        print(f"  {q['id']} ({q['intent']:<8}) rank=#{r}  {q['query'][:70]}")


if __name__ == "__main__":
    main()
