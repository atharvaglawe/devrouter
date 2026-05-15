#!/usr/bin/env node
/**
 * Build script for codegraph (devrouter's slim graph-engine, forked from
 * gitnexus). The upstream gitnexus-shared package is folded in under
 * src/_shared/, so the multi-package coordination dance the original
 * upstream build did (compile shared, copy into dist/_shared, rewrite
 * bare specifiers) is no longer needed.
 *
 * Steps:
 *  1. tsc
 *  2. chmod +x dist/cli/index.js so the bin entry is runnable
 */
import { execSync } from 'node:child_process';
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const ROOT = path.resolve(__dirname, '..');
const DIST = path.join(ROOT, 'dist');

console.log('[build] compiling codegraph…');
execSync('npx tsc', { cwd: ROOT, stdio: 'inherit' });

const cliEntry = path.join(DIST, 'cli', 'index.js');
if (fs.existsSync(cliEntry)) fs.chmodSync(cliEntry, 0o755);

console.log('[build] done.');
