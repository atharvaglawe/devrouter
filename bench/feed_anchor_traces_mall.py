"""Synthesize ~50 memory_save events that mimic an agent exploring mall.

Anchorlearn's reward path requires that memory_save events are interleaved
with the dev_context calls they relate to (the recent-observations ring is
process-local). So this driver replays a session as:

    for each (query, files-the-agent-saved):
        dev_context(query)              # populates recent-obs ring
        memory_save_file(file_1)        # credits anchors injected for query
        memory_save_file(file_2)
        ...

50 memory_save_file events spread across ~25 distinct dev_context queries —
realistic ratio for an agent that actually reads what it retrieves.

Important: queries below are NOT identical to the bench questions. They cover
the same domain areas (an agent exploring a billing/cart/auth codebase WOULD
ask about those areas) but with different phrasing, so we're testing the
bandit's generalisation, not its memorisation of the eval set.
"""

import json
import subprocess
import sys
import time
from pathlib import Path
from typing import Optional

REPO = "mall"
DEVROUTER_BIN = Path("/Users/atharva.ag/IdeaProjects/devrouter/devrouter")


# (query, [files agent then saves a memory for])
#
# IMPORTANT: To trigger anchor injection (the path the bandit learns through),
# each query MUST contain BOTH:
#   * a "service-trace verb" — start/listen/register/wire/serve/expose/install/mount
#   * a top-level dir name as a service token — mall-admin/mall-portal/
#     mall-search/mall-security
#
# Without both, injectQueryAnchors() returns no anchors and no Observation is
# persisted, so RewardMemorySave has nothing to credit. This is the same gate
# bench questions hit, and we deliberately match it here so the simulation
# exercises the same surface a real agent on this codebase would exercise
# when asking "how does the X service start?" / "where does X register Y?".
SESSIONS: list[tuple[str, list[str]]] = [
    # --- mall-admin entry-point exploration ---
    ("Where does mall-admin start its HTTP server?", [
        "mall-admin/src/main/java/com/macro/mall/MallAdminApplication.java",
    ]),
    ("Where does mall-admin register its Spring Security filter chain?", [
        "mall-admin/src/main/java/com/macro/mall/config/SecurityConfig.java",
        "mall-admin/src/main/java/com/macro/mall/config/MyMetaObjectHandler.java",
    ]),
    ("Where does mall-admin install the global CORS filter?", [
        "mall-admin/src/main/java/com/macro/mall/config/GlobalCorsConfig.java",
    ]),
    ("Where does mall-admin expose its admin login endpoint?", [
        "mall-admin/src/main/java/com/macro/mall/controller/UmsAdminController.java",
        "mall-admin/src/main/java/com/macro/mall/service/impl/UmsAdminServiceImpl.java",
    ]),
    ("Where does mall-admin expose product SKU stock endpoints?", [
        "mall-admin/src/main/java/com/macro/mall/controller/PmsSkuStockController.java",
        "mall-admin/src/main/java/com/macro/mall/service/impl/PmsSkuStockServiceImpl.java",
    ]),
    ("Where does mall-admin register the order-return-apply controller?", [
        "mall-admin/src/main/java/com/macro/mall/controller/OmsOrderReturnApplyController.java",
        "mall-admin/src/main/java/com/macro/mall/service/impl/OmsOrderReturnApplyServiceImpl.java",
    ]),

    # --- mall-portal entry-point + service exploration ---
    ("Where does mall-portal start its storefront HTTP listener?", [
        "mall-portal/src/main/java/com/macro/mall/portal/MallPortalApplication.java",
    ]),
    ("Where does mall-portal register the order generation flow?", [
        "mall-portal/src/main/java/com/macro/mall/portal/controller/OmsPortalOrderController.java",
        "mall-portal/src/main/java/com/macro/mall/portal/service/impl/OmsPortalOrderServiceImpl.java",
    ]),
    ("Where does mall-portal register the order-cancel TTL queue listener?", [
        "mall-portal/src/main/java/com/macro/mall/portal/component/CancelOrderReceiver.java",
        "mall-portal/src/main/java/com/macro/mall/portal/component/CancelOrderSender.java",
        "mall-portal/src/main/java/com/macro/mall/portal/config/RabbitMqConfig.java",
        "mall-portal/src/main/java/com/macro/mall/portal/domain/QueueEnum.java",
    ]),
    ("Where does mall-portal register Alipay payment endpoints?", [
        "mall-portal/src/main/java/com/macro/mall/portal/controller/AlipayController.java",
        "mall-portal/src/main/java/com/macro/mall/portal/service/impl/AlipayServiceImpl.java",
        "mall-portal/src/main/java/com/macro/mall/portal/config/AlipayConfig.java",
    ]),
    ("Where does mall-portal expose member coupon collection endpoints?", [
        "mall-portal/src/main/java/com/macro/mall/portal/controller/UmsMemberCouponController.java",
        "mall-portal/src/main/java/com/macro/mall/portal/service/impl/UmsMemberCouponServiceImpl.java",
    ]),
    ("Where does mall-portal serve the homepage content endpoint?", [
        "mall-portal/src/main/java/com/macro/mall/portal/controller/HomeController.java",
        "mall-portal/src/main/java/com/macro/mall/portal/service/impl/HomeServiceImpl.java",
    ]),
    ("Where does mall-portal serve the cart endpoints to the storefront?", [
        "mall-portal/src/main/java/com/macro/mall/portal/controller/OmsCartItemController.java",
        "mall-portal/src/main/java/com/macro/mall/portal/service/impl/OmsCartItemServiceImpl.java",
    ]),
    ("Where does mall-portal expose the brand-listing endpoint to customers?", [
        "mall-portal/src/main/java/com/macro/mall/portal/controller/PmsPortalBrandController.java",
        "mall-portal/src/main/java/com/macro/mall/portal/service/impl/PmsPortalBrandServiceImpl.java",
    ]),

    # --- mall-search entry-point exploration ---
    ("Where does mall-search start its Elasticsearch service?", [
        "mall-search/src/main/java/com/macro/mall/search/MallSearchApplication.java",
    ]),
    ("Where does mall-search register its product search controller?", [
        "mall-search/src/main/java/com/macro/mall/search/controller/EsProductController.java",
        "mall-search/src/main/java/com/macro/mall/search/service/impl/EsProductServiceImpl.java",
        "mall-search/src/main/java/com/macro/mall/search/repository/EsProductRepository.java",
    ]),

    # --- mall-security shared-lib exploration ---
    ("Where does mall-security install the JWT authentication filter?", [
        "mall-security/src/main/java/com/macro/mall/security/component/JwtAuthenticationTokenFilter.java",
        "mall-security/src/main/java/com/macro/mall/security/config/CommonSecurityConfig.java",
    ]),
    ("Where does mall-security expose the JWT mint and validate helpers?", [
        "mall-security/src/main/java/com/macro/mall/security/util/JwtTokenUtil.java",
    ]),
    ("Where does mall-security mount the unauthenticated and access-denied handlers?", [
        "mall-security/src/main/java/com/macro/mall/security/component/RestAuthenticationEntryPoint.java",
        "mall-security/src/main/java/com/macro/mall/security/component/RestfulAccessDeniedHandler.java",
    ]),
    ("Where does mall-security register the dynamic role-based access filter?", [
        "mall-security/src/main/java/com/macro/mall/security/component/DynamicSecurityFilter.java",
        "mall-security/src/main/java/com/macro/mall/security/component/DynamicAccessDecisionManager.java",
        "mall-security/src/main/java/com/macro/mall/security/component/DynamicSecurityMetadataSource.java",
    ]),
]

total_saves = sum(len(files) for _, files in SESSIONS)
print(f"[design] {len(SESSIONS)} sessions, {total_saves} memory_save_file events", file=sys.stderr)


# ---------------------------------------------------------------------------
def drive() -> bool:
    proc = subprocess.Popen(
        [str(DEVROUTER_BIN)],
        stdin=subprocess.PIPE,
        stdout=subprocess.PIPE,
        stderr=open("/tmp/devrouter_drive.log", "w"),
        text=True,
        bufsize=1,
    )

    rid = 0

    def send(method: str, params: dict) -> dict:
        nonlocal rid
        rid += 1
        msg = {"jsonrpc": "2.0", "id": rid, "method": method, "params": params}
        proc.stdin.write(json.dumps(msg) + "\n")
        proc.stdin.flush()
        line = proc.stdout.readline()
        return json.loads(line) if line.strip() else {}

    init = send("initialize", {"protocolVersion": "2024-11-05", "capabilities": {}})
    if "error" in init:
        print(f"[drive] init error: {init['error']}", file=sys.stderr)
        return False

    ok_q, ok_s, fail = 0, 0, 0
    t0 = time.time()
    for query, files in SESSIONS:
        # 1. dev_context — populates the recent-obs ring
        resp = send(
            "tools/call",
            {
                "name": "dev_context",
                "arguments": {"repo": REPO, "query": query, "max_tokens": 6000},
            },
        )
        if "error" in resp:
            print(f"[drive] dev_context ERROR for {query!r}: {resp['error']}", file=sys.stderr)
            fail += 1
            continue
        ok_q += 1

        # 2. memory_save_file — each save credits the recent observation
        for path in files:
            r = send(
                "tools/call",
                {
                    "name": "memory_save_file",
                    "arguments": {
                        "repo": REPO,
                        "path": path,
                        "purpose": f"Agent reference for: {query}",
                        "scope": "global",
                    },
                },
            )
            if "error" in r:
                fail += 1
                print(f"[drive]   memory_save FAIL {path}: {r['error']}", file=sys.stderr)
            else:
                ok_s += 1

    elapsed = time.time() - t0
    print(f"[drive] sessions: queries ok={ok_q}, saves ok={ok_s}, fail={fail}, elapsed={elapsed:.2f}s",
          file=sys.stderr)

    proc.stdin.close()
    proc.wait(timeout=5)
    return fail == 0


if __name__ == "__main__":
    sys.exit(0 if drive() else 1)
