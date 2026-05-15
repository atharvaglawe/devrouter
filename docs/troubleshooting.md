# Troubleshooting devrouter

Quick lookup table for the failure modes we've actually hit. Symptoms are
the things you observe; causes are the underlying issue; fixes are the exact
commands.

| Symptom | Likely cause | Fix |
|---|---|---|
| `make status` shows `Codegraph: DOWN` | the per-repo indexer crashed or never started | `tail /tmp/devrouter-codegraph.log` to see why, then `make codegraph` to restart it |
| `dev_context` returns `NO MEMORIES AVAILABLE` for a repo you've used before | the running MCP server is a stale binary from before a recent rebuild — the agent host is still bound to the old stdio handler | `pkill -f '/devrouter$'` and let your agent host respawn it. Verify with `ps -ef \| grep devrouter` that the new PID's start time is post-rebuild. |
| `dev_feedback` errors with `cannot unmarshal …` | client sent `file_paths` as a JSON array | send `file_paths` as a comma-separated string (see [`agent-rules.md`](agent-rules.md)) |
| `make up` fails on `embedder-up` health check | Docker not running, or the model download stalled mid-pull | `docker ps` to confirm Docker daemon is up; `make embedder-logs` to watch the model download; `make embedder-down && make embedder-up` to retry from a clean state |
| Embedder health check times out (10+ min) | flaky HuggingFace CDN dropping the ~440MB model download | the entrypoint's outer retry loop handles transient blips; if it truly stalled, `make embedder-down && make embedder-up` resumes from the partial download in the `embedder-models` docker volume |
| Agent never calls `dev_feedback` | the rule block isn't installed in the agent context file | copy [`agent-rules.md`](agent-rules.md) into `CLAUDE.md` / `.cursor/rules/devrouter.mdc` / `AGENTS.md` |
| Memory count in `make status` stays at 0 | agent isn't following rule 2 (save memories) | confirm the rule block is in place; use `retrieve_debug` to inspect the search trace for one query |
| `dev_context` returns irrelevant memories with high `context_confidence` | running an old binary from before the 4-stage relevance gate landed | rebuild and restart MCP processes (see "stale binary" row above). The current binary derives confidence from real cosine similarity — see [`retrieval-rules.md`](retrieval-rules.md) Section 11. |
| `retrieve_debug` shows `plan.source = "auto"` and retrieval is weak | agent isn't supplying a structured `plan` on `dev_context` — devrouter only auto-anchored the rarest query token | install [`agent-rules.md`](agent-rules.md) in the agent context file (it instructs the agent to always pass a `plan`); inspect the JSON to confirm the agent is now sending `must_terms` etc. |
| Same memory keeps surfacing for a query it doesn't help | the false-positive demotion loop hasn't accumulated enough samples yet | call `dev_feedback` with `additional_files>0` and `file_paths` set to what you actually read — after ~3 such reports the FP centroid kicks in. See [`retrieval-rules.md`](retrieval-rules.md) Section 10. |

## Diagnostic commands

```bash
make status                                         # health snapshot for Redis / embedder / codegraph
redis-cli KEYS 'feedback:trace:*' | wc -l           # are dev_feedback events being recorded?
redis-cli HGETALL "feedback:trace:<query_id>"       # full trace for one specific query
redis-cli KEYS 'mem:fp:*'                           # which memories have accumulated false-positive signal
ps -ef | grep '[d]evrouter'                         # confirm running PIDs vs binary mtime
curl -sf http://localhost:11435/api/health          # is the bundled ONNX embedder healthy?
```

## When things stay broken

If a query still misbehaves after the table above:

1. Run `retrieve_debug` (the MCP tool) for the failing query — it prints
   the stage-by-stage breakdown including the active plan (and its
   `source`: `agent` vs `auto`), candidate counts, and ranking signals.
2. Look for the trace hash in Redis: `redis-cli HGETALL feedback:trace:<query_id>`.
   Older binaries omit `memory_keys` / `memory_recall_count`; their presence
   confirms you're on the post-Phase A/B build.
3. File an issue with the trace contents attached.
