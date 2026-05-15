/**
 * Repository Manager
 *
 * Manages codegraph index storage in .codegraph/ at repo root.
 * Also maintains a global registry at ~/.codegraph/registry.json
 * so the HTTP server can discover indexed repos from any cwd.
 *
 * Backwards compat: legacy `.gitnexus/` (per-repo) and `~/.gitnexus/`
 * (global) paths are still recognised at read time so any users mid-
 * migration aren't stranded. New writes always go to the new names.
 * Run `codegraph migrate-from-gitnexus` (or the script directly) to
 * relocate existing data.
 */

import fs from 'fs/promises';
import { existsSync } from 'fs';
import path from 'path';
import os from 'os';

export interface RepoMeta {
  repoPath: string;
  lastCommit: string;
  indexedAt: string;
  stats?: {
    files?: number;
    nodes?: number;
    edges?: number;
    communities?: number;
    processes?: number;
    embeddings?: number;
  };
}

export interface IndexedRepo {
  repoPath: string;
  storagePath: string;
  lbugPath: string;
  metaPath: string;
  meta: RepoMeta;
}

/**
 * Shape of an entry in the global registry (~/.codegraph/registry.json)
 */
export interface RegistryEntry {
  name: string;
  path: string;
  storagePath: string;
  indexedAt: string;
  lastCommit: string;
  stats?: RepoMeta['stats'];
}

const STORAGE_DIR = '.codegraph';
const LEGACY_STORAGE_DIR = '.gitnexus';

// ─── Local Storage Helpers ─────────────────────────────────────────────

/**
 * Get the .codegraph storage path for a repository. If a legacy .gitnexus/
 * directory exists at the repo root and the new one does not, returns that
 * path so we can read pre-migration data without forcing a re-analyze.
 */
export const getStoragePath = (repoPath: string): string => {
  const resolved = path.resolve(repoPath);
  const newPath = path.join(resolved, STORAGE_DIR);
  const legacyPath = path.join(resolved, LEGACY_STORAGE_DIR);
  if (existsSync(newPath)) return newPath;
  if (existsSync(legacyPath)) return legacyPath;
  return newPath; // neither exists → return canonical name for fresh writes
};

/**
 * Get paths to key storage files
 */
export const getStoragePaths = (repoPath: string) => {
  const storagePath = getStoragePath(repoPath);
  return {
    storagePath,
    lbugPath: path.join(storagePath, 'lbug'),
    metaPath: path.join(storagePath, 'meta.json'),
  };
};

/**
 * Check whether a KuzuDB index exists in the given storage path.
 * Non-destructive — safe to call from status commands.
 */
export const hasKuzuIndex = async (storagePath: string): Promise<boolean> => {
  try {
    await fs.stat(path.join(storagePath, 'kuzu'));
    return true;
  } catch {
    return false;
  }
};

/**
 * Clean up stale KuzuDB files after migration to LadybugDB.
 *
 * Returns:
 *   found        — true if .codegraph/kuzu existed and was deleted
 *   needsReindex — true if kuzu existed but lbug does not (re-analyze required)
 *
 * Callers own the user-facing messaging; this function only deletes files.
 */
export const cleanupOldKuzuFiles = async (
  storagePath: string,
): Promise<{ found: boolean; needsReindex: boolean }> => {
  const oldPath = path.join(storagePath, 'kuzu');
  const newPath = path.join(storagePath, 'lbug');
  try {
    await fs.stat(oldPath);
    let needsReindex = false;
    try {
      await fs.stat(newPath);
    } catch {
      needsReindex = true;
    }
    for (const suffix of ['', '.wal', '.lock']) {
      try {
        await fs.unlink(oldPath + suffix);
      } catch {}
    }
    try {
      await fs.rm(oldPath, { recursive: true, force: true });
    } catch {}
    return { found: true, needsReindex };
  } catch {
    return { found: false, needsReindex: false };
  }
};

/**
 * Load metadata from an indexed repo
 */
export const loadMeta = async (storagePath: string): Promise<RepoMeta | null> => {
  try {
    const metaPath = path.join(storagePath, 'meta.json');
    const raw = await fs.readFile(metaPath, 'utf-8');
    return JSON.parse(raw) as RepoMeta;
  } catch {
    return null;
  }
};

/**
 * Save metadata to storage
 */
export const saveMeta = async (storagePath: string, meta: RepoMeta): Promise<void> => {
  await fs.mkdir(storagePath, { recursive: true });
  const metaPath = path.join(storagePath, 'meta.json');
  await fs.writeFile(metaPath, JSON.stringify(meta, null, 2), 'utf-8');
};

/**
 * Check if a path has a codegraph index (recognises .codegraph or legacy .gitnexus)
 */
export const hasIndex = async (repoPath: string): Promise<boolean> => {
  const { metaPath } = getStoragePaths(repoPath);
  try {
    await fs.access(metaPath);
    return true;
  } catch {
    return false;
  }
};

/**
 * Load an indexed repo from a path
 */
export const loadRepo = async (repoPath: string): Promise<IndexedRepo | null> => {
  const paths = getStoragePaths(repoPath);
  const meta = await loadMeta(paths.storagePath);
  if (!meta) return null;

  return {
    repoPath: path.resolve(repoPath),
    ...paths,
    meta,
  };
};

/**
 * Find a codegraph (or legacy gitnexus) index by walking up from a starting path
 */
export const findRepo = async (startPath: string): Promise<IndexedRepo | null> => {
  let current = path.resolve(startPath);
  const root = path.parse(current).root;

  while (current !== root) {
    const repo = await loadRepo(current);
    if (repo) return repo;
    current = path.dirname(current);
  }

  return null;
};

/**
 * Add .codegraph (and legacy .gitnexus) to .gitignore if not already present
 */
export const addToGitignore = async (repoPath: string): Promise<void> => {
  const gitignorePath = path.join(repoPath, '.gitignore');
  const lines = [STORAGE_DIR, LEGACY_STORAGE_DIR];

  try {
    let content = await fs.readFile(gitignorePath, 'utf-8');
    let mutated = false;
    for (const line of lines) {
      if (!content.includes(line)) {
        content = content.endsWith('\n') ? `${content}${line}\n` : `${content}\n${line}\n`;
        mutated = true;
      }
    }
    if (mutated) await fs.writeFile(gitignorePath, content, 'utf-8');
  } catch {
    await fs.writeFile(gitignorePath, `${lines.join('\n')}\n`, 'utf-8');
  }
};

// ─── Global Registry (~/.codegraph/registry.json) ──────────────────────

/**
 * Get the path to the global codegraph directory.
 *
 * Resolution order:
 *   1. $CODEGRAPH_HOME (preferred)
 *   2. $GITNEXUS_HOME  (legacy fallback so old env exports keep working)
 *   3. ~/.codegraph/   (canonical default for new installs)
 *   4. ~/.gitnexus/    (only if it exists and the new one doesn't — pre-migration users)
 */
export const getGlobalDir = (): string => {
  if (process.env.CODEGRAPH_HOME) return process.env.CODEGRAPH_HOME;
  if (process.env.GITNEXUS_HOME) return process.env.GITNEXUS_HOME;
  const home = os.homedir();
  const newDir = path.join(home, '.codegraph');
  const legacyDir = path.join(home, '.gitnexus');
  if (existsSync(newDir)) return newDir;
  if (existsSync(legacyDir)) return legacyDir;
  return newDir;
};

/**
 * Get the path to the global registry file
 */
export const getGlobalRegistryPath = (): string => {
  return path.join(getGlobalDir(), 'registry.json');
};

/**
 * Read the global registry. Returns empty array if not found.
 */
export const readRegistry = async (): Promise<RegistryEntry[]> => {
  try {
    const raw = await fs.readFile(getGlobalRegistryPath(), 'utf-8');
    const data = JSON.parse(raw);
    return Array.isArray(data) ? data : [];
  } catch {
    return [];
  }
};

/**
 * Write the global registry to disk
 */
const writeRegistry = async (entries: RegistryEntry[]): Promise<void> => {
  const dir = getGlobalDir();
  await fs.mkdir(dir, { recursive: true });
  await fs.writeFile(getGlobalRegistryPath(), JSON.stringify(entries, null, 2), 'utf-8');
};

/**
 * Register (add or update) a repo in the global registry.
 * Called after `codegraph analyze` completes.
 */
export const registerRepo = async (repoPath: string, meta: RepoMeta): Promise<void> => {
  const resolved = path.resolve(repoPath);
  const name = path.basename(resolved);
  const { storagePath } = getStoragePaths(resolved);

  const entries = await readRegistry();
  const existing = entries.findIndex((e) => {
    const a = path.resolve(e.path);
    const b = resolved;
    return process.platform === 'win32' ? a.toLowerCase() === b.toLowerCase() : a === b;
  });

  const entry: RegistryEntry = {
    name,
    path: resolved,
    storagePath,
    indexedAt: meta.indexedAt,
    lastCommit: meta.lastCommit,
    stats: meta.stats,
  };

  if (existing >= 0) {
    entries[existing] = entry;
  } else {
    entries.push(entry);
  }

  await writeRegistry(entries);
};

/**
 * Remove a repo from the global registry.
 * Called after `codegraph clean`.
 */
export const unregisterRepo = async (repoPath: string): Promise<void> => {
  const resolved = path.resolve(repoPath);
  const entries = await readRegistry();
  const filtered = entries.filter((e) => path.resolve(e.path) !== resolved);
  await writeRegistry(filtered);
};

/**
 * List all registered repos from the global registry.
 * Optionally validates that each entry's storage dir still exists.
 */
export const listRegisteredRepos = async (opts?: {
  validate?: boolean;
}): Promise<RegistryEntry[]> => {
  const entries = await readRegistry();
  if (!opts?.validate) return entries;

  const valid: RegistryEntry[] = [];
  for (const entry of entries) {
    try {
      await fs.access(path.join(entry.storagePath, 'meta.json'));
      valid.push(entry);
    } catch {
      // Index no longer exists — skip
    }
  }

  if (valid.length !== entries.length) {
    await writeRegistry(valid);
  }

  return valid;
};

// ─── Global CLI Config (~/.codegraph/config.json) ────────────────────────

export interface CLIConfig {
  apiKey?: string;
  model?: string;
  baseUrl?: string;
  provider?: 'openai' | 'openrouter' | 'azure' | 'custom' | 'cursor';
  cursorModel?: string;
  /** Azure api-version query param (e.g. '2024-10-21'). Only used when provider is 'azure'. */
  apiVersion?: string;
  /** Set true when the deployment is a reasoning model (o1, o3, o4-mini). Auto-detected for OpenAI; must be set for Azure deployments. */
  isReasoningModel?: boolean;
}

/**
 * Get the path to the global CLI config file
 */
export const getGlobalConfigPath = (): string => {
  return path.join(getGlobalDir(), 'config.json');
};

/**
 * Load CLI config from ~/.codegraph/config.json
 */
export const loadCLIConfig = async (): Promise<CLIConfig> => {
  try {
    const raw = await fs.readFile(getGlobalConfigPath(), 'utf-8');
    return JSON.parse(raw) as CLIConfig;
  } catch {
    return {};
  }
};

/**
 * Save CLI config to ~/.codegraph/config.json
 */
export const saveCLIConfig = async (config: CLIConfig): Promise<void> => {
  const dir = getGlobalDir();
  await fs.mkdir(dir, { recursive: true });
  const configPath = getGlobalConfigPath();
  await fs.writeFile(configPath, JSON.stringify(config, null, 2), 'utf-8');
  if (process.platform !== 'win32') {
    try {
      await fs.chmod(configPath, 0o600);
    } catch {
      /* best-effort */
    }
  }
};
