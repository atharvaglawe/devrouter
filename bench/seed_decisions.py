"""Seed DevRouter with hand-authored architectural decisions for the
three benchmark repos (goserving, mall, airflow-core).

Purpose
-------
The bench/runner.py path exercises read-side memory (dev_context →
SearchAll → no writes). Decisions are write-side: they only land in
Redis when an agent calls the `decision_save` / `decision_supersede`
MCP tools. This script does exactly that for a small, realistic set
per repo so:

  - the dashboard's Decisions tab has populated content per repo, and
  - one supersession chain per repo demonstrates the lineage tree
    rendering end to end.

Idempotent — DevRouter upserts on (repo, name), so re-running just
refreshes the contents and re-applies any supersession links.

Usage
-----
    python3 bench/seed_decisions.py                   # all three repos
    python3 bench/seed_decisions.py --repo goserving  # one repo
    python3 bench/seed_decisions.py --dry-run         # print, don't send
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

# ---------------------------------------------------------------------------
# Decision corpus
#
# Each entry maps 1:1 onto decision_save's arguments. Where a supersession
# chain is desired, list the older decision first and set `supersedes` on
# the newer one — the seeder calls decision_supersede after both saves
# have landed so the lineage edge sticks.
# ---------------------------------------------------------------------------

DECISIONS: dict[str, list[dict]] = {
    "goserving": [
        {
            "name": "use-redis-for-session-cache",
            "decision_type": "architecture",
            "decision": "Use Redis as the primary session cache instead of an in-process LRU.",
            "rationale": "Multiple FMS controller pods need shared session state; an in-process LRU diverges under load and forces sticky routing we don't want.",
            "alternatives": "memcached (no persistence), in-process LRU (per-pod drift), Postgres (latency too high for per-request hits).",
            "decision_scope": "all FMS controllers + session middleware",
            "files": "controllers/fms_controller.go,middleware/session.go",
        },
        {
            "name": "prefer-context-over-globals-v1",
            "decision_type": "coding_standard",
            "decision": "Pass request-scoped values via context.Context, not package-level globals.",
            "rationale": "Globals make request boundaries invisible in stack traces and break under concurrent reuse.",
            "files": "controllers/fms_controller.go,routes/api.go",
        },
        {
            "name": "prefer-context-over-globals-v2",
            "decision_type": "coding_standard",
            "decision": "Pass request-scoped values via context.Context with typed keys; reject string keys.",
            "rationale": "v1 didn't prevent string-key collisions across packages. Typed keys are an unforgeable contract enforced at compile time.",
            "alternatives": "string keys (collision-prone), thread-local-style globals (concurrency hazard).",
            "files": "controllers/fms_controller.go,routes/api.go,middleware/ctxkeys.go",
            "supersedes": "prefer-context-over-globals-v1",
        },
        {
            "name": "tracing-via-otel-not-zap-fields",
            "decision_type": "architecture",
            "decision": "Use OpenTelemetry spans for cross-service tracing; keep zap logs as the human-readable channel only.",
            "rationale": "Mixing trace IDs into zap fields couples log schema to tracer choice and prevents drop-in tracer swaps.",
            "decision_scope": "every outbound HTTP/gRPC client + every controller entry point",
        },
        {
            "name": "no-direct-db-access-in-controllers",
            "decision_type": "constraint",
            "decision": "Controllers must not import database drivers directly; go through the repository layer.",
            "rationale": "Direct DB access in controllers bypasses retry/circuit-break/metrics that the repo layer enforces.",
        },
    ],
    "mall": [
        {
            "name": "use-mybatis-not-jpa",
            "decision_type": "architecture",
            "decision": "Use MyBatis for all persistence; do not introduce Spring Data JPA.",
            "rationale": "Mall has heavy custom SQL for product search and order aggregation; JPA's HQL/Criteria API obscures the actual queries and makes performance tuning opaque.",
            "alternatives": "Spring Data JPA, plain JdbcTemplate.",
            "decision_scope": "every *Mapper + every service that hits the DB",
        },
        {
            "name": "controller-service-mapper-strict-v1",
            "decision_type": "coding_standard",
            "decision": "Controllers may only call services; services may only call mappers.",
            "rationale": "Direct controller→mapper calls bypass transactional boundaries declared at the service layer.",
            "files": "src/main/java/com/macro/mall/controller,src/main/java/com/macro/mall/service",
        },
        {
            "name": "controller-service-mapper-strict-v2",
            "decision_type": "coding_standard",
            "decision": "Controllers → services → mappers; service interfaces required and must live in mall-service:service, impls in mall-service:service.impl.",
            "rationale": "v1 only enforced the direction. We also need every service to expose an interface so test doubles can substitute cleanly and the impl package is isolated for autowiring.",
            "files": "mall-admin/src/main/java/com/macro/mall/controller,mall-admin/src/main/java/com/macro/mall/service",
            "supersedes": "controller-service-mapper-strict-v1",
        },
        {
            "name": "dto-not-entity-on-the-wire",
            "decision_type": "constraint",
            "decision": "REST endpoints must return DTOs, never JPA/MyBatis entities directly.",
            "rationale": "Leaking entities couples API consumers to DB schema and forces breaking API changes on every column rename.",
            "decision_scope": "every @RestController return type",
        },
        {
            "name": "redis-for-product-detail-cache",
            "decision_type": "optimization",
            "decision": "Cache product detail responses in Redis with a 10-minute TTL; invalidate on product update.",
            "rationale": "Product detail is hit ~50× more than it's updated; per-request DB joins dominate p95 latency without this cache.",
            "files": "mall-portal/src/main/java/com/macro/mall/portal/service/PmsPortalProductService.java",
        },
    ],
    "airflow-core": [
        {
            "name": "executor-defaults-to-local-not-sequential",
            "decision_type": "architecture",
            "decision": "Default executor in airflow-core's example configs is LocalExecutor, not SequentialExecutor.",
            "rationale": "SequentialExecutor is incompatible with SQLite-on-shared-disk in real installs and gives users a wrong first impression of scheduler behaviour.",
            "alternatives": "SequentialExecutor (toy default), CeleryExecutor (requires extra services).",
        },
        {
            "name": "scheduler-uses-asyncio-loop-v1",
            "decision_type": "architecture",
            "decision": "Scheduler internal loop is asyncio-based, not thread-pool-based.",
            "rationale": "Most DAG-evaluation work is I/O-bound (DB + filesystem); asyncio gives us better concurrency without GIL contention.",
            "files": "src/airflow/jobs/scheduler_job_runner.py",
        },
        {
            "name": "scheduler-uses-asyncio-loop-v2",
            "decision_type": "architecture",
            "decision": "Scheduler internal loop is asyncio with a bounded thread pool for the synchronous DAG-parser shim only.",
            "rationale": "Pure asyncio v1 starved on the long-running DAG parser (CPU-bound, blocking). A bounded ThreadPoolExecutor isolates that one workload without giving up async benefits for the rest of the scheduler.",
            "alternatives": "all-asyncio (starvation), all-threadpool (GIL contention on I/O paths).",
            "files": "src/airflow/jobs/scheduler_job_runner.py,src/airflow/dag_processing/processor.py",
            "supersedes": "scheduler-uses-asyncio-loop-v1",
        },
        {
            "name": "no-implicit-dag-discovery",
            "decision_type": "constraint",
            "decision": "DAG files must be explicitly listed in the dag bundle config; do not scan dags_folder recursively at scheduler boot.",
            "rationale": "Implicit discovery makes deployment behaviour depend on filesystem layout and slows scheduler startup on large repos.",
        },
        {
            "name": "task-context-must-be-pickleable",
            "decision_type": "constraint",
            "decision": "Every value placed on TaskInstance.context must be pickleable; reject non-pickleable values at task-build time, not at execution.",
            "rationale": "Late failures from non-pickleable context values are extremely hard to debug — they only surface inside the executor process. Fail-fast at build time.",
            "files": "src/airflow/models/taskinstance.py",
        },
    ],
}


# ---------------------------------------------------------------------------
# MCP plumbing
# ---------------------------------------------------------------------------

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
        raise RuntimeError(f"devrouter binary missing at {DEVROUTER_BIN}; run `make all` first")
    # Send stderr to a logfile rather than subprocess.PIPE — DevRouter
    # logs liberally on startup (picker init, dashboard bind, embedder
    # ping, …) and the OS pipe buffer (~64 KB on macOS) will fill and
    # wedge the child if nothing drains it. We never read these logs
    # in the seed flow, so DEVNULL would also work; logging to a file
    # keeps them around for post-mortem if a save fails.
    stderr_log = open("/tmp/seed_decisions.devrouter.log", "w")
    proc = subprocess.Popen(
        [str(DEVROUTER_BIN)],
        stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=stderr_log,
        text=True, bufsize=1,
    )
    # Initialize handshake — same shape the MCP host sends.
    _send(proc, 1, "initialize", {
        "protocolVersion": "2024-11-05",
        "capabilities": {},
        "clientInfo": {"name": "seed_decisions", "version": "0"},
    })
    _notify(proc, "notifications/initialized", {})
    return proc


def call_tool(proc: subprocess.Popen, rid: int, name: str, args: dict) -> dict:
    return _send(proc, rid, "tools/call", {"name": name, "arguments": args})


def seed_repo(proc: subprocess.Popen, repo: str, decisions: list[dict], dry_run: bool) -> tuple[int, int]:
    """Save every decision, then apply any supersession links."""
    rid = 100
    saved, errors = 0, 0
    pending_supersedes: list[tuple[str, str]] = []  # (old_name, new_name)

    for d in decisions:
        # Pull supersedes out — it's not part of decision_save's schema;
        # we apply it as a separate decision_supersede call after both
        # decisions exist.
        new_name = d["name"]
        supersedes = d.pop("supersedes", None)

        payload = {
            "repo": repo,
            "name": new_name,
            "decision_type": d["decision_type"],
            "decision": d["decision"],
            "rationale": d["rationale"],
        }
        for opt in ("alternatives", "constraint", "decision_scope", "files", "scope"):
            if opt in d and d[opt]:
                payload[opt] = d[opt]

        if dry_run:
            print(f"  [dry-run] decision_save {repo}/{new_name} ({d['decision_type']})")
        else:
            rid += 1
            resp = call_tool(proc, rid, "decision_save", payload)
            if "error" in resp:
                print(f"  ERR {repo}/{new_name}: {resp['error']}")
                errors += 1
            else:
                saved += 1

        if supersedes:
            pending_supersedes.append((supersedes, new_name))

    for old_name, new_name in pending_supersedes:
        if dry_run:
            print(f"  [dry-run] decision_supersede {repo}: {old_name} → {new_name}")
            continue
        rid += 1
        resp = call_tool(proc, rid, "decision_supersede", {
            "repo": repo,
            "old_name": old_name,
            "new_name": new_name,
            "reason": "explicit supersession from seed_decisions.py",
        })
        if "error" in resp:
            print(f"  ERR supersede {repo}/{old_name}→{new_name}: {resp['error']}")
            errors += 1
        else:
            print(f"  supersede ok: {old_name} → {new_name}")

    return saved, errors


def main() -> int:
    ap = argparse.ArgumentParser(description="Seed DevRouter Redis with sample decisions per repo.")
    ap.add_argument("--repo", choices=sorted(DECISIONS.keys()),
                    help="seed just one repo (default: all)")
    ap.add_argument("--dry-run", action="store_true",
                    help="print what would be sent without spawning devrouter")
    args = ap.parse_args()

    repos = [args.repo] if args.repo else list(DECISIONS.keys())

    if args.dry_run:
        for repo in repos:
            print(f"\n=== {repo} ===")
            seed_repo(proc=None, repo=repo, decisions=[dict(d) for d in DECISIONS[repo]], dry_run=True)  # type: ignore[arg-type]
        return 0

    t0 = time.perf_counter()
    proc = open_session()
    total_saved, total_errors = 0, 0
    try:
        for repo in repos:
            print(f"\n=== {repo} ===")
            saved, errors = seed_repo(proc, repo, [dict(d) for d in DECISIONS[repo]], dry_run=False)
            total_saved += saved
            total_errors += errors
            print(f"  {repo}: {saved} saved, {errors} errors")
    finally:
        try:
            proc.stdin.close()  # type: ignore[union-attr]
            proc.wait(timeout=5)
        except Exception:
            proc.kill()

    elapsed_ms = (time.perf_counter() - t0) * 1000.0
    print(f"\n[seed] total: {total_saved} saved, {total_errors} errors in {elapsed_ms:.0f} ms")
    return 0 if total_errors == 0 else 1


if __name__ == "__main__":
    sys.exit(main())
