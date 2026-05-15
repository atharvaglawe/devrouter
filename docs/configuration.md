# devrouter configuration

Every devrouter knob is controlled by an environment variable. The
defaults are sane for a local Redis + bundled ONNX embedder setup;
override only what differs.

## Variable reference

| Variable | Default | Description |
|----------|---------|-------------|
| `DEVROUTER_REDIS` | `localhost:6379` | Redis Stack address (`host:port`). Doesn't have to be local — any reachable Redis Stack (Redis Cloud, in-cluster, shared box) works as long as the **RediSearch** module is loaded. |
| `DEVROUTER_EMBEDDING_URL` | `http://localhost:11435/api/embed` | Embedding endpoint. Must speak the canonical `/api/embed` wire shape (request: `{model, input}`, response: `{embeddings: [[...]]}`). Default points at the bundled Dockerized ONNX embedder (`make embedder-up`). Swap in any compatible service — hosted, in-cluster, or a custom worker — without code changes. |
| `DEVROUTER_EMBEDDING_MODEL` | `nomic-embed-text-v1.5` | Model name passed in the `model` field of every embed request. The bundled embedder ignores this (it serves exactly one model per container), but the value is preserved in log lines for diagnosability. **Must produce 768-dim vectors** — the Redis index is built for that dim and the wrapper rejects mismatches with a clear error. |
| `DEVROUTER_MEMORY_MAX_DISTANCE` | `0.60` | Cosine-distance ceiling for vector recall. Lower = stricter relevance. Distance bands for `nomic-embed-text-v1.5`: `<0.2` paraphrase, `0.2–0.4` same topic, `0.4–0.6` weak topical, `>0.6` incidental. See [`retrieval-rules.md`](retrieval-rules.md) Section 6.1. |
| `DEVROUTER_HEURISTICS_FROZEN` | `false` | When `true`, disables all bandit profile mutations (telemetry still collected). Used during incident recovery and isolated experiments. |
| `DEVROUTER_HEURISTICS_BANDIT` | _(unset)_ | Comma-separated knob names to perturb (`max_trace`, `caller_hops`, …) or `all`. Empty = no perturbation. |
| `CODEGRAPH_URL` | `http://localhost:4747` | Override only if devrouter's per-repo indexer is running on a non-default port or remote host. (`GITNEXUS_URL` is an accepted legacy alias.) |

> **There is no in-process query planner.** Query planning is the MCP
> caller's responsibility — the agent fills in the optional `plan` field
> on `dev_context` with structured retrieval terms. devrouter ships no
> LLM, no Ollama dependency, and no `DEVROUTER_PLANNER*` env vars. See
> [`agent-rules.md`](agent-rules.md) for the schema and worked examples.

## MCP-host config example

Minimal local setup. For Cursor, add this to `.cursor/mcp.json`
(project-level) or `~/.cursor/mcp.json` (global). Same shape works for
Claude Code (`~/.claude/mcp.json`), Codex CLI, and other MCP hosts:

```json
{
  "mcpServers": {
    "devrouter": {
      "command": "/path/to/devrouter",
      "args": []
    }
  }
}
```

That's the whole config for the default path — devrouter dials
`localhost:6379` for Redis and `localhost:11435` for the bundled
embedder out of the box. Add an `env` block only when you need to
override:

```json
"env": {
  "DEVROUTER_REDIS": "my-cluster.cache.redis:6379",
  "DEVROUTER_EMBEDDING_URL": "https://your-embedder/api/embed",
  "DEVROUTER_HEURISTICS_FROZEN": "true"
}
```

devrouter just makes HTTP/Redis calls, so any reachable Redis Stack and
any service speaking the `/api/embed` wire shape will work.
`DEVROUTER_HEURISTICS_FROZEN=true` is useful during an incident so the
bandit doesn't mutate trim/budget knobs underneath you.

## Operational notes

- **Stale binary trap:** after rebuilding `./devrouter`, drop the running
  processes (`pkill -f '/devrouter$'`) so your MCP host respawns the new
  binary. Otherwise the client stays bound to the old stdio handler and
  silently uses old code. See [`troubleshooting.md`](troubleshooting.md)
  for the full diagnosis pattern.
- **Custom embedder shape:** if you build your own embedder and point
  `DEVROUTER_EMBEDDING_URL` at it, it must return JSON of the form
  `{"embeddings": [[<768 floats>]]}` for a
  `POST {"model": "<m>", "input": "<text>"}` request. Status non-200 or
  a vector of any dim other than 768 is rejected with a clear error
  (devrouter would rather fail loud than corrupt the Redis index).
- **Bundled ONNX embedder:** the repo ships one at
  [`embedder/`](../embedder/README.md) — single static-ish Go binary on
  top of the canonical Rust tokenizer + Microsoft ONNX Runtime, no
  Python. Produces canonical `nomic-embed-text-v1.5` vectors. Brought
  up automatically by `make up`; swap in a hosted endpoint instead by
  skipping `make embedder-up` and setting `DEVROUTER_EMBEDDING_URL` to
  whatever you want devrouter to call. Switching embedding models
  requires re-indexing Redis (`make flush-memories` + repopulate) —
  vectors from different model spaces are not comparable.
