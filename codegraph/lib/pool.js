'use strict';

// Per-repo connection pool. Resolves a repo name to its on-disk
// `.codegraph/codegraph.db` via the registry, opens it read-only through the
// engine's DatabaseConnection, and caches the (connection, QueryBuilder) pair.
//
// The MCP engine writes in WAL mode, so multiple read connections never block
// on the indexer. We keep one cached read connection per repo for the life of
// the sidecar; `codegraph index` runs in a separate process.

const cg = require('../dist/index.js');
const registry = require('./registry');

const cache = new Map(); // repoName -> { conn, qb, repoPath, dbPath }

class RepoNotFoundError extends Error {
  constructor(name) {
    super(`codegraph: repo ${JSON.stringify(name)} not found in registry`);
    this.code = 'REPO_NOT_FOUND';
  }
}

class RepoNotIndexedError extends Error {
  constructor(name, dbPath) {
    super(`codegraph: repo ${JSON.stringify(name)} has no index at ${dbPath} (run: codegraph-sidecar index)`);
    this.code = 'REPO_NOT_INDEXED';
  }
}

function open(repoName) {
  const cached = cache.get(repoName);
  if (cached && cached.conn.isOpen()) return cached;

  const entry = registry.resolve(repoName);
  if (!entry) throw new RepoNotFoundError(repoName);

  const dbPath = cg.getDatabasePath(entry.path);
  let conn;
  try {
    conn = cg.DatabaseConnection.open(dbPath);
  } catch (e) {
    throw new RepoNotIndexedError(repoName, dbPath);
  }
  const handle = { conn, qb: new cg.QueryBuilder(conn.getDb()), repoPath: entry.path, dbPath };
  cache.set(repoName, handle);
  return handle;
}

// db returns the raw SqliteDatabase for a repo (for the few raw-SQL queries:
// route manifest, name-hit counts, related-files, file-path search).
function rawDb(repoName) {
  return open(repoName).conn.getDb();
}

function closeAll() {
  for (const { conn } of cache.values()) {
    try { conn.close(); } catch (_) { /* ignore */ }
  }
  cache.clear();
}

module.exports = { open, rawDb, closeAll, RepoNotFoundError, RepoNotIndexedError };
