"""Seed DevRouter and mem0 with the same hand-authored memory corpus.

Used by the memory-augmented retrieval bench (see docs/benchmarks.md
"Memory-augmented retrieval" section). The mem0 adapter does its own
seeding inside `setup()`, so this script is primarily about getting
the same notes into DevRouter via the `memory_save_file` MCP tool.

DevRouter has the codegraph layer behind it, so we deliberately
DON'T pre-warm it with `dev_context` calls before saving — we want
mem0 and DevRouter to face the question set with the same a priori
context: a flat set of 30 file→note pairs, no graph traversal
history, no anchor-learning credit signal yet.

Usage:
    python3 bench/seed_memories.py --repo mall
    python3 bench/seed_memories.py --repo mall --dry-run

What it does:
    For each {file_path, memory} in bench/memories/<repo>.jsonl:
        1. memory_save_file(repo=<repo>, path=<file_path>,
                            purpose=<memory>, scope='global')

Idempotency:
    DevRouter's memory_save_file is upsert-on-(repo,path), so re-running
    overwrites earlier notes with the current JSONL contents. Safe to
    re-run after editing memories/<repo>.jsonl.
"""

from __future__ import annotations

import argparse
import json
import subprocess
import sys
import time
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
DEVROUTER_BIN = ROOT / "devrouter"


def _send(proc: subprocess.Popen, rid: int, method: str, params: dict) -> dict:
    if proc.stdin is None or proc.stdout is None:
        raise RuntimeError("devrouter subprocess pipes closed")
    msg = {"jsonrpc": "2.0", "id": rid, "method": method, "params": params}
    proc.stdin.write(json.dumps(msg) + "\n")
    proc.stdin.flush()
    line = proc.stdout.readline()
    return json.loads(line) if line.strip() else {}


def seed_devrouter(repo: str, memories: list[dict]) -> tuple[int, int]:
    """Open one MCP session and stream all memory_save_file calls.

    One long-lived session amortizes the ~2-3s devrouter startup
    cost across all 30 memories instead of paying it per save.
    """
    if not DEVROUTER_BIN.exists():
        raise RuntimeError(f"devrouter binary missing at {DEVROUTER_BIN}; run `make all` first")

    proc = subprocess.Popen(
        [str(DEVROUTER_BIN)],
        stdin=subprocess.PIPE,
        stdout=subprocess.PIPE,
        stderr=open("/tmp/devrouter_seed.log", "w"),
        text=True,
        bufsize=1,
    )

    rid = 0
    rid += 1
    init = _send(proc, rid, "initialize", {"protocolVersion": "2024-11-05", "capabilities": {}})
    if "error" in init:
        proc.terminate()
        raise RuntimeError(f"devrouter init failed: {init['error']}")

    ok, fail = 0, 0
    t0 = time.time()
    for i, m in enumerate(memories, 1):
        rid += 1
        r = _send(
            proc,
            rid,
            "tools/call",
            {
                "name": "memory_save_file",
                "arguments": {
                    "repo": repo,
                    "path": m["file_path"],
                    "purpose": m["memory"],
                    "scope": "global",
                },
            },
        )
        if "error" in r:
            fail += 1
            print(f"  [{i}/{len(memories)}] FAIL {m['file_path']}: {r['error']}", file=sys.stderr)
        else:
            ok += 1

    elapsed = time.time() - t0
    print(f"[seed] devrouter: {ok} saved, {fail} failed, elapsed={elapsed:.2f}s", file=sys.stderr)

    try:
        proc.stdin.close()
        proc.wait(timeout=5)
    except Exception:
        proc.kill()
    return ok, fail


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--repo", required=True, help="repo name (matches bench/memories/<repo>.jsonl)")
    ap.add_argument("--dry-run", action="store_true",
                    help="print what would be saved without calling devrouter")
    args = ap.parse_args()

    mem_path = ROOT / "bench" / "memories" / f"{args.repo}.jsonl"
    if not mem_path.exists():
        print(f"[seed] no corpus at {mem_path}", file=sys.stderr)
        return 1

    memories: list[dict] = []
    with mem_path.open() as f:
        for line in f:
            line = line.strip()
            if not line:
                continue
            memories.append(json.loads(line))
    print(f"[seed] loaded {len(memories)} memories from {mem_path}", file=sys.stderr)

    if args.dry_run:
        for m in memories[:3]:
            print(f"  would save: {m['file_path']}")
            print(f"             {m['memory'][:80]}...")
        print(f"  ... ({len(memories) - 3} more)")
        return 0

    ok, fail = seed_devrouter(args.repo, memories)
    return 0 if fail == 0 else 1


if __name__ == "__main__":
    sys.exit(main())
