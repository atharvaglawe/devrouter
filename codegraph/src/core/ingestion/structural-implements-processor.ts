/**
 * Structural Implements Processor
 *
 * Detects implicit interface implementations for languages that use structural
 * (duck-typed) interface satisfaction — primarily Go and TypeScript — but is
 * also useful as a backstop for any language whose tree-sitter `implements`
 * capture is incomplete (e.g. Python protocols, Ruby `include` of duck-typed
 * modules).
 *
 * For Go the heritage processor never emits IMPLEMENTS, because Go has no
 * `implements` keyword. For TypeScript the heritage processor only sees
 * `class X implements Y { ... }`, missing every `const x: Y = { ... }` /
 * `interface Foo` / "looks-like-a-duck" satisfaction. This processor closes
 * that gap by walking the symbol table and emitting an IMPLEMENTS edge
 * whenever a concrete type's method-set is a superset of an interface's
 * method-set (matched by name + arity).
 *
 * Algorithm:
 *   1. Index Interface → required-method-name set (skip 0-method interfaces;
 *      they would match every concrete type and add only noise).
 *   2. Index concrete-type (Class | Struct | Trait | Record) → owned-method
 *      -name set.
 *   3. For each concrete type, gather candidate interfaces via an inverted
 *      method-name index (avoids the O(types × interfaces) cross product).
 *   4. For each (concrete, interface) candidate pair, accept the match when
 *      every interface method has a same-name method on the concrete type
 *      with a compatible arity. Emit:
 *        - IMPLEMENTS: concrete → interface
 *        - METHOD_IMPLEMENTS: concreteMethod → interfaceMethod (only when the
 *          mapping is unambiguous — exactly one concrete method matches).
 *
 * Idempotency:
 *   - Existing IMPLEMENTS / METHOD_IMPLEMENTS edges (from the heritage
 *     processor or a previous run of this pass) are deduped using the same
 *     `generateId(...)` scheme used elsewhere in the ingestion pipeline.
 *
 * Confidence scale:
 *   - 0.85 — name + arity match on every interface method.
 *   - 0.70 — name-only fallback when both sides report an unknown arity (e.g.
 *     interface methods extracted without parameter info).
 *
 * Why post-MRO:
 *   - We need HAS_METHOD edges to be complete.
 *   - METHOD_OVERRIDES from MRO does not affect structural matching, but
 *     running structural-implements after MRO means new IMPLEMENTS edges are
 *     visible to community detection.
 */

import { KnowledgeGraph } from '../graph/types.js';
import { generateId } from '../../lib/utils.js';
import type { GraphNode, NodeLabel, SupportedLanguages } from '../../_shared/index.js';
import { getProvider } from './languages/index.js';

export interface StructuralImplementsResult {
  implementsEdges: number;
  methodImplementsEdges: number;
  candidateInterfaces: number;
  candidateConcreteTypes: number;
  skippedEmptyInterfaces: number;
  /** Interfaces dropped by per-language `structuralImplementsMinMethods` gate. */
  skippedSmallInterfaces: number;
  /** Interfaces dropped because their language has structural-implements
   *  disabled (Java / Kotlin / C# / PHP — see provider config). */
  skippedDisabledLanguage: number;
}

const DEFAULT_MIN_INTERFACE_METHODS = 1;

interface InterfaceLanguagePolicy {
  enabled: boolean;
  minMethods: number;
}

function policyFor(language: string | undefined): InterfaceLanguagePolicy {
  if (!language) return { enabled: true, minMethods: DEFAULT_MIN_INTERFACE_METHODS };
  try {
    const provider = getProvider(language as SupportedLanguages);
    return {
      enabled: provider.structuralImplementsEnabled !== false,
      minMethods: provider.structuralImplementsMinMethods ?? DEFAULT_MIN_INTERFACE_METHODS,
    };
  } catch {
    // Unknown language — fall back to default. getProvider throws on mismatch.
    return { enabled: true, minMethods: DEFAULT_MIN_INTERFACE_METHODS };
  }
}

interface MethodInfo {
  id: string;
  name: string;
  /** -1 when arity is unknown (variadic or not extracted). */
  arity: number;
}

interface TypeMethodIndex {
  typeId: string;
  language: string | undefined;
  methods: MethodInfo[];
  /** name → arities seen (allows overloads). */
  byName: Map<string, Set<number>>;
}

const CONCRETE_LABELS: ReadonlySet<NodeLabel> = new Set<NodeLabel>([
  'Class',
  'Struct',
  'Trait',
  'Record',
]);
const INTERFACE_LABELS: ReadonlySet<NodeLabel> = new Set<NodeLabel>(['Interface']);

const STRUCTURAL_IMPL_CONFIDENCE_WITH_ARITY = 0.85;
const STRUCTURAL_IMPL_CONFIDENCE_NAME_ONLY = 0.7;

function buildTypeMethodIndex(
  graph: KnowledgeGraph,
  acceptLabels: ReadonlySet<NodeLabel>,
): { types: Map<string, TypeMethodIndex>; methodNodes: Map<string, GraphNode> } {
  const methodNodes = new Map<string, GraphNode>();
  const types = new Map<string, TypeMethodIndex>();

  graph.forEachNode((n) => {
    if (acceptLabels.has(n.label)) {
      const language =
        typeof n.properties.language === 'string' ? n.properties.language : undefined;
      types.set(n.id, {
        typeId: n.id,
        language,
        methods: [],
        byName: new Map(),
      });
    } else if (n.label === 'Method' || n.label === 'Function') {
      methodNodes.set(n.id, n);
    }
  });

  graph.forEachRelationship((rel) => {
    if (rel.type !== 'HAS_METHOD') return;
    const t = types.get(rel.sourceId);
    if (!t) return;
    const m = methodNodes.get(rel.targetId);
    if (!m) return;

    const name = typeof m.properties.name === 'string' ? m.properties.name : '';
    if (!name) return;
    const arity =
      typeof m.properties.parameterCount === 'number' ? m.properties.parameterCount : -1;

    t.methods.push({ id: m.id, name, arity });
    let arities = t.byName.get(name);
    if (!arities) {
      arities = new Set();
      t.byName.set(name, arities);
    }
    arities.add(arity);
  });

  return { types, methodNodes };
}

function edgeKey(rel: { sourceId: string; targetId: string }): string {
  return `${rel.sourceId}->${rel.targetId}`;
}

/**
 * For interfaces that embed/extend other interfaces (Go: `type X interface { Y; Z }`,
 * Java: `interface X extends Y, Z`), expand each interface's method-set to
 * include methods inherited from its parents via EXTENDS / IMPLEMENTS edges.
 *
 * Without this expansion an interface like Go's `ReadCloser interface { Reader;
 * Closer }` has zero direct methods and would be skipped as "empty", missing
 * every concrete implementor.
 */
function expandInterfaceMethodSets(
  graph: KnowledgeGraph,
  interfaceIdx: Map<string, TypeMethodIndex>,
): void {
  // Build parent map: child interface → parent interface IDs.
  const parents = new Map<string, string[]>();
  graph.forEachRelationship((rel) => {
    if (rel.type !== 'EXTENDS' && rel.type !== 'IMPLEMENTS') return;
    if (!interfaceIdx.has(rel.sourceId) || !interfaceIdx.has(rel.targetId)) return;
    let arr = parents.get(rel.sourceId);
    if (!arr) {
      arr = [];
      parents.set(rel.sourceId, arr);
    }
    arr.push(rel.targetId);
  });
  if (parents.size === 0) return;

  // Topological/iterative closure: for each interface, gather all transitive
  // ancestor methods. We bound recursion with a visited set per child to
  // tolerate accidental cycles.
  for (const child of interfaceIdx.values()) {
    const stack = [...(parents.get(child.typeId) ?? [])];
    if (stack.length === 0) continue;
    const visited = new Set<string>();
    while (stack.length > 0) {
      const ancestorId = stack.pop() as string;
      if (visited.has(ancestorId) || ancestorId === child.typeId) continue;
      visited.add(ancestorId);
      const ancestor = interfaceIdx.get(ancestorId);
      if (!ancestor) continue;
      for (const m of ancestor.methods) {
        // Skip duplicates: same name + same arity already present.
        const arities = child.byName.get(m.name);
        if (arities && (arities.has(m.arity) || arities.has(-1))) continue;
        child.methods.push(m);
        let s = child.byName.get(m.name);
        if (!s) {
          s = new Set();
          child.byName.set(m.name, s);
        }
        s.add(m.arity);
      }
      for (const p of parents.get(ancestorId) ?? []) stack.push(p);
    }
  }
}

/**
 * Run structural-implements detection across the full graph.
 *
 * Adds IMPLEMENTS (and best-effort METHOD_IMPLEMENTS) edges where a concrete
 * type's method-set covers an interface's method-set, by name + arity.
 *
 * Safe to call after MRO and before community detection — it only adds edges
 * to existing nodes and is fully idempotent.
 */
export function processStructuralImplements(
  graph: KnowledgeGraph,
): StructuralImplementsResult {
  const concreteIdx = buildTypeMethodIndex(graph, CONCRETE_LABELS).types;
  const interfaceIdx = buildTypeMethodIndex(graph, INTERFACE_LABELS).types;

  // Pull inherited methods from parent interfaces into each interface's
  // method-set so e.g. Go's `ReadCloser interface { Reader; Closer }` has
  // `{Read, Close}` rather than `{}`.
  expandInterfaceMethodSets(graph, interfaceIdx);

  let skippedEmptyInterfaces = 0;
  let skippedSmallInterfaces = 0;
  let skippedDisabledLanguage = 0;
  const usableInterfaces: TypeMethodIndex[] = [];
  for (const ti of interfaceIdx.values()) {
    if (ti.byName.size === 0) {
      skippedEmptyInterfaces++;
      continue;
    }
    // Per-language gate: Java/Kotlin/C#/PHP have explicit `implements`, so
    // the heritage processor already captures every real implementation.
    // Structural-only matches in those languages collide with shared CRUD
    // method names (`create`, `update`, `delete`, `list`) — controllers
    // end up "implementing" every small service interface in the codebase.
    const policy = policyFor(ti.language);
    if (!policy.enabled) {
      skippedDisabledLanguage++;
      continue;
    }
    if (ti.byName.size < policy.minMethods) {
      skippedSmallInterfaces++;
      continue;
    }
    usableInterfaces.push(ti);
  }

  // Track existing IMPLEMENTS / METHOD_IMPLEMENTS edges so we don't duplicate
  // anything heritage-processor or a previous pass already emitted.
  const existingImplements = new Set<string>();
  const existingMethodImplements = new Set<string>();
  graph.forEachRelationship((rel) => {
    if (rel.type === 'IMPLEMENTS') existingImplements.add(edgeKey(rel));
    else if (rel.type === 'METHOD_IMPLEMENTS') existingMethodImplements.add(edgeKey(rel));
  });

  // Inverted index: methodName → interface IDs requiring it. Lets us skip
  // the full O(I × J) cross product per concrete type.
  const methodNameToInterfaces = new Map<string, Set<string>>();
  for (const iface of usableInterfaces) {
    for (const name of iface.byName.keys()) {
      let s = methodNameToInterfaces.get(name);
      if (!s) {
        s = new Set();
        methodNameToInterfaces.set(name, s);
      }
      s.add(iface.typeId);
    }
  }

  let implementsEdges = 0;
  let methodImplementsEdges = 0;

  for (const concrete of concreteIdx.values()) {
    if (concrete.byName.size === 0) continue;

    // Gather candidate interfaces — those that require at least one of this
    // concrete type's method names.
    const candidateIfaceIds = new Set<string>();
    for (const name of concrete.byName.keys()) {
      const ifaces = methodNameToInterfaces.get(name);
      if (!ifaces) continue;
      for (const id of ifaces) candidateIfaceIds.add(id);
    }
    if (candidateIfaceIds.size === 0) continue;

    for (const ifaceId of candidateIfaceIds) {
      const iface = interfaceIdx.get(ifaceId);
      if (!iface) continue;
      if (concrete.typeId === iface.typeId) continue;

      // Cross-language matches are off — Go structs don't structurally
      // implement TypeScript interfaces. When either side has no language
      // tag (older nodes) we allow the match.
      if (concrete.language && iface.language && concrete.language !== iface.language) {
        continue;
      }

      // Every interface method must have a same-name method on the concrete
      // type with a compatible arity.
      let matched = true;
      let usedArity = false;
      let nameOnlyFallback = false;
      for (const [name, ifaceArities] of iface.byName) {
        const concreteArities = concrete.byName.get(name);
        if (!concreteArities) {
          matched = false;
          break;
        }
        let arityHit = false;
        for (const ifaceArity of ifaceArities) {
          if (ifaceArity === -1) {
            // Unknown arity on the interface side → fall back to name match.
            nameOnlyFallback = true;
            arityHit = true;
            break;
          }
          if (concreteArities.has(ifaceArity) || concreteArities.has(-1)) {
            arityHit = true;
            usedArity = true;
            break;
          }
        }
        if (!arityHit) {
          matched = false;
          break;
        }
      }
      if (!matched) continue;

      const confidence =
        usedArity && !nameOnlyFallback
          ? STRUCTURAL_IMPL_CONFIDENCE_WITH_ARITY
          : STRUCTURAL_IMPL_CONFIDENCE_NAME_ONLY;

      const implKey = edgeKey({ sourceId: concrete.typeId, targetId: iface.typeId });
      if (!existingImplements.has(implKey)) {
        graph.addRelationship({
          id: generateId('IMPLEMENTS', implKey),
          sourceId: concrete.typeId,
          targetId: iface.typeId,
          type: 'IMPLEMENTS',
          confidence,
          reason: 'structural',
        });
        existingImplements.add(implKey);
        implementsEdges++;
      }

      // Best-effort METHOD_IMPLEMENTS — only when the mapping is unambiguous
      // (exactly one concrete method with the same name + matching arity),
      // otherwise we'd risk wrong edges between overloads.
      for (const ifaceMethod of iface.methods) {
        const matchingConcrete: MethodInfo[] = [];
        for (const cm of concrete.methods) {
          if (cm.name !== ifaceMethod.name) continue;
          if (
            ifaceMethod.arity === -1 ||
            cm.arity === -1 ||
            cm.arity === ifaceMethod.arity
          ) {
            matchingConcrete.push(cm);
          }
        }
        if (matchingConcrete.length !== 1) continue;
        const cm = matchingConcrete[0];
        const mKey = edgeKey({ sourceId: cm.id, targetId: ifaceMethod.id });
        if (existingMethodImplements.has(mKey)) continue;
        graph.addRelationship({
          id: generateId('METHOD_IMPLEMENTS', mKey),
          sourceId: cm.id,
          targetId: ifaceMethod.id,
          type: 'METHOD_IMPLEMENTS',
          confidence,
          reason: 'structural',
        });
        existingMethodImplements.add(mKey);
        methodImplementsEdges++;
      }
    }
  }

  return {
    implementsEdges,
    methodImplementsEdges,
    candidateInterfaces: usableInterfaces.length,
    candidateConcreteTypes: concreteIdx.size,
    skippedEmptyInterfaces,
    skippedSmallInterfaces,
    skippedDisabledLanguage,
  };
}
