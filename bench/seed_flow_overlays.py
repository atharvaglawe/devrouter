#!/usr/bin/env python3
"""Seed demo flow-overlay hashes into Redis so the dashboard renders
feedback-derived hints (validated / dead / missing) on saved flows
without requiring a corpus of real agent feedback first.

Writes directly to `flow:overlay:{keyspace}:{repo}:{name}` matching the
shape Store.UpdateFlowOverlay produces. Idempotent: rerun rebuilds the
demo overlays from scratch.

This is demo seed data — production overlays come from `dev_feedback`
calls that include `flow_id`.

Path source: we prefer file paths drawn from the flow's saved codegraph
subgraph (subgraph_json[].nodes[].file) so the dashboard's in-SVG tints
actually have something to match. The bipartite `flow.files` list is
agent-authored and often uses logical paths that don't align with the
codegraph's filesystem layout (e.g. seed_flows.py says
"internal/advertiserblocker/check.go" while codegraph has
"weaver/app/pkg/appwarmup/appwarmup.go"). When subgraph paths exist
we use those; otherwise we fall back to flow.files so the chips below
the graph still light up.
"""
from __future__ import annotations

import json
import os
import random
import sys
import time

try:
    import redis
except ImportError:
    print("pip install redis", file=sys.stderr)
    sys.exit(1)

REDIS_ADDR = os.environ.get("REDIS_ADDR", "localhost:6379")
KEYSPACE   = os.environ.get("DEVROUTER_KEYSPACE", "mem")
SEED       = int(os.environ.get("SEED", "42"))


# Each flow gets a "shape" that paints a recognisably different picture
# on the dashboard so the heuristics-tab aggregates (stale / under-
# specified) actually have something to rank.
SHAPES = [
    # (name,            n_feedback, p_useful, p_dead, n_missing)
    ("mostly_validated",  12,        0.85,    0.10,     0),
    ("balanced",          8,         0.55,    0.35,     1),
    ("stale",             10,        0.20,    0.70,     0),
    ("augmented",         6,         0.65,    0.15,     3),
    ("low_signal",        3,         0.50,    0.40,     0),
]

# Plausible-looking files devrouter agents commonly report as missing
# from real flows, used when a shape calls for `n_missing > 0`. These
# don't have to exist in the flow's `files` list — that's the whole
# point of the missing channel.
SYNTHETIC_MISSING = {
    "goserving": [
        "internal/middleware/auth.go",
        "internal/metrics/prometheus.go",
        "config/loader.go",
    ],
    "mall": [
        "src/main/java/com/mall/common/exception/GlobalHandler.java",
        "src/main/java/com/mall/auth/JwtFilter.java",
        "src/main/resources/application.yml",
    ],
    "airflow-core": [
        "airflow/utils/log/logging_mixin.py",
        "airflow/configuration.py",
        "airflow/utils/db.py",
    ],
}


def overlay_key(repo: str, name: str) -> str:
    """Mirror memory.sanitizeKey: replace : / \\ space with _."""
    sanitised = "".join("_" if c in ":/\\ " else c for c in name)
    return f"flow:overlay:{KEYSPACE}:{repo}:{sanitised}"


def resolve_overlay_files(r: redis.Redis, flow_key: str) -> list[str]:
    """Pull file paths the SVG renderer will actually try to match
    against. Order of preference:

      1. Unique non-empty subgraph_json[].nodes[].file values — these
         are the exact strings rendered as node-file-text inside each
         <rect>, so the in-SVG tints will line up byte-for-byte.
      2. flow.files CSV — fallback when the flow had no subgraph
         (codegraph unreachable at save time, or pre-snapshot flow).
         The chips below the graph still light up; the in-SVG tints
         just don't have rect anchors to attach to.

    HMGET-not-HGETALL because the `embedding` field contains a packed
    768×float32 blob that breaks decode_responses=True.
    """
    files_csv, subgraph_json = r.hmget(flow_key, "files", "subgraph_json")
    paths = []
    if subgraph_json:
        try:
            sg = json.loads(subgraph_json)
            seen = set()
            for node in sg.get("nodes", []):
                fp = (node.get("file") or "").strip()
                if fp and fp not in seen:
                    seen.add(fp)
                    paths.append(fp)
        except (ValueError, TypeError):
            pass
    if not paths and files_csv:
        paths = [p.strip() for p in files_csv.split(",") if p.strip()]
    return paths


def seed_one_flow(r: redis.Redis, key: str, repo: str, name: str, files: list[str]):
    shape = SHAPES[random.randrange(len(SHAPES))]
    sh_name, n_feedback, p_useful, p_dead, n_missing = shape
    if not files:
        return shape[0], 0

    # Wipe any prior state so reruns don't pile counters on top of
    # each other (HINCRBY is additive).
    r.delete(key)

    pipe = r.pipeline()
    for f in files:
        # Per file, weighted coin flip n_feedback times. We collapse the
        # per-event outcomes into a single HINCRBY per state to keep the
        # write count linear in `files` rather than `files * n_feedback`.
        useful = 0
        dead = 0
        for _ in range(n_feedback):
            roll = random.random()
            if roll < p_useful:
                useful += 1
            elif roll < p_useful + p_dead:
                dead += 1
        if useful:
            pipe.hincrby(key, f"file_useful:{f}", useful)
        if dead:
            pipe.hincrby(key, f"file_dead:{f}", dead)

    pool = SYNTHETIC_MISSING.get(repo, [])
    chosen_missing = random.sample(pool, min(n_missing, len(pool))) if pool else []
    for m in chosen_missing:
        # Variable hit count per missing file — some files multiple
        # agents reported, others just one. More realistic distribution
        # for the dashboard's "+N missing" sort.
        hits = random.randint(1, max(2, n_feedback // 3))
        pipe.hincrby(key, f"missing:{m}", hits)

    pipe.hincrby(key, "total_feedback", n_feedback)
    pipe.hset(key, mapping={
        "last_feedback_at": int(time.time() * 1000),
        "last_query_id":    f"demo-{int(time.time())}-{random.randint(1000,9999)}",
    })
    pipe.execute()
    return sh_name, n_feedback


def main() -> int:
    random.seed(SEED)
    host, _, port = REDIS_ADDR.partition(":")
    r = redis.Redis(host=host or "localhost", port=int(port or "6379"), decode_responses=True)
    try:
        r.ping()
    except Exception as e:
        print(f"redis unreachable at {REDIS_ADDR}: {e}", file=sys.stderr)
        return 1

    flow_keys = sorted(r.keys(f"{KEYSPACE}:*:flow:*"))
    if not flow_keys:
        print(f"no flows found under {KEYSPACE}:*:flow:* — seed flows first")
        return 1

    print(f"seeding overlays for {len(flow_keys)} flows (seed={SEED})")
    for fk in flow_keys:
        # mem:goserving:flow:advertiserblocker-decision-path -> repo, name
        _, repo, _, name = fk.split(":", 3)
        files = resolve_overlay_files(r, fk)
        # Subgraphs can hold dozens of files; tinting all of them is
        # noisy. Sample a realistic subset (~6-10) so the dashboard
        # shows a mix of validated / dead / un-tinted nodes per band.
        if len(files) > 8:
            files = random.sample(files, 8)
        key = overlay_key(repo, name)
        shape, n = seed_one_flow(r, key, repo, name, files)
        print(f"  {repo}/{name}  shape={shape:<18}  events={n}  paths={len(files)}")

    print(f"\ndone. {len(flow_keys)} overlays written.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
