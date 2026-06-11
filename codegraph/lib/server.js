'use strict';

// Minimal HTTP server that re-exposes the codegraph engine to devrouter's Go
// client on :4747. Dependency-free (Node built-in http) to keep the sidecar
// as light as the engine it wraps.

const http = require('http');
const { URL } = require('url');
const h = require('./handlers');
const pool = require('./pool');

// route table: METHOD + path -> (body) => object
const ROUTES = {
  'GET /api/repos': () => h.repos(),
  'GET /api/file': (body) => h.file(body),
  'POST /api/files': (body) => h.files(body),
  'POST /api/symbols': (body) => h.symbols(body),
  'POST /api/search': (body) => h.search(body),
  'POST /api/graph/callers': (body) => h.callers(body),
  'POST /api/graph/callees': (body) => h.callees(body),
  'POST /api/graph/upstream': (body) => h.upstream(body),
  'POST /api/graph/importers': (body) => h.importers(body),
  'POST /api/graph/importers-by-package': (body) => h.importersByPackage(body),
  'POST /api/graph/extends': (body) => h.extendsRel(body),
  'POST /api/graph/methods': (body) => h.methods(body),
  'POST /api/graph/cross-wire': (body) => h.crossWire(body),
  'POST /api/graph/siblings': (body) => h.siblings(body),
  'POST /api/graph/related-files': (body) => h.relatedFiles(body),
  'POST /api/graph/name-hits': (body) => h.nameHits(body),
  'POST /api/search-by-path': (body) => h.searchByPath(body),
  'POST /api/search-by-name': (body) => h.searchByName(body),
};

function readBody(req) {
  return new Promise((resolve, reject) => {
    const chunks = [];
    let size = 0;
    req.on('data', (c) => {
      size += c.length;
      if (size > 8 * 1024 * 1024) { reject(h.httpError(413, 'body too large')); req.destroy(); return; }
      chunks.push(c);
    });
    req.on('end', () => {
      const raw = Buffer.concat(chunks).toString('utf8');
      if (!raw) return resolve({});
      try { resolve(JSON.parse(raw)); } catch (_) { reject(h.httpError(400, 'invalid JSON body')); }
    });
    req.on('error', reject);
  });
}

function statusForError(err) {
  if (err && typeof err.status === 'number') return err.status;
  if (err && err.code === 'REPO_NOT_FOUND') return 404;
  if (err && err.code === 'REPO_NOT_INDEXED') return 404;
  return 500;
}

function createServer() {
  return http.createServer(async (req, res) => {
    const send = (status, obj) => {
      const payload = JSON.stringify(obj);
      res.writeHead(status, { 'Content-Type': 'application/json', 'Content-Length': Buffer.byteLength(payload) });
      res.end(payload);
    };
    try {
      const u = new URL(req.url, 'http://localhost');
      const key = req.method + ' ' + u.pathname;
      const fn = ROUTES[key];
      if (!fn) return send(404, { error: 'not found' });

      let body;
      if (req.method === 'GET') {
        body = Object.fromEntries(u.searchParams.entries());
      } else {
        body = await readBody(req);
      }
      const out = fn(body);
      send(200, out);
    } catch (err) {
      send(statusForError(err), { error: String(err && err.message ? err.message : err) });
    }
  });
}

function serve(port) {
  const server = createServer();
  server.listen(port, () => {
    // eslint-disable-next-line no-console
    console.log(`[codegraph-sidecar] listening on http://localhost:${port} (engine: @colbymchenry/codegraph)`);
  });
  const shutdown = () => { try { pool.closeAll(); } catch (_) {} server.close(() => process.exit(0)); };
  process.on('SIGINT', shutdown);
  process.on('SIGTERM', shutdown);
  return server;
}

module.exports = { createServer, serve, ROUTES };
