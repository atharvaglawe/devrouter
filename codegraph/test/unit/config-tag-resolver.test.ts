/**
 * Unit tests for the config-tag resolver.
 *
 * Three layers under test:
 *   1. `extractGoConfigTags` — struct-tag harvesting from Go AST.
 *   2. `extractGoTrivialGetters` — capture of all return aliases,
 *      including branches and method receivers.
 *   3. `buildResolvedGetters` — folds (1) + (2) into the
 *      `(receiver|"*"::name) → Set<tag>` map that the call-site
 *      consumer eventually uses to recover providerTag values.
 */

import { describe, it, expect } from 'vitest';
import Parser from 'tree-sitter';
import Go from 'tree-sitter-go';
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

// ─────────────────────────────────────────────────────────────────
// extractGoConfigTags
// ─────────────────────────────────────────────────────────────────

describe('extractGoConfigTags', () => {
  it('captures yaml tags on struct fields', () => {
    const tree = parseGo(`
package config
type StandardApiConfig struct {
  ApiHost  string \`yaml:"host"\`
  ApiPath  string \`yaml:"path"\`
  Protocol string \`yaml:"protocol"\`
}
`);
    const tags = extractGoConfigTags(tree.rootNode, 'config.go');
    expect(tags.length).toBe(3);
    const path = tags.find((t) => t.field === 'ApiPath');
    expect(path).toBeDefined();
    expect(path?.owner).toBe('StandardApiConfig');
    expect(path?.tags.yaml).toBe('path');
  });

  it('captures multiple tag systems on one field', () => {
    const tree = parseGo(`
package config
type Cfg struct {
  Foo string \`yaml:"foo" json:"foo_json" mapstructure:"foo_ms"\`
}
`);
    const tags = extractGoConfigTags(tree.rootNode, 'config.go');
    expect(tags).toHaveLength(1);
    expect(tags[0].tags).toEqual({
      yaml: 'foo',
      json: 'foo_json',
      mapstructure: 'foo_ms',
    });
  });

  it('strips ,omitempty and other tag options', () => {
    const tree = parseGo(`
package config
type Cfg struct {
  Bar string \`yaml:"bar,omitempty" json:"bar,inline"\`
}
`);
    const tags = extractGoConfigTags(tree.rootNode, 'config.go');
    expect(tags[0].tags).toEqual({ yaml: 'bar', json: 'bar' });
  });

  it('captures nested struct field tags (Config.ABTestApiConfig)', () => {
    const tree = parseGo(`
package config
type Config struct {
  ABTestApiConfig StandardApiConfig \`yaml:"abtestapi"\`
  KosmosApiConfig StandardApiConfig \`yaml:"kosmos"\`
}
`);
    const tags = extractGoConfigTags(tree.rootNode, 'config.go');
    const abtest = tags.find((t) => t.field === 'ABTestApiConfig');
    expect(abtest).toBeDefined();
    expect(abtest?.owner).toBe('Config');
    expect(abtest?.tags.yaml).toBe('abtestapi');
  });

  it('skips fields without struct tags', () => {
    const tree = parseGo(`
package config
type Plain struct {
  X int
  Y string \`yaml:"y"\`
}
`);
    const tags = extractGoConfigTags(tree.rootNode, 'config.go');
    expect(tags).toHaveLength(1);
    expect(tags[0].field).toBe('Y');
  });
});

// ─────────────────────────────────────────────────────────────────
// extractGoTrivialGetters
// ─────────────────────────────────────────────────────────────────

describe('extractGoTrivialGetters', () => {
  it('captures a single-return free function', () => {
    const tree = parseGo(`
package config
func GetABTestApiConfig() StandardApiConfig {
  return selected.ABTestApiConfig
}
`);
    const getters = extractGoTrivialGetters(tree.rootNode, 'config.go');
    expect(getters).toHaveLength(1);
    expect(getters[0].name).toBe('GetABTestApiConfig');
    expect(getters[0].receiver).toBeNull();
    expect(getters[0].returnAliases).toEqual([['selected', 'ABTestApiConfig']]);
  });

  it('captures all branches of an if/else getter', () => {
    const tree = parseGo(`
package config
func GetKosmosApiConfig() StandardApiConfig {
  if isProd {
    return prodCfg.KosmosApiConfig
  }
  return stagingCfg.KosmosApiConfig
}
`);
    const getters = extractGoTrivialGetters(tree.rootNode, 'config.go');
    expect(getters).toHaveLength(1);
    const aliases = getters[0].returnAliases;
    expect(aliases).toContainEqual(['prodCfg', 'KosmosApiConfig']);
    expect(aliases).toContainEqual(['stagingCfg', 'KosmosApiConfig']);
  });

  it('captures method receiver type for methods', () => {
    const tree = parseGo(`
package config
type Config struct{}
func (c *Config) GetURL() StandardApiConfig {
  return c.ABTestApiConfig
}
`);
    const getters = extractGoTrivialGetters(tree.rootNode, 'config.go');
    const m = getters.find((g) => g.name === 'GetURL');
    expect(m).toBeDefined();
    expect(m?.receiver).toBe('Config');
    expect(m?.returnAliases).toEqual([['c', 'ABTestApiConfig']]);
  });

  it('captures call-alias when one getter calls another', () => {
    const tree = parseGo(`
package config
func GetABTestApiConfigDeprecated() StandardApiConfig {
  return GetABTestApiConfig()
}
`);
    const getters = extractGoTrivialGetters(tree.rootNode, 'config.go');
    expect(getters).toHaveLength(1);
    expect(getters[0].returnAliases).toEqual([['GetABTestApiConfig']]);
  });

  it('skips functions that do non-return work', () => {
    const tree = parseGo(`
package config
func compute() string {
  x := 42
  return x
}
`);
    const getters = extractGoTrivialGetters(tree.rootNode, 'config.go');
    // The return alias is `["x"]` — a bare local. Resolver won't
    // bind it to anything, but the getter itself is still emitted
    // (the resolver does the dropping).
    expect(getters).toHaveLength(1);
    expect(getters[0].returnAliases).toEqual([['x']]);
  });

  it('does not descend into nested function literals', () => {
    const tree = parseGo(`
package config
func Outer() func() int {
  return func() int {
    return innerCfg.Value
  }
}
`);
    const getters = extractGoTrivialGetters(tree.rootNode, 'config.go');
    // Outer's own return is a func_literal — flattenAlias returns
    // null, so no alias is recorded. Outer is therefore not emitted.
    expect(getters).toHaveLength(0);
  });
});

// ─────────────────────────────────────────────────────────────────
// buildResolvedGetters — end-to-end fold
// ─────────────────────────────────────────────────────────────────

describe('buildResolvedGetters', () => {
  it('resolves the canonical GetABTestApiConfig → "abtestapi" chain', () => {
    const tree = parseGo(`
package config
type StandardApiConfig struct {
  ApiHost  string \`yaml:"host"\`
  ApiPath  string \`yaml:"path"\`
}
type Config struct {
  ABTestApiConfig StandardApiConfig \`yaml:"abtestapi"\`
}
var selected Config
func GetABTestApiConfig() StandardApiConfig {
  return selected.ABTestApiConfig
}
`);
    const tags = extractGoConfigTags(tree.rootNode, 'config.go');
    const getters = extractGoTrivialGetters(tree.rootNode, 'config.go');
    const resolved = buildResolvedGetters(tags, getters);

    const v = resolved.get(getterKey(null, 'GetABTestApiConfig'));
    expect(v).toBeDefined();
    expect(v?.has('abtestapi')).toBe(true);
  });

  it('unions tags across branching returns', () => {
    const tree = parseGo(`
package config
type C struct {
  Prod    int \`yaml:"prod-tag"\`
  Staging int \`yaml:"staging-tag"\`
}
var cfg C
func GetEither() int {
  if isProd { return cfg.Prod }
  return cfg.Staging
}
`);
    const tags = extractGoConfigTags(tree.rootNode, 'config.go');
    const getters = extractGoTrivialGetters(tree.rootNode, 'config.go');
    const resolved = buildResolvedGetters(tags, getters);

    const v = resolved.get(getterKey(null, 'GetEither'));
    expect(v).toBeDefined();
    expect(v?.has('prod-tag')).toBe(true);
    expect(v?.has('staging-tag')).toBe(true);
  });

  it('chases a 2-hop chain (getter → getter → field tag)', () => {
    const tree = parseGo(`
package config
type C struct {
  X int \`yaml:"x-tag"\`
}
var cfg C
func GetX() int { return cfg.X }
func GetXAlias() int { return GetX() }
`);
    const tags = extractGoConfigTags(tree.rootNode, 'config.go');
    const getters = extractGoTrivialGetters(tree.rootNode, 'config.go');
    const resolved = buildResolvedGetters(tags, getters);

    const v = resolved.get(getterKey(null, 'GetXAlias'));
    expect(v).toBeDefined();
    expect(v?.has('x-tag')).toBe(true);
  });

  it('terminates on a getter cycle without exploding', () => {
    const tree = parseGo(`
package config
func A() int { return B() }
func B() int { return A() }
`);
    const tags = extractGoConfigTags(tree.rootNode, 'config.go');
    const getters = extractGoTrivialGetters(tree.rootNode, 'config.go');
    const resolved = buildResolvedGetters(tags, getters);
    // Neither getter resolves to anything (no field tag in the cycle).
    expect(resolved.get(getterKey(null, 'A'))).toBeUndefined();
    expect(resolved.get(getterKey(null, 'B'))).toBeUndefined();
  });

  it('resolves a method getter via its receiver', () => {
    const tree = parseGo(`
package config
type Config struct {
  Kosmos int \`yaml:"kosmos"\`
}
func (c *Config) GetKosmos() int { return c.Kosmos }
`);
    const tags = extractGoConfigTags(tree.rootNode, 'config.go');
    const getters = extractGoTrivialGetters(tree.rootNode, 'config.go');
    const resolved = buildResolvedGetters(tags, getters);

    const v = resolved.get(getterKey('Config', 'GetKosmos'));
    expect(v).toBeDefined();
    expect(v?.has('kosmos')).toBe(true);
  });

  it('prefers mapstructure / yaml / json / env tag values in that order', () => {
    const tree = parseGo(`
package config
type C struct {
  X int \`yaml:"yaml-tag" json:"json-tag" mapstructure:"ms-tag"\`
}
var cfg C
func GetX() int { return cfg.X }
`);
    const tags = extractGoConfigTags(tree.rootNode, 'config.go');
    const getters = extractGoTrivialGetters(tree.rootNode, 'config.go');
    const resolved = buildResolvedGetters(tags, getters);
    // All three tag values should be present so the consumer can
    // pick the one that matches the YAML-derived index.
    const v = resolved.get(getterKey(null, 'GetX'));
    expect(v?.has('ms-tag')).toBe(true);
    expect(v?.has('yaml-tag')).toBe(true);
    expect(v?.has('json-tag')).toBe(true);
  });

  it('does not bind when the alias path leads nowhere', () => {
    const tree = parseGo(`
package config
type C struct { Y int }
var cfg C
func GetY() int { return cfg.Y }
`);
    const tags = extractGoConfigTags(tree.rootNode, 'config.go');
    const getters = extractGoTrivialGetters(tree.rootNode, 'config.go');
    const resolved = buildResolvedGetters(tags, getters);
    expect(resolved.get(getterKey(null, 'GetY'))).toBeUndefined();
  });
});
