'use strict';

// Repo registry for the devrouter codegraph sidecar.
//
// The MIT @colbymchenry/codegraph engine indexes one project per
// `<repo>/.codegraph/codegraph.db` and keeps NO global registry. devrouter,
// however, is multi-repo: it addresses repos by name and expects a
// `~/.codegraph/registry.json` (the same file internal/crossrepo reads).
// So the sidecar owns that file.
//
// Entry shape (a superset of what internal/crossrepo/registry.go decodes):
//   { name, path, storagePath, indexedAt }
//     name        logical repo name used in API calls / `repo` params
//     path        absolute repo root on disk
//     storagePath absolute path to the repo's `.codegraph` dir (db lives here)
//     indexedAt   ISO timestamp of the last index/sync

const fs = require('fs');
const os = require('os');
const path = require('path');

function homeCandidates() {
  const out = [];
  const env = process.env;
  if (env.CODEGRAPH_HOME && env.CODEGRAPH_HOME.trim()) out.push(env.CODEGRAPH_HOME.trim());
  if (env.GITNEXUS_HOME && env.GITNEXUS_HOME.trim()) out.push(env.GITNEXUS_HOME.trim());
  out.push(path.join(os.homedir(), '.codegraph'));
  out.push(path.join(os.homedir(), '.gitnexus'));
  return out;
}

// registryReadPath returns the first existing registry.json, following the
// same precedence as internal/crossrepo/registry.go. Falls back to the
// canonical default path when none exist.
function registryReadPath() {
  for (const home of homeCandidates()) {
    const p = path.join(home, 'registry.json');
    try {
      if (fs.existsSync(p)) return p;
    } catch (_) { /* ignore */ }
  }
  return path.join(os.homedir(), '.codegraph', 'registry.json');
}

// registryWritePath returns where new entries are written: CODEGRAPH_HOME if
// set, else ~/.codegraph. The directory is created if missing.
function registryWritePath() {
  const env = process.env;
  const home = (env.CODEGRAPH_HOME && env.CODEGRAPH_HOME.trim())
    ? env.CODEGRAPH_HOME.trim()
    : path.join(os.homedir(), '.codegraph');
  fs.mkdirSync(home, { recursive: true });
  return path.join(home, 'registry.json');
}

function loadRegistry() {
  const p = registryReadPath();
  let raw;
  try {
    raw = fs.readFileSync(p, 'utf8');
  } catch (e) {
    if (e && e.code === 'ENOENT') return [];
    throw e;
  }
  let entries;
  try {
    entries = JSON.parse(raw);
  } catch (_) {
    return [];
  }
  if (!Array.isArray(entries)) return [];
  // Drop half-written rows; dedup by name keeping the last (freshest) write.
  const byName = new Map();
  for (const e of entries) {
    if (!e || !e.name || !e.path) continue;
    byName.set(e.name, {
      name: e.name,
      path: e.path,
      storagePath: e.storagePath || path.join(e.path, '.codegraph'),
      indexedAt: e.indexedAt || '',
    });
  }
  return [...byName.values()];
}

function resolve(name) {
  if (!name) return null;
  for (const e of loadRegistry()) {
    if (e.name === name) return e;
  }
  return null;
}

// upsertEntry adds or replaces an entry by name and persists the registry to
// the write path. Returns the written entry.
function upsertEntry(entry) {
  if (!entry || !entry.name || !entry.path) {
    throw new Error('registry: entry needs name and path');
  }
  const normalized = {
    name: entry.name,
    path: entry.path,
    storagePath: entry.storagePath || path.join(entry.path, '.codegraph'),
    indexedAt: entry.indexedAt || new Date().toISOString(),
  };
  const existing = loadRegistry().filter((e) => e.name !== normalized.name);
  existing.push(normalized);
  const out = registryWritePath();
  fs.writeFileSync(out, JSON.stringify(existing, null, 2) + '\n', 'utf8');
  return normalized;
}

module.exports = {
  registryReadPath,
  registryWritePath,
  loadRegistry,
  resolve,
  upsertEntry,
};
