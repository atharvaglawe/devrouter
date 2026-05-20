"""Seed DevRouter with hand-authored end-to-end flow memories for the
three benchmark repos (goserving, mall, airflow-core).

Why
---
DevRouter's `dev_context` retrieval pipeline pulls FlowMemory hits
alongside file/func/decision memories. Until a real agent has spent
time tracing flows and calling `memory_save_flow`, the Flows tab in
the dashboard sits empty, and `dev_context` can't surface step-by-step
sequences when the LLM asks integration-style questions like
"how does a request travel through X". This seeder front-loads a
realistic per-repo corpus so the dashboard and the retrieval
pipeline have something to work with from a clean Redis.

Each flow is a JSON-encoded `memory_save_flow` call. Required fields
are `repo`, `name`, `purpose`. `files` and `entry_points` are highly
recommended — they're what makes the flow searchable by file overlap
and resolvable to specific call sites.

Idempotency
-----------
DevRouter upserts on (repo, name), so re-running this script just
refreshes the contents. Safe to re-run after editing the corpus.

Usage
-----
    python3 bench/seed_flows.py                   # all three repos
    python3 bench/seed_flows.py --repo goserving  # one repo
    python3 bench/seed_flows.py --dry-run         # print, don't send
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
# Flow corpus
#
# Each entry maps 1:1 onto memory_save_flow's arguments. The `purpose`
# field is intentionally a multi-step description (per the MCP tool's
# own examples — "1) Create X, 2) Register Y, ..."), not a one-line
# summary, because that's what the retrieval pipeline keys on when an
# LLM asks an integration-shaped question.
# ---------------------------------------------------------------------------

FLOWS: dict[str, list[dict]] = {
    "goserving": [
        {
            "name": "oscar-http-request-lifecycle",
            "purpose": (
                "End-to-end lifecycle of an inbound HTTP request hitting the oscar service: "
                "1) cmd/oscar/main.go boots and calls web.Start with the configured port from config.GetPort. "
                "2) oscar/web/web.go installs the router and binds the listener via server.Serve. "
                "3) Each request hits the AddRoute-registered controller (e.g. health, ads, content). "
                "4) Controllers pull request-scoped values from context.Context (typed keys, not globals). "
                "5) Controller returns a structured response that the middleware wraps in the standard "
                "envelope before flushing back to the client."
            ),
            "files": "cmd/oscar/main.go,oscar/web/web.go,oscar/web/controllers/health.go,oscar/web/controllers/ads.go,oscar/routes/api.go,middleware/session.go",
            "entry_points": "main,web.Start,health.ServeRequest",
        },
        {
            "name": "graceful-shutdown-sequence",
            "purpose": (
                "How oscar tears down on SIGINT/SIGTERM without dropping in-flight requests: "
                "1) cmd/oscar/main.go installs a signal handler that closes a quit channel. "
                "2) The HTTP server's Shutdown(ctx) is called with a bounded deadline. "
                "3) In-flight handlers continue running on their request contexts but no new connections are accepted. "
                "4) Background workers (terminator, throttle clear, dchealthchecker reporting node) "
                "watch the same quit channel and drain their queues. "
                "5) Once Shutdown returns or the deadline fires, main exits with status reflecting drain success."
            ),
            "files": "cmd/oscar/main.go,oscar/web/web.go,internal/terminator/terminator.go,internal/healthcheck/reporter.go",
            "entry_points": "main,terminator.Stop,server.Shutdown",
        },
        {
            "name": "cache-backend-failover",
            "purpose": (
                "How the cache layer falls back when the primary Redis backend is unreachable: "
                "1) cmpkg/cache opens connections to both primary and replica via cache.NewClient. "
                "2) On a primary Get error, the client transparently retries against the replica with a shorter timeout. "
                "3) If both fail, the cache miss is recorded and the call site computes the value the slow way. "
                "4) The failure is reported via dchealthchecker so the on-call sees the degradation. "
                "5) A background goroutine (loop opens one connection at a time) re-probes the primary every 5s; "
                "first successful Ping flips the client back to primary-only mode."
            ),
            "files": "cmpkg/cache/client.go,cmpkg/cache/connections.go,internal/healthcheck/reporter.go",
            "entry_points": "cache.NewClient,cache.Get,reporter.Report",
        },
        {
            "name": "consumer-service-registration",
            "purpose": (
                "How a new Kafka consumer service is wired into oscar at startup: "
                "1) Author the consumer in internal/consumers/<name>/, implementing IConsumer.Start(ctx). "
                "2) Register it in internal/consumers/registry.go's switch on consumer name. "
                "3) Add the consumer's topic + group ID to config under consumers:<name>:. "
                "4) Add the consumer name to the enabled list in cmd/oscar/main.go's startup sequence. "
                "5) On boot, startup.RegisterConsumers iterates the enabled list, calls Start, and tracks "
                "the goroutine so graceful-shutdown-sequence can drain it."
            ),
            "files": "internal/consumers/registry.go,internal/consumers/kafka_base.go,cmd/oscar/main.go,config/consumers.yaml",
            "entry_points": "startup.RegisterConsumers,consumer.Start",
        },
        {
            "name": "advertiserblocker-decision-path",
            "purpose": (
                "How a request gets blocked or allowed by the advertiserblocker service: "
                "1) Request hits the ads controller carrying advertiser_id in its body. "
                "2) Controller calls advertiserblocker.Check(ctx, advertiser_id). "
                "3) Check loads the active block list from Redis (TTL-cached for 60s in cache-backend-failover terms). "
                "4) If advertiser_id matches a block rule, the call returns BlockDecision{blocked: true, reason: rule_id}. "
                "5) Controller short-circuits with HTTP 403 + structured reason payload; otherwise falls through to ad serving."
            ),
            "files": "internal/advertiserblocker/check.go,internal/advertiserblocker/rules.go,oscar/web/controllers/ads.go",
            "entry_points": "advertiserblocker.Check,ads.Serve",
        },
    ],
    "mall": [
        {
            "name": "product-detail-cache-flow",
            "purpose": (
                "How a /product/detail/{id} request is served and cached in mall-portal: "
                "1) Request hits PmsPortalProductController.detail(id). "
                "2) Controller calls PmsPortalProductService.detail(id). "
                "3) Service first looks in Redis via redisOps.get('product:detail:' + id). "
                "4) On cache miss, queries PmsProductMapper + related mappers (sku, attribute, ladder), "
                "assembles a PmsPortalProductDetail DTO, and writes it to Redis with a 10-minute TTL "
                "(per the redis-for-product-detail-cache decision). "
                "5) On product update via PmsProductController.update, "
                "the same Redis key is invalidated synchronously before responding."
            ),
            "files": "mall-portal/src/main/java/com/macro/mall/portal/controller/PmsPortalProductController.java,mall-portal/src/main/java/com/macro/mall/portal/service/PmsPortalProductService.java,mall-portal/src/main/java/com/macro/mall/portal/service/impl/PmsPortalProductServiceImpl.java",
            "entry_points": "PmsPortalProductController.detail,PmsPortalProductService.detail",
        },
        {
            "name": "order-creation-pipeline",
            "purpose": (
                "Full order-creation pipeline triggered by POST /order/generateOrder: "
                "1) OmsPortalOrderController.generateOrder receives the cart snapshot + receiver address. "
                "2) Service validates stock via PmsSkuStockService.lockStock under an optimistic-lock retry. "
                "3) Coupon + integration discounts applied via UmsMemberService.calcDiscount. "
                "4) OmsOrder + OmsOrderItem rows persisted via OmsOrderMapper inside a @Transactional boundary. "
                "5) Payment intent created via PaymentGatewayClient; on failure, the transaction rolls back "
                "and the stock lock is released. "
                "6) Successful order publishes OrderCreatedEvent for downstream consumers (notification, fulfillment)."
            ),
            "files": "mall-portal/src/main/java/com/macro/mall/portal/controller/OmsPortalOrderController.java,mall-portal/src/main/java/com/macro/mall/portal/service/OmsPortalOrderService.java,mall-portal/src/main/java/com/macro/mall/portal/service/impl/OmsPortalOrderServiceImpl.java",
            "entry_points": "OmsPortalOrderController.generateOrder,OmsPortalOrderService.generateOrder",
        },
        {
            "name": "admin-login-jwt-flow",
            "purpose": (
                "Admin user login + JWT issuance in mall-admin: "
                "1) UmsAdminController.login receives username + password. "
                "2) UmsAdminService.login looks up the admin via UmsAdminMapper.getAdminByUsername. "
                "3) Password verified with BCrypt against the stored hash. "
                "4) On success, JwtTokenUtil.generateToken builds a token containing username + role claims. "
                "5) UmsAdminLoginLogService records the login in the audit table asynchronously. "
                "6) Response carries {token, tokenHead} which the frontend stores and replays in Authorization header."
            ),
            "files": "mall-admin/src/main/java/com/macro/mall/controller/UmsAdminController.java,mall-admin/src/main/java/com/macro/mall/service/UmsAdminService.java,mall-security/src/main/java/com/macro/mall/security/util/JwtTokenUtil.java",
            "entry_points": "UmsAdminController.login,UmsAdminService.login,JwtTokenUtil.generateToken",
        },
        {
            "name": "es-product-search-flow",
            "purpose": (
                "Full-text product search backed by Elasticsearch in mall-search: "
                "1) EsProductController.search receives keyword + filter facets (brand, category, price range). "
                "2) EsProductService.search builds a NativeSearchQuery with bool filter clauses. "
                "3) ElasticsearchRestTemplate executes the query against the 'product' index. "
                "4) Hits map back to EsProduct documents that the frontend renders. "
                "5) Mutations on PmsProduct (create/update/delete) flow through EsProductService.importAll/delete "
                "so the index stays consistent with MySQL."
            ),
            "files": "mall-search/src/main/java/com/macro/mall/search/controller/EsProductController.java,mall-search/src/main/java/com/macro/mall/search/service/EsProductService.java,mall-search/src/main/java/com/macro/mall/search/repository/EsProductRepository.java",
            "entry_points": "EsProductController.search,EsProductService.search",
        },
        {
            "name": "global-exception-handling",
            "purpose": (
                "How exceptions thrown anywhere in mall surface to the API consumer as structured JSON: "
                "1) Any controller/service throws ApiException (or a subtype) carrying an IErrorCode. "
                "2) GlobalExceptionHandler (@ControllerAdvice) catches ApiException + Spring's MethodArgumentNotValidException. "
                "3) Handler wraps the error in CommonResult.failed(errorCode, msg). "
                "4) Spring serializes CommonResult to JSON via Jackson — DTO-only on the wire, "
                "per the dto-not-entity-on-the-wire decision. "
                "5) Frontend reads result.code to decide between toast (4xx user error) and crash dialog (5xx)."
            ),
            "files": "mall-common/src/main/java/com/macro/mall/common/exception/GlobalExceptionHandler.java,mall-common/src/main/java/com/macro/mall/common/exception/ApiException.java,mall-common/src/main/java/com/macro/mall/common/api/CommonResult.java",
            "entry_points": "GlobalExceptionHandler.handle,CommonResult.failed",
        },
    ],
    "airflow-core": [
        {
            "name": "dag-parse-to-scheduling",
            "purpose": (
                "Lifecycle of a DAG file from filesystem to scheduled task instances: "
                "1) Scheduler boot reads the dag bundle config (no implicit dags_folder scan, per the "
                "no-implicit-dag-discovery decision). "
                "2) DagFileProcessor parses each listed file in a bounded ThreadPoolExecutor (the asyncio-bounded-threadpool shim). "
                "3) Parsed DAGs serialize into the SerializedDagModel table; ImportError rows capture any parse failures. "
                "4) Scheduler loop picks up serialized DAGs, evaluates schedules, and creates DagRun rows. "
                "5) For each DagRun, TaskInstance rows are scheduled honouring upstream dependencies + pool slots. "
                "6) Executor (Local/Celery/Kubernetes) picks up queued TIs and runs them."
            ),
            "files": "src/airflow/dag_processing/processor.py,src/airflow/dag_processing/manager.py,src/airflow/jobs/scheduler_job_runner.py,src/airflow/models/dagrun.py",
            "entry_points": "SchedulerJobRunner._execute,DagFileProcessor.process_file",
        },
        {
            "name": "task-instance-execution-flow",
            "purpose": (
                "What happens between 'TaskInstance is queued' and 'TaskInstance is success': "
                "1) Scheduler marks TI as queued; executor picks it up off its internal queue. "
                "2) Executor fork/exec's a fresh worker process with `airflow tasks run`. "
                "3) Worker imports the DAG, materializes the task's context dict "
                "(all values must be pickleable, per the task-context-must-be-pickleable decision). "
                "4) TaskInstance.run executes the operator's execute() under retry/timeout policy. "
                "5) Operator's return value (XCom push) and final state (success/failed/skipped) are recorded. "
                "6) Trigger rules on downstream tasks fire based on this TI's state."
            ),
            "files": "src/airflow/models/taskinstance.py,src/airflow/executors/base_executor.py,src/airflow/cli/commands/task_command.py",
            "entry_points": "TaskInstance.run,BaseExecutor.execute_async,task_command.task_run",
        },
        {
            "name": "xcom-push-pull-flow",
            "purpose": (
                "How a value flows between tasks via XCom: "
                "1) Upstream task's execute() returns a value (or explicitly calls ti.xcom_push(key, value)). "
                "2) TaskInstance.xcom_push serializes via the configured XCom backend "
                "(default: BaseXCom → JSON-serialized into xcom table; custom backends can override). "
                "3) Downstream task's execute() (or its Jinja templates) calls ti.xcom_pull(task_ids=...). "
                "4) BaseXCom.get_value reads back and deserializes. "
                "5) For TaskFlow API, push/pull is implicit: function return → push, "
                "argument bindings from upstream → pull, all under the hood."
            ),
            "files": "src/airflow/models/xcom.py,src/airflow/models/taskinstance.py,src/airflow/models/baseoperator.py",
            "entry_points": "TaskInstance.xcom_push,TaskInstance.xcom_pull,BaseXCom.get_value",
        },
        {
            "name": "dagrun-trigger-rule-evaluation",
            "purpose": (
                "How a TaskInstance decides whether to run given upstream state: "
                "1) Scheduler evaluates dependencies for each non-terminal TI in a DagRun. "
                "2) For each TI, DepContext gathers all upstream TIs' states. "
                "3) The TI's trigger_rule (default: ALL_SUCCESS, alternatives: ALL_DONE, ONE_SUCCESS, ALL_FAILED, NONE_FAILED, ...) "
                "is evaluated by TriggerRuleDep.get_dep_statuses. "
                "4) Returns a TIDepStatus tuple per dependency; if all pass, TI is promoted to scheduled. "
                "5) If any FAIL_FAST dep is unmet, TI is set to upstream_failed/skipped without waiting."
            ),
            "files": "src/airflow/ti_deps/deps/trigger_rule_dep.py,src/airflow/ti_deps/dep_context.py,src/airflow/models/taskinstance.py",
            "entry_points": "TriggerRuleDep.get_dep_statuses,TaskInstance.are_dependencies_met",
        },
        {
            "name": "webserver-dag-rendering-flow",
            "purpose": (
                "How the webserver renders the DAG graph view for a UI request: "
                "1) HTTP GET /dags/<dag_id>/graph hits airflow.api_fastapi.core_api.routes.public.dag.get_dag_graph. "
                "2) Endpoint loads the SerializedDAG from SerializedDagModel.get (NOT the live Python file — "
                "scheduler is the only component that imports DAG modules). "
                "3) The serialized representation includes tasks, dependencies, edge_info, and visual hints. "
                "4) Response shape is a typed Pydantic model that the React frontend consumes. "
                "5) Recent DagRun states (last 25) are joined in so the UI can colour-code nodes."
            ),
            "files": "src/airflow/api_fastapi/core_api/routes/public/dags.py,src/airflow/models/serialized_dag.py,src/airflow/www/views.py",
            "entry_points": "get_dag_graph,SerializedDagModel.get",
        },
    ],
}


# ---------------------------------------------------------------------------
# MCP plumbing (mirrors seed_decisions.py — kept verbatim so the two
# seeders behave identically and bugs found in one apply to the other)
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
    # See seed_decisions.py for why stderr goes to a log file instead
    # of subprocess.PIPE — short version: DevRouter logs liberally and
    # the OS pipe buffer wedges the child if nothing drains it.
    stderr_log = open("/tmp/seed_flows.devrouter.log", "w")
    proc = subprocess.Popen(
        [str(DEVROUTER_BIN)],
        stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=stderr_log,
        text=True, bufsize=1,
    )
    _send(proc, 1, "initialize", {
        "protocolVersion": "2024-11-05",
        "capabilities": {},
        "clientInfo": {"name": "seed_flows", "version": "0"},
    })
    _notify(proc, "notifications/initialized", {})
    return proc


def call_tool(proc: subprocess.Popen, rid: int, name: str, args: dict) -> dict:
    return _send(proc, rid, "tools/call", {"name": name, "arguments": args})


def seed_repo(proc: subprocess.Popen, repo: str, flows: list[dict], dry_run: bool) -> tuple[int, int]:
    rid = 100
    saved, errors = 0, 0
    for f in flows:
        payload = {
            "repo":    repo,
            "name":    f["name"],
            "purpose": f["purpose"],
            # Pin scope to "global" so DevRouter's SaveFlowMemory skips
            # the per-file `git diff <release-ref> -- <file>` walk in
            # memory.ScopeForFiles (where <release-ref> defaults to
            # `origin/release`, configurable via DEVROUTER_RELEASE_BRANCH).
            # Without this each save shells out to git once per listed
            # file (and once more for fetchRelease), which dominates
            # wall-clock for seed data — turning a 100ms save into a
            # 5-15s one. Synthetic seed corpora are never branch-specific
            # anyway.
            "scope":   "global",
        }
        for opt in ("files", "entry_points"):
            if opt in f and f[opt]:
                payload[opt] = f[opt]
        if "scope" in f and f["scope"]:
            payload["scope"] = f["scope"]  # allow explicit override per entry

        if dry_run:
            files_count = len(f.get("files", "").split(",")) if f.get("files") else 0
            print(f"  [dry-run] memory_save_flow {repo}/{f['name']}  ({files_count} files)")
            continue

        rid += 1
        resp = call_tool(proc, rid, "memory_save_flow", payload)
        if "error" in resp:
            print(f"  ERR {repo}/{f['name']}: {resp['error']}")
            errors += 1
        else:
            saved += 1
            print(f"  saved {repo}/{f['name']}", flush=True)

    return saved, errors


def main() -> int:
    ap = argparse.ArgumentParser(description="Seed DevRouter Redis with sample end-to-end flows per repo.")
    ap.add_argument("--repo", choices=sorted(FLOWS.keys()),
                    help="seed just one repo (default: all)")
    ap.add_argument("--dry-run", action="store_true",
                    help="print what would be sent without spawning devrouter")
    args = ap.parse_args()

    repos = [args.repo] if args.repo else list(FLOWS.keys())

    if args.dry_run:
        for repo in repos:
            print(f"\n=== {repo} ===")
            seed_repo(proc=None, repo=repo, flows=FLOWS[repo], dry_run=True)  # type: ignore[arg-type]
        return 0

    t0 = time.perf_counter()
    proc = open_session()
    total_saved, total_errors = 0, 0
    try:
        for repo in repos:
            print(f"\n=== {repo} ===", flush=True)
            saved, errors = seed_repo(proc, repo, FLOWS[repo], dry_run=False)
            total_saved += saved
            total_errors += errors
            print(f"  {repo}: {saved} saved, {errors} errors", flush=True)
    finally:
        try:
            proc.stdin.close()  # type: ignore[union-attr]
            proc.wait(timeout=5)
        except Exception:
            proc.kill()

    elapsed_ms = (time.perf_counter() - t0) * 1000.0
    print(f"\n[seed] total: {total_saved} saved, {total_errors} errors in {elapsed_ms:.0f} ms", flush=True)
    return 0 if total_errors == 0 else 1


if __name__ == "__main__":
    sys.exit(main())
