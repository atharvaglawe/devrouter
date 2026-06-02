/**
 * Apache `mod_rewrite` mini-parser for plain-PHP route discovery.
 *
 * Why this exists
 * ───────────────
 * Plain-PHP repos (no Laravel / Symfony / Slim) have no framework
 * registration calls to scan. Their public URL contract lives in
 * `.htaccess` `RewriteRule` directives. Treating `.htaccess` as the
 * authoritative route registry gives us file-accurate Route nodes
 * (so cross-repo `FETCHES → Route` joins work) with zero false
 * positives from random `.php` files that aren't endpoints.
 *
 * Scope
 * ─────
 * We only handle the narrow `RewriteCond` + `RewriteRule` shape used
 * by Apache for URL-to-file routing:
 *
 *     RewriteCond %{REQUEST_URI}?%{QUERY_STRING} ^/trf\?(.*)
 *     RewriteRule ^(.*)$ /transfer.php?%1  [UnsafeAllow3F,BCTLS,L]
 *
 * Out of scope: external redirects (`[R]` to absolute URLs), static
 * asset rewrites (target isn't `.php`), nested `.htaccess` files,
 * `mod_alias` directives.
 */

import { promises as fs } from 'node:fs';
import path from 'node:path';

/** A single, parsed rewrite rule treated as a route registration. */
export interface HtaccessRoute {
  /** Cleaned URL pattern from the most recent `RewriteCond`
   *  (or the rule's own LHS when no cond was given). Anchors (`^`,
   *  `$`), backslash escapes, and trailing capture groups have been
   *  stripped so it reads as a path template (`/trf`, `/jsonAds`). */
  urlPattern: string;
  /** Repo-relative path of the `.php` file this rule rewrites to. */
  targetFile: string;
  /** Repo-relative path of the `.htaccess` file the rule came from
   *  (for diagnostics — never used in graph edges). */
  htaccessFile: string;
  /** 0-indexed line number of the `RewriteRule` directive. */
  ruleLine: number;
  /** Parsed `[FLAG1,FLAG2]` set, lower-cased and untrimmed of `?`
   *  prefixes for downstream confidence weighting. */
  flags: ReadonlySet<string>;
}

/** Map: repo-relative `.php` file → every rewrite rule pointing at it.
 *  A handler may legitimately serve multiple URLs (the legacy
 *  cmadserving codebase has `transfer.php` reachable via `/trf` and
 *  also via `/transfer.php`-as-literal), hence the array value. */
export type HtaccessIndex = Map<string, HtaccessRoute[]>;

/* ─────────────────────────────────────────────────────────────────
 * Pure parsing — no I/O
 * ────────────────────────────────────────────────────────────── */

/** Match a `RewriteCond` line, capturing the variable expansion and
 *  the pattern. Both `%{REQUEST_URI}` and `%{REQUEST_URI}?%{QUERY_STRING}`
 *  appear in real cmadserving files. */
const COND_RE = /^\s*RewriteCond\s+(%\{[^}]+\}(?:\?%\{[^}]+\})?)\s+(\S+)/i;

/** Match a `RewriteRule` line, capturing LHS, RHS, and optional flag
 *  block. Real-world flag blocks contain comma-separated tokens with
 *  occasional `=` (e.g. `[R=301,L]`); we accept any printable chars. */
const RULE_RE = /^\s*RewriteRule\s+(\S+)\s+(\S+)(?:\s+\[([^\]]+)\])?/i;

/** Look-behind for the path component of a RewriteRule RHS like
 *  `/transfer.php?%1` — keeps the `.php` file portion, drops the
 *  query string. */
function stripQueryString(s: string): string {
  const q = s.indexOf('?');
  return q >= 0 ? s.slice(0, q) : s;
}

/** Strip Apache regex syntax that's noise in a path template:
 *  leading/trailing anchors, escaped slashes/dots, optional-marker
 *  question marks, and trailing capture groups. */
function cleanUrlPattern(raw: string): string {
  let s = raw.trim();
  // Drop leading anchor.
  if (s.startsWith('^')) s = s.slice(1);
  // Drop trailing anchor.
  if (s.endsWith('$')) s = s.slice(0, -1);
  // Strip trailing capture groups like `(.*)`, `(.+)?`, `(.*)?`.
  s = s.replace(/\(\.[*+]\)\??$/, '');
  // Drop a trailing `/?` (optional-trailing-slash idiom) but preserve
  // the slash so `/dynamiclander/?` → `/dynamiclander/`.
  s = s.replace(/\/\?$/, '/');
  // Unescape `\.` and `\/` — Apache requires the escapes, paths don't.
  s = s.replace(/\\([./])/g, '$1');
  // Remove any leftover backslash escapes.
  s = s.replace(/\\(.)/g, '$1');
  // Collapse leading whitespace.
  s = s.trim();
  // Ensure leading slash so the matcher can compare apples to apples.
  if (s.length === 0) return '/';
  if (!s.startsWith('/')) s = '/' + s;
  return s;
}

/** Pull the rewrite-rule's target `.php` file out of the RHS. Returns
 *  `null` for non-`.php` targets (static assets, etc.). The returned
 *  path is **as-written** in the htaccess, sans query string and
 *  leading slash — callers normalise to repo-relative. */
function extractPhpTarget(rhs: string): string | null {
  const noQuery = stripQueryString(rhs);
  // Absolute URLs (`http://...`) are external — caller filters via [R].
  if (/^[a-z][a-z0-9+.-]*:\/\//i.test(noQuery)) return null;
  // Apache rewrites use `-` to mean "no substitution".
  if (noQuery === '-') return null;
  // Drop `%N` back-references and leading slash; keep the file path.
  let target = noQuery.replace(/^\//, '');
  // The target sometimes contains substitution variables like
  // `$1` or `%1`. They make the path non-static; skip.
  if (/[$%]\d/.test(target)) return null;
  if (!target.toLowerCase().endsWith('.php')) return null;
  return target;
}

/** Parse a single `.htaccess` file's text into structured rewrite
 *  pairs. Pure function — no filesystem access. */
export function parseHtaccess(filePath: string, content: string): HtaccessRoute[] {
  const out: HtaccessRoute[] = [];
  const lines = content.split(/\r?\n/);

  // `RewriteCond`s precede their `RewriteRule`. We accumulate
  // every cond we see and reset after each rule consumes them.
  let pendingConds: string[] = [];

  for (let i = 0; i < lines.length; i++) {
    const line = lines[i];
    // Comments and blanks reset nothing — Apache allows them mid-block.
    if (/^\s*#/.test(line)) continue;
    if (/^\s*$/.test(line)) continue;

    const condMatch = line.match(COND_RE);
    if (condMatch) {
      const pattern = condMatch[2];
      // We only care about REQUEST_URI-driven conds for URL routing.
      if (condMatch[1].includes('REQUEST_URI')) {
        // The pattern may glue REQUEST_URI to QUERY_STRING via
        // `?` (`^/trf\?(.*)`). The URL portion ends at the first
        // unescaped `?`.
        let urlPart = pattern;
        // Drop the QUERY_STRING glue if present (the `\?` is literal
        // in Apache; `?` is regex optional).
        const escapedQ = urlPart.indexOf('\\?');
        if (escapedQ >= 0) urlPart = urlPart.slice(0, escapedQ);
        pendingConds.push(urlPart);
      }
      continue;
    }

    const ruleMatch = line.match(RULE_RE);
    if (!ruleMatch) continue;

    const [, lhs, rhs, flagsRaw] = ruleMatch;
    const flagSet = new Set<string>(
      (flagsRaw ?? '')
        .split(',')
        .map((f) => f.trim().toLowerCase())
        .filter((f) => f.length > 0),
    );

    // External redirect — not a route into THIS app.
    // Apache treats `[R]` and `[R=NNN]` as external redirect.
    const isExternal = [...flagSet].some((f) => f === 'r' || f.startsWith('r='));
    if (isExternal) {
      pendingConds = [];
      continue;
    }

    const target = extractPhpTarget(rhs);
    if (!target) {
      pendingConds = [];
      continue;
    }

    // Choose the URL pattern source: the most recent REQUEST_URI cond
    // when one exists (most authoritative); else the rule's own LHS
    // (only useful when it's a fixed path, not `^(.*)$`).
    let rawUrl: string | null = null;
    if (pendingConds.length > 0) {
      rawUrl = pendingConds[pendingConds.length - 1];
    } else if (lhs !== '^(.*)$' && lhs !== '^.*$' && lhs !== '.*') {
      rawUrl = lhs;
    }
    if (rawUrl === null) {
      pendingConds = [];
      continue;
    }

    out.push({
      urlPattern: cleanUrlPattern(rawUrl),
      targetFile: target,
      htaccessFile: filePath,
      ruleLine: i,
      flags: flagSet,
    });
    pendingConds = [];
  }

  return out;
}

/* ─────────────────────────────────────────────────────────────────
 * Async repo-root scanning
 * ────────────────────────────────────────────────────────────── */

/** Filenames at a directory that look like Apache config:
 *  `.htaccess`, `.htaccess-dev`, `.htaccess-prod`, `.htaccess-serp`,
 *  and similar env-variant suffixes that cmadserving uses. */
function isHtaccessFile(name: string): boolean {
  if (name === '.htaccess') return true;
  return name.startsWith('.htaccess-') || name.startsWith('.htaccess.');
}

/** Scan one directory for `.htaccess*` files. Returns absolute paths. */
async function listHtaccessIn(dir: string): Promise<string[]> {
  let entries: import('node:fs').Dirent[];
  try {
    entries = await fs.readdir(dir, { withFileTypes: true });
  } catch {
    return [];
  }
  const out: string[] = [];
  for (const e of entries) {
    if (!e.isFile() && !e.isSymbolicLink()) continue;
    if (isHtaccessFile(e.name)) out.push(path.join(dir, e.name));
  }
  return out;
}

/** Recursively scan `repoRoot` for `.htaccess*` files up to a
 *  shallow depth. Plain-PHP repos put their htaccess at the doc
 *  root, but in a polyglot "mega" index each child repo's root sits
 *  one level down. Two levels covers both cases without walking
 *  the whole tree. */
async function findAllHtaccessFiles(repoRoot: string, maxDepth = 2): Promise<string[]> {
  const out: string[] = [];
  const stack: Array<{ dir: string; depth: number }> = [{ dir: repoRoot, depth: 0 }];
  while (stack.length > 0) {
    const { dir, depth } = stack.pop()!;
    const found = await listHtaccessIn(dir);
    out.push(...found);
    if (depth >= maxDepth) continue;
    let children: import('node:fs').Dirent[];
    try {
      children = await fs.readdir(dir, { withFileTypes: true });
    } catch {
      continue;
    }
    for (const c of children) {
      // Follow directories AND symlinks (mega-index uses symlinks for
      // each child repo).
      if (c.isDirectory() || c.isSymbolicLink()) {
        // Skip obvious noise.
        if (c.name === 'node_modules' || c.name === '.git' || c.name === 'vendor') continue;
        if (c.name.startsWith('.') && c.name !== '.') {
          // Allow dotfiles only if the entry IS a `.htaccess*` (handled
          // by listHtaccessIn) — never descend into hidden directories.
          continue;
        }
        stack.push({ dir: path.join(dir, c.name), depth: depth + 1 });
      }
    }
  }
  return out;
}

/** Build an index of (targetFile → HtaccessRoute[]) by parsing every
 *  `.htaccess*` file under `repoRoot`. Target file paths are
 *  repo-relative and POSIX-normalised. Rules are deduped on
 *  `(urlPattern, targetFile)` so multi-variant configs (dev/prod)
 *  collapse to one entry per real URL.
 *
 *  Returns an empty Map when the repo has no htaccess files — that
 *  signal lets the caller fall back to AST-based route detection. */
export async function buildHtaccessIndex(repoRoot: string): Promise<HtaccessIndex> {
  const files = await findAllHtaccessFiles(repoRoot);
  const seen = new Set<string>(); // dedup key = urlPattern|targetFile
  const idx: HtaccessIndex = new Map();
  for (const abs of files) {
    let content: string;
    try {
      content = await fs.readFile(abs, 'utf8');
    } catch {
      continue;
    }
    const repoRel = path.relative(repoRoot, abs).replace(/\\/g, '/');
    // The target file resolves relative to the directory holding the
    // htaccess (Apache behaviour). For nested htaccess files we
    // prefix that dir; root-level files use no prefix.
    const dirRel = path.dirname(repoRel);
    const prefix = dirRel === '.' ? '' : `${dirRel}/`;

    for (const route of parseHtaccess(repoRel, content)) {
      const fullTarget = (prefix + route.targetFile).replace(/\\/g, '/');
      const dedupKey = `${route.urlPattern}|${fullTarget}`;
      if (seen.has(dedupKey)) continue;
      seen.add(dedupKey);
      const arr = idx.get(fullTarget);
      const entry: HtaccessRoute = { ...route, targetFile: fullTarget };
      if (arr) arr.push(entry);
      else idx.set(fullTarget, [entry]);
    }
  }
  return idx;
}
