"""Replay synthetic dev_feedback calls against every recent trace.

Why
---
The bench runner (bench/runner.py) only fires dev_context — it never
calls dev_feedback. As a result the dashboard's "Heuristics" and
"Topics" tabs sit at samples=0 even after a full bench cycle, and the
bandit can't tune anything because no reward signal has been observed.

This script closes that loop by walking the live trace index and
issuing one dev_feedback per trace, with a deliberately *varied*
synthetic distribution (not all-success, not all-zero additional
files) so the bandit actually has a gradient to learn from. The
distribution is fixed-seed so re-runs are byte-deterministic and the
dashboard's reward rows can be diffed across runs.

It is NOT a fidelity benchmark — it's a UI / heuristics population
helper. Use bench/runner.py + the real agent feedback path for
quality measurement.

Usage
-----
    python3 bench/sweep_feedback.py
    python3 bench/sweep_feedback.py --limit 20      # smoke test
    python3 bench/sweep_feedback.py --dry-run       # print, no calls
"""

from __future__ import annotations

import argparse
import hashlib
import json
import random
import subprocess
import sys
import time
from pathlib import Path

import urllib.request

ROOT = Path(__file__).resolve().parent.parent
DEVROUTER_BIN = ROOT / "devrouter"
DASHBOARD_URL = "http://127.0.0.1:8089"


def fetch_query_ids(limit: int | None) -> list[dict]:
    """Pull recent queries from the live dashboard /api/queries endpoint.

    The dashboard already does the trace-key enumeration + decoding, so
    reusing it is shorter and safer than re-implementing parseFTHits
    here. Falls back to a hard error if the dashboard isn't up — we
    need it anyway to verify the feedback rows land in the UI.
    """
    # ?limit=500 matches heuristics.TraceIndexCap, the hard ceiling on
    # what the trace index holds. The dashboard UI defaults to 100, but
    # for a backfill sweep we want every retained trace, not the recent
    # window.
    try:
        with urllib.request.urlopen(f"{DASHBOARD_URL}/api/queries?limit=500", timeout=5) as r:
            rows = json.loads(r.read())
    except Exception as e:
        raise SystemExit(
            f"[feedback] dashboard {DASHBOARD_URL}/api/queries unreachable: {e}\n"
            f"            start devrouter with DEVROUTER_DASHBOARD_ADDR=127.0.0.1:8089 first"
        )
    if rows is None:
        rows = []
    rows = [r for r in rows if r.get("query_id")]
    if limit:
        rows = rows[:limit]
    return rows


def synth_feedback(query_id: str, intent: str) -> dict:
    """Generate a deterministic synthetic feedback payload for one trace.

    Seeded on query_id so re-runs produce identical rewards. Intent
    biases the success distribution slightly — "trace" / "debug"
    queries are harder, so we model 60% success vs 80% for the easier
    intents. This gives the bandit a realistic per-intent reward
    gradient without making anyone read the actual retrieval.
    """
    rng = random.Random(hashlib.sha256(query_id.encode()).digest())

    hard = intent in ("trace", "debug")
    success = rng.random() < (0.60 if hard else 0.80)

    # additional_files: Poisson-ish, mean 2 for easy / 3 for hard.
    # Cap at 8 to keep the dashboard sparkline readable.
    mean = 3.0 if hard else 2.0
    k = 0
    p = pow(2.718281828, -mean)
    cum = p
    u = rng.random()
    while u > cum and k < 8:
        k += 1
        p *= mean / k
        cum += p
    additional = k

    revisited = 1 if rng.random() < 0.15 else 0

    return {
        "query_id": query_id,
        "additional_files": additional,
        "revisited_files": revisited,
        "success": success,
    }


def _send(proc: subprocess.Popen, rid: int, method: str, params: dict) -> dict:
    if proc.stdin is None or proc.stdout is None:
        raise RuntimeError("devrouter subprocess pipes closed")
    msg = {"jsonrpc": "2.0", "id": rid, "method": method, "params": params}
    proc.stdin.write(json.dumps(msg) + "\n")
    proc.stdin.flush()
    line = proc.stdout.readline()
    return json.loads(line) if line.strip() else {}


def _notify(proc: subprocess.Popen, method: str, params: dict) -> None:
    if proc.stdin is None:
        return
    proc.stdin.write(json.dumps({"jsonrpc": "2.0", "method": method, "params": params}) + "\n")
    proc.stdin.flush()


def open_session() -> subprocess.Popen:
    if not DEVROUTER_BIN.exists():
        raise SystemExit(f"devrouter binary missing at {DEVROUTER_BIN}; run `make all` first")
    # Stderr → file so devrouter's chatty logs don't deadlock the pipe.
    # Same pattern as seed_flows.py — see that file for context.
    stderr_log = open("/tmp/sweep_feedback.devrouter.log", "w")
    proc = subprocess.Popen(
        [str(DEVROUTER_BIN)],
        stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=stderr_log,
        text=True, bufsize=1,
    )
    _send(proc, 1, "initialize", {
        "protocolVersion": "2024-11-05",
        "capabilities": {},
        "clientInfo": {"name": "sweep_feedback", "version": "0"},
    })
    _notify(proc, "notifications/initialized", {})
    return proc


def main() -> int:
    ap = argparse.ArgumentParser(description="Replay synthetic dev_feedback against every recent trace.")
    ap.add_argument("--limit", type=int, default=0, help="cap number of traces (0 = all)")
    ap.add_argument("--dry-run", action="store_true", help="print payloads without spawning devrouter")
    args = ap.parse_args()

    rows = fetch_query_ids(args.limit if args.limit > 0 else None)
    if not rows:
        print("[feedback] no traces found — run bench/runner.py first to populate dev_context calls")
        return 1
    print(f"[feedback] {len(rows)} traces to sweep")

    # Bucket histogram by intent so the post-run "what does the bandit
    # now know?" question is answerable from this script's output alone.
    by_intent: dict[str, list[dict]] = {}
    payloads: list[dict] = []
    for row in rows:
        payload = synth_feedback(row["query_id"], row.get("intent") or "general")
        payloads.append(payload)
        by_intent.setdefault(row.get("intent") or "general", []).append(payload)

    print("[feedback] synthetic reward distribution by intent:")
    for intent, plds in sorted(by_intent.items()):
        ok = sum(1 for p in plds if p["success"])
        avg_add = sum(p["additional_files"] for p in plds) / max(1, len(plds))
        print(f"           {intent:8s} n={len(plds):3d}  success={ok}/{len(plds)}  avg_additional_files={avg_add:.2f}")

    if args.dry_run:
        for p in payloads[:5]:
            print("  dry:", json.dumps(p))
        if len(payloads) > 5:
            print(f"  ... ({len(payloads) - 5} more)")
        return 0

    proc = open_session()
    rid = 100
    ok, err = 0, 0
    t0 = time.perf_counter()
    try:
        for p in payloads:
            rid += 1
            resp = _send(proc, rid, "tools/call", {"name": "dev_feedback", "arguments": p})
            if "error" in resp:
                err += 1
                print(f"  ERR {p['query_id'][:8]}: {resp['error']}")
            else:
                ok += 1
    finally:
        try:
            if proc.stdin is not None:
                proc.stdin.close()
            proc.wait(timeout=5)
        except Exception:
            proc.kill()

    elapsed_ms = (time.perf_counter() - t0) * 1000.0
    print(f"\n[feedback] sent {ok} ok, {err} errors in {elapsed_ms:.0f} ms")
    return 0 if err == 0 else 1


if __name__ == "__main__":
    sys.exit(main())
