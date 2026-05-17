/**
 * Integration-flavoured tests for the Go options-bag → pending-getter
 * path. Exercises the full extract → resolve → backfill chain so a
 * regression in any one layer surfaces through this single fixture.
 *
 * The fixture mirrors the real `goserving` shape that motivated this
 * work — `oscar` calls `abtestv2` via an `ApiConfig{Path: …, Host: …,
 * Protocol: …}` literal whose three fields all delegate to
 * `config.GetABTestApiConfig()`. The struct field is YAML-tagged
 * `abtestapi`; a provider-resolver index that knows about that tag
 * should be enough for Phase 3.4 to recover the providerTag.
 */

import { describe, it, expect } from 'vitest';
import Parser from 'tree-sitter';
import Go from 'tree-sitter-go';
import { extractGoApiEndpoints } from '../../src/core/ingestion/route-extractors/api-endpoint-go.js';
import {
  extractGoConfigTags,
  extractGoTrivialGetters,
  buildResolvedGetters,
  getterKey,
} from '../../src/core/ingestion/route-extractors/config-tag-resolver.js';

function parseGo(src: string) {
  const parser = new Parser();
  parser.setLanguage(Go);
  return parser.parse(src);
}

describe('options-bag pending-getter resolution', () => {
  // Single-file fixture that mirrors the real shape: a config
  // package declares the struct + getters; a caller package builds
  // the options bag with the chained accessor.
  const FIXTURE = `
package x

// — config layer —
type StandardApiConfig struct {
  ApiHost  string \`yaml:"host"\`
  ApiPath  string \`yaml:"path"\`
  Protocol string \`yaml:"protocol"\`
}
type Config struct {
  ABTestApiConfig StandardApiConfig \`yaml:"abtestapi"\`
  KosmosApiConfig StandardApiConfig \`yaml:"kosmos"\`
}
var selected Config
func GetABTestApiConfig() StandardApiConfig {
  return selected.ABTestApiConfig
}
func GetKosmosApiConfig() StandardApiConfig {
  return selected.KosmosApiConfig
}

// — caller layer —
type ApiConfig struct {
  Method   string
  Host     string
  Path     string
  Protocol string
}

func CallAbtest() {
  cfg := ApiConfig{
    Method:   "POST",
    Path:     GetABTestApiConfig().ApiPath,
    Host:     GetABTestApiConfig().ApiHost,
    Protocol: GetABTestApiConfig().Protocol,
  }
  _ = cfg
}
`;

  it('emits a ClientCall with pending-getter lookups when URL/host are non-literal', () => {
    const tree = parseGo(FIXTURE);
    const api = extractGoApiEndpoints(tree.rootNode, 'x.go');
    // Should have at least one ClientCall — the ApiConfig literal.
    const optsCall = api.clientCalls.find((c) => c.framework === 'go.options');
    expect(optsCall).toBeDefined();
    // Path/Host non-literal → no pathLiteral, no providerTag yet…
    expect(optsCall?.pathLiteral).toBeNull();
    expect(optsCall?.providerTag).toBeNull();
    // …but the getter chain was harvested.
    const lookups = optsCall?.pendingGetterLookups ?? [];
    expect(lookups.some((l) => l.name === 'GetABTestApiConfig')).toBe(true);
  });

  it('resolved-getter map links the getter to its struct-tag (yaml:"abtestapi")', () => {
    const tree = parseGo(FIXTURE);
    const tags = extractGoConfigTags(tree.rootNode, 'x.go');
    const getters = extractGoTrivialGetters(tree.rootNode, 'x.go');
    const resolved = buildResolvedGetters(tags, getters);

    const v = resolved.get(getterKey(null, 'GetABTestApiConfig'));
    expect(v).toBeDefined();
    expect(v?.has('abtestapi')).toBe(true);

    const k = resolved.get(getterKey(null, 'GetKosmosApiConfig'));
    expect(k?.has('kosmos')).toBe(true);
  });

  it('end-to-end: lookups + resolved-getter map → providerTag candidate', () => {
    // Simulates Phase 3.4's backfill loop. We don't run the real
    // pipeline (that needs a temp repo + provider-resolver index),
    // but we do exercise the exact sequence of calls Phase 3.4
    // makes in pipeline.ts.
    const tree = parseGo(FIXTURE);
    const api = extractGoApiEndpoints(tree.rootNode, 'x.go');
    const tags = extractGoConfigTags(tree.rootNode, 'x.go');
    const getters = extractGoTrivialGetters(tree.rootNode, 'x.go');
    const resolved = buildResolvedGetters(tags, getters);

    const optsCall = api.clientCalls.find((c) => c.framework === 'go.options');
    expect(optsCall?.pendingGetterLookups?.length).toBeGreaterThan(0);

    // Mirror the pipeline: for each pending lookup, take the union
    // of (receiver, name) and ("*", name) candidates.
    const candidateTags = new Set<string>();
    for (const lk of optsCall!.pendingGetterLookups!) {
      const a = resolved.get(getterKey(lk.receiver, lk.name));
      const b = resolved.get(getterKey(null, lk.name));
      if (a) for (const t of a) candidateTags.add(t);
      if (b) for (const t of b) candidateTags.add(t);
    }
    expect(candidateTags.has('abtestapi')).toBe(true);
    // Sub-keys of the StandardApiConfig (`path`, `host`, `protocol`)
    // are *not* part of the candidate set — the resolver only emits
    // the tag at the getter's terminal selector, not any nested
    // fields of its return type. This is the correct behaviour: the
    // call site's `.ApiPath` access is irrelevant to provider
    // identity (which is owned by the parent `ABTestApiConfig` tag).
    expect(candidateTags.has('path')).toBe(false);
    expect(candidateTags.has('host')).toBe(false);
    expect(candidateTags.has('protocol')).toBe(false);
  });

  it('handles multiple distinct getter chains in the same options bag', () => {
    const FX = `
package x
type Cfg struct {
  Kosmos int \`yaml:"kosmos"\`
  Abtest int \`yaml:"abtestapi"\`
}
var c Cfg
func GetKosmos() int { return c.Kosmos }
func GetAbtest() int { return c.Abtest }
type ApiConfig struct{ Method, Host, Path string }
func mix() {
  _ = ApiConfig{
    Method: "GET",
    Host:   GetKosmos().Host,
    Path:   GetAbtest().Path,
  }
}
`;
    const tree = parseGo(FX);
    const api = extractGoApiEndpoints(tree.rootNode, 'x.go');
    const tags = extractGoConfigTags(tree.rootNode, 'x.go');
    const getters = extractGoTrivialGetters(tree.rootNode, 'x.go');
    const resolved = buildResolvedGetters(tags, getters);

    const c = api.clientCalls.find((c) => c.framework === 'go.options');
    const lookups = c?.pendingGetterLookups ?? [];
    expect(lookups.some((l) => l.name === 'GetKosmos')).toBe(true);
    expect(lookups.some((l) => l.name === 'GetAbtest')).toBe(true);

    const all = new Set<string>();
    for (const lk of lookups) {
      const v = resolved.get(getterKey(null, lk.name));
      if (v) for (const t of v) all.add(t);
    }
    // Both candidates surface — the pipeline picks the first that
    // also resolves through the provider-resolver index.
    expect(all.has('kosmos')).toBe(true);
    expect(all.has('abtestapi')).toBe(true);
  });
});
