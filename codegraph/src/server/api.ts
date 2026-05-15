/**
 * HTTP API Server (devrouter slim build)
 *
 * REST API consumed by devrouter (Go). Codegraph (forked from gitnexus) shipped many more
 * routes for its React UI and MCP-over-HTTP layer; those have been removed.
 *
 * Routes kept:
 *   GET  /api/heartbeat              SSE liveness ping
 *   GET  /api/info                   server version + launch context
 *   GET  /api/repos                  list registered repos (devrouter)
 *   POST /api/query                  raw read-only Cypher (devrouter)
 *   POST /api/search                 hybrid / bm25 / semantic search (devrouter)
 *   GET  /api/file                   read source file (devrouter)
 *   POST /api/analyze                start analysis job
 *   GET  /api/analyze/:jobId         poll analysis job
 *   GET  /api/analyze/:jobId/progress SSE progress stream
 *   DELETE /api/analyze/:jobId       cancel analysis job
 *
 * Security: binds to 127.0.0.1 by default. CORS allows localhost and RFC 1918
 * private/LAN networks.
 */

import express from 'express';
import cors from 'cors';
import path from 'path';
import fs from 'fs/promises';
import { createRequire } from 'node:module';
import { listRegisteredRepos, getStoragePath } from '../storage/repo-manager.js';
import {
  executeQuery,
  executePrepared,
  closeLbug,
  withLbugDb,
} from '../core/lbug/lbug-adapter.js';
import { isWriteQuery } from '../core/lbug/pool-adapter.js';
import { NODE_TABLES } from '../_shared/index.js';
import { searchFTSFromLbug } from '../core/search/bm25-index.js';
import { hybridSearch } from '../core/search/hybrid-search.js';
// Embedding imports are lazy (dynamic import) to avoid loading onnxruntime-node
// at server startup — crashes on unsupported Node ABI versions (#89)
import { fork } from 'child_process';
import { fileURLToPath, pathToFileURL } from 'url';
import { JobManager } from './analyze-job.js';
import { extractRepoName, getCloneDir, cloneOrPull } from './git-clone.js';

const _require = createRequire(import.meta.url);
const pkg = _require('../../package.json');

/**
 * Determine whether an HTTP Origin header value is allowed by CORS policy.
 *
 * Permitted origins:
 * - No origin (non-browser requests such as curl or server-to-server calls)
 * - http://localhost:<port> — local development
 * - http://127.0.0.1:<port> — loopback alias
 * - RFC 1918 private/LAN networks (any port):
 *     10.0.0.0/8      → 10.x.x.x
 *     172.16.0.0/12   → 172.16.x.x – 172.31.x.x
 *     192.168.0.0/16  → 192.168.x.x
 */
export const isAllowedOrigin = (origin: string | undefined): boolean => {
  if (origin === undefined) return true;

  if (
    origin.startsWith('http://localhost:') ||
    origin === 'http://localhost' ||
    origin.startsWith('http://127.0.0.1:') ||
    origin === 'http://127.0.0.1' ||
    origin.startsWith('http://[::1]:') ||
    origin === 'http://[::1]'
  ) {
    return true;
  }

  let hostname: string;
  let protocol: string;
  try {
    const parsed = new URL(origin);
    hostname = parsed.hostname;
    protocol = parsed.protocol;
  } catch {
    return false;
  }

  if (protocol !== 'http:' && protocol !== 'https:') return false;

  const octets = hostname.split('.').map(Number);
  if (octets.length !== 4 || octets.some((o) => !Number.isInteger(o) || o < 0 || o > 255)) {
    return false;
  }

  const [a, b] = octets;
  if (a === 10) return true;
  if (a === 172 && b >= 16 && b <= 31) return true;
  if (a === 192 && b === 168) return true;
  return false;
};

/**
 * Mount an SSE progress endpoint for a JobManager.
 * Handles: initial state, terminal events, heartbeat, event IDs, client disconnect.
 */
const mountSSEProgress = (app: express.Express, routePath: string, jm: JobManager) => {
  app.get(routePath, (req, res) => {
    const job = jm.getJob(req.params.jobId);
    if (!job) {
      res.status(404).json({ error: 'Job not found' });
      return;
    }

    let eventId = 0;
    res.writeHead(200, {
      'Content-Type': 'text/event-stream',
      'Cache-Control': 'no-cache',
      Connection: 'keep-alive',
      'X-Accel-Buffering': 'no',
    });

    eventId++;
    res.write(`id: ${eventId}\ndata: ${JSON.stringify(job.progress)}\n\n`);

    if (job.status === 'complete' || job.status === 'failed') {
      eventId++;
      res.write(
        `id: ${eventId}\nevent: ${job.status}\ndata: ${JSON.stringify({
          repoName: job.repoName,
          error: job.error,
        })}\n\n`,
      );
      res.end();
      return;
    }

    const heartbeat = setInterval(() => {
      try {
        res.write(':heartbeat\n\n');
      } catch {
        clearInterval(heartbeat);
        unsubscribe();
      }
    }, 30_000);

    const unsubscribe = jm.onProgress(job.id, (progress) => {
      try {
        eventId++;
        if (progress.phase === 'complete' || progress.phase === 'failed') {
          const eventJob = jm.getJob(req.params.jobId);
          res.write(
            `id: ${eventId}\nevent: ${progress.phase}\ndata: ${JSON.stringify({
              repoName: eventJob?.repoName,
              error: eventJob?.error,
            })}\n\n`,
          );
          clearInterval(heartbeat);
          res.end();
          unsubscribe();
        } else {
          res.write(`id: ${eventId}\ndata: ${JSON.stringify(progress)}\n\n`);
        }
      } catch {
        clearInterval(heartbeat);
        unsubscribe();
      }
    });

    req.on('close', () => {
      clearInterval(heartbeat);
      unsubscribe();
    });
  });
};

const requestedRepo = (req: express.Request): string | undefined => {
  const fromQuery = typeof req.query.repo === 'string' ? req.query.repo : undefined;
  if (fromQuery) return fromQuery;
  if (req.body && typeof req.body === 'object' && typeof req.body.repo === 'string') {
    return req.body.repo;
  }
  return undefined;
};

export const createServer = async (port: number, host: string = '127.0.0.1') => {
  const app = express();
  app.disable('x-powered-by');

  app.use(
    cors({
      origin: (origin, callback) => {
        callback(null, isAllowedOrigin(origin));
      },
    }),
  );
  app.use(express.json({ limit: '10mb' }));

  // Chromium Private Network Access support
  app.use((_req, res, next) => {
    res.setHeader('Access-Control-Allow-Private-Network', 'true');
    next();
  });
  app.options('*', (_req, res, next) => {
    next();
  });

  const jobManager = new JobManager();

  // Shared repo lock — prevents concurrent analyze on the same repo path,
  // which would corrupt LadybugDB.
  const activeRepoPaths = new Set<string>();
  const acquireRepoLock = (repoPath: string): string | null => {
    if (activeRepoPaths.has(repoPath)) {
      return `Another job is already active for this repository`;
    }
    activeRepoPaths.add(repoPath);
    return null;
  };
  const releaseRepoLock = (repoPath: string): void => {
    activeRepoPaths.delete(repoPath);
  };

  /** Hold-queue timeout when a request races an in-flight analyze job. */
  const HOLD_QUEUE_TIMEOUT_SECS = 300;

  /**
   * Resolve a repo by name from the global registry, or default to first.
   * Pass `req` to enable early exit if the client disconnects during the wait.
   */
  const resolveRepo = async (repoName?: string, _isRetry = false, req?: any): Promise<any> => {
    const repos = await listRegisteredRepos();
    let found = null;

    const normalizedName = repoName ? path.basename(repoName) : undefined;

    if (normalizedName) {
      found =
        repos.find((r) => r.name === normalizedName) ||
        repos.find((r) => r.name.toLowerCase() === normalizedName.toLowerCase()) ||
        null;
    } else if (repos.length > 0) {
      found = repos[0];
    }

    if (!found && normalizedName) {
      const lower = normalizedName.toLowerCase();

      let clientGone = false;
      req?.on('close', () => {
        clientGone = true;
      });

      for (const job of jobManager.listJobs()) {
        const isMatch =
          job.repoName?.toLowerCase() === lower ||
          (job.repoUrl && path.basename(job.repoUrl).replace('.git', '').toLowerCase() === lower) ||
          (job.repoPath && path.basename(job.repoPath).toLowerCase() === lower);

        if (isMatch && ['queued', 'cloning', 'analyzing'].includes(job.status)) {
          if (process.env.DEBUG) {
            console.log(
              `[debug] resolveRepo waiting for active job ${job.id} (${normalizedName})...`,
            );
          }
          for (let wait = 0; wait < HOLD_QUEUE_TIMEOUT_SECS; wait++) {
            if (clientGone) return null;
            const currentJob = jobManager.getJob(job.id);
            if (!currentJob || currentJob.status === 'failed') break;
            if (currentJob.status === 'complete') {
              const freshRepos = await listRegisteredRepos({ validate: true });
              return freshRepos.find((r) => r.name === normalizedName) || null;
            }
            await new Promise((r) => setTimeout(r, 1000));
          }
          return { __timedOut: true, repoName: normalizedName };
        }
      }
    }

    return found;
  };

  // ── Liveness / metadata ────────────────────────────────────────────

  app.get('/api/heartbeat', (_req, res) => {
    res.set({
      'Content-Type': 'text/event-stream',
      'Cache-Control': 'no-cache',
      Connection: 'keep-alive',
    });
    res.flushHeaders();
    res.write(':ok\n\n');
    const interval = setInterval(() => res.write(':ping\n\n'), 15_000);
    _req.on('close', () => clearInterval(interval));
  });

  app.get('/api/info', (_req, res) => {
    const execPath = process.env.npm_execpath ?? '';
    const argv0 = process.argv[1] ?? '';
    let launchContext: 'npx' | 'global' | 'local';
    if (
      execPath.includes('npx') ||
      argv0.includes('_npx') ||
      process.env.npm_config_prefix?.includes('_npx')
    ) {
      launchContext = 'npx';
    } else if (argv0.includes('node_modules')) {
      launchContext = 'local';
    } else {
      launchContext = 'global';
    }
    res.json({ version: pkg.version, launchContext, nodeVersion: process.version });
  });

  // ── Devrouter-consumed routes ──────────────────────────────────────

  app.get('/api/repos', async (_req, res) => {
    try {
      const repos = await listRegisteredRepos();
      res.json(
        repos.map((r) => ({
          name: r.name,
          path: r.path,
          indexedAt: r.indexedAt,
          lastCommit: r.lastCommit,
          stats: r.stats,
        })),
      );
    } catch (err: any) {
      res.status(500).json({ error: err.message || 'Failed to list repos' });
    }
  });

  app.post('/api/query', async (req, res) => {
    try {
      const cypher = req.body.cypher as string;
      if (!cypher) {
        res.status(400).json({ error: 'Missing "cypher" in request body' });
        return;
      }
      if (isWriteQuery(cypher)) {
        res.status(403).json({ error: 'Write queries are not allowed via the HTTP API' });
        return;
      }
      const entry = await resolveRepo(requestedRepo(req));
      if (!entry) {
        res.status(404).json({ error: 'Repository not found' });
        return;
      }
      const lbugPath = path.join(entry.storagePath, 'lbug');
      const result = await withLbugDb(lbugPath, () => executeQuery(cypher));
      res.json({ result });
    } catch (err: any) {
      res.status(500).json({ error: err.message || 'Query failed' });
    }
  });

  app.post('/api/search', async (req, res) => {
    try {
      const query = (req.body.query ?? '').trim();
      if (!query) {
        res.status(400).json({ error: 'Missing "query" in request body' });
        return;
      }
      const entry = await resolveRepo(requestedRepo(req));
      if (!entry) {
        res.status(404).json({ error: 'Repository not found' });
        return;
      }
      const lbugPath = path.join(entry.storagePath, 'lbug');
      const parsedLimit = Number(req.body.limit ?? 10);
      const limit = Number.isFinite(parsedLimit)
        ? Math.max(1, Math.min(100, Math.trunc(parsedLimit)))
        : 10;
      const mode: string = req.body.mode ?? 'hybrid';
      const enrich: boolean = req.body.enrich !== false;
      // include_source: when true, also returns the matched code body
      // for each result (symbol slice [startLine,endLine] for code
      // labels, or up to MAX_SOURCE_BYTES of the file head for File
      // labels). This brings the response shape to parity with eager-
      // content retrievers like agentmemory so a single round-trip
      // gives the agent everything it needs. Off by default to keep
      // existing callers cheap; opt-in for context-window-aware ones.
      const includeSource: boolean = req.body.include_source === true;
      const MAX_SOURCE_BYTES = 64 * 1024;

      const results = await withLbugDb(lbugPath, async () => {
        let searchResults: any[];

        // Vector-capable modes need the embedder loaded in *this* Node
        // process. The analyze CLI loaded the model in its own process,
        // but the serve daemon is separate and never called
        // initEmbedder(). Previously this caused `mode=hybrid` to
        // silently degrade to BM25-only on any repo whose embeddings
        // were generated by a previous CLI run — the most expensive
        // feature in the product was effectively dead code via the API.
        //
        // We now lazy-load on first vector-touching request. The model
        // (~90 MB, ~25-30 s cold load on macOS arm64 CPU) is cached
        // after the first request; subsequent calls are free. We only
        // pay the cost when the DB actually has embeddings to use —
        // checking CodeEmbedding row count is a 1-2 ms query.
        const ensureEmbedderForVectorSearch = async (): Promise<boolean> => {
          const { isEmbedderReady, initEmbedder } = await import(
            '../core/embeddings/embedder.js'
          );
          if (isEmbedderReady()) return true;
          try {
            const rows = await executeQuery(
              `MATCH (e:CodeEmbedding) RETURN count(e) AS c`,
            );
            const cnt = rows?.[0]?.c ?? 0;
            if (cnt === 0) return false;
          } catch {
            return false; // CodeEmbedding table missing entirely
          }
          try {
            await initEmbedder();
            return true;
          } catch (err) {
            // Don't crash the API on a single failed lazy-load; the
            // caller will fall through to BM25-only.
            console.warn('[api] embedder lazy-load failed:', err);
            return false;
          }
        };

        if (mode === 'semantic') {
          const ready = await ensureEmbedderForVectorSearch();
          if (!ready) return [] as any[];
          const { semanticSearch: semSearch } =
            await import('../core/embeddings/embedding-pipeline.js');
          searchResults = await semSearch(executeQuery, query, limit);
          searchResults = searchResults.map((r: any, i: number) => ({
            ...r,
            score: r.score ?? 1 - (r.distance ?? 0),
            rank: i + 1,
            sources: ['semantic'],
          }));
        } else if (mode === 'bm25') {
          searchResults = await searchFTSFromLbug(query, limit);
          searchResults = searchResults.map((r: any, i: number) => ({
            ...r,
            rank: i + 1,
            sources: ['bm25'],
          }));
        } else {
          const ready = await ensureEmbedderForVectorSearch();
          if (ready) {
            const { semanticSearch: semSearch } =
              await import('../core/embeddings/embedding-pipeline.js');
            searchResults = await hybridSearch(query, limit, executeQuery, semSearch);
          } else {
            searchResults = await searchFTSFromLbug(query, limit);
          }
        }

        // Source-loading helper. Used by both the enrich path and the
        // bare-results path so include_source works regardless of
        // whether the caller also asked for graph enrichment.
        //
        // For symbol-shaped labels (Function/Method/Class/Interface)
        // with valid line ranges, we return ONLY those lines — that's
        // the structural advantage of having a code graph: the agent
        // gets the function body that matched, not the whole file.
        // For File-shaped or missing-range cases, we return up to
        // MAX_SOURCE_BYTES of the file head — the same accounting
        // every "eager content" retriever (agentmemory, etc.) uses.
        const repoRootForSource = path.resolve(entry.path);
        const loadSourceForRow = async (r: any): Promise<string> => {
          if (!includeSource) return '';
          const fp: string = r.filePath || '';
          if (!fp) return '';
          const fullPath = path.resolve(repoRootForSource, fp);
          if (
            !fullPath.startsWith(repoRootForSource + path.sep) &&
            fullPath !== repoRootForSource
          ) {
            return ''; // path traversal — silently drop
          }
          try {
            const buf = await fs.readFile(fullPath);
            const capped = buf.subarray(0, MAX_SOURCE_BYTES).toString('utf-8');
            const sl = Number(r.startLine);
            const el = Number(r.endLine);
            const label: string = r.label || '';
            if (
              Number.isFinite(sl) &&
              sl > 0 &&
              Number.isFinite(el) &&
              el >= sl &&
              ['Function', 'Method', 'Class', 'Interface'].includes(label)
            ) {
              const lines = capped.split('\n');
              if (sl - 1 < lines.length) {
                return lines.slice(sl - 1, el).join('\n');
              }
            }
            return capped;
          } catch {
            return '';
          }
        };

        if (!enrich) {
          if (!includeSource) return searchResults;
          return Promise.all(
            searchResults.slice(0, limit).map(async (r: any) => ({
              ...r,
              source: await loadSourceForRow(r),
            })),
          );
        }

        const validLabel = (label: string): boolean =>
          (NODE_TABLES as readonly string[]).includes(label);

        const enriched = await Promise.all(
          searchResults.slice(0, limit).map(async (r: any) => {
            const nodeId: string = r.nodeId || r.id || '';
            const nodeLabel = nodeId.split(':')[0];
            const enrichment: { connections?: any; cluster?: string; processes?: any[] } = {};

            // Bare-result early-return path. BM25-only hits land here
            // because their result objects don't carry a node id (the
            // FTS layer only knows file paths). We still want to load
            // source for them when include_source is set — these are
            // file-level hits, so the loader returns the file head.
            if (!nodeId || !validLabel(nodeLabel)) {
              const source = await loadSourceForRow(r);
              return { ...r, ...enrichment, ...(source ? { source } : {}) };
            }

            const [connRes, clusterRes, procRes] = await Promise.all([
              executePrepared(
                `
              MATCH (n:${nodeLabel} {id: $nid})
              OPTIONAL MATCH (n)-[r1:CodeRelation]->(dst)
              OPTIONAL MATCH (src)-[r2:CodeRelation]->(n)
              RETURN
                collect(DISTINCT {name: dst.name, type: r1.type, confidence: r1.confidence}) AS outgoing,
                collect(DISTINCT {name: src.name, type: r2.type, confidence: r2.confidence}) AS incoming
              LIMIT 1
            `,
                { nid: nodeId },
              ).catch(() => []),
              executePrepared(
                `
              MATCH (n:${nodeLabel} {id: $nid})
              MATCH (n)-[:CodeRelation {type: 'MEMBER_OF'}]->(c:Community)
              RETURN c.label AS label, c.description AS description
              LIMIT 1
            `,
                { nid: nodeId },
              ).catch(() => []),
              executePrepared(
                `
              MATCH (n:${nodeLabel} {id: $nid})
              MATCH (n)-[rel:CodeRelation {type: 'STEP_IN_PROCESS'}]->(p:Process)
              RETURN p.id AS id, p.label AS label, rel.step AS step, p.stepCount AS stepCount
              ORDER BY rel.step
            `,
                { nid: nodeId },
              ).catch(() => []),
            ]);

            if (connRes.length > 0) {
              const row = connRes[0];
              const outgoing = (Array.isArray(row) ? row[0] : row.outgoing || [])
                .filter((c: any) => c?.name)
                .slice(0, 5);
              const incoming = (Array.isArray(row) ? row[1] : row.incoming || [])
                .filter((c: any) => c?.name)
                .slice(0, 5);
              enrichment.connections = { outgoing, incoming };
            }

            if (clusterRes.length > 0) {
              const row = clusterRes[0];
              enrichment.cluster = Array.isArray(row) ? row[0] : row.label;
            }

            if (procRes.length > 0) {
              enrichment.processes = procRes
                .map((row: any) => ({
                  id: Array.isArray(row) ? row[0] : row.id,
                  label: Array.isArray(row) ? row[1] : row.label,
                  step: Array.isArray(row) ? row[2] : row.step,
                  stepCount: Array.isArray(row) ? row[3] : row.stepCount,
                }))
                .filter((p: any) => p.id && p.label);
            }

            const source = await loadSourceForRow(r);
            return { ...r, ...enrichment, ...(source ? { source } : {}) };
          }),
        );

        return enriched;
      });
      res.json({ results });
    } catch (err: any) {
      res.status(500).json({ error: err.message || 'Search failed' });
    }
  });

  app.get('/api/file', async (req, res) => {
    try {
      const entry = await resolveRepo(requestedRepo(req));
      if (!entry) {
        res.status(404).json({ error: 'Repository not found' });
        return;
      }
      const filePath = req.query.path as string;
      if (!filePath) {
        res.status(400).json({ error: 'Missing path' });
        return;
      }

      const repoRoot = path.resolve(entry.path);
      const fullPath = path.resolve(repoRoot, filePath);
      if (!fullPath.startsWith(repoRoot + path.sep) && fullPath !== repoRoot) {
        res.status(403).json({ error: 'Path traversal denied' });
        return;
      }

      const raw = await fs.readFile(fullPath, 'utf-8');

      const startLine = req.query.startLine !== undefined ? Number(req.query.startLine) : undefined;
      const endLine = req.query.endLine !== undefined ? Number(req.query.endLine) : undefined;

      if (startLine !== undefined && Number.isFinite(startLine)) {
        const lines = raw.split('\n');
        const start = Math.max(0, startLine);
        const end =
          endLine !== undefined && Number.isFinite(endLine)
            ? Math.min(lines.length, endLine + 1)
            : lines.length;
        res.json({
          content: lines.slice(start, end).join('\n'),
          startLine: start,
          endLine: end - 1,
          totalLines: lines.length,
        });
      } else {
        res.json({ content: raw, totalLines: raw.split('\n').length });
      }
    } catch (err: any) {
      if (err.code === 'ENOENT') {
        res.status(404).json({ error: 'File not found' });
      } else {
        res.status(500).json({ error: err.message || 'Failed to read file' });
      }
    }
  });

  // ── Analyze API ────────────────────────────────────────────────────

  app.post('/api/analyze', async (req, res) => {
    try {
      const { url: repoUrl, path: repoLocalPath, force, embeddings } = req.body;

      if (repoUrl !== undefined && typeof repoUrl !== 'string') {
        res.status(400).json({ error: '"url" must be a string' });
        return;
      }
      if (repoLocalPath !== undefined && typeof repoLocalPath !== 'string') {
        res.status(400).json({ error: '"path" must be a string' });
        return;
      }
      if (!repoUrl && !repoLocalPath) {
        res.status(400).json({ error: 'Provide "url" (git URL) or "path" (local path)' });
        return;
      }
      if (repoLocalPath) {
        if (!path.isAbsolute(repoLocalPath)) {
          res.status(400).json({ error: '"path" must be an absolute path' });
          return;
        }
        if (path.normalize(repoLocalPath) !== path.resolve(repoLocalPath)) {
          res.status(400).json({ error: '"path" must not contain traversal sequences' });
          return;
        }
      }

      const job = jobManager.createJob({ repoUrl, repoPath: repoLocalPath });

      if (job.status !== 'queued') {
        res.status(202).json({ jobId: job.id, status: job.status });
        return;
      }

      jobManager.updateJob(job.id, { status: 'cloning' });

      (async () => {
        let targetPath = repoLocalPath;
        try {
          if (repoUrl && !repoLocalPath) {
            const repoName = extractRepoName(repoUrl);
            targetPath = getCloneDir(repoName);

            jobManager.updateJob(job.id, {
              status: 'cloning',
              repoName,
              progress: { phase: 'cloning', percent: 0, message: `Cloning ${repoUrl}...` },
            });

            await cloneOrPull(repoUrl, targetPath, (progress) => {
              jobManager.updateJob(job.id, {
                progress: { phase: progress.phase, percent: 5, message: progress.message },
              });
            });
          }

          if (!targetPath) throw new Error('No target path resolved');

          const analyzeLockKey = getStoragePath(targetPath);
          const lockErr = acquireRepoLock(analyzeLockKey);
          if (lockErr) {
            jobManager.updateJob(job.id, { status: 'failed', error: lockErr });
            return;
          }

          jobManager.updateJob(job.id, { repoPath: targetPath, status: 'analyzing' });

          const MAX_WORKER_RETRIES = 2;
          const callerPath = fileURLToPath(import.meta.url);
          const isDev = callerPath.endsWith('.ts');
          const workerFile = isDev ? 'analyze-worker.ts' : 'analyze-worker.js';
          const workerPath = path.join(path.dirname(callerPath), workerFile);
          const tsxHookArgs: string[] = isDev
            ? ['--import', pathToFileURL(_require.resolve('tsx/esm')).href]
            : [];

          const forkWorker = () => {
            const currentJob = jobManager.getJob(job.id);
            if (!currentJob || currentJob.status === 'complete' || currentJob.status === 'failed')
              return;

            const child = fork(workerPath, [], {
              execArgv: [...tsxHookArgs, '--max-old-space-size=8192'],
              stdio: ['ignore', 'pipe', 'pipe', 'ipc'],
            });

            let stderrChunks = '';
            child.stderr?.on('data', (chunk: Buffer) => {
              stderrChunks += chunk.toString();
              if (stderrChunks.length > 4096) stderrChunks = stderrChunks.slice(-4096);
            });

            child.on('message', (msg: any) => {
              if (msg.type === 'progress') {
                jobManager.updateJob(job.id, {
                  status: 'analyzing',
                  progress: { phase: msg.phase, percent: msg.percent, message: msg.message },
                });
              } else if (msg.type === 'complete') {
                releaseRepoLock(analyzeLockKey);
                // Refresh registry so the new repo is queryable when the SSE
                // complete event reaches the client.
                listRegisteredRepos({ validate: true })
                  .then(() => {
                    jobManager.updateJob(job.id, {
                      status: 'complete',
                      repoName: msg.result.repoName,
                    });
                  })
                  .catch((err) => {
                    console.error('Registry refresh failed after analyze:', err);
                    jobManager.updateJob(job.id, {
                      status: 'failed',
                      error: 'Server failed to reload after analysis. Try again.',
                    });
                  });
              } else if (msg.type === 'error') {
                releaseRepoLock(analyzeLockKey);
                jobManager.updateJob(job.id, {
                  status: 'failed',
                  error: msg.message,
                });
              }
            });

            child.on('error', (err) => {
              releaseRepoLock(analyzeLockKey);
              jobManager.updateJob(job.id, {
                status: 'failed',
                error: `Worker process error: ${err.message}`,
              });
            });

            child.on('exit', (code) => {
              const j = jobManager.getJob(job.id);
              if (!j || j.status === 'complete' || j.status === 'failed') return;

              if (j.retryCount < MAX_WORKER_RETRIES) {
                j.retryCount++;
                const delay = 1000 * Math.pow(2, j.retryCount - 1);
                const lastErr = stderrChunks.trim().split('\n').pop() || '';
                console.warn(
                  `Analyze worker crashed (code ${code}), retry ${j.retryCount}/${MAX_WORKER_RETRIES} in ${delay}ms` +
                    (lastErr ? `: ${lastErr}` : ''),
                );
                jobManager.updateJob(job.id, {
                  status: 'analyzing',
                  progress: {
                    phase: 'retrying',
                    percent: j.progress.percent,
                    message: `Worker crashed, retrying (${j.retryCount}/${MAX_WORKER_RETRIES})...`,
                  },
                });
                stderrChunks = '';
                setTimeout(forkWorker, delay);
              } else {
                releaseRepoLock(analyzeLockKey);
                jobManager.updateJob(job.id, {
                  status: 'failed',
                  error: `Worker crashed ${MAX_WORKER_RETRIES + 1} times (code ${code})${stderrChunks ? ': ' + stderrChunks.trim().split('\n').pop() : ''}`,
                });
              }
            });

            jobManager.registerChild(job.id, child);

            child.send({
              type: 'start',
              repoPath: targetPath,
              options: { force: !!force, embeddings: !!embeddings },
            });
          };

          forkWorker();
        } catch (err: any) {
          if (targetPath) releaseRepoLock(getStoragePath(targetPath));
          jobManager.updateJob(job.id, {
            status: 'failed',
            error: err.message || 'Analysis failed',
          });
        }
      })();

      res.status(202).json({ jobId: job.id, status: job.status });
    } catch (err: any) {
      if (err.message?.includes('already in progress')) {
        res.status(409).json({ error: err.message });
      } else {
        res.status(500).json({ error: err.message || 'Failed to start analysis' });
      }
    }
  });

  app.get('/api/analyze/:jobId', (req, res) => {
    const job = jobManager.getJob(req.params.jobId);
    if (!job) {
      res.status(404).json({ error: 'Job not found' });
      return;
    }
    res.json({
      id: job.id,
      status: job.status,
      repoUrl: job.repoUrl,
      repoPath: job.repoPath,
      repoName: job.repoName,
      progress: job.progress,
      error: job.error,
      startedAt: job.startedAt,
      completedAt: job.completedAt,
    });
  });

  mountSSEProgress(app, '/api/analyze/:jobId/progress', jobManager);

  app.delete('/api/analyze/:jobId', (req, res) => {
    const job = jobManager.getJob(req.params.jobId);
    if (!job) {
      res.status(404).json({ error: 'Job not found' });
      return;
    }
    if (job.status === 'complete' || job.status === 'failed') {
      res.status(400).json({ error: `Job already ${job.status}` });
      return;
    }
    jobManager.cancelJob(req.params.jobId, 'Cancelled by user');
    res.json({ id: job.id, status: 'failed', error: 'Cancelled by user' });
  });

  // Global error handler — catch anything the route handlers miss
  app.use((err: any, _req: express.Request, res: express.Response, _next: express.NextFunction) => {
    console.error('Unhandled error:', err);
    res.status(500).json({ error: 'Internal server error' });
  });

  // Wrap listen in a promise so errors (EADDRINUSE, EACCES, etc.) propagate
  // to the caller instead of crashing with an unhandled 'error' event.
  await new Promise<void>((resolve, reject) => {
    const server = app.listen(port, host, () => {
      const displayHost = host === '::' || host === '0.0.0.0' ? 'localhost' : host;
      console.log(`codegraph server running on http://${displayHost}:${port}`);
      resolve();
    });
    server.on('error', (err) => reject(err));

    const shutdown = async () => {
      console.log('\nShutting down...');
      server.close();
      jobManager.dispose();
      await closeLbug();
      process.exit(0);
    };
    process.once('SIGINT', shutdown);
    process.once('SIGTERM', shutdown);
  });
};
