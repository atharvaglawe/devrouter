"""DevRouter adapter — the system under test.

Spawns the `devrouter` binary as a child process, speaks JSON-RPC over its
stdio MCP transport, calls the `dev_context` tool, and flattens the rich
DevPrompt response into a ranked file list comparable with the other
adapters' outputs.

DevPrompt → file ranking
------------------------
DevRouter returns more than just files. To produce the canonical "top-K
files" the harness scores against, we walk the DevPrompt fields in the
order DevRouter itself prioritises them (per `internal/prompt/types.go`):

    1. PrimaryContext[].File         — explicit, ranked file memories
    2. CodeSnippets[].File           — ranked code snippets
    3. CallChain.{Upstream,Downstream}[].FilePath
    4. Graph.{Importers,Extends,Methods}[].FilePath
    5. Graph.Siblings[]              — sibling file paths

Dedup by path while preserving the first-seen order. This mirrors how a
real agent would consume the prompt (top-down attention) so the ranking
we score is the ranking the agent would actually use.

Symbols are extracted from `Symbols[]` and `CodeSnippets[]` (paths-with-
spans count as symbol-ish anchors) and reported but not scored.

Plan argument
-------------
We do NOT pass a `plan` from the harness, on purpose. The benchmark is
"given the raw query, how good is the system out of the box?". If we
hand-authored a plan, we'd be benchmarking *the harness author*, not
DevRouter. Real agents will supply plans, but the gain from doing so is a
separate experiment (Phase 3, with two adapter modes: `devrouter` and
`devrouter-with-plan`).
"""

from __future__ import annotations

import json
import os
import subprocess
import time
from typing import Any

from .base import Adapter, AdapterResult, approx_tokens, normalize_path, register


@register
class DevRouterAdapter(Adapter):
    name = "devrouter"

    def __init__(self, binary: str | None = None) -> None:
        # Default: the binary built by `make all` at the repo root. Override
        # via DEVROUTER_BIN env if benchmarking a different build.
        self.binary = binary or os.environ.get(
            "DEVROUTER_BIN",
            os.path.join(os.path.dirname(__file__), "..", "..", "devrouter"),
        )
        self.binary = os.path.abspath(self.binary)
        self._proc: subprocess.Popen | None = None
        self._req_id = 0
        self._repo_root: str = ""
        self._repo: str = ""

    # ------------------------------------------------------------------
    # Lifecycle
    # ------------------------------------------------------------------

    def setup(self, repo: str, repo_root: str) -> None:
        if not os.path.isfile(self.binary):
            raise RuntimeError(
                f"devrouter binary not found at {self.binary}. "
                f"Run `make all` first or set DEVROUTER_BIN."
            )
        self._repo_root = repo_root
        self._repo = repo

        # Pass through the host environment unchanged so the user's existing
        # DEVROUTER_REDIS / DEVROUTER_EMBEDDING_URL config takes effect.
        self._proc = subprocess.Popen(  # noqa: S603 - binary path is operator-controlled
            [self.binary],
            stdin=subprocess.PIPE,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            bufsize=0,
            env=os.environ.copy(),
        )

        self._call("initialize", {
            "protocolVersion": "2024-11-05",
            "capabilities": {},
            "clientInfo": {"name": "devrouter-bench", "version": "0.1"},
        })
        # MCP requires this notification after initialize. It has no response;
        # we just write it and move on.
        self._notify("notifications/initialized", {})

    def teardown(self) -> None:
        if self._proc is None:
            return
        try:
            if self._proc.stdin:
                self._proc.stdin.close()
            self._proc.terminate()
            self._proc.wait(timeout=5)
        except (subprocess.TimeoutExpired, OSError):
            self._proc.kill()
        finally:
            self._proc = None

    # ------------------------------------------------------------------
    # Query
    # ------------------------------------------------------------------

    def query(self, q: str, repo: str, k: int) -> AdapterResult:
        if self._proc is None:
            return AdapterResult(error="adapter not set up")

        start = time.perf_counter()
        try:
            resp = self._call("tools/call", {
                "name": "dev_context",
                "arguments": {"query": q, "repo": repo or self._repo},
            })
        except Exception as e:  # noqa: BLE001 - report any failure verbatim
            return AdapterResult(error=f"dev_context failed: {e}")
        elapsed_ms = (time.perf_counter() - start) * 1000.0

        # MCP wraps the actual payload in {content:[{type:"text", text:"…JSON…"}]}.
        try:
            content = resp["result"]["content"]
            text = content[0]["text"]
            prompt = json.loads(text)
        except (KeyError, IndexError, json.JSONDecodeError, TypeError) as e:
            return AdapterResult(
                error=f"unexpected dev_context response: {e}",
                raw={"resp": resp},
                latency_ms=elapsed_ms,
            )

        files = self._extract_files(prompt, k)
        symbols = self._extract_symbols(prompt)

        # Token cost: the agent would receive the entire serialized
        # DevPrompt JSON as its context block. That's the right thing
        # to bill — symbol lists, code snippets, call chains, memories,
        # graph edges all of it gets injected, not just files. We use
        # the original `text` string from the MCP response (already
        # produced by devrouter) rather than re-serializing so we
        # measure what actually reaches the agent over MCP.
        tokens = approx_tokens(text)

        return AdapterResult(
            files=files,
            symbols=symbols[: max(k * 2, 20)],
            latency_ms=elapsed_ms,
            tokens_returned=tokens,
            raw={
                "intent": prompt.get("intent"),
                "context_confidence": prompt.get("context_confidence"),
                "memory_coverage": prompt.get("memory_coverage"),
            },
        )

    # ------------------------------------------------------------------
    # Internals
    # ------------------------------------------------------------------

    def _extract_files(self, prompt: dict[str, Any], k: int) -> list[str]:
        seen: set[str] = set()
        out: list[str] = []

        def add(p: str | None) -> None:
            if not p:
                return
            norm = normalize_path(p, self._repo_root)
            if not norm or norm in seen:
                return
            seen.add(norm)
            out.append(norm)

        for entry in prompt.get("primary_context") or []:
            add(entry.get("file"))
        for snip in prompt.get("code_snippets") or []:
            add(snip.get("file"))
        chain = prompt.get("call_chain") or {}
        for edge in chain.get("upstream") or []:
            add(edge.get("file"))
        for edge in chain.get("downstream") or []:
            add(edge.get("file"))
        graph = prompt.get("graph") or {}
        for bucket in ("importers", "extends", "methods"):
            for edge in graph.get(bucket) or []:
                add(edge.get("file"))
        for sibling in graph.get("siblings") or []:
            add(sibling)

        return out[:k]

    @staticmethod
    def _extract_symbols(prompt: dict[str, Any]) -> list[str]:
        out: list[str] = []
        seen: set[str] = set()
        for sym in prompt.get("symbols") or []:
            if sym and sym not in seen:
                seen.add(sym)
                out.append(sym)
        for entry in prompt.get("primary_context") or []:
            name = entry.get("name")
            if name and name not in seen:
                seen.add(name)
                out.append(name)
        return out

    # ------------------------------------------------------------------
    # JSON-RPC plumbing
    # ------------------------------------------------------------------

    def _next_id(self) -> int:
        self._req_id += 1
        return self._req_id

    def _call(self, method: str, params: dict[str, Any], timeout: float = 30.0) -> dict[str, Any]:
        if self._proc is None or self._proc.stdin is None or self._proc.stdout is None:
            raise RuntimeError("adapter not set up")
        req_id = self._next_id()
        msg = {"jsonrpc": "2.0", "id": req_id, "method": method, "params": params}
        line = (json.dumps(msg) + "\n").encode("utf-8")
        self._proc.stdin.write(line)
        self._proc.stdin.flush()

        deadline = time.monotonic() + timeout
        while True:
            if time.monotonic() > deadline:
                raise TimeoutError(f"{method} timed out after {timeout}s")
            raw = self._proc.stdout.readline()
            if not raw:
                # Process exited; surface stderr for debugging.
                err = b""
                if self._proc.stderr:
                    try:
                        err = self._proc.stderr.read() or b""
                    except OSError:
                        pass
                raise RuntimeError(
                    f"devrouter exited unexpectedly (rc={self._proc.poll()}): "
                    f"{err.decode(errors='ignore')[:500]}"
                )
            try:
                resp = json.loads(raw)
            except json.JSONDecodeError:
                # devrouter logs to stdout in some build modes; skip non-JSON lines
                continue
            # Skip notifications and unrelated responses (defensive — devrouter
            # currently only writes responses, but MCP allows server-initiated
            # notifications and we don't want a future change to break us).
            if resp.get("id") != req_id:
                continue
            if "error" in resp:
                raise RuntimeError(f"{method} error: {resp['error']}")
            return resp

    def _notify(self, method: str, params: dict[str, Any]) -> None:
        if self._proc is None or self._proc.stdin is None:
            raise RuntimeError("adapter not set up")
        msg = {"jsonrpc": "2.0", "method": method, "params": params}
        line = (json.dumps(msg) + "\n").encode("utf-8")
        self._proc.stdin.write(line)
        self._proc.stdin.flush()
