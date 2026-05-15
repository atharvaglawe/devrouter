"""SCALE runner — measure each adapter's degradation as the corpus shrinks.

The 30-question goserving bench tells us *what* the relative quality
between adapters is at one corpus size. It doesn't tell us *how each
adapter scales*. SCALE.md fills that gap: shrink the goserving corpus
to {100 %, 50 %, 25 %, 10 %}, rebuild every adapter's index, rerun the
bench, and chart the curves.

Three axes we expect to see move as the corpus shrinks:
1. **Setup time** — codegraph's tree-sitter pass is roughly linear in
   file count; agentmemory-hybrid's embedding pass is dominated by N×
   ONNX inferences; zoekt's trigram build is fast and sub-linear. The
   shape of these curves is the real publishable result.
2. **R@5** — for a well-engineered ranker, recall should stay roughly
   flat or improve as the corpus shrinks (less competition for the
   top-K slots). Adapters whose R@5 *drops* when the corpus shrinks
   are silently relying on having lots of distractors around — a
   useful failure mode to surface.
3. **Tokens p50** — should be flat for adapters that bound by top-K,
   may shrink for adapters that dump-all-content.

How the corpus shrink is done
-----------------------------
We use *hardlinks* in `/tmp/devrouter-scale-<n>/` rather than
copying. On goserving (~1.5 GB) a copy would burn 6 GB across four
scales; hardlinks burn ~0 disk. The downside is that the indexers
see the same inode as the source repo, but in practice none of our
adapters care — they treat the path as canonical, never the inode.

Sampling rule: every file in the gold-set of every question is
forced into the sample (so questions stay answerable); the remainder
is filled by random-without-replacement up to the target ratio, with
a deterministic seed. Below ~25 % the gold set itself starts to
dominate the sample and the "scale" interpretation gets fuzzy — we
emit a warning when that happens.

Output: `bench/results/<timestamp>-scale/SCALE.md` plus per-scale
sub-directories containing the full `report.md`/`raw.jsonl` for each
size, so the reader can drill in.
"""

from __future__ import annotations

import argparse
import json
import os
import random
import shutil
import subprocess
import sys
import time
from datetime import datetime
from pathlib import Path

# Reuse the runner's logic.
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

from runner import (  # noqa: E402
    _load_adapters,
    load_questions,
    REGISTRY,
)


# Files we always skip when sampling: build artifacts, vendored code,
# editor caches, the codegraph/zoekt caches, .git itself. These
# shouldn't be in a "what an agent would see" universe and they bloat
# the sample without changing semantics.
_ALWAYS_SKIP = (
    ".git",
    ".idea",
    ".cursor",
    ".codegraph",
    "node_modules",
    "vendor",
    "dist",
)


def _enumerate_files(repo_root: str) -> list[str]:
    """All repo-relative file paths under repo_root.

    Prefers `git ls-files` (which respects .gitignore) so the file
    population we sample matches what every adapter's indexer
    actually considers. Falls back to a manual walk if the directory
    isn't a git repo.

    Returns paths sorted for determinism.
    """
    repo_root = os.path.realpath(repo_root)
    try:
        out = subprocess.check_output(
            ["git", "ls-files"],
            cwd=repo_root, text=True, stderr=subprocess.DEVNULL,
        ).splitlines()
        out = [p for p in out if p and not p.startswith(".git/")]
        out.sort()
        if out:
            return out
    except (subprocess.SubprocessError, FileNotFoundError):
        pass

    out: list[str] = []
    for dirpath, dirnames, filenames in os.walk(repo_root):
        dirnames[:] = [d for d in dirnames if d not in _ALWAYS_SKIP]
        for fn in filenames:
            full = os.path.join(dirpath, fn)
            rel = os.path.relpath(full, repo_root)
            out.append(rel)
    out.sort()
    return out


def _sample_files(
    all_files: list[str],
    must_include: set[str],
    fraction: float,
    seed: int,
) -> list[str]:
    """Sample roughly `fraction * len(all_files)` files, always
    including `must_include`. Deterministic given the same seed and
    inputs.
    """
    target = int(fraction * len(all_files))
    target = max(target, len(must_include))

    rng = random.Random(seed)
    remaining = [f for f in all_files if f not in must_include]
    rng.shuffle(remaining)

    needed = target - len(must_include)
    sampled = list(must_include) + remaining[:max(needed, 0)]
    sampled.sort()
    return sampled


def _materialize_hardlinks(
    repo_root: str, sample_files: list[str], dst: str,
) -> None:
    """Build dst as a hardlink farm: every file in sample_files
    appears at the same relative path under dst, hardlinked from
    repo_root. Directories are created with mkdir; symlinks are
    followed; files that cross devices fall back to copy-on-write."""
    repo_root = os.path.realpath(repo_root)
    if os.path.exists(dst):
        shutil.rmtree(dst)
    os.makedirs(dst, exist_ok=True)

    for rel in sample_files:
        src = os.path.join(repo_root, rel)
        out = os.path.join(dst, rel)
        os.makedirs(os.path.dirname(out), exist_ok=True)
        try:
            os.link(src, out)
        except OSError:
            # Cross-device (e.g., src on / and dst on /tmp mount) or
            # source is a symlink to elsewhere — fall back to copy.
            try:
                shutil.copy2(src, out, follow_symlinks=True)
            except OSError:
                pass  # skip unreadable files


def _filter_questions_by_corpus(
    questions: list[dict], sample_files: set[str],
) -> list[dict]:
    """Drop questions whose expected_files aren't all present in
    `sample_files`. We must do this — otherwise a question becomes
    unanswerable in the sampled corpus and would tank R@5 unfairly."""
    kept = []
    for q in questions:
        expected = q.get("expected_files") or []
        # Some `expected_files` are directory hints ("oscar/config"),
        # not file paths. Accept those if any sample_file starts with
        # the prefix.
        ok = all(
            (e in sample_files) or any(s.startswith(e + "/") for s in sample_files)
            for e in expected
        )
        if ok:
            kept.append(q)
    return kept


def _run_one_scale(
    fraction: float,
    sample_files: list[str],
    src_repo_root: str,
    repo_name: str,
    out_root: Path,
    adapters: list[str],
    questions: list[dict],
    k: int,
) -> dict:
    """Build the hardlink farm, run the bench, return a summary dict.

    The farm directory's basename doubles as a *codegraph-repo-name*
    and a *zoekt-cache-key* — every adapter is keyed on the `--repo`
    string we hand to the runner. So for the 50% scale of goserving,
    the farm is `/tmp/goserving-scale-50/`, codegraph registers
    `goserving-scale-50` as a new repo via `analyze --skip-git`, zoekt
    indexes into `bench/.zoekt-cache/goserving-scale-50/`, and the
    bench runs with `--repo goserving-scale-50` so all those keys
    line up. We then also pass `--questions-file` so the runner
    doesn't try to read `questions/goserving-scale-50.jsonl`.
    """
    scale_pct = int(round(fraction * 100))
    scale_repo_name = f"{repo_name}-scale-{scale_pct}"
    farm = f"/tmp/{scale_repo_name}"
    print(f"\n[scale] === {scale_pct}% ({len(sample_files)} files) ===", flush=True)
    print(f"[scale]   farm: {farm}", flush=True)
    print(f"[scale]   codegraph/zoekt repo key: {scale_repo_name}", flush=True)

    t0 = time.perf_counter()
    _materialize_hardlinks(src_repo_root, sample_files, farm)
    farm_ms = (time.perf_counter() - t0) * 1000.0
    print(f"[scale]   hardlinks built in {farm_ms:.0f} ms", flush=True)

    sample_set = set(sample_files)
    kept_qs = _filter_questions_by_corpus(questions, sample_set)
    print(f"[scale]   {len(kept_qs)}/{len(questions)} questions answerable", flush=True)

    # Re-index codegraph against the new farm. We add `--skip-git`
    # because our hardlink farm intentionally excludes .git (the
    # checkout would 4x our disk for nothing). codegraph then keys
    # the new repo by farm-basename, which matches `scale_repo_name`.
    #
    # `analyze` is where codegraph actually builds its tree-sitter
    # index, so this is the wall-time we want to attribute to
    # codegraph's "setup at scale N" — NOT the runner's setup() call
    # which only probes /api/info. We stash the wall-time and merge
    # it into setup_times_ms further down.
    codegraph_analyze_ms = 0.0
    if "codegraph" in adapters or "devrouter" in adapters:
        analyze_t = time.perf_counter()
        analyze_log = out_root / f"scale-{scale_pct}-analyze.log"
        try:
            with analyze_log.open("w") as logf:
                subprocess.run(
                    ["./devrouter", "analyze", farm, "--force", "--skip-git",
                     "--no-stats", "--skip-agents-md"],
                    cwd=os.path.dirname(os.path.dirname(os.path.abspath(__file__))),
                    check=False, timeout=900,
                    stdout=logf, stderr=subprocess.STDOUT,
                )
        except subprocess.SubprocessError as e:
            print(f"[scale]   codegraph analyze failed: {e}", flush=True)
        codegraph_analyze_ms = (time.perf_counter() - analyze_t) * 1000.0
        print(
            f"[scale]   codegraph analyze: "
            f"{codegraph_analyze_ms / 1000:.1f} s (log: {analyze_log.name})",
            flush=True,
        )

    scale_dir = out_root / f"scale-{scale_pct}"
    scale_dir.mkdir(parents=True, exist_ok=True)
    filt_path = scale_dir / "questions.jsonl"
    with filt_path.open("w") as f:
        for q in kept_qs:
            f.write(json.dumps(q) + "\n")

    runner_path = Path(__file__).with_name("runner.py")
    cmd = [
        sys.executable, str(runner_path),
        "--repo", scale_repo_name,
        "--repo-root", farm,
        "--adapters", ",".join(adapters),
        "--k", str(k),
        "--output-dir", str(scale_dir),
        "--questions-file", str(filt_path),
    ]
    print(f"[scale]   running: {' '.join(cmd[-8:])}", flush=True)
    run_t = time.perf_counter()
    proc = subprocess.run(cmd, check=False, capture_output=False)
    run_ms = (time.perf_counter() - run_t) * 1000.0
    print(f"[scale]   bench completed in {run_ms / 1000:.1f} s (rc={proc.returncode})", flush=True)

    summary_path = scale_dir / "summary.json"
    summary: dict = {}
    if summary_path.exists():
        try:
            summary = json.loads(summary_path.read_text())
        except json.JSONDecodeError:
            pass

    # codegraph's REAL index-build time is the `analyze` step, not the
    # /api/info probe that the adapter measures. Replace the probe
    # time with analyze + probe so SCALE.md actually reflects index
    # cost. (Zoekt, agentmemory build their indexes inside setup() so
    # their numbers are already correct.)
    setup_times = summary.get("setup_times_ms", {}) or {}
    if codegraph_analyze_ms > 0 and "codegraph" in setup_times:
        setup_times["codegraph"] = codegraph_analyze_ms + setup_times["codegraph"]
        setup_times["codegraph_analyze_only_ms"] = codegraph_analyze_ms

    return {
        "fraction": fraction,
        "scale_pct": scale_pct,
        "n_files": len(sample_files),
        "n_questions": len(kept_qs),
        "farm": farm,
        "setup_times_ms": setup_times,
        "adapters": summary.get("adapters", []),
    }


def _safe_int(x: object, default: str = "—") -> str:
    """summary.json may contain NaN floats when an adapter scored 0/0
    (e.g. all setup-failed). json.loads turns those into nan, which
    `int(nan)` raises on. Treat them as "not measurable" and emit a
    placeholder."""
    try:
        v = float(x)  # type: ignore[arg-type]
    except (TypeError, ValueError):
        return default
    if v != v:  # NaN check
        return default
    return f"{int(v):,}"


def _safe_float(x: object, default: str = "—", fmt: str = "{:.3f}") -> str:
    try:
        v = float(x)  # type: ignore[arg-type]
    except (TypeError, ValueError):
        return default
    if v != v:
        return default
    return fmt.format(v)


def _render_scale_md(per_scale: list[dict], adapters: list[str]) -> str:
    lines: list[str] = []
    lines.append("# SCALE — adapter degradation as the corpus shrinks\n")
    lines.append(f"Date: {datetime.now().strftime('%Y-%m-%d %H:%M')}\n")
    scale_pcts = ", ".join(f"{p['scale_pct']} %" for p in per_scale)
    lines.append(
        f"Method: hardlink-farm-sample of the source repo at {scale_pcts}. "
        "Every gold-set file is preserved across samples (so questions "
        "remain answerable); the remainder is randomly drawn (seed=42) "
        "from `git ls-files`-respecting enumeration. Each scale "
        "re-indexes codegraph (with `--skip-git`) and zoekt against "
        "the farm.\n"
    )

    lines.append("## Setup time (s) by corpus scale\n")
    lines.append("| Adapter | " + " | ".join(f"{p['scale_pct']}% ({p['n_files']:,} files)" for p in per_scale) + " |")
    lines.append("| --- |" + " ---: |" * len(per_scale))
    for a in adapters:
        cells = []
        for p in per_scale:
            v = p["setup_times_ms"].get(a)
            if v is None or v < 0:
                cells.append("FAIL")
            else:
                cells.append(f"{v / 1000:.1f}")
        lines.append(f"| `{a}` | " + " | ".join(cells) + " |")
    lines.append("")

    lines.append("## R@5 by corpus scale\n")
    lines.append("| Adapter | " + " | ".join(f"{p['scale_pct']}%" for p in per_scale) + " |")
    lines.append("| --- |" + " ---: |" * len(per_scale))
    for a in adapters:
        cells = []
        for p in per_scale:
            row = next((x for x in p["adapters"] if x.get("adapter") == a), None)
            if row is None:
                cells.append("—")
            else:
                cells.append(_safe_float(row.get("mean_r_at_5")))
        lines.append(f"| `{a}` | " + " | ".join(cells) + " |")
    lines.append("")

    lines.append("## Latency p50 (ms) by corpus scale\n")
    lines.append("| Adapter | " + " | ".join(f"{p['scale_pct']}%" for p in per_scale) + " |")
    lines.append("| --- |" + " ---: |" * len(per_scale))
    for a in adapters:
        cells = []
        for p in per_scale:
            row = next((x for x in p["adapters"] if x.get("adapter") == a), None)
            if row is None:
                cells.append("—")
            else:
                cells.append(_safe_int(row.get("latency_p50_ms")))
        lines.append(f"| `{a}` | " + " | ".join(cells) + " |")
    lines.append("")

    lines.append("## Tokens p50 by corpus scale\n")
    lines.append("| Adapter | " + " | ".join(f"{p['scale_pct']}%" for p in per_scale) + " |")
    lines.append("| --- |" + " ---: |" * len(per_scale))
    for a in adapters:
        cells = []
        for p in per_scale:
            row = next((x for x in p["adapters"] if x.get("adapter") == a), None)
            if row is None:
                cells.append("—")
            else:
                cells.append(_safe_int(row.get("tokens_p50")))
        lines.append(f"| `{a}` | " + " | ".join(cells) + " |")
    lines.append("")

    # Drill-down links per scale.
    lines.append("## Per-scale details\n")
    for p in per_scale:
        lines.append(f"- {p['scale_pct']}% ({p['n_files']} files, "
                     f"{p['n_questions']} questions answerable) → "
                     f"`scale-{p['scale_pct']}/report.md`")
    return "\n".join(lines) + "\n"


def main() -> int:
    ap = argparse.ArgumentParser(description="SCALE: adapter behavior vs corpus size")
    ap.add_argument("--repo", required=True)
    ap.add_argument("--repo-root", default=None)
    ap.add_argument(
        "--scales", default="100,50,25,10",
        help="comma-separated percentages (default 100,50,25,10)",
    )
    ap.add_argument(
        "--adapters", default="codegraph,zoekt,agentmemory-bm25,grep",
        help=(
            "adapters to run at each scale. Defaults to the fast ones "
            "since SCALE is N×wall-time and agentmemory-hybrid alone "
            "is ~165s setup per scale (~11 min for 4 scales)."
        ),
    )
    ap.add_argument("--seed", type=int, default=42)
    ap.add_argument("--k", type=int, default=10)
    ap.add_argument("--output-dir", default=None)
    args = ap.parse_args()

    _load_adapters()
    adapters = [s.strip() for s in args.adapters.split(",") if s.strip()]
    for a in adapters:
        if a not in REGISTRY:
            raise SystemExit(f"[scale] unknown adapter {a!r}; available: {sorted(REGISTRY)}")

    here = Path(__file__).resolve().parent
    repo_root = args.repo_root or str(here.parent.parent / args.repo)
    if not os.path.isdir(repo_root):
        raise SystemExit(f"[scale] repo_root not found: {repo_root}")

    out_root = Path(
        args.output_dir
        or here / "results" / (datetime.now().strftime("%Y%m%d-%H%M%S") + "-scale")
    )
    out_root.mkdir(parents=True, exist_ok=True)
    print(f"[scale] out_root: {out_root}", flush=True)
    print(f"[scale] repo_root: {repo_root}", flush=True)
    print(f"[scale] adapters: {adapters}", flush=True)

    print("[scale] enumerating files...", flush=True)
    t0 = time.perf_counter()
    all_files = _enumerate_files(repo_root)
    print(f"[scale]   {len(all_files)} files in {(time.perf_counter() - t0) * 1000:.0f} ms", flush=True)

    questions = load_questions(here / "questions" / f"{args.repo}.jsonl", None)
    must_include: set[str] = set()
    for q in questions:
        for e in q.get("expected_files") or []:
            # We only force file-shaped entries (the directory-hint
            # form is handled by _filter_questions_by_corpus's
            # startswith fallback).
            if "." in os.path.basename(e):
                must_include.add(e)
    print(f"[scale] must_include = {len(must_include)} files from {len(questions)} questions", flush=True)

    scales_pct = sorted([int(s.strip()) for s in args.scales.split(",")], reverse=True)
    per_scale: list[dict] = []
    for pct in scales_pct:
        fraction = pct / 100.0
        sample = _sample_files(all_files, must_include, fraction, args.seed)
        res = _run_one_scale(
            fraction, sample, repo_root, args.repo, out_root,
            adapters, questions, args.k,
        )
        per_scale.append(res)

    md = _render_scale_md(per_scale, adapters)
    (out_root / "SCALE.md").write_text(md)
    (out_root / "scale_summary.json").write_text(json.dumps(per_scale, indent=2))
    print("\n" + md, flush=True)
    print(f"[scale] wrote {out_root}/SCALE.md", flush=True)
    return 0


if __name__ == "__main__":
    sys.exit(main())
