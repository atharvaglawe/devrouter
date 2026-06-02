/**
 * Unit tests for the URL-via-getter in-code constant resolver.
 *
 * Two layers under test:
 *   1. `extractGoStringConsts` — package-level string const/var
 *      harvesting from Go AST (single, grouped, multi-name, var).
 *   2. `buildResolvedGetterConstURLs` — folds string consts +
 *      trivial getters into a `(receiver|"*"::name) → Set<literal>`
 *      map, resolving const refs and recursing nested getters with a
 *      cycle guard.
 */

import { describe, it, expect } from 'vitest';
import Parser from 'tree-sitter';
import Go from 'tree-sitter-go';
import {
  extractGoStringConsts,
  buildResolvedGetterConstURLs,
} from '../../src/core/ingestion/route-extractors/url-const-resolver.js';
import {
  extractGoTrivialGetters,
  getterKey,
} from '../../src/core/ingestion/route-extractors/config-tag-resolver.js';

function parseGo(src: string) {
  const parser = new Parser();
  parser.setLanguage(Go);
  return parser.parse(src);
}

// ─────────────────────────────────────────────────────────────────
// extractGoStringConsts
// ─────────────────────────────────────────────────────────────────

describe('extractGoStringConsts', () => {
  it('captures a single const string', () => {
    const tree = parseGo(`
package constant
const DefaultPath = "/trf"
`);
    const consts = extractGoStringConsts(tree.rootNode, 'constant.go');
    expect(consts).toHaveLength(1);
    expect(consts[0]).toMatchObject({ name: 'DefaultPath', value: '/trf' });
  });

  it('captures grouped const blocks', () => {
    const tree = parseGo(`
package constant
const (
  DefaultClickTransferDomain = "clks.example.net"
  DefaultPath                = "/trf"
)
`);
    const consts = extractGoStringConsts(tree.rootNode, 'constant.go');
    const byName = Object.fromEntries(consts.map((c) => [c.name, c.value]));
    expect(byName.DefaultPath).toBe('/trf');
    expect(byName.DefaultClickTransferDomain).toBe('clks.example.net');
  });

  it('captures multi-name specs positionally', () => {
    const tree = parseGo(`
package constant
const A, B = "/a", "/b"
`);
    const consts = extractGoStringConsts(tree.rootNode, 'constant.go');
    const byName = Object.fromEntries(consts.map((c) => [c.name, c.value]));
    expect(byName.A).toBe('/a');
    expect(byName.B).toBe('/b');
  });

  it('captures var string declarations', () => {
    const tree = parseGo(`
package constant
var FallbackPath = "/fallback"
`);
    const consts = extractGoStringConsts(tree.rootNode, 'constant.go');
    expect(consts.find((c) => c.name === 'FallbackPath')?.value).toBe('/fallback');
  });

  it('ignores non-string consts', () => {
    const tree = parseGo(`
package constant
const MaxRetries = 3
const Enabled = true
`);
    const consts = extractGoStringConsts(tree.rootNode, 'constant.go');
    expect(consts).toHaveLength(0);
  });
});

// ─────────────────────────────────────────────────────────────────
// buildResolvedGetterConstURLs
// ─────────────────────────────────────────────────────────────────

describe('buildResolvedGetterConstURLs', () => {
  it('resolves a getter that returns a package constant', () => {
    const constsTree = parseGo(`
package constant
const DefaultPath = "/trf"
`);
    const getterTree = parseGo(`
package defaultpath
func (d *defaultPath) GetPath() string { return constant.DefaultPath }
`);
    const consts = extractGoStringConsts(constsTree.rootNode, 'constant.go');
    const getters = extractGoTrivialGetters(getterTree.rootNode, 'defaultpath.go');

    const resolved = buildResolvedGetterConstURLs(consts, getters);
    // Output is receiver-scoped only (no wildcard key).
    expect([...(resolved.get(getterKey('defaultPath', 'GetPath')) ?? [])]).toContain('/trf');
    expect(resolved.get(getterKey(null, 'GetPath'))).toBeUndefined();
  });

  it('recurses a delegating getter (interface-field hop) to the concrete const', () => {
    const constsTree = parseGo(`
package constant
const DefaultPath = "/trf"
`);
    // AdClickRoute.GetPath delegates to a.pathSelector.GetPath(); the
    // concrete defaultPath.GetPath returns the constant. Name-based
    // recursion bridges the interface field without concrete-type
    // inference.
    const getterTree = parseGo(`
package adclickroute
func (a *AdClickRoute) GetPath() string { return a.pathSelector.GetPath() }
func (d *defaultPath) GetPath() string { return constant.DefaultPath }
`);
    const consts = extractGoStringConsts(constsTree.rootNode, 'constant.go');
    const getters = extractGoTrivialGetters(getterTree.rootNode, 'adclickroute.go');

    const resolved = buildResolvedGetterConstURLs(consts, getters);
    expect([...(resolved.get(getterKey('AdClickRoute', 'GetPath')) ?? [])]).toContain('/trf');
  });

  it('terminates on a delegation cycle', () => {
    const constsTree = parseGo(`
package constant
const P = "/p"
`);
    // Mutual delegation A.Get -> B.Get -> A.Get. Cycle guard must
    // prevent infinite recursion; neither resolves to a const.
    const getterTree = parseGo(`
package x
func (a *A) Get() string { return b.Get() }
func (b *B) Get() string { return a.Get() }
`);
    const consts = extractGoStringConsts(constsTree.rootNode, 'constant.go');
    const getters = extractGoTrivialGetters(getterTree.rootNode, 'x.go');
    const resolved = buildResolvedGetterConstURLs(consts, getters);
    // No const reachable — empty (or absent) entry, and no hang.
    expect((resolved.get(getterKey('A', 'Get')) ?? new Set()).size).toBe(0);
    expect((resolved.get(getterKey('B', 'Get')) ?? new Set()).size).toBe(0);
  });

  it('keeps same-named getters on different receivers separate (no cross-contamination)', () => {
    const constsTree = parseGo(`
package constant
const DefaultPath = "/trf"
const OtherPath = "/notroute"
`);
    const getterTree = parseGo(`
package p
func (d *defaultPath) GetPath() string { return constant.DefaultPath }
func (o *otherPath) GetPath() string { return constant.OtherPath }
`);
    const consts = extractGoStringConsts(constsTree.rootNode, 'constant.go');
    const getters = extractGoTrivialGetters(getterTree.rootNode, 'p.go');
    const resolved = buildResolvedGetterConstURLs(consts, getters);
    expect([...(resolved.get(getterKey('defaultPath', 'GetPath')) ?? [])]).toEqual(['/trf']);
    expect([...(resolved.get(getterKey('otherPath', 'GetPath')) ?? [])]).toEqual(['/notroute']);
    // Receiver-scoped only — no wildcard merge.
    expect(resolved.get(getterKey(null, 'GetPath'))).toBeUndefined();
  });
});
