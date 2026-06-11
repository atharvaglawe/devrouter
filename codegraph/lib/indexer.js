'use strict';

// Indexing wrapper: build (or refresh) a repo's `.codegraph` index using the
// MIT engine, then register it so the sidecar and devrouter's crossrepo
// loader can resolve it by name.

const path = require('path');
const cg = require('../dist/index.js');
const registry = require('./registry');

async function indexRepo(repoPath, name) {
  const root = path.resolve(repoPath);
  const repoName = name && name.trim() ? name.trim() : path.basename(root);

  let inst;
  if (cg.isInitialized(root)) {
    inst = await cg.CodeGraph.open(root);
  } else {
    inst = await cg.CodeGraph.init(root);
  }
  try {
    await inst.indexAll({
      onProgress: (p) => {
        if (p && p.phase) {
          process.stdout.write(`\r[codegraph-sidecar] ${p.phase}: ${p.current ?? ''}/${p.total ?? ''}   `);
        }
      },
    });
  } finally {
    inst.close();
  }
  process.stdout.write('\n');

  const entry = registry.upsertEntry({
    name: repoName,
    path: root,
    storagePath: path.dirname(cg.getDatabasePath(root)),
    indexedAt: new Date().toISOString(),
  });
  // eslint-disable-next-line no-console
  console.log(`[codegraph-sidecar] indexed ${repoName} -> ${entry.storagePath}`);
  return entry;
}

module.exports = { indexRepo };
