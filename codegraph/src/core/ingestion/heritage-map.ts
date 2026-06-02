/**
 * Heritage Map
 *
 * Unified inheritance data structure built from accumulated
 * {@link ExtractedHeritage} records **after all chunks complete** (between
 * chunk processing and call resolution). Consumes `ExtractedHeritage[]` and
 * resolves type names to nodeIds via `lookupClassByName`, NOT graph-edge
 * queries.
 *
 * Combines two previously separate concerns:
 * 1. **Parent/ancestor lookup** (MRO-aware method resolution)
 * 2. **Implementor lookup** (interface dispatch — which files contain
 *    classes implementing a given interface)
 */

import type { ExtractedHeritage } from './workers/parse-worker.js';
import type { ResolutionContext } from './resolution-context.js';
import { getLanguageFromFilename } from '../../_shared/index.js';
import { resolveExtendsType } from './heritage-processor.js';

// ---------------------------------------------------------------------------
// Public types
// ---------------------------------------------------------------------------

/** Maximum ancestor chain depth to prevent runaway traversal. */
const MAX_ANCESTOR_DEPTH = 32;

export interface HeritageMap {
  /** Direct parents of `childNodeId` (extends + implements + trait-impl). */
  getParents(childNodeId: string): string[];
  /** Full ancestor chain (BFS, bounded depth, cycle-safe). */
  getAncestors(childNodeId: string): string[];
  /**
   * File paths of classes that directly implement or extend-as-interface the
   * given interface/abstract-class **name**. Replaces the standalone
   * `ImplementorMap` — used by interface-dispatch in call resolution.
   */
  getImplementorFiles(interfaceName: string): ReadonlySet<string>;
}

/** Shared empty set returned when no implementors are found. */
const EMPTY_SET: ReadonlySet<string> = new Set();

// ---------------------------------------------------------------------------
// Builder
// ---------------------------------------------------------------------------

/**
 * Build a HeritageMap from accumulated ExtractedHeritage records.
 *
 * Resolves class/interface/struct/trait names to nodeIds via
 * `ctx.symbols.lookupClassByName`. When a name resolves to multiple
 * candidates, all are recorded (partial-class / cross-file scenario).
 * Unresolvable names are silently skipped — a missing parent is better
 * than a wrong edge.
 *
 * Also builds the implementor index (interface name → implementing file
 * paths) that was previously maintained by `buildImplementorMap` in
 * call-processor.ts.
 */
export const buildHeritageMap = (
  heritage: readonly ExtractedHeritage[],
  ctx: ResolutionContext,
  /**
   * Optional structural implementor index (interface name → implementor file
   * paths) computed by `buildStructuralImplementorFiles`. Merged into the
   * heritage-derived implementor index so interface-dispatch resolution works
   * for languages without an `implements` keyword (notably Go), where the
   * heritage stream yields no implementor entries.
   */
  seededImplementorFiles?: ReadonlyMap<string, ReadonlySet<string>>,
): HeritageMap => {
  // childNodeId → Set<parentNodeId>  (Set to deduplicate cross-chunk duplicates)
  const directParents = new Map<string, Set<string>>();

  // interfaceName → Set<filePath>  (implementor lookup for interface dispatch)
  const implementorFiles = new Map<string, Set<string>>();

  // Seed structural (duck-typed) implementors first; heritage-derived entries
  // below are merged on top into the same sets.
  if (seededImplementorFiles) {
    for (const [interfaceName, files] of seededImplementorFiles) {
      if (files.size === 0) continue;
      let set = implementorFiles.get(interfaceName);
      if (!set) {
        set = new Set();
        implementorFiles.set(interfaceName, set);
      }
      for (const f of files) set.add(f);
    }
  }

  for (const h of heritage) {
    // ── Parent lookup (nodeId-based) ────────────────────────────────
    const childDefs = ctx.symbols.lookupClassByName(h.className);
    const parentDefs = ctx.symbols.lookupClassByName(h.parentName);

    if (childDefs.length > 0 && parentDefs.length > 0) {
      for (const child of childDefs) {
        for (const parent of parentDefs) {
          // Skip self-references
          if (child.nodeId === parent.nodeId) continue;

          let parents = directParents.get(child.nodeId);
          if (!parents) {
            parents = new Set();
            directParents.set(child.nodeId, parents);
          }
          parents.add(parent.nodeId);
        }
      }
    }

    // ── Implementor index (name-based) ──────────────────────────────
    //
    // Known limitation: Rust `kind: 'trait-impl'` entries are intentionally NOT
    // added to the implementor index. Interface dispatch resolution currently
    // does not traverse Rust trait objects, so recording them here would
    // inflate the index without a consumer. Revisit if/when trait-object
    // dispatch is added.
    //
    // Known limitation: `getImplementorFiles` is keyed by interface **name**
    // (string), so two interfaces with the same unqualified name in different
    // packages (e.g. `pkgA.IRepository` vs `pkgB.IRepository`) collide. This
    // matches the behavior of the prior standalone `ImplementorMap` and is
    // not a regression introduced by this consolidation.
    let isImpl = false;
    if (h.kind === 'implements') {
      isImpl = true;
    } else if (h.kind === 'extends') {
      const lang = getLanguageFromFilename(h.filePath);
      if (lang) {
        const { type } = resolveExtendsType(h.parentName, h.filePath, ctx, lang);
        isImpl = type === 'IMPLEMENTS';
      }
    }
    if (isImpl) {
      let files = implementorFiles.get(h.parentName);
      if (!files) {
        files = new Set();
        implementorFiles.set(h.parentName, files);
      }
      files.add(h.filePath);
    }
  }

  // --- Public API ---------------------------------------------------

  const getParents = (childNodeId: string): string[] => {
    const parents = directParents.get(childNodeId);
    return parents ? [...parents] : [];
  };

  const getAncestors = (childNodeId: string): string[] => {
    const result: string[] = [];
    const visited = new Set<string>();
    visited.add(childNodeId); // prevent cycles through the start node

    // BFS with bounded depth
    let frontier = getParents(childNodeId);
    let depth = 0;

    while (frontier.length > 0 && depth < MAX_ANCESTOR_DEPTH) {
      const nextFrontier: string[] = [];
      for (const parentId of frontier) {
        if (visited.has(parentId)) continue;
        visited.add(parentId);
        result.push(parentId);
        // Expand parent's own parents for next level
        const grandparents = directParents.get(parentId);
        if (grandparents) {
          for (const gp of grandparents) {
            if (!visited.has(gp)) nextFrontier.push(gp);
          }
        }
      }
      frontier = nextFrontier;
      depth++;
    }

    return result;
  };

  const getImplementorFiles = (interfaceName: string): ReadonlySet<string> => {
    return implementorFiles.get(interfaceName) ?? EMPTY_SET;
  };

  return { getParents, getAncestors, getImplementorFiles };
};
