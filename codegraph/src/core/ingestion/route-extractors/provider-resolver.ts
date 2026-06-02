/**
 * Provider-tag resolver.
 *
 * Many internal HTTP wrappers — Go's `httpclient.GetClient("kosmos")`,
 * Spring's `@FeignClient(name="kosmos")`, etc. — defer the *path* to a
 * configuration value loaded at runtime. Statically we only see the
 * provider tag (`"kosmos"`), not the URL. This resolver scans common
 * config-file shapes (YAML / JSON / properties / .env) plus the repo's
 * directory layout and maps a tag to:
 *
 *   - **hosts**: bare hostnames recovered (`kosmos.internal`)
 *   - **urls**:  full base URLs recovered (`https://kosmos.internal/api`)
 *   - **serviceDirs**: directory hints (`services/kosmos`,
 *                       `cmd/kosmos`) that signal "files under this
 *                       prefix belong to provider `kosmos`"
 *
 * Downstream the {@link ClientCall.providerTag}-only calls join via
 * either:
 *   - a {@link RouteRegistration} whose `filePath` lives under one of
 *     the resolved `serviceDirs`, or
 *   - a Route node whose URL host matches a resolved `hosts` entry
 *     (when the indexer ever stamps host on Route nodes).
 *
 * The resolver is intentionally repo-agnostic. We extract candidate
 * tags from the *key path* of a leaf URL value (last 1-2 segments
 * before a `url` / `host` / `baseUrl` field) — so a YAML like
 * `services: { kosmos: { url: "..." } }` and a properties line like
 * `kosmos.url=…` and an env var `KOSMOS_URL=…` all collapse to the
 * same tag `kosmos`. Casing is normalised to lower-case.
 */

import * as path from 'node:path';
import * as fs from 'node:fs/promises';
import { glob } from 'glob';
import yaml from 'js-yaml';

// ─────────────────────────────────────────────────────────────────
// Public types
// ─────────────────────────────────────────────────────────────────

export interface ProviderInfo {
  /** Bare hostnames recovered for this tag (`kosmos.internal`). */
  hosts: Set<string>;
  /** Full base URLs recovered (`https://kosmos.internal/api`). */
  urls: Set<string>;
  /** Directory prefixes that appear to "own" this provider. Paths
   *  are repo-relative, slash-normalised, with no trailing slash. */
  serviceDirs: Set<string>;
  /** Source tags for diagnostics: which file(s) contributed. */
  sourceFiles: Set<string>;
  /** 0..1 confidence in the join. 1.0 when both a config URL *and*
   *  a service-dir hint corroborate the tag. */
  confidence: number;
}

export interface ProviderResolverIndex {
  /** Tag (lower-cased) → resolved info. */
  byTag: Map<string, ProviderInfo>;
  /** Full dotted key path (lower-cased) → URL-shaped string values
   *  harvested from YAML/JSON/properties leaves. Used by the
   *  trivial-getter URL resolver to chase Go alias chains all the
   *  way to literal URL/path values (not just to tags). For example
   *  YAML `origins.cmserving.renderer: "/scrr.php"` lands here as
   *  `"origins.cmserving.renderer" → {"/scrr.php"}`. Multiple
   *  environments (canary/staging/production) for the same key are
   *  unioned. */
  byKeyPath: Map<string, Set<string>>;
}

export const EMPTY_PROVIDER_INDEX: ProviderResolverIndex = Object.freeze({
  byTag: new Map(),
  byKeyPath: new Map(),
});

// ─────────────────────────────────────────────────────────────────
// Tag normalisation + URL helpers
// ─────────────────────────────────────────────────────────────────

/** Stop-words that appear in config key paths but should NOT be
 *  considered tags themselves (e.g. `url`, `host`, `client`). */
const KEY_STOPWORDS: ReadonlySet<string> = new Set([
  'url',
  'urls',
  'host',
  'hosts',
  'hostname',
  'baseurl',
  'base_url',
  'base-url',
  'endpoint',
  'endpoints',
  'address',
  'addresses',
  'server',
  'servers',
  'port',
  'scheme',
  'protocol',
  'config',
  'configs',
  'configuration',
  'client',
  'clients',
  'service',
  'services',
  'feign',
  'http',
  'https',
  'rest',
  'api',
  'apis',
  'name',
]);

/** Field names that, when seen as the leaf key of a string value,
 *  indicate the value is a URL / host. */
const URL_LEAF_KEYS: ReadonlySet<string> = new Set([
  'url',
  'host',
  'hostname',
  'baseurl',
  'base_url',
  'base-url',
  'endpoint',
  'address',
  'server',
  'name',
]);

/** Normalise a tag — lowercase, strip surrounding whitespace, replace
 *  underscores/dashes with empty for the comparison key (so
 *  `kosmos-svc` and `kosmos_svc` collapse). Returns the *display*
 *  tag (lowercased, original separators preserved). */
function normalizeTag(raw: string | null | undefined): string | null {
  if (!raw) return null;
  const t = raw.trim().toLowerCase();
  if (!t) return null;
  if (KEY_STOPWORDS.has(t)) return null;
  // Reject obvious non-tags.
  if (t.includes('/') || t.includes(' ') || t.includes('=')) return null;
  if (t.length < 2 || t.length > 64) return null;
  return t;
}

/** Extract a host from a URL-like string. Returns null when the
 *  input doesn't parse cleanly. */
function extractHost(value: string): string | null {
  const v = value.trim();
  if (!v) return null;
  try {
    // URL parser handles full URLs.
    const u = new URL(v);
    return u.hostname || null;
  } catch {
    // Not a URL, but could still be `host:port` or bare hostname.
    if (v.includes(' ') || v.includes('\n')) return null;
    if (v.startsWith('${') || v.startsWith('$(')) return null;
    if (/^[a-zA-Z0-9._-]+(:\d+)?$/.test(v)) {
      return v.split(':')[0] ?? null;
    }
    return null;
  }
}

/** Recognise that a string *looks* like a URL or host. */
function looksLikeUrlOrHost(value: string): boolean {
  return extractHost(value) !== null;
}

/** Recognise that a string *looks* like an HTTP URL or absolute
 *  path — `/scrr.php`, `/api/v1/things`, `https://x.com/api`. Used
 *  by the `byKeyPath` harvester to keep only values a downstream
 *  call site could actually plug into a Go `url.URL{Path: …}` or
 *  similar — bare hostnames are already handled by the tag index. */
function looksLikeUrlOrPath(value: string): boolean {
  const v = value.trim();
  if (!v) return false;
  if (v.length > 2048) return false;
  if (v.includes(' ') || v.includes('\n') || v.includes('\t')) return false;
  if (v.startsWith('${') || v.startsWith('$(') || v.startsWith('{{')) return false;
  if (/^https?:\/\/[^\s]+/i.test(v)) return true;
  // Absolute path starting with `/` and at least one non-slash
  // character. Excludes `//x` style protocol-relative URLs (rare in
  // server configs and ambiguous), as well as pure `/`.
  if (/^\/[^/\s]/.test(v)) return true;
  return false;
}

// ─────────────────────────────────────────────────────────────────
// Parsers — pure, file-content in / candidate-tags out
// ─────────────────────────────────────────────────────────────────

interface KeyedValue {
  /** Full key path joined with '.', lower-cased. */
  keyPath: string[];
  /** Leaf string value. */
  value: string;
}

/** Recursively walk a parsed object/array and yield every leaf
 *  string with its full key path. */
function* walkLeaves(
  value: unknown,
  pathSoFar: string[] = [],
): Generator<KeyedValue> {
  if (value == null) return;
  if (typeof value === 'string') {
    yield { keyPath: pathSoFar, value };
    return;
  }
  if (typeof value === 'number' || typeof value === 'boolean') {
    return;
  }
  if (Array.isArray(value)) {
    for (let i = 0; i < value.length; i++) {
      yield* walkLeaves(value[i], [...pathSoFar, String(i)]);
    }
    return;
  }
  if (typeof value === 'object') {
    for (const [k, v] of Object.entries(value as Record<string, unknown>)) {
      yield* walkLeaves(v, [...pathSoFar, k]);
    }
  }
}

/** Pick the most likely *tag* from a key path. Strategy:
 *    1. If the leaf key is a URL_LEAF_KEY, use the parent segment.
 *    2. Otherwise scan the path right-to-left for the first non-stopword.
 *    3. If still nothing, give up.
 */
/** Derive a normalised provider tag from a config-key path.
 *
 *  Walks right-to-left, skipping leaf URL-style keys (`url`, `host`,
 *  `endpoint`, …) and stop-words (`config`, `client`, `api`, …) until
 *  it finds a real tag, then lower-cases it.
 *
 *  Examples:
 *    `["kosmos", "url"]`           → `"kosmos"`
 *    `["abtest", "api", "host"]`   → `"abtest"`
 *    `["KOSMOS"]`                  → `"kosmos"`
 *    `["url"]`                     → `null` (stop-word only)
 *
 *  Exported so the Java / Python config-tag extractors can apply
 *  the *same* normalisation as the YAML / properties / env parsers.
 *  This keeps tag identity consistent across the SpEL `@Value` →
 *  YAML and `os.getenv` → `.env` pipelines so the resolver-fold
 *  index lookups join correctly. */
export function tagFromKeyPath(keyPath: string[]): string | null {
  if (keyPath.length === 0) return null;
  const leaf = keyPath[keyPath.length - 1].toLowerCase();
  if (URL_LEAF_KEYS.has(leaf) && keyPath.length >= 2) {
    const parent = normalizeTag(keyPath[keyPath.length - 2]);
    if (parent) return parent;
  }
  for (let i = keyPath.length - 1; i >= 0; i--) {
    const t = normalizeTag(keyPath[i]);
    if (t) return t;
  }
  return null;
}

/** Parse a `*.properties` file into KeyedValue triples. */
function parseProperties(content: string): KeyedValue[] {
  const out: KeyedValue[] = [];
  for (const rawLine of content.split('\n')) {
    const line = rawLine.trim();
    if (!line || line.startsWith('#') || line.startsWith('!')) continue;
    const eq = line.indexOf('=');
    const colon = line.indexOf(':');
    let sep = -1;
    if (eq >= 0 && (colon < 0 || eq < colon)) sep = eq;
    else if (colon >= 0) sep = colon;
    if (sep <= 0) continue;
    const key = line.slice(0, sep).trim();
    const value = line.slice(sep + 1).trim();
    if (!key || !value) continue;
    out.push({ keyPath: key.split('.'), value });
  }
  return out;
}

/** Parse a `.env`-shape file: `KEY=value`, `export KEY=value`. */
function parseEnv(content: string): KeyedValue[] {
  const out: KeyedValue[] = [];
  for (const rawLine of content.split('\n')) {
    let line = rawLine.trim();
    if (!line || line.startsWith('#')) continue;
    if (line.startsWith('export ')) line = line.slice('export '.length).trim();
    const eq = line.indexOf('=');
    if (eq <= 0) continue;
    const key = line.slice(0, eq).trim();
    let value = line.slice(eq + 1).trim();
    if (!key || !value) continue;
    // Strip wrapping quotes.
    if (
      (value.startsWith('"') && value.endsWith('"')) ||
      (value.startsWith("'") && value.endsWith("'"))
    ) {
      value = value.slice(1, -1);
    }
    // Normalise UPPER_SNAKE → lower.snake → split into key path.
    const keyPath = key
      .toLowerCase()
      .split('_')
      .filter((s) => s.length > 0);
    out.push({ keyPath, value });
  }
  return out;
}

/** Parse YAML content with js-yaml. Returns leaves or [] on failure. */
function parseYaml(content: string): KeyedValue[] {
  try {
    const docs = yaml.loadAll(content) as unknown[];
    const out: KeyedValue[] = [];
    for (const doc of docs) {
      for (const leaf of walkLeaves(doc)) out.push(leaf);
    }
    return out;
  } catch {
    return [];
  }
}

/** Parse JSON content. Returns leaves or [] on failure. */
function parseJsonFile(content: string): KeyedValue[] {
  try {
    const doc = JSON.parse(content);
    return [...walkLeaves(doc)];
  } catch {
    return [];
  }
}

/** Pick the right parser by filename + content sniff. Returns
 *  `null` when the file isn't a recognised config shape. */
export function parseConfigFile(filename: string, content: string): KeyedValue[] | null {
  const lower = filename.toLowerCase();
  if (lower.endsWith('.yaml') || lower.endsWith('.yml')) return parseYaml(content);
  if (lower.endsWith('.json')) return parseJsonFile(content);
  if (lower.endsWith('.properties')) return parseProperties(content);
  // .env, .env.local, .env.production, env-file
  const base = path.basename(lower);
  if (base === '.env' || base.startsWith('.env.') || base === 'env' || base === '.env.local') {
    return parseEnv(content);
  }
  return null;
}

// ─────────────────────────────────────────────────────────────────
// Index assembly
// ─────────────────────────────────────────────────────────────────

function ensureInfo(idx: ProviderResolverIndex, tag: string): ProviderInfo {
  let info = idx.byTag.get(tag);
  if (!info) {
    info = {
      hosts: new Set<string>(),
      urls: new Set<string>(),
      serviceDirs: new Set<string>(),
      sourceFiles: new Set<string>(),
      confidence: 0.0,
    };
    idx.byTag.set(tag, info);
  }
  return info;
}

/** Build the dotted lower-cased key for the `byKeyPath` index.
 *  Skips numeric segments (array indices) so a YAML list element's
 *  position doesn't get baked into the lookup key — alias chains
 *  on the Go side never carry array indices. */
function keyPathKey(keyPath: string[]): string | null {
  const parts: string[] = [];
  for (const seg of keyPath) {
    if (!seg) continue;
    if (/^\d+$/.test(seg)) continue;
    parts.push(seg.toLowerCase());
  }
  return parts.length > 0 ? parts.join('.') : null;
}

/** Fold a single config file's leaves into the index. */
export function ingestConfigFile(
  idx: ProviderResolverIndex,
  filename: string,
  leaves: KeyedValue[],
): void {
  for (const leaf of leaves) {
    const value = leaf.value.trim();
    if (!value) continue;

    // Channel 1: tag-grained host/url index (legacy behaviour).
    if (looksLikeUrlOrHost(value)) {
      const tag = tagFromKeyPath(leaf.keyPath);
      if (tag) {
        const info = ensureInfo(idx, tag);
        info.sourceFiles.add(filename);
        const host = extractHost(value);
        if (host) info.hosts.add(host);
        if (value.startsWith('http://') || value.startsWith('https://')) {
          info.urls.add(value);
        }
      }
    }

    // Channel 2: key-path-grained URL/path index. Captures values
    // the tag channel skips — most importantly leaf keys like
    // `renderer`, `path`, `route` whose value is `/scrr.php` or a
    // full URL. Indexed by the full dotted key path so the Go-side
    // alias-chain resolver can join via struct-tag fragments.
    if (looksLikeUrlOrPath(value)) {
      const key = keyPathKey(leaf.keyPath);
      if (key) {
        let bucket = idx.byKeyPath.get(key);
        if (!bucket) {
          bucket = new Set<string>();
          idx.byKeyPath.set(key, bucket);
        }
        bucket.add(value);
      }
    }
  }
}

/** Recognise repo dir-tree hints like `services/<name>/…`,
 *  `cmd/<name>/…`, `apps/<name>/…`, `pkg/<name>/…` and bind them
 *  as service-dir hints to whatever tag matches `<name>`.
 *
 *  Also handles flat monorepo layouts (`<name>/...` directly under
 *  the repo root) — but only for tags that already exist in the
 *  index (i.e. were learned from a config file). This avoids
 *  binding noisy top-level dirs (`docs/`, `scripts/`, `internal/`)
 *  as services. */
export function ingestDirectoryHints(
  idx: ProviderResolverIndex,
  repoRelativePaths: string[],
): void {
  // Prefix conventions in monorepo layouts.
  const patterns: Array<{ prefix: string; depth: number }> = [
    { prefix: 'services/', depth: 1 },
    { prefix: 'cmd/', depth: 1 },
    { prefix: 'apps/', depth: 1 },
    { prefix: 'app/', depth: 1 },
    { prefix: 'pkg/', depth: 1 },
    { prefix: 'modules/', depth: 1 },
    { prefix: 'projects/', depth: 1 },
  ];
  const seen = new Map<string, Set<string>>();

  // Tags already known from config files. We use these as a
  // whitelist when binding bare top-level dirs so `internal/`,
  // `lib/`, `vendor/` aren't accidentally treated as providers.
  const knownTags = new Set(idx.byTag.keys());

  for (const file of repoRelativePaths) {
    const norm = file.replace(/\\/g, '/');

    // Prefix patterns.
    let matched = false;
    for (const { prefix, depth } of patterns) {
      if (!norm.startsWith(prefix)) continue;
      const rest = norm.slice(prefix.length);
      const segments = rest.split('/');
      if (segments.length <= depth) continue;
      const name = segments[depth - 1];
      const tag = normalizeTag(name);
      if (!tag) continue;
      const dir = prefix + name;
      let bucket = seen.get(tag);
      if (!bucket) {
        bucket = new Set<string>();
        seen.set(tag, bucket);
      }
      bucket.add(dir);
      matched = true;
    }

    // Flat layout fallback: bare `<name>/...` at the repo root.
    // Bind only when `name` is a tag we already know about (from
    // config). This is what unblocks flat monorepos like goserving
    // (`kosmos/web/...`, `oscar/app/...`) where the YAML key
    // `abtestapi` resolves to host `kosmos-…` and we want to point
    // the FETCHES edge at routes under `kosmos/`.
    if (!matched) {
      const segments = norm.split('/');
      if (segments.length >= 2 && segments[0].length > 0) {
        const name = segments[0];
        const tag = normalizeTag(name);
        if (tag && knownTags.has(tag)) {
          let bucket = seen.get(tag);
          if (!bucket) {
            bucket = new Set<string>();
            seen.set(tag, bucket);
          }
          bucket.add(name);
        }
      }
    }
  }
  for (const [tag, dirs] of seen) {
    const info = ensureInfo(idx, tag);
    for (const d of dirs) info.serviceDirs.add(d);
  }
}

/** Cross-link tags to top-level repo dirs via host substrings.
 *
 *  Many monorepos have config keys whose name doesn't match the
 *  hosting service's directory. The canonical example from goserving
 *  is `abtestapi:` whose `host: "kosmos-neg.goapps.svc.cluster.local"`
 *  points at the `kosmos/` service dir — without this step the
 *  resolver knows the host but can't bind FETCHES to a Route under
 *  `kosmos/web/routes/...`.
 *
 *  Strategy: for every tag with a known host, if any top-level dir
 *  name (≥3 chars) appears as a hostname segment, attach that dir
 *  as a serviceDir on the tag. We require the dir to actually exist
 *  in the file list so we don't bind imaginary services. */
export function crossLinkDirsByHost(
  idx: ProviderResolverIndex,
  repoRelativePaths: string[],
): void {
  // Distinct top-level dir names with at least one file under them.
  const topDirs = new Set<string>();
  for (const file of repoRelativePaths) {
    const slash = file.indexOf('/');
    if (slash < 0) continue;
    const name = file.slice(0, slash);
    if (name.length >= 3 && /^[a-zA-Z][a-zA-Z0-9_-]*$/.test(name)) {
      topDirs.add(name.toLowerCase());
    }
  }
  if (topDirs.size === 0) return;

  for (const info of idx.byTag.values()) {
    if (info.hosts.size === 0) continue;
    for (const host of info.hosts) {
      // Hostname is dot- or dash-separated; check each fragment.
      const fragments = host.toLowerCase().split(/[.\-_]/);
      for (const frag of fragments) {
        if (frag.length < 3) continue;
        if (topDirs.has(frag)) {
          info.serviceDirs.add(frag);
        }
      }
    }
  }
}

/** Once all config + dir hints have been folded in, score each
 *  entry: 0.5 for config-only, 0.5 for dir-only, 1.0 for both. */
export function finalizeIndex(idx: ProviderResolverIndex): ProviderResolverIndex {
  for (const info of idx.byTag.values()) {
    let score = 0;
    if (info.hosts.size > 0 || info.urls.size > 0) score += 0.5;
    if (info.serviceDirs.size > 0) score += 0.5;
    info.confidence = score;
  }
  return idx;
}

/** Lookup helper. Returns `null` when the tag is unknown or has
 *  zero corroborating signal. */
export function resolveTag(
  tag: string,
  idx: ProviderResolverIndex,
): ProviderInfo | null {
  const norm = normalizeTag(tag);
  if (!norm) return null;
  const info = idx.byTag.get(norm);
  if (!info) return null;
  if (info.confidence === 0) return null;
  return info;
}

// ─────────────────────────────────────────────────────────────────
// Filesystem entry point
// ─────────────────────────────────────────────────────────────────

const CONFIG_GLOB = [
  '**/*.yaml',
  '**/*.yml',
  '**/application*.properties',
  '**/*.env',
  '**/.env',
  '**/.env.*',
  '**/services.json',
  '**/consul.json',
  '**/clients.json',
];

const SCAN_IGNORE = [
  '**/node_modules/**',
  '**/.git/**',
  '**/vendor/**',
  '**/dist/**',
  '**/build/**',
  '**/target/**',
  '**/.next/**',
  '**/.venv/**',
  '**/__pycache__/**',
];

/** Scan a repo root for config files and assemble a
 *  {@link ProviderResolverIndex}. Optional file-path list lets
 *  callers reuse an already-walked tree (production path) instead
 *  of re-globbing. */
export async function scanProviderConfig(
  repoPath: string,
  options: { allRepoFiles?: string[] } = {},
): Promise<ProviderResolverIndex> {
  const idx: ProviderResolverIndex = { byTag: new Map(), byKeyPath: new Map() };

  // Pull the config-file subset. follow:true is gated on the same
  // CODEGRAPH_FOLLOW_SYMLINKS opt-in the file walker uses, so a
  // mega-root that symlinks multiple repos can discover each repo's
  // config-driven provider tags (services.cmadserving.url etc.).
  const followSymlinks = process.env.CODEGRAPH_FOLLOW_SYMLINKS === '1';
  const configFiles = await glob(CONFIG_GLOB, {
    cwd: repoPath,
    nodir: true,
    ignore: SCAN_IGNORE,
    absolute: false,
    follow: followSymlinks,
  });

  for (const rel of configFiles) {
    let content: string;
    try {
      content = await fs.readFile(path.join(repoPath, rel), 'utf-8');
    } catch {
      continue;
    }
    if (content.length > 1024 * 1024) continue; // skip > 1 MB
    const leaves = parseConfigFile(rel, content);
    if (!leaves) continue;
    ingestConfigFile(idx, rel, leaves);
  }

  // Dir-tree hints: prefer caller-supplied list (already walked
  // for tree-sitter parsing) — falls back to a fast glob.
  let allFiles = options.allRepoFiles;
  if (!allFiles) {
    allFiles = await glob('**/*', {
      cwd: repoPath,
      nodir: false,
      ignore: SCAN_IGNORE,
      absolute: false,
      follow: followSymlinks,
    });
  }
  ingestDirectoryHints(idx, allFiles);
  // Cross-link tags to repo dirs via shared host substrings — covers
  // the common pattern where a YAML key (`abtestapi`) and its host
  // (`kosmos-…`) point at different services.
  crossLinkDirsByHost(idx, allFiles);

  return finalizeIndex(idx);
}
