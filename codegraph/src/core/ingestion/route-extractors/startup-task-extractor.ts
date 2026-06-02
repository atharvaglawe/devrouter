/**
 * Go startup/cron background-task recognizer.
 *
 * Many goserving binaries warm caches / run periodic refreshers through a
 * shared task framework:
 *
 *   // main.go (or a helper it calls)
 *   startup.RegisterTasks(smartcache.GetStartuptasks())
 *
 *   // startuptasks.go — slice literal naming CONCRETE task constructors
 *   func GetStartuptasks() []startup.StartupTaskInterface {
 *       return []startup.StartupTaskInterface{
 *           jsversiontask.GetTask(...),   // -> *jsVersionTask
 *           plutotask.GetTask(...),       // -> *task
 *       }
 *   }
 *
 *   // jsversiontask/task.go — the lifecycle methods the framework invokes
 *   func (t *jsVersionTask) StartupRun()  { ... fetches /scrr.php etc ... }
 *   func (t *jsVersionTask) PeriodicRun() { ... }
 *
 * The framework dispatches `StartupRun`/`PeriodicRun` through the
 * `StartupTaskInterface` — an interface hop the `CALLS` graph does not
 * model — so those lifecycle methods (and everything they fetch) are
 * orphaned from any `main`. This recognizer recovers the missing link.
 *
 * It is deliberately **shape-based and repo-name-agnostic**: it keys on
 * the framework's interface type name (`StartupTaskInterface`) and the
 * fixed lifecycle method names (`StartupRun`, `PeriodicRun`), not on any
 * package or repo identity.
 *
 * Target resolution uses **package co-location** rather than fragile
 * package-qualified return-type inference: a slice element
 * `pkg.GetTask(...)` is resolved to its defining package directory (the
 * import qualifier matches the Go package directory's basename), and the
 * lifecycle methods declared in that same directory are linked. The
 * concrete task type and its `GetTask` constructor always live together,
 * so this is precise without per-type inference. See
 * {@link buildStartupTaskEdges}.
 */

import { generateId } from '../../../lib/utils.js';
import type { SyntaxNode } from '../utils/ast-helpers.js';

// ─────────────────────────────────────────────────────────────────
// Public types
// ─────────────────────────────────────────────────────────────────

/** The framework interface whose element type anchors detection. */
const TASK_INTERFACE_NAME = 'StartupTaskInterface';

/** Lifecycle methods the framework invokes on each registered task. */
export const LIFECYCLE_METHODS = ['StartupRun', 'PeriodicRun'] as const;

/** A constructor-call slice element, e.g. `jsversiontask.GetTask(...)`. */
export interface TaskConstructorRef {
  /** Called function name (e.g. `"GetTask"`). */
  calleeName: string;
  /** Package qualifier when the call is a selector (`pkg.GetTask`). */
  qualifier?: string;
}

/** A direct composite slice element, e.g. `&SprigTask{}` / `pkg.Foo{}`. */
export interface TaskTypeRef {
  typeName: string;
  qualifier?: string;
}

/** One `[]startup.StartupTaskInterface{ ... }` registration site. */
export interface StartupTaskRegistration {
  filePath: string;
  /** Node id of the enclosing top-level function holding the slice literal
   *  (e.g. `Function:<file>:GetStartuptasks`). This is the synthetic edge's
   *  source — it is already on a real CALLS path from the binary `main`. */
  sourceId: string;
  /** Constructor-call elements (`pkg.GetTask(...)`). */
  constructorRefs: TaskConstructorRef[];
  /** Direct composite-literal elements (`&Foo{}` / `Foo{}`). */
  typeRefs: TaskTypeRef[];
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

/** Resolve a Go type node to its base name + optional package qualifier,
 *  stripping pointers and unwrapping generics/qualified types. */
function typeBaseAndQualifier(
  node: SyntaxNode | null | undefined,
): { name: string; qualifier?: string } | null {
  let n: SyntaxNode | null | undefined = node;
  while (n && n.type === 'pointer_type') n = n.firstNamedChild;
  if (!n) return null;
  if (n.type === 'qualified_type') {
    const pkg = n.childForFieldName?.('package')?.text;
    const nm = n.childForFieldName?.('name')?.text;
    if (nm) return pkg ? { name: nm, qualifier: pkg } : { name: nm };
    return null;
  }
  if (n.type === 'type_identifier' || n.type === 'identifier') {
    return n.text ? { name: n.text } : null;
  }
  if (n.type === 'generic_type') {
    const base = n.childForFieldName?.('type') ?? n.firstNamedChild;
    return base ? typeBaseAndQualifier(base) : null;
  }
  return null;
}

/** If `composite` is a `[]X{...}` / `[N]X{...}` whose element base type is
 *  the framework interface, return its element type info; else null. */
function sliceElementTypeIfTaskList(
  composite: SyntaxNode,
): { name: string } | null {
  const typeNode = composite.childForFieldName?.('type');
  if (!typeNode) return null;
  if (typeNode.type !== 'slice_type' && typeNode.type !== 'array_type') return null;
  const elem = typeNode.childForFieldName?.('element') ?? null;
  const base = typeBaseAndQualifier(elem);
  if (!base || base.name !== TASK_INTERFACE_NAME) return null;
  return { name: base.name };
}

/** Unwrap a `literal_value` element wrapper to its value expression. */
function elementExpr(el: SyntaxNode): SyntaxNode {
  if (el.type === 'keyed_element') {
    const kids = el.namedChildren ?? [];
    return kids[kids.length - 1] ?? el;
  }
  if (el.type === 'literal_element') {
    return el.firstNamedChild ?? el;
  }
  return el;
}

/** Classify a slice element expression into a constructor-call or
 *  direct-type reference (or null when it is an unsupported form such as
 *  a bare variable, which we cannot resolve). */
function classifyElement(
  expr: SyntaxNode,
): { call?: TaskConstructorRef; type?: TaskTypeRef } | null {
  if (expr.type === 'call_expression') {
    const fn = expr.childForFieldName?.('function');
    if (!fn) return null;
    if (fn.type === 'selector_expression') {
      const operand = fn.childForFieldName?.('operand');
      const field = fn.childForFieldName?.('field');
      const callee = field?.text;
      if (!callee) return null;
      const qualifier = operand?.type === 'identifier' ? operand.text : undefined;
      return { call: qualifier ? { calleeName: callee, qualifier } : { calleeName: callee } };
    }
    if (fn.type === 'identifier' && fn.text) {
      return { call: { calleeName: fn.text } };
    }
    return null;
  }
  if (expr.type === 'unary_expression') {
    // `&Foo{}`
    const operand = expr.childForFieldName?.('operand');
    if (operand?.type === 'composite_literal') {
      const tq = typeBaseAndQualifier(operand.childForFieldName?.('type'));
      if (tq) return { type: tq.qualifier ? { typeName: tq.name, qualifier: tq.qualifier } : { typeName: tq.name } };
    }
    return null;
  }
  if (expr.type === 'composite_literal') {
    const tq = typeBaseAndQualifier(expr.childForFieldName?.('type'));
    if (tq) return { type: tq.qualifier ? { typeName: tq.name, qualifier: tq.qualifier } : { typeName: tq.name } };
    return null;
  }
  return null;
}

/** Compute the enclosing top-level function's node id for `node`.
 *  Returns null when the nearest function-like ancestor is a method or
 *  closure (those registration shapes are not observed and their ids are
 *  arity-/receiver-sensitive — degrade gracefully rather than guess). */
function enclosingFunctionId(node: SyntaxNode, filePath: string): string | null {
  let cur: SyntaxNode | null | undefined = node.parent;
  while (cur) {
    if (cur.type === 'function_declaration') {
      const nm = cur.childForFieldName?.('name')?.text;
      return nm ? generateId('Function', `${filePath}:${nm}`) : null;
    }
    if (cur.type === 'method_declaration' || cur.type === 'func_literal') {
      return null;
    }
    cur = cur.parent;
  }
  return null;
}

// ─────────────────────────────────────────────────────────────────
// Per-file extractor
// ─────────────────────────────────────────────────────────────────

/** Find every `[]startup.StartupTaskInterface{ ... }` slice literal in a Go
 *  file and capture its enclosing function plus the concrete task
 *  constructors / types named in the slice. */
export function extractGoStartupTaskRegistrations(
  rootNode: SyntaxNode,
  filePath: string,
): StartupTaskRegistration[] {
  const out: StartupTaskRegistration[] = [];

  for (const node of walk(rootNode)) {
    if (node.type !== 'composite_literal') continue;
    if (!sliceElementTypeIfTaskList(node)) continue;

    const sourceId = enclosingFunctionId(node, filePath);
    if (!sourceId) continue;

    const body = node.childForFieldName?.('body');
    if (!body) continue;

    const constructorRefs: TaskConstructorRef[] = [];
    const typeRefs: TaskTypeRef[] = [];
    for (const el of body.namedChildren ?? []) {
      const classified = classifyElement(elementExpr(el));
      if (!classified) continue;
      if (classified.call) constructorRefs.push(classified.call);
      if (classified.type) typeRefs.push(classified.type);
    }

    if (constructorRefs.length === 0 && typeRefs.length === 0) continue;
    out.push({ filePath, sourceId, constructorRefs, typeRefs });
  }

  return out;
}

// ─────────────────────────────────────────────────────────────────
// Repo-wide resolver
// ─────────────────────────────────────────────────────────────────

/** Minimal symbol-table surface the resolver needs (satisfied by the
 *  ingestion `SymbolTable`). */
export interface StartupTaskSymbolLookup {
  lookupCallableByName(name: string): ReadonlyArray<{ filePath: string; nodeId: string }>;
  lookupClassByName(name: string): ReadonlyArray<{ filePath: string; nodeId: string }>;
}

/** A synthesized lifecycle edge (a `CALLS` relationship). */
export interface StartupTaskEdge {
  id: string;
  sourceId: string;
  targetId: string;
  type: 'CALLS';
  confidence: number;
  reason: string;
}

/** Confidence for synthetic lifecycle edges. Banded low-ish: the link is
 *  framework-mediated (interface dispatch) and recovered by co-location,
 *  one hop removed from a direct call. */
const STARTUP_TASK_EDGE_CONFIDENCE = 0.75;

/** Slightly lower confidence for the subtree fallback (wrapper
 *  indirection): the registered element resolved to a package that has no
 *  lifecycle methods of its own, so we reached into its subtree. */
const STARTUP_TASK_SUBTREE_CONFIDENCE = 0.7;

const EDGE_REASON = 'startup-task-lifecycle';

/** Directory portion of a file path (`a/b/c.go` → `a/b`). */
function dirOf(filePath: string): string {
  const p = filePath.replace(/\\/g, '/');
  const i = p.lastIndexOf('/');
  return i >= 0 ? p.slice(0, i) : '';
}

/** Final path segment of a directory (`a/b/jsversiontask` → `jsversiontask`). */
function baseName(dir: string): string {
  const i = dir.lastIndexOf('/');
  return i >= 0 ? dir.slice(i + 1) : dir;
}

/** Fold registration sites + the symbol table into synthetic `CALLS` edges
 *  from each registration's enclosing function to the co-located lifecycle
 *  methods (`StartupRun`/`PeriodicRun`) of the registered task packages.
 *
 *  Disambiguation is conservative: a constructor element only contributes
 *  its package directory when it resolves unambiguously — a package
 *  qualifier matching the directory basename, or a single global
 *  definition for an unqualified call. Ambiguous elements are skipped
 *  (no edge) rather than fanning out to every same-named constructor. */
export function buildStartupTaskEdges(
  registrations: ReadonlyArray<StartupTaskRegistration>,
  symbols: StartupTaskSymbolLookup,
): StartupTaskEdge[] {
  if (registrations.length === 0) return [];

  // Pre-index lifecycle method definitions by directory.
  const lifecycleByDir = new Map<string, Array<{ method: string; nodeId: string }>>();
  for (const method of LIFECYCLE_METHODS) {
    for (const def of symbols.lookupCallableByName(method)) {
      const dir = dirOf(def.filePath);
      let list = lifecycleByDir.get(dir);
      if (!list) {
        list = [];
        lifecycleByDir.set(dir, list);
      }
      list.push({ method, nodeId: def.nodeId });
    }
  }
  if (lifecycleByDir.size === 0) return [];

  /** Resolve a constructor/type element to the package directory that
   *  defines it, or null when it cannot be pinned unambiguously. */
  const resolveDir = (
    name: string,
    qualifier: string | undefined,
    defs: ReadonlyArray<{ filePath: string }>,
  ): string | null => {
    if (defs.length === 0) return null;
    if (qualifier) {
      const matched = defs.filter((d) => baseName(dirOf(d.filePath)) === qualifier);
      if (matched.length === 0) return null;
      // All matches should share a directory; if not, ambiguous → skip.
      const dirs = new Set(matched.map((d) => dirOf(d.filePath)));
      return dirs.size === 1 ? [...dirs][0]! : null;
    }
    // Unqualified: only safe when there is exactly one definition.
    if (defs.length !== 1) return null;
    return dirOf(defs[0]!.filePath);
  };

  const lifecycleDirs = [...lifecycleByDir.keys()];
  const edges: StartupTaskEdge[] = [];
  const seen = new Set<string>();

  for (const reg of registrations) {
    const dirs = new Set<string>();

    for (const c of reg.constructorRefs) {
      const dir = resolveDir(c.calleeName, c.qualifier, symbols.lookupCallableByName(c.calleeName));
      if (dir !== null) dirs.add(dir);
    }
    for (const t of reg.typeRefs) {
      const dir = resolveDir(t.typeName, t.qualifier, symbols.lookupClassByName(t.typeName));
      if (dir !== null) dirs.add(dir);
    }

    for (const dir of dirs) {
      let lifecycle = lifecycleByDir.get(dir);
      let confidence = STARTUP_TASK_EDGE_CONFIDENCE;

      // Wrapper indirection: the registered element is a single-interface
      // factory (e.g. `serpjs.GetStartupTasks()` → `return cron.GetTask()`)
      // that lives one package ABOVE the concrete task. The resolved
      // package then has no lifecycle methods of its own — fall back to
      // lifecycle methods declared anywhere in its package SUBTREE. Gated
      // on the direct lookup being empty so normal co-located tasks never
      // fan out to sibling subpackages.
      if (!lifecycle || lifecycle.length === 0) {
        const prefix = dir + '/';
        const collected: Array<{ method: string; nodeId: string }> = [];
        for (const ld of lifecycleDirs) {
          if (!ld.startsWith(prefix)) continue;
          const m = lifecycleByDir.get(ld);
          if (m) collected.push(...m);
        }
        if (collected.length === 0) continue;
        lifecycle = collected;
        confidence = STARTUP_TASK_SUBTREE_CONFIDENCE;
      }

      for (const { method, nodeId } of lifecycle) {
        const id = generateId('CALLS', `${reg.sourceId}:${method}->${nodeId}`);
        if (seen.has(id)) continue;
        seen.add(id);
        edges.push({
          id,
          sourceId: reg.sourceId,
          targetId: nodeId,
          type: 'CALLS',
          confidence,
          reason: EDGE_REASON,
        });
      }
    }
  }

  return edges;
}
