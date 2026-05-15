# devrouter MCP tools

Twelve tools, exposed over the MCP protocol on stdio. Grouped by purpose.

For the agent-side rules that govern *when* to call these (mandatory call
order, the `dev_context` → save → `dev_feedback` cycle), see
[`agent-rules.md`](agent-rules.md).

## Read

### `dev_context`
Retrieve structured context for a developer question. Returns
`primary_context` (memories), `symbols`, `call_chain`, `graph`,
`code_snippets`, plus an honest `context_confidence`, `memory_coverage`,
and a top-level `query_id`.

**Input:** `{ "query": "...", "repo": "...", "plan"? }`

`plan` is optional but **strongly recommended**: an object with
`must_terms`, `should_terms`, `exclude_terms`, `phrases`, and
`context_hints` (caps: 2 / 6 / 3 / 3 / 3 respectively). devrouter has
no in-process planner LLM, so the agent owns plan production using the
conversation context it already has. Full schema and worked examples in
[`agent-rules.md`](agent-rules.md). If omitted, devrouter falls back to
auto-anchoring the rarest query token — workable but materially worse
for retrieval quality.

The `query_id` should be passed back to `dev_feedback` once the agent
has acted on the response — this closes the tuning loop.

### `retrieve_debug`
Same retrieval as `dev_context` but rendered as a human-readable trace:
stage-by-stage latencies, the active plan (with `source = "agent" |
"auto"`), ranking signals, candidate counts in/out at each stage. Use
to investigate why a specific memory was/wasn't surfaced.

**Input:** `{ "query": "...", "repo": "...", "plan"? }` (same schema as
`dev_context`)

## Write (memory)

### `memory_save_file`
Save what you learned about a source file.

**Input:** `{ "repo", "path", "purpose", "key_symbols"?, "scope"? }`

### `memory_save_func`
Save what you learned about a function or method.

**Input:** `{ "repo", "name", "file", "purpose", "callers"?, "callees"?, "scope"? }`

### `memory_save_flow`
Save what you learned about an end-to-end flow or integration pattern.

**Input:** `{ "repo", "name", "purpose", "files"?, "entry_points"?, "scope"? }`

### `memory_populate`
One-shot bootstrap: synthesise skeleton memories from the indexed code for
a new repo. Auto-written entries (`source=auto`) are never overwritten by
agent-written ones.

**Input:** `{ "repo", "max_files"?, "max_funcs"?, "max_flows"? }`

## Write (decisions)

### `decision_save`
Record a deliberate architectural / refactor / constraint / tradeoff
decision. Surfaced in future `dev_context` so it isn't forgotten or
contradicted.

**Input:** `{ "repo", "name", "decision_type", "decision", "rationale", "alternatives"?, "constraint"?, "decision_scope"?, "files"?, "scope"? }`

### `decision_list`
List saved decisions, optionally filtered by `decision_type`,
`scope` substring, or `files` overlap.

**Input:** `{ "repo", "decision_type"?, "scope"?, "files"? }`

### `decision_supersede`
Mark an old decision as superseded by a newer one. The old decision is
preserved as lineage, never deleted.

**Input:** `{ "repo", "old_name", "new_name" }`

## Feedback & tuning

### `dev_feedback`
Report retrieval quality after acting on a `dev_context` response. Drives
both the bandit reward (`additional_files`, `revisited_files`,
`prompt_tokens`) and the per-memory false-positive loop (`file_paths`
overlap with the memories that were returned).

**Input:** `{ "query_id"?, "additional_files", "revisited_files"?, "file_paths"?, "success"? }`

If `query_id` is omitted, devrouter falls back to the most recent
`dev_context` call on the same MCP connection (best-effort LRU). Pass it
explicitly when you can.

### `dev_feedback_stats`
Inspect per-intent reward distribution, current heuristic profiles, and
recent profile promotions / rollbacks. Useful for verifying the bandit is
converging.

**Input:** `{}`

### `dev_heuristics_reset`
Roll one (or all) heuristic profiles back to the frozen default snapshot.
Use during incident recovery when the bandit has settled on a regression.

**Input:** `{ "intent"? }` (omit or pass `"all"` to reset every intent)
