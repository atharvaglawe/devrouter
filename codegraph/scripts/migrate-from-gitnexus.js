#!/usr/bin/env node
/**
 * One-shot migration: rename legacy `.gitnexus/` paths to `.codegraph/`.
 *
 * Touches:
 *   1. The global directory `~/.gitnexus/`        -> `~/.codegraph/`
 *      (or `$GITNEXUS_HOME` -> `$CODEGRAPH_HOME` if either is set).
 *   2. Every per-repo `<repoPath>/.gitnexus/`     -> `<repoPath>/.codegraph/`
 *      for repos listed in the (already moved) global registry.
 *   3. Each registry entry's `storagePath` field, rewritten in-place to
 *      point at the new directory name.
 *   4. `<repoPath>/.gitignore`, with a `.codegraph` line appended if the
 *      file already had a `.gitnexus` line and lacks the new name.
 *
 * Safety properties:
 *   - Idempotent: re-running after a successful migration is a no-op.
 *   - Conservative: refuses to overwrite an existing `.codegraph/` (would
 *     mean a fresh codegraph index already lives next to a stale one;
 *     user must resolve manually).
 *   - Pre-flight: bails out cleanly if any codegraph server is currently
 *     listening on $CODEGRAPH_URL / $GITNEXUS_URL, since LadybugDB holds
 *     an exclusive file lock that would silently corrupt a half-migrated
 *     directory.
 *   - Always prints the exact moves it performs.
 *
 * Run via:  make codegraph-migrate
 *      or:  node codegraph/scripts/migrate-from-gitnexus.js
 */

import fs from 'fs/promises';
import { existsSync } from 'fs';
import path from 'path';
import os from 'os';
import http from 'http';

const NEW_DIR_NAME = '.codegraph';
const LEGACY_DIR_NAME = '.gitnexus';

async function main() {
  console.log('codegraph migration: gitnexus -> codegraph');
  console.log('==========================================');

  await assertNoServerListening();

  const summary = {
    globalMoved: false,
    perRepoMoved: 0,
    perRepoSkipped: 0,
    perRepoMissing: 0,
    registryRewritten: false,
    gitignoresPatched: 0,
  };

  // 1) Global directory rename
  const newGlobalDir = process.env.CODEGRAPH_HOME ?? path.join(os.homedir(), '.codegraph');
  const legacyGlobalDir = process.env.GITNEXUS_HOME ?? path.join(os.homedir(), '.gitnexus');
  if (newGlobalDir === legacyGlobalDir) {
    // env vars set them equal — unusual, but treat as "already migrated".
    console.log(`  global dir: $CODEGRAPH_HOME == $GITNEXUS_HOME (${newGlobalDir}) — skipping`);
  } else if (existsSync(newGlobalDir) && existsSync(legacyGlobalDir)) {
    console.log(
      `  global dir: both ${legacyGlobalDir} and ${newGlobalDir} exist — refusing to merge`,
    );
    console.log('              resolve manually (delete or back up the stale one), then re-run.');
    process.exitCode = 1;
    return;
  } else if (existsSync(legacyGlobalDir)) {
    await fs.rename(legacyGlobalDir, newGlobalDir);
    console.log(`  global dir: ${legacyGlobalDir} -> ${newGlobalDir}`);
    summary.globalMoved = true;
  } else {
    console.log(`  global dir: ${legacyGlobalDir} not present — nothing to do`);
  }

  // 2) Walk the registry and migrate per-repo .gitnexus dirs
  const registryPath = path.join(newGlobalDir, 'registry.json');
  let entries = [];
  try {
    const raw = await fs.readFile(registryPath, 'utf-8');
    const parsed = JSON.parse(raw);
    entries = Array.isArray(parsed) ? parsed : [];
  } catch (err) {
    if (err.code === 'ENOENT') {
      console.log(`  registry: ${registryPath} not found — no per-repo migration needed`);
    } else {
      console.error(`  registry: failed to read ${registryPath}: ${err.message}`);
      process.exitCode = 1;
      return;
    }
  }

  let registryDirty = false;

  for (const entry of entries) {
    if (!entry || typeof entry.path !== 'string') continue;
    const repoPath = path.resolve(entry.path);
    const repoLegacy = path.join(repoPath, LEGACY_DIR_NAME);
    const repoNew = path.join(repoPath, NEW_DIR_NAME);

    const legacyExists = existsSync(repoLegacy);
    const newExists = existsSync(repoNew);

    if (newExists && !legacyExists) {
      // Already migrated; just make sure the registry entry agrees.
      if (entry.storagePath && entry.storagePath !== repoNew) {
        entry.storagePath = repoNew;
        registryDirty = true;
      }
      summary.perRepoSkipped += 1;
      continue;
    }

    if (newExists && legacyExists) {
      console.log(
        `  repo: ${repoPath} has BOTH ${LEGACY_DIR_NAME}/ and ${NEW_DIR_NAME}/ — skipping ` +
          `(resolve manually)`,
      );
      summary.perRepoSkipped += 1;
      continue;
    }

    if (!legacyExists && !newExists) {
      console.log(`  repo: ${repoPath} has neither directory — registry entry is stale`);
      summary.perRepoMissing += 1;
      continue;
    }

    // legacyExists && !newExists  →  rename
    await fs.rename(repoLegacy, repoNew);
    console.log(`  repo: ${repoPath} ${LEGACY_DIR_NAME}/ -> ${NEW_DIR_NAME}/`);
    summary.perRepoMoved += 1;

    if (entry.storagePath && entry.storagePath !== repoNew) {
      entry.storagePath = repoNew;
      registryDirty = true;
    }

    // Patch .gitignore if it pinned the old name but not the new one
    const gitignorePath = path.join(repoPath, '.gitignore');
    try {
      const giContent = await fs.readFile(gitignorePath, 'utf-8');
      if (giContent.includes(LEGACY_DIR_NAME) && !giContent.includes(NEW_DIR_NAME)) {
        const next = giContent.endsWith('\n')
          ? `${giContent}${NEW_DIR_NAME}\n`
          : `${giContent}\n${NEW_DIR_NAME}\n`;
        await fs.writeFile(gitignorePath, next, 'utf-8');
        summary.gitignoresPatched += 1;
        console.log(`        .gitignore patched (added ${NEW_DIR_NAME})`);
      }
    } catch (err) {
      if (err.code !== 'ENOENT') {
        console.error(`        .gitignore: ${err.message}`);
      }
    }
  }

  if (registryDirty) {
    await fs.writeFile(registryPath, JSON.stringify(entries, null, 2), 'utf-8');
    console.log(`  registry: rewrote storagePath fields in ${registryPath}`);
    summary.registryRewritten = true;
  }

  // 3) Print summary
  console.log('');
  console.log('Summary:');
  console.log(`  global dir moved      : ${summary.globalMoved}`);
  console.log(`  per-repo moved        : ${summary.perRepoMoved}`);
  console.log(`  per-repo already new  : ${summary.perRepoSkipped}`);
  console.log(`  per-repo missing both : ${summary.perRepoMissing}`);
  console.log(`  registry rewritten    : ${summary.registryRewritten}`);
  console.log(`  .gitignore patched    : ${summary.gitignoresPatched}`);
  console.log('');
  if (summary.perRepoMoved > 0 || summary.globalMoved) {
    console.log('Migration complete. You can now restart the codegraph server.');
  } else {
    console.log('Nothing to migrate — already on the new layout.');
  }
}

/**
 * Refuse to migrate while a codegraph (or legacy gitnexus) HTTP server is
 * running. LadybugDB holds an exclusive lock per database file, and a
 * mid-migration rename would leave the running process pointing at a
 * directory that no longer exists.
 */
async function assertNoServerListening() {
  const candidates = [
    process.env.CODEGRAPH_URL,
    process.env.GITNEXUS_URL,
    'http://localhost:4747',
  ].filter(Boolean);

  const seen = new Set();
  for (const url of candidates) {
    if (seen.has(url)) continue;
    seen.add(url);
    const reachable = await pingHeartbeat(url);
    if (reachable) {
      console.error(`A codegraph/gitnexus server is responding at ${url}.`);
      console.error('Stop it first (e.g. `make down`) so the migration can rename files safely.');
      process.exit(1);
    }
  }
}

function pingHeartbeat(baseUrl) {
  return new Promise((resolve) => {
    let parsed;
    try {
      parsed = new URL(baseUrl + '/api/heartbeat');
    } catch {
      return resolve(false);
    }
    const req = http.request(
      {
        hostname: parsed.hostname,
        port: parsed.port || 80,
        path: parsed.pathname,
        method: 'GET',
        timeout: 1000,
      },
      (res) => {
        // Any HTTP response means something is listening.
        res.resume();
        resolve(true);
      },
    );
    req.on('error', () => resolve(false));
    req.on('timeout', () => {
      req.destroy();
      resolve(false);
    });
    req.end();
  });
}

main().catch((err) => {
  console.error('migration failed:', err);
  process.exit(1);
});
