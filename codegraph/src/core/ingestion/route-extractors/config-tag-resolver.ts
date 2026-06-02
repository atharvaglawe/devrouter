/**
 * Config-tag resolver — bridges Go getter functions to the YAML/JSON
 * keys their return values were tagged with.
 *
 * The provider-tag resolver (see `provider-resolver.ts`) scans config
 * files and produces a `tag → {hosts, urls, serviceDirs}` index. That
 * tells us *what* a tag means once we have one. This module produces
 * the missing piece: *how to recover a tag from a non-literal call
 * site* — specifically when an HTTP options-bag literal binds a path
 * to something like `config.GetABTestApiConfig().ApiPath`, where:
 *
 *   1. `GetABTestApiConfig()` is a trivial getter returning a field,
 *   2. that field has a struct tag `yaml:"abtestapi"`,
 *   3. and `abtestapi` is the YAML key the provider-resolver indexed.
 *
 * We don't need to *evaluate* the function — only walk its body once
 * at parse time, capture the chain of selector expressions it returns,
 * and at repo-resolve time chase those aliases through field tags.
 *
 * Three pieces:
 *
 * 1. `extractGoConfigTags` — per-file AST pass. For every
 *    `field_declaration` with a struct tag, emits `{owner, field, tags}`.
 *
 * 2. `extractGoTrivialGetters` — per-file AST pass. For every
 *    `function_declaration` / `method_declaration` whose body is
 *    nothing but `return …` statements (any number, including
 *    branches), emits `{name, receiverType, returnAliases}`.
 *    Branching returns yield multiple aliases that all flow into the
 *    final tag set.
 *
 * 3. `buildResolvedGetters` — repo-wide resolution. Folds the per-file
 *    extractor outputs into a flat `(receiver|"*"::name) → Set<tag>`
 *    map by chasing alias chains through the field-tag index, with a
 *    cycle guard.
 *
 * Generic across any tag system: `yaml:`, `json:`, `mapstructure:`,
 * `env:`. The resolver returns *raw* tag values; the call-site
 * consumer cross-checks them against the YAML-derived
 * provider-resolver index to pick the most useful one.
 */

import type { SyntaxNode } from '../utils/ast-helpers.js';

// ─────────────────────────────────────────────────────────────────
// Public types
// ─────────────────────────────────────────────────────────────────

/** A single struct field with one or more recognised struct tags. */
export interface ConfigTagBinding {
  /** Owner struct type name (e.g. `"Config"`). When the field is
   *  declared on an anonymous embedded struct we use `"*"` so the
   *  resolver can still match by field name alone. */
  owner: string;
  /** Field name (e.g. `"ABTestApiConfig"`). */
  field: string;
  /** Tag system → tag value, e.g. `{yaml: "abtestapi", json: "abtestApi"}`. */
  tags: Record<string, string>;
  filePath: string;
  /** 0-indexed line of the field declaration. */
  lineNumber: number;
}

/** A function or method whose body is a (possibly branched) chain of
 *  `return <selector>` / `return <call>` statements. */
export interface TrivialGetterBinding {
  /** Bare function or method name (e.g. `"GetABTestApiConfig"`). */
  name: string;
  /** Receiver type when this is a method (no pointer prefix); `null`
   *  for free functions. */
  receiver: string | null;
  /** Every return-statement's recovered alias path. An alias is a
   *  list of `field_identifier` tokens, e.g.
   *    `return selected.ABTestApiConfig`           → `["selected","ABTestApiConfig"]`
   *    `return c.ABTestApiConfig.ApiPath`          → `["c","ABTestApiConfig","ApiPath"]`
   *    `return GetXyz()`                           → `["GetXyz"]` (call alias)
   *    `return c.GetXyz()`                         → `["c","GetXyz"]` (method-call alias)
   */
  returnAliases: string[][];
  filePath: string;
  lineNumber: number;
}

/** Flat result map. Key is `receiver + "::" + name` where `receiver`
 *  is the type name (no pointer) or `"*"` for free functions. Values
 *  are *all* tag values reachable through alias chasing — typically
 *  one, but a branching getter can yield several. */
export type ResolvedGetterMap = Map<string, Set<string>>;

const FREE_FN = '*';

/** Build the lookup key. */
export function getterKey(receiver: string | null, name: string): string {
  return `${receiver ?? FREE_FN}::${name}`;
}

// ─────────────────────────────────────────────────────────────────
// AST helpers (Go-specific, kept local)
// ─────────────────────────────────────────────────────────────────

function* walk(root: SyntaxNode): Generator<SyntaxNode> {
  const stack: SyntaxNode[] = [root];
  while (stack.length > 0) {
    const node = stack.pop()!;
    yield node;
    const children = node.children ?? [];
    for (let i = children.length - 1; i >= 0; i--) {
      const c = children[i];
      if (c?.isNamed === false) continue;
      stack.push(c);
    }
  }
}

/** Strip the wrapping backticks from a Go raw_string_literal. */
function readRawString(node: SyntaxNode | null | undefined): string | null {
  if (!node) return null;
  if (node.type !== 'raw_string_literal' && node.type !== 'interpreted_string_literal') {
    return null;
  }
  const raw = node.text ?? '';
  if (raw.length < 2) return null;
  return raw.slice(1, -1);
}

/** Parse a struct-tag string `key:"value" key2:"value2"` into a map.
 *  Lenient: silently drops malformed pairs. */
function parseStructTag(raw: string): Record<string, string> {
  const out: Record<string, string> = {};
  // Spec: spaces separate key:"value" pairs. Values are double-quoted.
  let i = 0;
  while (i < raw.length) {
    while (i < raw.length && /\s/.test(raw[i])) i++;
    const keyStart = i;
    while (i < raw.length && raw[i] !== ':' && !/\s/.test(raw[i])) i++;
    if (i === keyStart || raw[i] !== ':') break;
    const key = raw.slice(keyStart, i);
    i++; // skip ':'
    if (raw[i] !== '"') break;
    i++;
    const valStart = i;
    while (i < raw.length && raw[i] !== '"') {
      if (raw[i] === '\\' && i + 1 < raw.length) i++;
      i++;
    }
    if (raw[i] !== '"') break;
    let value = raw.slice(valStart, i);
    i++; // skip closing '"'
    // Strip option suffixes: `yaml:"name,omitempty"` → `name`.
    const comma = value.indexOf(',');
    if (comma >= 0) value = value.slice(0, comma);
    if (key && value) out[key] = value;
  }
  return out;
}

/** Walk a selector_expression / call_expression / identifier and
 *  flatten it into a left-to-right path. Stops at the first non-trivial
 *  shape (literals, indexed access, slice, type assertion, etc.). */
function flattenAlias(node: SyntaxNode | null | undefined): string[] | null {
  if (!node) return null;
  // Identifier / field_identifier — base case.
  if (node.type === 'identifier' || node.type === 'field_identifier' || node.type === 'type_identifier') {
    return node.text ? [node.text] : null;
  }
  // Selector: <operand>.<field>
  if (node.type === 'selector_expression') {
    const operand = node.childForFieldName?.('operand') ?? null;
    const field = node.childForFieldName?.('field') ?? null;
    const left = flattenAlias(operand);
    if (left === null) return null;
    if (!field?.text) return null;
    return [...left, field.text];
  }
  // Call: <function>(...) — drop args, keep the function path.
  if (node.type === 'call_expression') {
    const fn = node.childForFieldName?.('function') ?? null;
    return flattenAlias(fn);
  }
  // Parenthesised expression — unwrap.
  if (node.type === 'parenthesized_expression') {
    const inner = node.namedChildren?.[0] ?? null;
    return flattenAlias(inner);
  }
  // Star/pointer — `&x` or `*x` — unwrap.
  if (node.type === 'unary_expression') {
    const operand = node.childForFieldName?.('operand') ?? null;
    return flattenAlias(operand);
  }
  return null;
}

/** Find every `return_statement` that's a *direct* descendant of the
 *  function body — including those inside `if/else/switch/for`
 *  blocks. Skips returns inside nested function literals (closures)
 *  since those wouldn't apply to the outer getter's return type. */
function* iterReturns(body: SyntaxNode): Generator<SyntaxNode> {
  const stack: SyntaxNode[] = [body];
  while (stack.length > 0) {
    const node = stack.pop()!;
    if (node.type === 'function_declaration' || node.type === 'method_declaration') {
      // Don't descend into nested function defs.
      continue;
    }
    if (node.type === 'func_literal') continue;
    if (node.type === 'return_statement') {
      yield node;
      continue;
    }
    const children = node.children ?? [];
    for (let i = children.length - 1; i >= 0; i--) {
      const c = children[i];
      if (c?.isNamed === false) continue;
      stack.push(c);
    }
  }
}

/** Pull the type name from a method receiver `(c *Config)` /
 *  `(c Config)`. Drops the pointer and parameter name. */
function extractReceiverType(recvList: SyntaxNode | null | undefined): string | null {
  if (!recvList) return null;
  for (const child of recvList.namedChildren ?? []) {
    if (child.type !== 'parameter_declaration') continue;
    const t = child.childForFieldName?.('type');
    if (!t) continue;
    if (t.type === 'pointer_type') {
      const inner = t.children?.find((c) => c.type === 'type_identifier');
      return inner?.text ?? null;
    }
    if (t.type === 'type_identifier') return t.text ?? null;
    if (t.type === 'generic_type') {
      const inner = t.namedChildren?.find((c) => c.type === 'type_identifier');
      return inner?.text ?? null;
    }
  }
  return null;
}

// ─────────────────────────────────────────────────────────────────
// Per-file extractors
// ─────────────────────────────────────────────────────────────────

/** Walk a Go file's AST and emit every struct field that carries a
 *  recognisable struct tag. Owner type is the enclosing struct's
 *  declared name when statically determinable, or `"*"` for embedded
 *  / anonymous structs (so the resolver can still match by field
 *  name alone — useful for "field name happens to match a YAML key"
 *  cases). */
export function extractGoConfigTags(
  rootNode: SyntaxNode,
  filePath: string,
): ConfigTagBinding[] {
  const out: ConfigTagBinding[] = [];

  for (const node of walk(rootNode)) {
    // Visit `type_spec` only — `type_declaration` always wraps one
    // (or a list of them inside parens), so this de-duplicates without
    // missing any struct.
    if (node.type !== 'type_spec') continue;
    const ownerName =
      node.childForFieldName?.('name')?.text ??
      node.namedChildren?.find((c) => c.type === 'type_identifier')?.text ??
      '*';
    const typeNode = node.childForFieldName?.('type');
    if (!typeNode || typeNode.type !== 'struct_type') continue;
    const fieldList =
      typeNode.namedChildren?.find((c) => c.type === 'field_declaration_list') ?? null;
    if (!fieldList) continue;

    for (const fd of fieldList.namedChildren ?? []) {
      if (fd.type !== 'field_declaration') continue;
      const tagNode = fd.childForFieldName?.('tag') ??
        fd.namedChildren?.find((c) => c.type === 'raw_string_literal' || c.type === 'interpreted_string_literal');
      const rawTag = readRawString(tagNode);
      if (!rawTag) continue;
      const tags = parseStructTag(rawTag);
      if (Object.keys(tags).length === 0) continue;
      // A field_declaration can declare multiple field names sharing
      // a tag (`X, Y string \`yaml:"x"\``). Emit one binding per name.
      const nameNodes: SyntaxNode[] = [];
      const explicitName = fd.childForFieldName?.('name');
      if (explicitName) nameNodes.push(explicitName);
      else {
        for (const c of fd.namedChildren ?? []) {
          if (c.type === 'field_identifier') nameNodes.push(c);
        }
      }
      const lineNumber = fd.startPosition?.row ?? 0;
      for (const n of nameNodes) {
        const fieldName = n.text;
        if (!fieldName) continue;
        out.push({ owner: ownerName, field: fieldName, tags, filePath, lineNumber });
      }
    }
  }

  return out;
}

/** Walk a Go file's AST and emit every function/method that *only*
 *  returns selectors / calls — i.e. the trivial getter shape. Any
 *  function with non-return statements (assignments, loops, side
 *  effects) is skipped, since we can't statically follow what they
 *  return. Branching is fine: each `return …` contributes one alias,
 *  and the resolver unions them. */
export function extractGoTrivialGetters(
  rootNode: SyntaxNode,
  filePath: string,
): TrivialGetterBinding[] {
  const out: TrivialGetterBinding[] = [];

  for (const node of walk(rootNode)) {
    const isFunc = node.type === 'function_declaration';
    const isMethod = node.type === 'method_declaration';
    if (!isFunc && !isMethod) continue;
    const nameNode = node.childForFieldName?.('name');
    const name = nameNode?.text;
    if (!name) continue;
    const body = node.childForFieldName?.('body');
    if (!body || body.type !== 'block') continue;
    const receiver = isMethod
      ? extractReceiverType(node.childForFieldName?.('receiver'))
      : null;

    // Collect every return-statement reachable without entering a
    // nested function literal.
    const returns = [...iterReturns(body)];
    if (returns.length === 0) continue;

    // For each return, the value must be a single expression that
    // flattens cleanly to an alias path. If *any* return is opaque
    // (assignment side-effect, complex expression, ad-hoc literal),
    // we still record the resolvable ones; the unresolvable ones
    // simply don't contribute.
    const aliases: string[][] = [];
    for (const ret of returns) {
      // return_statement → expression_list → expr
      const list = ret.namedChildren?.find((c) => c.type === 'expression_list');
      if (!list) continue;
      const expr = list.namedChildren?.[0];
      if (!expr) continue;
      const alias = flattenAlias(expr);
      if (!alias || alias.length === 0) continue;
      aliases.push(alias);
    }
    if (aliases.length === 0) continue;

    out.push({
      name,
      receiver,
      returnAliases: aliases,
      filePath,
      lineNumber: node.startPosition?.row ?? 0,
    });
  }

  return out;
}

// ─────────────────────────────────────────────────────────────────
// Repo-wide resolver
// ─────────────────────────────────────────────────────────────────

/** Preferred order when picking *one* tag system from a multi-tagged
 *  field.
 *
 *  `mapstructure` and `yaml` are most often the actual config key;
 *  `json` second; `env` third. `java` and `python` are pre-normalised
 *  tags emitted by the Spring `@Value` / Pydantic `Field(env=)` etc.
 *  extractors — they're already derived through {@link tagFromKeyPath}
 *  so they're as authoritative as the original-language tags. */
const TAG_PREFERENCE: ReadonlyArray<string> = [
  'mapstructure',
  'yaml',
  'toml',
  'json',
  'env',
  'java',
  'python',
];

function pickTagValues(tags: Record<string, string>): string[] {
  const out: string[] = [];
  for (const key of TAG_PREFERENCE) {
    if (tags[key]) out.push(tags[key]);
  }
  return out;
}

/** Pick a single canonical tag value for a field — the first hit in
 *  `TAG_PREFERENCE`. Used by the alias-to-keypath translator where a
 *  field must map to *one* YAML/JSON segment. */
function pickPrimaryTagValue(tags: Record<string, string>): string | null {
  for (const key of TAG_PREFERENCE) {
    if (tags[key]) return tags[key];
  }
  return null;
}

// ─────────────────────────────────────────────────────────────────
// URL-via-alias resolver — joins struct-field chains to YAML key
// paths, then to literal URL values in `byKeyPath`.
// ─────────────────────────────────────────────────────────────────

/** Flat `(owner|"*")::field → primary tag value` map used by the
 *  alias-chain → YAML key path translator. Primary tag follows the
 *  same `TAG_PREFERENCE` order the rest of the resolver uses, so
 *  Go `yaml:` tags win over `json:`, etc. */
export type FieldTagMap = ReadonlyMap<string, string>;

/** Build a {@link FieldTagMap} from `ConfigTagBinding`s. Each binding
 *  contributes both a specific `(owner, field)` entry and a wildcard
 *  `(*, field)` fallback. The wildcard never overwrites a specific
 *  entry — the per-type binding wins. */
export function buildFieldTagMap(
  configTags: ReadonlyArray<ConfigTagBinding>,
): FieldTagMap {
  const out = new Map<string, string>();
  for (const ct of configTags) {
    const tag = pickPrimaryTagValue(ct.tags);
    if (!tag) continue;
    const specific = `${ct.owner}::${ct.field}`;
    if (!out.has(specific)) out.set(specific, tag);
    const wildcard = `*::${ct.field}`;
    if (!out.has(wildcard)) out.set(wildcard, tag);
  }
  return out;
}

/** Translate a struct-field alias chain (left-to-right: the first
 *  element names the *root* receiver/identifier, subsequent ones are
 *  field accessors) into a dotted YAML/JSON key path by joining
 *  consecutive elements through {@link FieldTagMap}.
 *
 *  The root element is NOT translated — it's typically a local var
 *  or a package name and has no field-tag. We start from index 1.
 *
 *  Example:
 *    `["selected", "Origins", "CmServing", "Renderer"]` with
 *    `Origins:"origins"`, `CmServing:"cmserving"`, `Renderer:"renderer"`
 *    → `"origins.cmserving.renderer"`.
 *
 *  Returns `null` when any link can't be mapped or the resulting
 *  path is empty.
 */
export function aliasToKeyPath(
  alias: ReadonlyArray<string>,
  fieldTagMap: FieldTagMap,
): string | null {
  if (alias.length < 2) return null;
  const parts: string[] = [];
  for (let i = 1; i < alias.length; i++) {
    const owner = alias[i - 1];
    const field = alias[i];
    const tag =
      fieldTagMap.get(`${owner}::${field}`) ?? fieldTagMap.get(`*::${field}`);
    if (!tag) return null;
    parts.push(tag.toLowerCase());
  }
  return parts.length > 0 ? parts.join('.') : null;
}

/** Resolved getter → URL-literal map. Folds:
 *
 *  - Trivial getters whose return alias translates to a YAML key
 *    path that {@link byKeyPath} indexes as a URL leaf — i.e. the
 *    getter itself returns a URL-typed field.
 *  - Direct field bindings: a field whose primary tag is also a
 *    top-level YAML key holding a URL literal (mirrors the
 *    receiver-agnostic `getterKey(null, field)` fallback that
 *    {@link buildResolvedGetters} emits for tags).
 *
 *  Per-call-site cases that need a *tail* accessor on top of a
 *  getter (the common `originConfig.Renderer` shape) are handled by
 *  {@link resolveGetterTailURL} called from the pipeline — kept
 *  separate so this map stays a clean static fold.
 */
export function buildResolvedGetterURLs(
  configTags: ReadonlyArray<ConfigTagBinding>,
  trivialGetters: ReadonlyArray<TrivialGetterBinding>,
  byKeyPath: ReadonlyMap<string, ReadonlySet<string>>,
): Map<string, Set<string>> {
  const out = new Map<string, Set<string>>();
  if (byKeyPath.size === 0) return out;

  const fieldTagMap = buildFieldTagMap(configTags);

  const merge = (key: string, urls: Iterable<string>) => {
    let cur = out.get(key);
    if (!cur) {
      cur = new Set<string>();
      out.set(key, cur);
    }
    for (const u of urls) cur.add(u);
  };

  // Direct field bindings (top-level YAML keys whose tag matches a
  // field's primary tag). e.g., a field `Renderer yaml:"renderer"`
  // and a top-level YAML `renderer: "/x"` → emit at
  // (null, "Renderer") and (owner, "Renderer").
  for (const ct of configTags) {
    const tag = pickPrimaryTagValue(ct.tags);
    if (!tag) continue;
    const urls = byKeyPath.get(tag.toLowerCase());
    if (!urls || urls.size === 0) continue;
    merge(getterKey(null, ct.field), urls);
    if (ct.owner !== '*') {
      merge(getterKey(ct.owner, ct.field), urls);
    }
  }

  // Trivial getters: translate the return alias chain to a YAML
  // key path via field-tag fragments, then look up in byKeyPath.
  // Yields URLs for getters whose return value is itself a leaf
  // string at a tagged-key path.
  for (const g of trivialGetters) {
    for (const alias of g.returnAliases) {
      const keyPath = aliasToKeyPath(alias, fieldTagMap);
      if (!keyPath) continue;
      const urls = byKeyPath.get(keyPath);
      if (!urls || urls.size === 0) continue;
      merge(getterKey(g.receiver, g.name), urls);
      merge(getterKey(null, g.name), urls);
    }
  }

  return out;
}

/** Per-call-site extension: given a trivial getter known to point
 *  at a YAML key-path prefix (via its return alias) and a `tail`
 *  field accessor observed at the call site (e.g.
 *  `originConfig.Renderer` where `originConfig := GetOriginConfig()`
 *  and `tail = "Renderer"`), compute the URL(s) at the extended key
 *  path.
 *
 *  Returns an empty set when no link can be made.
 */
export function resolveGetterTailURL(
  getter: TrivialGetterBinding,
  tail: string,
  fieldTagMap: FieldTagMap,
  byKeyPath: ReadonlyMap<string, ReadonlySet<string>>,
): Set<string> {
  const out = new Set<string>();
  for (const alias of getter.returnAliases) {
    const prefix = aliasToKeyPath(alias, fieldTagMap);
    if (!prefix) continue;
    const tailOwner = alias[alias.length - 1];
    const tailTag =
      fieldTagMap.get(`${tailOwner}::${tail}`) ?? fieldTagMap.get(`*::${tail}`);
    if (!tailTag) continue;
    const fullKey = `${prefix}.${tailTag.toLowerCase()}`;
    const urls = byKeyPath.get(fullKey);
    if (!urls) continue;
    for (const u of urls) out.add(u);
  }
  return out;
}

/** Resolve every getter's return alias chain into a set of tag values
 *  by chasing through:
 *    - struct field tags (terminal — emits the tag values)
 *    - other getters (recursive — re-resolves via the same map)
 *
 *  Cycle-guarded by tracking visited (receiver|"*", name) pairs.
 *  Bounded depth (8) so pathological chains don't explode. */
export function buildResolvedGetters(
  configTags: ConfigTagBinding[],
  trivialGetters: TrivialGetterBinding[],
): ResolvedGetterMap {
  // ── Index 1: field tags by (owner, field) and by ("*", field). ──
  // The "*" bucket lets resolution succeed even when the alias chain
  // doesn't carry the owner type (e.g., bare `selected.X` where we
  // don't know `selected`'s type).
  const tagsByOwnerField = new Map<string, string[]>();
  const ownerKey = (owner: string, field: string) => `${owner}::${field}`;
  for (const ct of configTags) {
    const values = pickTagValues(ct.tags);
    if (values.length === 0) continue;
    const k1 = ownerKey(ct.owner, ct.field);
    const merge = (key: string, vals: string[]) => {
      const cur = tagsByOwnerField.get(key);
      if (cur) {
        for (const v of vals) if (!cur.includes(v)) cur.push(v);
      } else {
        tagsByOwnerField.set(key, [...vals]);
      }
    };
    merge(k1, values);
    merge(ownerKey('*', ct.field), values);
  }

  // ── Index 2: getters by (receiver|"*", name). ──
  const gettersByKey = new Map<string, TrivialGetterBinding[]>();
  for (const g of trivialGetters) {
    const k = getterKey(g.receiver, g.name);
    const list = gettersByKey.get(k);
    if (list) list.push(g);
    else gettersByKey.set(k, [g]);
  }

  // ── BFS resolution ──
  const cache = new Map<string, Set<string>>();
  const MAX_DEPTH = 8;

  function resolveAlias(
    alias: string[],
    visited: Set<string>,
    depth: number,
  ): Set<string> {
    if (depth > MAX_DEPTH) return new Set();
    if (alias.length === 0) return new Set();

    const result = new Set<string>();

    // Try to interpret the alias as a struct field reference. Walk
    // adjacent pairs from right-to-left: `[a,b,c]` could mean
    // `(b).c`, `(*).c`, or `(a).b` etc. Each hit emits the tag set.
    for (let i = 0; i < alias.length; i++) {
      // Try (alias[i-1], alias[i]) → field tag, and ("*", alias[i]).
      const owner = i > 0 ? alias[i - 1] : '*';
      const field = alias[i];
      for (const k of [ownerKey(owner, field), ownerKey('*', field)]) {
        const tags = tagsByOwnerField.get(k);
        if (tags) for (const t of tags) result.add(t);
      }
    }

    // Try to interpret a leaf identifier as a getter call:
    //   alias = [GetXyz]                    → ("*", "GetXyz")
    //   alias = [recv, GetXyz]              → (recv-type-via-symbol, "GetXyz")
    //   For methods called on locals we approximate by trying both
    //   the receiver type itself and "*" — generates extra hits but
    //   never a wrong tag (YAML keys are essentially unique).
    const lastIdent = alias[alias.length - 1];
    const candidateGetterKeys: string[] = [getterKey(null, lastIdent)];
    if (alias.length >= 2) {
      const prev = alias[alias.length - 2];
      candidateGetterKeys.push(getterKey(prev, lastIdent));
    }
    for (const k of candidateGetterKeys) {
      if (visited.has(k)) continue;
      const fns = gettersByKey.get(k);
      if (!fns) continue;
      const childVisited = new Set(visited);
      childVisited.add(k);
      for (const fn of fns) {
        for (const subAlias of fn.returnAliases) {
          for (const t of resolveAlias(subAlias, childVisited, depth + 1)) {
            result.add(t);
          }
        }
      }
    }

    return result;
  }

  for (const [key, fns] of gettersByKey) {
    if (cache.has(key)) continue;
    const visited = new Set<string>([key]);
    const collected = new Set<string>();
    for (const fn of fns) {
      for (const alias of fn.returnAliases) {
        for (const t of resolveAlias(alias, visited, 0)) collected.add(t);
      }
    }
    if (collected.size > 0) cache.set(key, collected);
  }

  // Receiver-agnostic fallbacks. The pending-lookup receiver is the
  // *variable name* at the call site (`props.getUrl()` → "props"),
  // not the *type name* the getter is declared on (`KosmosProps`).
  // We already emit `(<type>, <getter>)` above; mirror each into
  // `("*", <getter>)` so the lookup succeeds without a type-env
  // pass at every call site. When two distinct types have a method
  // with the same name we union their tag sets — over-emission is
  // harmless because the call-site consumer cross-checks against
  // the provider-resolver index anyway.
  for (const [key, tags] of Array.from(cache.entries())) {
    const sep = key.indexOf('::');
    if (sep < 0) continue;
    const recv = key.slice(0, sep);
    if (recv === '*') continue;
    const name = key.slice(sep + 2);
    const k2 = getterKey(null, name);
    let entry = cache.get(k2);
    if (!entry) {
      entry = new Set<string>();
      cache.set(k2, entry);
    }
    for (const t of tags) entry.add(t);
  }

  // Direct-field bindings — a pending lookup may name a field
  // directly (e.g. Python `settings.weaver_url` or a bare
  // module-level constant `KOSMOS_URL`) rather than a method that
  // returns it. Emit map entries `(owner, field)` and `("*", field)`
  // for every config-tag binding so the call-site consumer can
  // resolve those shapes through the same {@link ResolvedGetterMap}
  // it uses for getters.
  for (const ct of configTags) {
    const values = pickTagValues(ct.tags);
    if (values.length === 0) continue;
    for (const ownerVariant of [ct.owner, '*']) {
      const k = getterKey(ownerVariant === '*' ? null : ownerVariant, ct.field);
      let entry = cache.get(k);
      if (!entry) {
        entry = new Set<string>();
        cache.set(k, entry);
      }
      for (const v of values) entry.add(v);
    }
  }

  return cache;
}
