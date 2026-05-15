/**
 * agentmemory bridge — exposes their SearchIndex / VectorIndex / HybridSearch
 * primitives to the Python benchmark harness over a simple NDJSON-over-stdio
 * protocol.
 *
 * Why a bridge instead of the REST API
 * ------------------------------------
 * agentmemory's REST product (`npx @agentmemory/agentmemory`) depends on the
 * `iii-engine` Docker container. On macOS arm64 that container starts but the
 * agentmemory worker code isn't bundled into the image, so the REST endpoint
 * `/agentmemory/smart-search` never comes up without a separate worker spawn.
 *
 * Their *own* published benchmark — `benchmark/longmemeval-bench.ts`, the
 * 95.2% R@5 number — drives the same primitives this bridge uses (SearchIndex,
 * VectorIndex, HybridSearch). We are running their measurement engine against
 * a code-retrieval haystack instead of a chat-memory haystack. That's apples-
 * to-apples with their headline number, modulo the haystack content.
 *
 * Protocol
 * --------
 * The Python adapter writes one JSON object per line to stdin and reads one
 * JSON object per line from stdout. Commands:
 *
 *   {"cmd":"setup", "mode":"bm25"|"hybrid",
 *    "docs":[{"id":"...","path":"...","text":"..."}]}
 *      -> {"ok":true, "n_docs":N, "ms":...}
 *
 *   {"cmd":"query", "q":"...", "k":10}
 *      -> {"ok":true, "files":[...], "scores":[...], "ms":...}
 *
 *   {"cmd":"shutdown"}
 *      -> {"ok":true} then process exits 0
 *
 * Any unrecognized command or runtime error returns
 *   {"ok":false, "error":"..."}
 * and the bridge keeps running so the harness can recover.
 *
 * Mode notes
 * ----------
 * - bm25:    SearchIndex only. Fast, no embedding model load. Matches
 *            agentmemory's "BM25-only fallback" cell (86.2% R@5 on LongMemEval).
 * - hybrid:  SearchIndex + VectorIndex + Xenova/all-MiniLM-L6-v2 embeddings,
 *            via HybridSearch with their default 0.4/0.6 BM25/vector weights
 *            and graphWeight=0 (we don't have graph data for arbitrary files).
 *            Rerank disabled. Matches their "BM25+Vector" cell (95.2% R@5).
 *
 * Logging goes to stderr only — stdout is the protocol channel and any stray
 * print would corrupt it. Their primitives are quiet by default so this is
 * mostly a defensive precaution.
 */

// The agentmemory source is cloned into ./agentmemory_src by
// bench/scripts/setup_agentmemory.sh. We import its primitives by relative
// path. tsx (the runner) handles .ts resolution; Node ESM resolves these
// imports relative to *this file's URL*, so the layout must be:
//   bench/adapters/agentmemory_bridge.mjs       (this file)
//   bench/adapters/agentmemory_src/src/state/*  (cloned)
import { SearchIndex } from "./agentmemory_src/src/state/search-index.ts";
import { VectorIndex } from "./agentmemory_src/src/state/vector-index.ts";
import { HybridSearch } from "./agentmemory_src/src/state/hybrid-search.ts";
import readline from "node:readline";

// In-memory KV that satisfies the StateKV shape HybridSearch needs for graph
// lookups. We never populate the graph scope, so all GraphRetrieval lookups
// return empty — equivalent to running their pipeline with the graph stream
// off, which is what we want here (we have no entity graph for a generic
// codebase).
class NullKV {
  store = new Map();
  async get(scope, key) {
    const m = this.store.get(scope);
    if (!m || !m.has(key)) throw new Error(`Not found: ${scope}/${key}`);
    return m.get(key);
  }
  async set(scope, key, value) {
    if (!this.store.has(scope)) this.store.set(scope, new Map());
    this.store.get(scope).set(key, value);
  }
  async list(scope) {
    const m = this.store.get(scope);
    return m ? Array.from(m.values()) : [];
  }
  async delete(scope, key) {
    this.store.get(scope)?.delete(key);
  }
}

const state = {
  mode: null,
  bm25: null,
  vector: null,
  embedder: null,
  hybrid: null,
  kv: null,
  // obsId -> repo-relative file path. SearchIndex.search returns obsId and we
  // need to project back to the file that produced it.
  obsToPath: new Map(),
  // sessionId stamping is required by their schema even though we don't use
  // it; one synthetic session per run keeps things tidy.
  sessionId: `bench_${Date.now()}`,
};

function log(msg) {
  process.stderr.write(`[am-bridge] ${msg}\n`);
}

function send(obj) {
  process.stdout.write(JSON.stringify(obj) + "\n");
}

async function loadEmbedder() {
  // Lazy-load to avoid the Xenova model download when running bm25 mode.
  // Their bench uses LocalEmbeddingProvider; we use the same one so the
  // result distribution matches what their LongMemEval numbers come from.
  const { LocalEmbeddingProvider } = await import(
    "./agentmemory_src/src/providers/embedding/local.ts"
  );
  return new LocalEmbeddingProvider();
}

async function handleSetup(cmd) {
  const t0 = Date.now();
  state.mode = cmd.mode === "hybrid" ? "hybrid" : "bm25";
  state.bm25 = new SearchIndex();
  state.obsToPath.clear();

  if (state.mode === "hybrid") {
    state.kv = new NullKV();
    state.vector = new VectorIndex();
    state.embedder = await loadEmbedder();
    state.hybrid = new HybridSearch(
      state.bm25,
      state.vector,
      state.embedder,
      state.kv,
      0.4, // bm25Weight — same defaults as their longmemeval-bench.ts
      0.6, // vectorWeight
      0.0, // graphWeight (no graph data for arbitrary code)
      false, // rerank disabled
    );
  }

  const docs = cmd.docs || [];
  let nDocs = 0;
  // Batch-embed in hybrid mode — one-by-one would be ~7800 model calls and
  // dominate setup time. But "all at once" OOMs Node on real codebases:
  // 7.6K docs × 512 tokens × 384-dim model = tensor mass > 4 GiB heap and
  // SIGKILL. Chunk into BATCH-sized pieces so peak RAM stays bounded
  // regardless of corpus size.
  let embeddings = null;
  if (state.mode === "hybrid") {
    // Truncate the embedding input to 512 chars per their longmemeval-bench
    // (model has a 512-token context; over that gets silently truncated
    // anyway, but trimming up front is faster and matches their setup).
    const texts = docs.map((d) => (d.text || "").slice(0, 512));
    const BATCH = 64;
    embeddings = [];
    const t1 = Date.now();
    for (let off = 0; off < texts.length; off += BATCH) {
      const slice = texts.slice(off, off + BATCH);
      const out = await state.embedder.embedBatch(slice);
      embeddings.push(...out);
      if ((off / BATCH) % 10 === 0) {
        log(
          `embed progress ${embeddings.length}/${texts.length} ` +
            `(${Date.now() - t1} ms)`,
        );
      }
    }
  }

  for (let i = 0; i < docs.length; i++) {
    const d = docs[i];
    const obsId = d.id || `obs_${i}`;
    const obs = {
      id: obsId,
      sessionId: state.sessionId,
      timestamp: new Date().toISOString(),
      type: "code",
      title: d.path || obsId,
      facts: [],
      narrative: d.text || "",
      concepts: [],
      files: d.path ? [d.path] : [],
      importance: 5,
    };
    state.bm25.add(obs);
    if (state.mode === "hybrid" && embeddings) {
      state.vector.add(obsId, state.sessionId, embeddings[i]);
      // HybridSearch.enrichResults reads the observation back from
      // kv.get(KV.observations(sessionId), obsId) and DROPS any result
      // whose KV lookup returns null. Their own longmemeval-bench.ts does
      // the same set. If we skip it the hybrid path silently returns
      // empty results — bm25/vector scores compute fine internally but
      // get filtered out at the enrichResults step.
      await state.kv.set(`mem:obs:${state.sessionId}`, obsId, obs);
    }
    if (d.path) state.obsToPath.set(obsId, d.path);
    nDocs++;
  }

  const ms = Date.now() - t0;
  log(`setup mode=${state.mode} docs=${nDocs} ms=${ms}`);
  send({ ok: true, n_docs: nDocs, ms });
}

async function handleQuery(cmd) {
  const t0 = Date.now();
  const q = String(cmd.q || "");
  const k = Math.max(1, Math.min(200, Number(cmd.k || 10)));
  // Over-request so dedup-by-file can still fill top-K when multiple
  // observations from the same file are returned. With one-file-per-obs
  // ingestion this is mostly defensive.
  const limit = k * 2;

  let results;
  let scores;
  if (state.mode === "hybrid") {
    const r = await state.hybrid.search(q, limit);
    results = r.map((x) => x.observation.id);
    scores = r.map((x) => x.combinedScore);
  } else {
    const r = state.bm25.search(q, limit);
    results = r.map((x) => x.obsId);
    scores = r.map((x) => x.score);
  }

  const seen = new Set();
  const files = [];
  const fileScores = [];
  for (let i = 0; i < results.length; i++) {
    const path = state.obsToPath.get(results[i]);
    if (!path || seen.has(path)) continue;
    seen.add(path);
    files.push(path);
    fileScores.push(scores[i]);
    if (files.length >= k) break;
  }

  const ms = Date.now() - t0;
  send({ ok: true, files, scores: fileScores, ms });
}

async function main() {
  const rl = readline.createInterface({ input: process.stdin });
  for await (const line of rl) {
    const trimmed = line.trim();
    if (!trimmed) continue;
    let cmd;
    try {
      cmd = JSON.parse(trimmed);
    } catch (e) {
      send({ ok: false, error: `bad json: ${e.message}` });
      continue;
    }
    try {
      switch (cmd.cmd) {
        case "setup":
          await handleSetup(cmd);
          break;
        case "query":
          await handleQuery(cmd);
          break;
        case "shutdown":
          send({ ok: true });
          process.exit(0);
        // eslint-disable-next-line no-unreachable
        default:
          send({ ok: false, error: `unknown cmd: ${cmd.cmd}` });
      }
    } catch (e) {
      log(`error: ${e.stack || e.message}`);
      send({ ok: false, error: `${e.message}` });
    }
  }
}

main().catch((e) => {
  log(`fatal: ${e.stack || e.message}`);
  process.exit(1);
});
