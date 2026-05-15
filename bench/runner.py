"""DevRouter benchmark runner.

Loads questions from `bench/questions/<repo>.jsonl`, runs them through the
selected adapters, scores per-question, aggregates per-adapter, and writes
a markdown report + raw JSONL into `bench/results/<timestamp>/`.

Design notes
------------
- **Sequential per adapter**: We run each adapter's questions sequentially
  (not in parallel across adapters) so latency numbers reflect what the
  agent would see in production, with no contention from sibling adapters
  hammering the same Redis or HTTP server.

- **Adapter-major loop order**: For each adapter, set up once, run all its
  questions, tear down. This amortizes adapter setup (especially the
  devrouter MCP child-process spawn and the codegraph heartbeat probe).

- **Failures isolated per question**: One question that crashes one adapter
  does not abort the run — it's recorded as `error=...` and excluded from
  the metric averages (counted in `n_errors` instead). This matters because
  competitor adapters in Phase 2 will fail in interesting ways and we want
  to keep going.

- **No retries**: Latency stats are honest only if we measure the real
  one-shot latency. Retrying a slow call would mask a real performance
  problem in the system under test.

Usage
-----

    python3 bench/runner.py --repo goserving
    python3 bench/runner.py --repo goserving --adapters devrouter,grep
    python3 bench/runner.py --repo goserving --question-id goserving-001
"""

from __future__ import annotations

import argparse
import dataclasses
import importlib
import json
import os
import sys
import time
from datetime import datetime
from pathlib import Path

# Make `bench/` importable when running from the repo root or anywhere else.
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

from adapters import REGISTRY  # noqa: E402
from adapters.base import Adapter, AdapterResult  # noqa: E402
from score import (  # noqa: E402
    QuestionScore,
    aggregate,
    recall_at_k,
    reciprocal_rank,
    render_markdown_report,
    uniform_tokens_for_files,
)


def _load_adapters() -> None:
    """Import every adapter module so they self-register via @register.

    Phase-2 modules are loaded lazily via `--adapters` to avoid importing
    heavy deps (qdrant, letta, npm bridges) when the user only wants the
    Phase-1 set.
    """
    for mod in ("devrouter", "codegraph", "grep", "claudemd", "agentmemory", "zoekt"):
        try:
            importlib.import_module(f"adapters.{mod}")
        except ImportError as e:
            print(f"[bench] skipping adapter {mod!r}: {e}", file=sys.stderr)


def _load_optional_adapter(name: str) -> None:
    """Import an adapter that may have heavy / optional deps.

    The runner calls this for every name in `--adapters` after the Phase-1
    set is loaded, so users opting in to e.g. mem0 see the actual ImportError
    (qdrant missing, etc.) rather than a silent skip.
    """
    if name in REGISTRY:
        return
    try:
        importlib.import_module(f"adapters.{name}")
    except ImportError as e:
        raise SystemExit(
            f"[bench] adapter {name!r} requested but failed to import: {e}\n"
            f"        See bench/README.md for that adapter's install steps."
        ) from e


def load_questions(path: Path, only_id: str | None = None) -> list[dict]:
    if not path.exists():
        raise SystemExit(f"[bench] question file not found: {path}")
    out: list[dict] = []
    for i, line in enumerate(path.read_text().splitlines(), start=1):
        line = line.strip()
        if not line or line.startswith("//"):
            continue
        try:
            obj = json.loads(line)
        except json.JSONDecodeError as e:
            raise SystemExit(f"[bench] {path}:{i}: invalid JSON: {e}") from e
        if only_id and obj.get("id") != only_id:
            continue
        out.append(obj)
    if only_id and not out:
        raise SystemExit(f"[bench] no question matched id={only_id!r}")
    return out


def resolve_repo_root(repo: str, override: str | None) -> str:
    if override:
        return os.path.abspath(override)
    # Convention: repos live as siblings of the devrouter checkout. Same
    # convention codegraph uses for resolving repo paths.
    candidate = os.path.abspath(
        os.path.join(os.path.dirname(__file__), "..", "..", repo)
    )
    if os.path.isdir(candidate):
        return candidate
    raise SystemExit(
        f"[bench] cannot locate repo {repo!r}. Pass --repo-root /abs/path."
    )


def score_one(
    q: dict,
    adapter_name: str,
    result: AdapterResult,
    k: int,
    repo_root: str,
) -> QuestionScore:
    expected = q.get("expected_files") or []
    mrr, hit_rank = reciprocal_rank(result.files, expected)
    return QuestionScore(
        question_id=q.get("id", "?"),
        adapter=adapter_name,
        intent=q.get("intent", ""),
        r_at_5=recall_at_k(result.files, expected, 5),
        r_at_10=recall_at_k(result.files, expected, 10),
        mrr=mrr,
        hit_rank=hit_rank,
        latency_ms=result.latency_ms,
        tokens_returned=result.tokens_returned,
        # Computed from the adapter's own file picks but with a fixed
        # serialization rule (path + 64 KB-capped file head). This is
        # what the bench treats as the strict apples-to-apples token
        # column. See score.uniform_tokens_for_files for the rule.
        tokens_uniform=uniform_tokens_for_files(result.files, repo_root, k=k),
        n_returned=len(result.files),
        n_expected=len(expected),
        error=result.error,
    )


def run_adapter(
    adapter_cls: type[Adapter],
    questions: list[dict],
    repo: str,
    repo_root: str,
    k: int,
    setup_times: dict[str, float] | None = None,
) -> tuple[list[QuestionScore], list[dict]]:
    """Run one adapter through all questions.

    If `setup_times` is provided, the adapter's setup wall time (or -1.0
    on failure) is recorded as `setup_times[adapter.name] = ms`. The
    SCALE runner uses this to extract per-adapter index time at each
    corpus scale — setup is the heavy phase for indexed adapters
    (codegraph ~47 s on goserving, agentmemory-hybrid ~165 s) and is
    the real story SCALE.md is supposed to tell.
    """
    adapter = adapter_cls()
    scores: list[QuestionScore] = []
    raw_rows: list[dict] = []

    print(f"[bench] === adapter: {adapter.name} ===", flush=True)
    setup_start = time.perf_counter()
    try:
        adapter.setup(repo, repo_root)
    except Exception as e:  # noqa: BLE001
        print(f"[bench]   setup failed: {e}", flush=True)
        for q in questions:
            scores.append(QuestionScore(
                question_id=q.get("id", "?"),
                adapter=adapter.name,
                intent=q.get("intent", ""),
                r_at_5=0.0, r_at_10=0.0, mrr=0.0,
                hit_rank=None, latency_ms=0.0, tokens_returned=0,
                n_returned=0, n_expected=len(q.get("expected_files") or []),
                error=f"setup failed: {e}",
            ))
        if setup_times is not None:
            setup_times[adapter.name] = -1.0
        return scores, raw_rows
    setup_ms = (time.perf_counter() - setup_start) * 1000.0
    if setup_times is not None:
        setup_times[adapter.name] = setup_ms
    print(f"[bench]   setup ok ({setup_ms:.0f} ms)", flush=True)

    try:
        for i, q in enumerate(questions, start=1):
            qid = q.get("id", f"#{i}")
            try:
                result = adapter.query(q["query"], repo, k)
            except Exception as e:  # noqa: BLE001
                result = AdapterResult(error=f"query crashed: {e}")
            score = score_one(q, adapter.name, result, k, repo_root)
            scores.append(score)
            raw_rows.append({
                "question_id": qid,
                "adapter": adapter.name,
                "files_returned": result.files,
                "symbols_returned": result.symbols[:20],
                "latency_ms": result.latency_ms,
                "error": result.error,
                "raw": result.raw,
                "score": dataclasses.asdict(score),
            })
            tag = "OK" if result.error is None else "ERR"
            print(
                f"[bench]   [{tag}] {qid:<24} "
                f"R@5={score.r_at_5:.2f} R@10={score.r_at_10:.2f} "
                f"MRR={score.mrr:.2f} ({result.latency_ms:.0f} ms)"
                + (f"  ← {result.error[:80]}" if result.error else ""),
                flush=True,
            )
    finally:
        try:
            adapter.teardown()
        except Exception as e:  # noqa: BLE001
            print(f"[bench]   teardown error: {e}", flush=True)

    return scores, raw_rows


def main() -> int:
    ap = argparse.ArgumentParser(description="DevRouter benchmark runner")
    ap.add_argument("--repo", required=True, help="repo name; questions loaded from bench/questions/<repo>.jsonl")
    ap.add_argument("--repo-root", default=None, help="absolute path to the repo (default: ../<repo>)")
    ap.add_argument("--adapters", default=None, help="comma-separated adapter names (default: all registered)")
    ap.add_argument("--k", type=int, default=10, help="top-K cap passed to each adapter (default 10)")
    ap.add_argument("--question-id", default=None, help="run only one question (debug)")
    ap.add_argument("--output-dir", default=None, help="results dir (default: bench/results/<timestamp>/)")
    ap.add_argument(
        "--questions-file", default=None,
        help=(
            "explicit path to a JSONL question file (overrides --repo "
            "lookup). Used by scale.py to feed a per-scale filtered "
            "question subset."
        ),
    )
    args = ap.parse_args()

    _load_adapters()
    requested: list[str]
    if args.adapters:
        requested = [s.strip() for s in args.adapters.split(",") if s.strip()]
        for n in requested:
            _load_optional_adapter(n)
    else:
        requested = list(REGISTRY.keys())

    unknown = [n for n in requested if n not in REGISTRY]
    if unknown:
        raise SystemExit(f"[bench] unknown adapter(s): {unknown}; known: {list(REGISTRY)}")

    here = Path(__file__).resolve().parent
    qpath = Path(args.questions_file) if args.questions_file else here / "questions" / f"{args.repo}.jsonl"
    questions = load_questions(qpath, only_id=args.question_id)
    print(f"[bench] loaded {len(questions)} question(s) from {qpath}", flush=True)

    repo_root = resolve_repo_root(args.repo, args.repo_root)
    print(f"[bench] repo root: {repo_root}", flush=True)

    out_dir = Path(args.output_dir) if args.output_dir else (
        here / "results" / datetime.now().strftime("%Y%m%d-%H%M%S")
    )
    out_dir.mkdir(parents=True, exist_ok=True)
    print(f"[bench] results: {out_dir}", flush=True)

    all_scores: list[QuestionScore] = []
    all_raw: list[dict] = []
    setup_times: dict[str, float] = {}

    for name in requested:
        cls = REGISTRY[name]
        scores, raw = run_adapter(
            cls, questions, args.repo, repo_root, args.k, setup_times=setup_times,
        )
        all_scores.extend(scores)
        all_raw.extend(raw)

    raw_path = out_dir / "raw.jsonl"
    with raw_path.open("w") as f:
        for row in all_raw:
            f.write(json.dumps(row) + "\n")

    aggs = aggregate(all_scores)
    report_md = render_markdown_report(
        aggs, repo=args.repo, n_questions=len(questions), k=args.k,
    )
    report_path = out_dir / "report.md"
    report_path.write_text(report_md)

    summary = {
        "repo": args.repo,
        "repo_root": repo_root,
        "n_questions": len(questions),
        "k": args.k,
        "setup_times_ms": setup_times,
        "adapters": [dataclasses.asdict(a) for a in aggs],
    }
    (out_dir / "summary.json").write_text(json.dumps(summary, indent=2))

    print("\n" + report_md, flush=True)
    print(f"\n[bench] wrote {report_path}", flush=True)
    return 0


if __name__ == "__main__":
    sys.exit(main())
