'use strict';

// Endpoint logic for the devrouter codegraph sidecar.
//
// Each function takes a parsed request body and returns a plain object that
// the Go client in internal/codegraph decodes. Shapes are intentionally
// frozen to match that client:
//   SearchResult { nodeId, id, name, filePath, label, content, source,
//                  score, startLine, endLine }
//   CallEdge     { from, to, file }
//
// Graph traversal uses the engine's QueryBuilder; a handful of aggregate /
// pattern queries drop to raw SQL via the same connection.

const fs = require('fs');
const path = require('path');
const pool = require('./pool');
const registry = require('./registry');

const SOURCE_CAP = 64 * 1024; // mirror the old engine's 64 KB source cap

// ---------------------------------------------------------------------------
// shared builders
// ---------------------------------------------------------------------------

function sliceSource(repoPath, filePath, startLine, endLine) {
  if (!filePath) return '';
  const abs = path.isAbsolute(filePath) ? filePath : path.join(repoPath, filePath);
  let text;
  try {
    text = fs.readFileSync(abs, 'utf8');
  } catch (_) {
    return '';
  }
  if (!startLine || startLine < 1) {
    return text.length > SOURCE_CAP ? text.slice(0, SOURCE_CAP) : text;
  }
  const lines = text.split('\n');
  const from = Math.max(0, startLine - 1);
  const to = endLine && endLine >= startLine ? Math.min(lines.length, endLine) : lines.length;
  const out = lines.slice(from, to).join('\n');
  return out.length > SOURCE_CAP ? out.slice(0, SOURCE_CAP) : out;
}

function nodeToResult(node, repoPath, { score = 0, includeSource = false } = {}) {
  return {
    nodeId: node.id,
    id: node.id,
    name: node.name,
    filePath: node.filePath,
    label: node.kind,
    content: '',
    source: includeSource ? sliceSource(repoPath, node.filePath, node.startLine, node.endLine) : '',
    score,
    startLine: node.startLine || 0,
    endLine: node.endLine || 0,
  };
}

function edge(from, to, file) {
  return { from, to, file: file || '' };
}

// dedupEdges keeps first occurrence per (from,to) and caps the slice.
function dedupEdges(edges, limit) {
  const seen = new Set();
  const out = [];
  for (const e of edges) {
    if (!e.from || !e.to || e.from === e.to) continue;
    const key = e.from + '|' + e.to;
    if (seen.has(key)) continue;
    seen.add(key);
    out.push(e);
    if (limit > 0 && out.length >= limit) break;
  }
  return out;
}

// ---------------------------------------------------------------------------
// /api/repos, /api/file
// ---------------------------------------------------------------------------

function repos() {
  return registry.loadRegistry().map((e) => ({
    name: e.name,
    path: e.path,
    indexedAt: e.indexedAt || '',
  }));
}

// files lists indexed file paths (for memory auto-population).
function files(body) {
  const { repo, limit } = body;
  const { qb } = pool.open(repo);
  const lim = limit && limit > 0 ? limit : 10000;
  const all = qb.getAllFiles() || [];
  return { paths: all.slice(0, lim).map((f) => f.path).filter(Boolean) };
}

// symbols lists named symbols + their file (for memory auto-population).
function symbols(body) {
  const { repo, limit } = body;
  const lim = limit && limit > 0 ? limit : 50000;
  const db = pool.rawDb(repo);
  const rows = db.prepare(
    `SELECT name, file_path AS file FROM nodes
      WHERE name IS NOT NULL AND start_line IS NOT NULL AND file_path IS NOT NULL
      LIMIT ?`
  ).all(lim);
  return { symbols: rows.filter((r) => r.name).map((r) => ({ name: r.name, file: r.file })) };
}

function file(body) {
  const { path: filePath, repo } = body;
  if (!filePath) throw httpError(400, 'missing path');
  const entry = registry.resolve(repo);
  const repoPath = entry ? entry.path : '';
  const abs = path.isAbsolute(filePath) ? filePath : path.join(repoPath, filePath);
  let content;
  try {
    content = fs.readFileSync(abs, 'utf8');
  } catch (e) {
    throw httpError(404, `file not found: ${filePath}`);
  }
  return { content, totalLines: content.split('\n').length };
}

// ---------------------------------------------------------------------------
// /api/search — FTS / hybrid / bm25 (semantic falls back to FTS)
// ---------------------------------------------------------------------------

function search(body) {
  const { query, repo, limit, include_source } = body;
  if (!query) return { results: [] };
  const { qb, repoPath } = pool.open(repo);
  const lim = limit && limit > 0 ? limit : 10;
  const hits = qb.searchNodes(String(query), { limit: lim });
  const results = hits.map((h) => nodeToResult(h.node, repoPath, {
    score: h.score,
    includeSource: include_source !== false,
  }));
  return { results };
}

// ---------------------------------------------------------------------------
// Graph: callers / callees / upstream
// ---------------------------------------------------------------------------

function nodesByName(qb, name) {
  return qb.getNodesByName(name) || [];
}

function callers(body) {
  const { name, repo, limit } = body;
  const { qb } = pool.open(repo);
  const lim = limit && limit > 0 ? limit : 15;
  const edges = [];
  for (const target of nodesByName(qb, name)) {
    for (const e of qb.getIncomingEdges(target.id, ['calls'])) {
      const src = qb.getNodeById(e.source);
      if (src) edges.push(edge(src.name, name, src.filePath));
    }
  }
  return { edges: dedupEdges(edges, lim) };
}

function callees(body) {
  const { name, repo, limit } = body;
  const { qb } = pool.open(repo);
  const lim = limit && limit > 0 ? limit : 15;
  const edges = [];
  for (const source of nodesByName(qb, name)) {
    for (const e of qb.getOutgoingEdges(source.id, ['calls'])) {
      const dst = qb.getNodeById(e.target);
      if (dst) edges.push(edge(name, dst.name, dst.filePath));
    }
  }
  return { edges: dedupEdges(edges, lim) };
}

// upstream: grandparent -> parent -> target (2-hop callers).
function upstream(body) {
  const { name, repo, limit } = body;
  const { qb } = pool.open(repo);
  const lim = limit && limit > 0 ? limit : 10;
  const edges = [];
  for (const target of nodesByName(qb, name)) {
    for (const pe of qb.getIncomingEdges(target.id, ['calls'])) {
      const parent = qb.getNodeById(pe.source);
      if (!parent) continue;
      for (const ge of qb.getIncomingEdges(parent.id, ['calls'])) {
        const gp = qb.getNodeById(ge.source);
        if (gp) edges.push(edge(gp.name, parent.name, gp.filePath));
      }
    }
  }
  return { edges: dedupEdges(edges, lim) };
}

// ---------------------------------------------------------------------------
// Graph: importers / extends / methods
// ---------------------------------------------------------------------------

// importers: things that reference the symbol from another file. Approximates
// the old IMPORTS+DEFINES walk on a schema where `imports` edges are
// file-local; the resolved reference graph is the real cross-file signal.
function importers(body) {
  const { name, repo, limit } = body;
  const { qb } = pool.open(repo);
  const lim = limit && limit > 0 ? limit : 15;
  const edges = [];
  for (const target of nodesByName(qb, name)) {
    for (const e of qb.getIncomingEdges(target.id, ['references', 'imports', 'calls'])) {
      const src = qb.getNodeById(e.source);
      if (src && src.filePath !== target.filePath) {
        edges.push(edge(src.name, name, src.filePath));
      }
    }
  }
  return { edges: dedupEdges(edges, lim) };
}

// importersByPackage: files declaring an import whose name contains the word.
function importersByPackage(body) {
  const { pkg, repo, limit } = body;
  const lim = limit && limit > 0 ? limit : 30;
  if (!pkg) return { edges: [] };
  const db = pool.rawDb(repo);
  const rows = db.prepare(
    `SELECT DISTINCT file_path AS file, name AS pkg
       FROM nodes
      WHERE kind IN ('import', 'module', 'namespace')
        AND lower(name) LIKE ?
      LIMIT ?`
  ).all('%' + String(pkg).toLowerCase() + '%', lim * 2);
  const word = String(pkg).toLowerCase();
  const edges = [];
  for (const r of rows) {
    const base = r.file ? r.file.split('/').pop() : r.pkg;
    if (base && base.toLowerCase().includes(word)) continue; // skip the pkg's own files
    edges.push(edge(base || r.pkg, r.pkg, r.file));
  }
  return { edges: dedupEdges(edges, lim) };
}

// extends: EXTENDS / IMPLEMENTS in either direction (struct embedding,
// interface implementation). Mirrors the old `child OR parent == name`.
function extendsRel(body) {
  const { name, repo, limit } = body;
  const { qb } = pool.open(repo);
  const lim = limit && limit > 0 ? limit : 15;
  const edges = [];
  for (const node of nodesByName(qb, name)) {
    for (const e of qb.getOutgoingEdges(node.id, ['extends', 'implements'])) {
      const parent = qb.getNodeById(e.target);
      if (parent) edges.push(edge(name, parent.name, node.filePath));
    }
    for (const e of qb.getIncomingEdges(node.id, ['extends', 'implements'])) {
      const child = qb.getNodeById(e.source);
      if (child) edges.push(edge(child.name, name, child.filePath));
    }
  }
  return { edges: dedupEdges(edges, lim) };
}

// methods: members a class/struct/interface contains (HAS_METHOD).
function methods(body) {
  const { name, repo, limit } = body;
  const { qb } = pool.open(repo);
  const lim = limit && limit > 0 ? limit : 15;
  const edges = [];
  const memberKinds = new Set(['method', 'function', 'property', 'field']);
  for (const owner of nodesByName(qb, name)) {
    for (const e of qb.getOutgoingEdges(owner.id, ['contains'])) {
      const m = qb.getNodeById(e.target);
      if (m && memberKinds.has(m.kind)) edges.push(edge(name, m.name, m.filePath));
    }
  }
  return { edges: dedupEdges(edges, lim) };
}

// ---------------------------------------------------------------------------
// Graph: cross-wire (route -> handler). Best-effort on the new schema, which
// links route nodes to handlers but does not emit a caller->route FETCHES
// edge. Returns route/handler pairs that touch `name` on either side.
// ---------------------------------------------------------------------------

function crossWire(body) {
  const { name, repo, direction, limit } = body;
  const lim = limit && limit > 0 ? limit : 15;
  const db = pool.rawDb(repo);
  const rows = db.prepare(
    `SELECT r.name AS route, r.file_path AS route_file,
            h.name AS handler, h.file_path AS handler_file
       FROM nodes r
       JOIN edges e ON e.source = r.id
       JOIN nodes h ON e.target = h.id
      WHERE r.kind = 'route'
        AND e.kind IN ('references', 'calls')
        AND h.kind IN ('function', 'method', 'class')
      LIMIT 2000`
  ).all();
  const edges = [];
  for (const r of rows) {
    // direction "callees": seed is a caller/route -> surface the handler.
    // direction "callers": seed is a handler -> surface its route binding.
    if (direction === 'callers') {
      if (r.handler === name) edges.push(edge(r.route, r.handler, r.handler_file));
    } else {
      if (r.route === name || r.handler === name) {
        edges.push(edge(name, r.handler, r.handler_file));
      }
    }
  }
  return { edges: dedupEdges(edges, lim) };
}

// ---------------------------------------------------------------------------
// siblings / related-files / name-hits
// ---------------------------------------------------------------------------

function siblings(body) {
  const { filePath, repo, limit } = body;
  const lim = limit && limit > 0 ? limit : 20;
  if (!filePath || !filePath.includes('/')) return { paths: [] };
  const dir = filePath.slice(0, filePath.lastIndexOf('/'));
  const db = pool.rawDb(repo);
  const rows = db.prepare(
    `SELECT DISTINCT file_path AS path
       FROM nodes
      WHERE file_path LIKE ?
        AND file_path NOT LIKE ?
      LIMIT ?`
  ).all(dir + '/%', dir + '/%/%', lim);
  return { paths: rows.map((r) => r.path).filter(Boolean) };
}

function relatedFiles(body) {
  const { keyword, repo, limit } = body;
  const lim = limit && limit > 0 ? limit : 100;
  if (!keyword) return { paths: [] };
  const db = pool.rawDb(repo);
  const rows = db.prepare(
    `SELECT DISTINCT file_path AS path
       FROM nodes
      WHERE lower(file_path) LIKE ?
      LIMIT ?`
  ).all('%' + String(keyword).toLowerCase() + '%', lim);
  return { paths: rows.map((r) => r.path).filter(Boolean) };
}

function nameHits(body) {
  const { term, repo } = body;
  if (!term) return { count: 0 };
  const db = pool.rawDb(repo);
  const row = db.prepare(
    `SELECT count(*) AS c FROM nodes WHERE lower(name) LIKE ?`
  ).get('%' + String(term).toLowerCase() + '%');
  return { count: row ? row.c : 0 };
}

// ---------------------------------------------------------------------------
// search-by-path
// ---------------------------------------------------------------------------

function rowToResult(row, repoPath) {
  const node = {
    id: row.id,
    kind: row.kind,
    name: row.name,
    filePath: row.file_path,
    startLine: row.start_line,
    endLine: row.end_line,
  };
  return nodeToResult(node, repoPath, { score: 0, includeSource: true });
}

function searchByPath(body) {
  const { filePath, repo, limit } = body;
  const lim = limit && limit > 0 ? limit : 20;
  if (!filePath) return { results: [] };
  const { repoPath } = pool.open(repo);
  const db = pool.rawDb(repo);
  const base = `SELECT id, kind, name, file_path, start_line, end_line FROM nodes
                WHERE %COND% AND start_line IS NOT NULL
                ORDER BY start_line LIMIT ?`;
  // exact, then suffix (ENDS WITH), then CONTAINS.
  const attempts = [
    { cond: 'file_path = ?', param: filePath },
    { cond: 'file_path LIKE ?', param: '%' + filePath },
    { cond: 'file_path LIKE ?', param: '%' + filePath + '%' },
  ];
  for (const a of attempts) {
    const rows = db.prepare(base.replace('%COND%', a.cond)).all(a.param, lim);
    if (rows.length > 0) {
      return { results: rows.map((r) => rowToResult(r, repoPath)) };
    }
  }
  return { results: [] };
}

// ---------------------------------------------------------------------------
// search-by-name (ported from the Go SearchByNameWithOpts)
// ---------------------------------------------------------------------------

const STOP_WORDS = new Set(('the a an and or but is in on at to for of with by from as into how what why ' +
  'where when which does do did can could should would will this that these those be been being have has had ' +
  'it its i we you me my about any all').split(' '));

function stemTerm(w) {
  if (w.length <= 6) return w;
  for (const suf of ['ings', 'ing', 'ers', 'ed', 'es', 's']) {
    if (!w.endsWith(suf)) continue;
    let stripped = w.slice(0, w.length - suf.length);
    if (stripped.length >= 6 && stripped.endsWith('ll')) stripped = stripped.slice(0, -1);
    if (stripped.length >= 4) return stripped;
  }
  return w;
}

function splitQueryWords(q) {
  const seen = new Set();
  const words = [];
  for (let raw of String(q).split(/\s+/)) {
    raw = raw.replace(/^[.,;:!?"'()[\]{}]+|[.,;:!?"'()[\]{}]+$/g, '');
    if (raw.length < 2) continue;
    const lower = raw.toLowerCase();
    if (STOP_WORDS.has(lower)) continue;
    const norm = stemTerm(lower);
    if (seen.has(norm)) continue;
    seen.add(norm);
    words.push(norm);
  }
  return words;
}

function surfaceWeight(s) {
  if (s === 'name') return 1.0;
  if (s === 'filePath') return 0.7;
  if (s === 'content') return 0.4;
  return 0;
}

function termIDF(hitCount) {
  if (hitCount <= 0) return 0;
  let v = Math.log(1.0 + 1000.0 / (hitCount + 1));
  return v > 8 ? 8 : v;
}

function contextBoost(filePath, hints) {
  if (!hints || hints.length === 0 || !filePath) return 1.0;
  const lower = filePath.toLowerCase();
  let matches = 0;
  for (const h of hints) {
    if (h && lower.includes(String(h).toLowerCase())) matches++;
  }
  if (matches === 0) return 1.0;
  if (matches === 1) return 2.0;
  return 3.0;
}

function shouldExclude(filePath, name, excludes) {
  if (!excludes || excludes.length === 0) return false;
  const pathLower = (filePath || '').toLowerCase();
  const nameLower = (name || '').toLowerCase();
  for (const ex of excludes) {
    if (!ex) continue;
    const exLower = String(ex).toLowerCase();
    if (pathLower.endsWith('_' + exLower + '.go')) return true;
    if (pathLower.includes('/' + exLower + '/') || pathLower.includes('/' + exLower + 's/')) return true;
    if (nameLower.startsWith(exLower)) {
      const rest = (name || '').slice(exLower.length);
      if (rest === '') return true;
      const first = rest.charCodeAt(0);
      if (first >= 65 && first <= 90) return true; // next char uppercase
    }
  }
  return false;
}

function searchByName(body) {
  const { query, repo, limit } = body;
  const mustTerms = body.mustTerms || [];
  const excludeTerms = body.excludeTerms || [];
  const contextHints = body.contextHints || [];
  const lim = limit && limit > 0 ? limit : 10;
  const words = splitQueryWords(query);
  if (words.length === 0) return { results: [] };

  const { repoPath } = pool.open(repo);
  const db = pool.rawDb(repo);

  const hits = new Map(); // key name|file -> { row, surfaces: Map term->surface }
  const hitCounts = new Map();
  const bump = (term) => hitCounts.set(term, (hitCounts.get(term) || 0) + 1);

  const collect = (rows, term, surface) => {
    for (const row of rows) {
      if (!row.name || !row.file_path) continue;
      const key = row.name + '|' + row.file_path;
      let h = hits.get(key);
      if (!h) { h = { row, surfaces: new Map() }; hits.set(key, h); }
      const cur = h.surfaces.get(term);
      if (cur === undefined || surfaceWeight(surface) > surfaceWeight(cur)) {
        h.surfaces.set(term, surface);
      }
      bump(term);
    }
  };

  const selNode = `SELECT id, kind, name, file_path, start_line, end_line FROM nodes
                   WHERE %COND% AND start_line IS NOT NULL LIMIT ?`;

  // 1) name CONTAINS per term
  for (const w of words) {
    const rows = db.prepare(selNode.replace('%COND%', 'lower(name) LIKE ?')).all('%' + w + '%', lim * 3);
    collect(rows, w, 'name');
  }

  // 2) signature/qualified-name fallback for the rarest seen term (the new
  // schema has no symbol-body column, so this stands in for "content CONTAINS")
  if (hits.size < lim) {
    let rarest = '', rarestCnt = 0;
    for (const w of words) {
      const c = hitCounts.get(w) || 0;
      if (c === 0) continue;
      if (rarest === '' || c < rarestCnt) { rarest = w; rarestCnt = c; }
    }
    if (rarest) {
      const rows = db.prepare(
        selNode.replace('%COND%', '(lower(signature) LIKE ? OR lower(qualified_name) LIKE ?)')
      ).all('%' + rarest + '%', '%' + rarest + '%', lim * 2);
      collect(rows, rarest, 'content');
    }
  }

  // 3) exclude conventions
  for (const [key, h] of [...hits]) {
    if (shouldExclude(h.row.file_path, h.row.name, excludeTerms)) hits.delete(key);
  }

  // 4) must-terms file-level filter
  if (mustTerms.length > 0) {
    const mustFiles = filesMatchingAnyTerm(db, mustTerms);
    if (mustFiles.size > 0) {
      for (const [key, h] of [...hits]) {
        if (!mustFiles.has(h.row.file_path)) hits.delete(key);
      }
    }
  }

  // 5) score + context boost
  const ranked = [];
  for (const h of hits.values()) {
    let s = 0;
    for (const [term, surface] of h.surfaces) {
      s += termIDF(hitCounts.get(term) || 0) * surfaceWeight(surface);
    }
    s *= contextBoost(h.row.file_path, contextHints);
    ranked.push({ row: h.row, score: s });
  }
  ranked.sort((a, b) => b.score - a.score);

  const results = ranked.slice(0, lim).map((r) => {
    const res = rowToResult(r.row, repoPath);
    res.score = r.score;
    return res;
  });
  return { results };
}

function filesMatchingAnyTerm(db, terms) {
  const out = new Set();
  for (const t of terms) {
    if (!t) continue;
    const p = '%' + String(t).toLowerCase() + '%';
    const rows = db.prepare(
      `SELECT DISTINCT file_path AS f FROM nodes
        WHERE (lower(file_path) LIKE ? OR lower(name) LIKE ?
               OR lower(signature) LIKE ? OR lower(qualified_name) LIKE ?)
          AND file_path IS NOT NULL
        LIMIT 500`
    ).all(p, p, p, p);
    for (const r of rows) if (r.f) out.add(r.f);
  }
  return out;
}

// ---------------------------------------------------------------------------

function httpError(status, message) {
  const e = new Error(message);
  e.status = status;
  return e;
}

module.exports = {
  repos,
  file,
  files,
  symbols,
  search,
  callers,
  callees,
  upstream,
  importers,
  importersByPackage,
  extendsRel,
  methods,
  crossWire,
  siblings,
  relatedFiles,
  nameHits,
  searchByPath,
  searchByName,
  httpError,
};
