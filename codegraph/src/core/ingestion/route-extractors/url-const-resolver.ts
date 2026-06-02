/**
 * URL-via-getter resolver — in-code string-constant variant.
 *
 * Sibling to {@link file://./config-tag-resolver.ts}. That module
 * chases a Go getter chain to a **YAML/JSON config key** (then to a
 * literal URL via the provider index). This module chases the same
 * trivial-getter chains to an **in-code string constant** instead:
 *
 *   urlBuilder.SetPath(path)                       // call site
 *   path := c.adClickRouteService.GetPath()        // → lookup {name:"GetPath"}
 *   func (a *AdClickRoute) GetPath() string {
 *       return a.pathSelector.GetPath()            // → recurse "GetPath"
 *   }
 *   func (d *defaultPath) GetPath() string {
 *       return constant.DefaultPath                // → const ref "DefaultPath"
 *   }
 *   const DefaultPath = "/trf"                      // → literal "/trf"
 *
 * The result is a `(receiver|"*")::method → Set<literal>` map, keyed
 * exactly like {@link ResolvedGetterMap} so Phase 3.4c in
 * `pipeline.ts` can reuse {@link getterKey} for lookups and the same
 * `pendingGetterLookups` carrier already populated by the Go API
 * extractor.
 *
 * Resolution is **name-based with a cycle guard**: a getter's return
 * alias is interpreted both as a possible constant reference (its
 * leaf token names a known string const) and as a possible nested
 * getter call (its leaf token names another trivial getter, recursed
 * with a visited-set of getter keys). This implicitly covers the
 * interface-field hop (`a.pathSelector.GetPath()` → the concrete
 * `defaultPath.GetPath`) without per-field concrete-type inference —
 * the unique implementor whose method resolves to a constant is
 * reached by name. Precision is preserved downstream: the secondary
 * URL matcher only emits a `FETCHES` edge when the recovered literal
 * matches a registered route, so constants that aren't routes (or
 * coincidental same-named getters) never create edges.
 */

import type { SyntaxNode } from '../utils/ast-helpers.js';
import {
  getterKey,
  type ResolvedGetterMap,
  type TrivialGetterBinding,
} from './config-tag-resolver.js';

// ─────────────────────────────────────────────────────────────────
// Public types
// ─────────────────────────────────────────────────────────────────

/** A package-level `const`/`var` bound to a string literal
 *  (e.g. `const DefaultPath = "/trf"`). */
export interface GoStringConst {
  /** Identifier name (e.g. `"DefaultPath"`). */
  name: string;
  /** The string literal value with quotes stripped (e.g. `"/trf"`). */
  value: string;
  filePath: string;
  /** 0-indexed line of the spec. */
  lineNumber: number;
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

/** Strip the wrapping quotes/backticks from a Go string literal node.
 *  Returns null for non-string nodes. */
function readStringLiteral(node: SyntaxNode | null | undefined): string | null {
  if (!node) return null;
  if (
    node.type !== 'interpreted_string_literal' &&
    node.type !== 'raw_string_literal'
  ) {
    return null;
  }
  const raw = node.text ?? '';
  if (raw.length < 2) return null;
  return raw.slice(1, -1);
}

// ─────────────────────────────────────────────────────────────────
// Per-file extractor
// ─────────────────────────────────────────────────────────────────

/** Walk a Go file's AST and emit every package-level `const`/`var`
 *  spec whose value is a single string literal. Grouped declarations
 *  (`const ( A = "x"; B = "y" )`) and multi-name specs
 *  (`const A, B = "x", "y"`) are handled by pairing names to values
 *  positionally. Non-string values are skipped. */
export function extractGoStringConsts(
  rootNode: SyntaxNode,
  filePath: string,
): GoStringConst[] {
  const out: GoStringConst[] = [];

  for (const node of walk(rootNode)) {
    if (node.type !== 'const_spec' && node.type !== 'var_spec') continue;

    // Names: every `identifier` child before the value/type. Use the
    // `name`-field children when the grammar exposes them; otherwise
    // fall back to leading identifier namedChildren.
    const names: SyntaxNode[] = [];
    const valueList = node.childForFieldName?.('value') ?? null;
    for (const c of node.namedChildren ?? []) {
      if (c === valueList) break;
      if (c.type === 'identifier') names.push(c);
    }
    if (names.length === 0) continue;
    if (!valueList) continue;

    const valueExprs = (valueList.namedChildren ?? []).filter((c) => c.isNamed === true);
    const lineNumber = node.startPosition?.row ?? 0;
    for (let i = 0; i < names.length; i++) {
      const value = readStringLiteral(valueExprs[i]);
      if (value === null) continue;
      const name = names[i].text;
      if (!name) continue;
      out.push({ name, value, filePath, lineNumber });
    }
  }

  return out;
}

// ─────────────────────────────────────────────────────────────────
// Repo-wide resolver
// ─────────────────────────────────────────────────────────────────

/** Depth cap for getter→getter recursion. Mirrors the bound used by
 *  `buildResolvedGetters` in config-tag-resolver.ts. Deep enough for
 *  realistic delegation chains (builder → service → selector → impl)
 *  while preventing pathological blowups. */
const MAX_GETTER_DEPTH = 8;

/** Directory portion of a file path (`a/b/c.go` → `a/b`). */
function dirOf(filePath: string): string {
  const i = filePath.lastIndexOf('/');
  return i >= 0 ? filePath.slice(0, i) : '';
}

/** Is `candidatePath` inside (or equal to) the package directory
 *  `scopeDir`? Used to bound getter→getter recursion to the receiver
 *  type's package subtree, so a delegating getter
 *  (`AdClickRoute.GetPath` → `a.pathSelector.GetPath()`) only follows
 *  same-package-tree implementors (`.../adclickroute/.../defaultpath`)
 *  and never a same-named getter from an unrelated package. */
function isWithinScope(candidatePath: string, scopeDir: string): boolean {
  if (scopeDir === '') return dirOf(candidatePath) === '';
  return candidatePath === scopeDir || candidatePath.startsWith(scopeDir + '/');
}

/** Fold every Go file's string constants + trivial getters into a
 *  `receiver::method → Set<literal>` map.
 *
 *  For each trivial getter, every return alias is resolved by treating
 *  its **leaf token** as either:
 *    - a string-constant name → contributes the constant's value
 *      (matched by name; consts are not locality-scoped), or
 *    - a nested getter name → recurse into those getters that live
 *      within the root getter's package subtree (guarded by a
 *      visited-set of `getterKey`s so delegation cycles terminate).
 *
 *  Output keys are receiver-scoped ONLY (`getterKey(receiver, name)`):
 *  a free-function/`"*"` wildcard would re-introduce the cross-repo
 *  name explosion this resolver exists to avoid. The call site must
 *  supply a resolved receiver type (see `tryClientUrlBuilder`). */
export function buildResolvedGetterConstURLs(
  stringConsts: ReadonlyArray<GoStringConst>,
  trivialGetters: ReadonlyArray<TrivialGetterBinding>,
): ResolvedGetterMap {
  // const name → set of literal values.
  const constByName = new Map<string, Set<string>>();
  for (const c of stringConsts) {
    let set = constByName.get(c.name);
    if (!set) {
      set = new Set<string>();
      constByName.set(c.name, set);
    }
    set.add(c.value);
  }

  // getters indexed by bare name (for leaf recursion).
  const gettersByName = new Map<string, TrivialGetterBinding[]>();
  for (const g of trivialGetters) {
    const list = gettersByName.get(g.name);
    if (list) list.push(g);
    else gettersByName.set(g.name, [g]);
  }

  /** Resolve a single getter's aliases into the literal-value set,
   *  following nested getters only within `scopeDir`. */
  const resolveGetter = (
    g: TrivialGetterBinding,
    scopeDir: string,
    visited: Set<string>,
    depth: number,
    out: Set<string>,
  ): void => {
    if (depth > MAX_GETTER_DEPTH) return;
    for (const alias of g.returnAliases) {
      if (alias.length === 0) continue;
      const leaf = alias[alias.length - 1];

      // (a) Constant reference: `return constant.DefaultPath` →
      //     alias leaf `DefaultPath` names a known string const.
      const constVals = constByName.get(leaf);
      if (constVals) for (const v of constVals) out.add(v);

      // (b) Nested getter call: `return a.pathSelector.GetPath()` →
      //     alias leaf `GetPath` names another trivial getter within
      //     the same package subtree.
      const nested = gettersByName.get(leaf);
      if (nested) {
        for (const ng of nested) {
          if (!isWithinScope(ng.filePath, scopeDir)) continue;
          const nk = getterKey(ng.receiver, ng.name);
          if (visited.has(nk)) continue;
          visited.add(nk);
          resolveGetter(ng, scopeDir, visited, depth + 1, out);
        }
      }
    }
  };

  const result: ResolvedGetterMap = new Map();
  for (const g of trivialGetters) {
    if (g.receiver === null) continue; // receiver-scoped output only
    const out = new Set<string>();
    const scopeDir = dirOf(g.filePath);
    const visited = new Set<string>([getterKey(g.receiver, g.name)]);
    resolveGetter(g, scopeDir, visited, 0, out);
    if (out.size === 0) continue;
    const key = getterKey(g.receiver, g.name);
    let set = result.get(key);
    if (!set) {
      set = new Set<string>();
      result.set(key, set);
    }
    for (const v of out) set.add(v);
  }
  return result;
}
