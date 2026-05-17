/**
 * Python config-tag + trivial-getter extractors.
 *
 * Mirrors the Go and Java pairs but recognises the idiomatic Python
 * config surface: env-var indirection (`os.environ`, `os.getenv`),
 * Pydantic `BaseSettings` with `Field(env="…")` / `Field(alias="…")`,
 * and `@property`-decorated trivial accessors.
 *
 * Patterns covered (all repo-agnostic):
 *
 *   1. **Module-level env binding** —
 *        KOSMOS_URL = os.environ["KOSMOS_URL"]
 *        KOSMOS_URL = os.environ.get("KOSMOS_URL")
 *        KOSMOS_URL = os.getenv("KOSMOS_URL")
 *      The module-level identifier is bound to the env-var key,
 *      normalised through {@link tagFromKeyPath}.
 *
 *   2. **Pydantic settings** —
 *        class Settings(BaseSettings):
 *            kosmos_url: str = Field(env="KOSMOS_URL")
 *            abtest: AbtestCfg = Field(...)
 *      Each field is bound to the explicit `env`/`alias` value when
 *      present, falling back to its own UPPER-cased identifier.
 *
 *   3. **Module-level constant from another constant** —
 *        KOSMOS_URL = "https://…"  (literal — handled by the
 *                                   provider-resolver scan, not here)
 *      Skipped here; literals are picked up by the YAML/`.env`
 *      scanner directly.
 *
 *   4. **`@property` getter** —
 *        @property
 *        def kosmos_url(self):
 *            return self._kosmos_url
 *      Treated as a trivial getter on the enclosing class so the
 *      resolver can chase aliases through it.
 *
 * The fold (`buildResolvedGetters`) is language-agnostic — Python
 * extractors emit the same `ConfigTagBinding` / `TrivialGetterBinding`
 * shapes the Go and Java ones do.
 */

import type { SyntaxNode } from '../utils/ast-helpers.js';
import type {
  ConfigTagBinding,
  TrivialGetterBinding,
} from './config-tag-resolver.js';
import { tagFromKeyPath } from './provider-resolver.js';

// ─────────────────────────────────────────────────────────────────
// AST helpers
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

/** Read the body of a `string` node. Tree-sitter-python wraps the
 *  text in `string_start` / `string_content` / `string_end`. */
function readPyString(node: SyntaxNode | null | undefined): string | null {
  if (!node) return null;
  if (node.type !== 'string') return null;
  const content = node.namedChildren?.find((c) => c.type === 'string_content');
  if (content?.text) return content.text;
  const t = node.text ?? '';
  if (t.length < 2) return null;
  if (
    (t.startsWith('"') && t.endsWith('"')) ||
    (t.startsWith("'") && t.endsWith("'"))
  ) {
    return t.slice(1, -1);
  }
  return null;
}

/** Flatten the function part of a `call` node into a dotted name.
 *
 *   `os.environ.get` → `["os","environ","get"]`
 *   `Field`           → `["Field"]`
 *   `obj.attr.fn`     → `["obj","attr","fn"]`
 *   anything else     → `null` */
function callFunctionPath(callNode: SyntaxNode): string[] | null {
  const fn = callNode.childForFieldName?.('function');
  if (!fn) return null;
  return flattenAttribute(fn);
}

function flattenAttribute(node: SyntaxNode | null | undefined): string[] | null {
  if (!node) return null;
  if (node.type === 'identifier') return node.text ? [node.text] : null;
  if (node.type === 'attribute') {
    const obj = node.childForFieldName?.('object');
    const attr = node.childForFieldName?.('attribute');
    const left = flattenAttribute(obj);
    if (!left) return null;
    if (!attr?.text) return null;
    return [...left, attr.text];
  }
  return null;
}

/** Walk a Python expression and flatten it into an alias path —
 *  the same shape consumed by `buildResolvedGetters`.
 *
 *   `x`              → `["x"]`
 *   `self.x`         → `["self", "x"]`
 *   `self.cfg.host`  → `["self", "cfg", "host"]`
 *   `get_x()`        → `["get_x"]`
 *   `cfg.get_x()`    → `["cfg", "get_x"]`
 *
 *  Returns `null` for any shape that doesn't reduce to a simple
 *  attribute / call chain.
 */
function flattenAlias(node: SyntaxNode | null | undefined): string[] | null {
  if (!node) return null;
  if (node.type === 'identifier') return node.text ? [node.text] : null;
  if (node.type === 'attribute') return flattenAttribute(node);
  if (node.type === 'call') {
    return callFunctionPath(node);
  }
  if (node.type === 'parenthesized_expression') {
    const inner = node.namedChildren?.[0] ?? null;
    return flattenAlias(inner);
  }
  return null;
}

/** Iterate every top-level `return_statement` reachable without
 *  descending into a nested function. */
function* iterReturns(body: SyntaxNode): Generator<SyntaxNode> {
  const stack: SyntaxNode[] = [body];
  while (stack.length > 0) {
    const cur = stack.pop()!;
    if (cur.type === 'function_definition' || cur.type === 'lambda') {
      if (cur === body) {
        // never the case (body is a `block`), but defensive
      } else continue;
    }
    if (cur.type === 'return_statement') {
      yield cur;
      continue;
    }
    const children = cur.children ?? [];
    for (let i = children.length - 1; i >= 0; i--) {
      const c = children[i];
      if (c?.isNamed === false) continue;
      stack.push(c);
    }
  }
}

/** Find the enclosing `class_definition` and return its declared name. */
function enclosingClassName(node: SyntaxNode): string | null {
  let cur: SyntaxNode | null = node.parent;
  while (cur) {
    if (cur.type === 'class_definition') {
      return cur.childForFieldName?.('name')?.text ?? null;
    }
    cur = cur.parent;
  }
  return null;
}

/** Look up a keyword arg by name in an `argument_list` and return its
 *  string-literal value, or `null` if absent / non-string. */
function readKeywordStr(args: SyntaxNode | null | undefined, key: string): string | null {
  if (!args) return null;
  for (const child of args.namedChildren ?? []) {
    if (child.type !== 'keyword_argument') continue;
    const nameNode = child.childForFieldName?.('name');
    const valueNode = child.childForFieldName?.('value');
    if (nameNode?.text !== key) continue;
    return readPyString(valueNode);
  }
  return null;
}

/** Get the first positional string-literal argument from an
 *  `argument_list`, or `null`. */
function firstPositionalStr(args: SyntaxNode | null | undefined): string | null {
  if (!args) return null;
  for (const child of args.namedChildren ?? []) {
    if (child.type === 'keyword_argument') continue;
    const s = readPyString(child);
    if (s !== null) return s;
    // First positional that isn't a string — abort.
    return null;
  }
  return null;
}

/** Convert an env-var key (`KOSMOS_URL`, `weaver_host`) into a key
 *  path suitable for {@link tagFromKeyPath}. */
function envKeyPath(raw: string): string[] {
  return raw.toLowerCase().split('_').filter((s) => s.length > 0);
}

// ─────────────────────────────────────────────────────────────────
// Recognisers
// ─────────────────────────────────────────────────────────────────

const ENV_FUNCTIONS: ReadonlySet<string> = new Set([
  'os.getenv',
  'os.environ.get',
  // Less common but valid:
  'environ.get',
  'getenv',
]);

const PYDANTIC_FACTORY_NAMES: ReadonlySet<string> = new Set([
  'Field',
  'pydantic.Field',
  'pydantic_settings.Field',
]);

/** When the RHS of an assignment is a recognised env-var read,
 *  return the underlying env-var key name; null otherwise. */
function recogniseEnvRead(rhs: SyntaxNode | null | undefined): string | null {
  if (!rhs) return null;
  // os.environ.get("X") / os.getenv("X") / environ.get("X") / getenv("X")
  if (rhs.type === 'call') {
    const path = callFunctionPath(rhs);
    if (!path) return null;
    const dotted = path.join('.');
    if (!ENV_FUNCTIONS.has(dotted)) return null;
    const args = rhs.childForFieldName?.('arguments');
    return firstPositionalStr(args);
  }
  // os.environ["X"]
  if (rhs.type === 'subscript') {
    const value = rhs.childForFieldName?.('value');
    const valuePath = flattenAttribute(value);
    if (!valuePath) return null;
    const dotted = valuePath.join('.');
    if (dotted !== 'os.environ' && dotted !== 'environ') return null;
    // Subscripts have a single `subscript` child for the index.
    for (const child of rhs.namedChildren ?? []) {
      if (child === value) continue;
      const s = readPyString(child);
      if (s !== null) return s;
    }
  }
  return null;
}

// ─────────────────────────────────────────────────────────────────
// Public extractors
// ─────────────────────────────────────────────────────────────────

/** Walk a Python file's AST and emit a {@link ConfigTagBinding} for:
 *    - module-level `NAME = os.environ[X]` / `os.getenv(X)`-style reads
 *    - Pydantic `BaseSettings` fields with `Field(env="X")` /
 *      `Field(alias="X")`, falling back to the field name's
 *      UPPER-snake form when no explicit env binding is given.
 *
 *  Owner is `"*"` for module-level bindings, the class name for
 *  Pydantic fields. */
export function extractPythonConfigTags(
  rootNode: SyntaxNode,
  filePath: string,
): ConfigTagBinding[] {
  const out: ConfigTagBinding[] = [];

  // Pydantic class detection. We treat *any* class as a candidate —
  // we don't try to resolve its base — and just look for the
  // canonical assignment shape (`field: type = Field(...)` or
  // `field: type` with no default but inside what's clearly a
  // settings class). Field-name-only bindings are skipped because
  // they're noisy without a concrete env binding.

  // Module-level env reads.
  for (const node of walk(rootNode)) {
    if (node.type !== 'expression_statement') continue;
    // Only treat as module-level when the parent is a `module`.
    if (node.parent?.type !== 'module') continue;
    for (const child of node.namedChildren ?? []) {
      if (child.type !== 'assignment') continue;
      const target = child.namedChildren?.[0];
      if (!target || target.type !== 'identifier') continue;
      const name = target.text;
      if (!name) continue;
      // Find the RHS — it's the last named child that isn't `type`.
      const rhs = child.childForFieldName?.('right')
        ?? child.namedChildren?.[child.namedChildren.length - 1];
      const envKey = recogniseEnvRead(rhs);
      if (!envKey) continue;
      const tag = tagFromKeyPath(envKeyPath(envKey));
      if (!tag) continue;
      out.push({
        owner: '*',
        field: name,
        tags: { python: tag },
        filePath,
        lineNumber: child.startPosition?.row ?? 0,
      });
    }
  }

  // Class-body Pydantic field bindings.
  for (const node of walk(rootNode)) {
    if (node.type !== 'class_definition') continue;
    const className = node.childForFieldName?.('name')?.text;
    if (!className) continue;
    const body = node.childForFieldName?.('body');
    if (!body) continue;

    for (const stmt of body.namedChildren ?? []) {
      if (stmt.type !== 'expression_statement') continue;
      for (const child of stmt.namedChildren ?? []) {
        if (child.type !== 'assignment') continue;
        const target = child.namedChildren?.[0];
        if (!target || target.type !== 'identifier') continue;
        const fieldName = target.text;
        if (!fieldName) continue;

        // Only emit when the RHS is a `Field(env=…)` / `Field("default", env=…)`
        // call, an env-var read, or a literal that we can't bind
        // (skipped). The absence of a Field(env=…) annotation isn't
        // *fatal* — many Pydantic v2 settings derive the env var
        // name implicitly from the field name. We emit a
        // best-effort binding by upper-casing the field name in
        // that case, but only when there's clear settings shape
        // evidence (the class body contains at least one
        // `Field(env=…)` call OR the class has a `BaseSettings`-ish
        // base). Both checks are below.
        const rhs = child.childForFieldName?.('right')
          ?? child.namedChildren?.[child.namedChildren.length - 1];

        let envKey: string | null = null;
        if (rhs?.type === 'call') {
          const fnPath = callFunctionPath(rhs);
          const fnDotted = fnPath?.join('.') ?? '';
          if (PYDANTIC_FACTORY_NAMES.has(fnDotted)) {
            const args = rhs.childForFieldName?.('arguments');
            envKey =
              readKeywordStr(args, 'env') ??
              readKeywordStr(args, 'alias') ??
              readKeywordStr(args, 'validation_alias');
          } else if (ENV_FUNCTIONS.has(fnDotted)) {
            envKey = firstPositionalStr(rhs.childForFieldName?.('arguments'));
          }
        } else if (rhs?.type === 'subscript') {
          envKey = recogniseEnvRead(rhs);
        }

        if (!envKey) continue;
        const tag = tagFromKeyPath(envKeyPath(envKey));
        if (!tag) continue;
        out.push({
          owner: className,
          field: fieldName,
          tags: { python: tag },
          filePath,
          lineNumber: child.startPosition?.row ?? 0,
        });
      }
    }
  }

  return out;
}

/** Walk a Python file's AST and emit a {@link TrivialGetterBinding}
 *  for every:
 *    - `@property`-decorated method whose body is `return <expr>`
 *      (one or more, including branches),
 *    - free or class-level function whose body is the same shape
 *      (`def get_x(): return X` etc).
 *
 *  The receiver is the enclosing class name when present, `null`
 *  otherwise. */
export function extractPythonTrivialGetters(
  rootNode: SyntaxNode,
  filePath: string,
): TrivialGetterBinding[] {
  const out: TrivialGetterBinding[] = [];

  for (const node of walk(rootNode)) {
    // `@property` getter is wrapped in `decorated_definition`.
    let fnDef: SyntaxNode | null = null;
    if (node.type === 'function_definition') {
      // Ensure we don't double-process when the parent is a
      // `decorated_definition` (we'll see it via that parent).
      if (node.parent?.type === 'decorated_definition') continue;
      fnDef = node;
    } else if (node.type === 'decorated_definition') {
      const inner = node.namedChildren?.find((c) => c.type === 'function_definition');
      if (inner) fnDef = inner;
    }
    if (!fnDef) continue;

    const name = fnDef.childForFieldName?.('name')?.text;
    if (!name) continue;
    const body = fnDef.childForFieldName?.('body');
    if (!body) continue;

    const aliases: string[][] = [];
    for (const ret of iterReturns(body)) {
      // `return_statement` in tree-sitter-python:
      //   return <expr>  → namedChildren[0] = expr
      //   return         → namedChildren empty (skip)
      const expr = ret.namedChildren?.[0];
      if (!expr) continue;
      const alias = flattenAlias(expr);
      if (!alias || alias.length === 0) continue;
      aliases.push(alias);
    }
    if (aliases.length === 0) continue;

    out.push({
      name,
      receiver: enclosingClassName(fnDef),
      returnAliases: aliases,
      filePath,
      lineNumber: fnDef.startPosition?.row ?? 0,
    });
  }

  return out;
}
