/**
 * Unit tests for the Python config-tag + trivial-getter extractors.
 *
 * Coverage:
 *   - module-level `os.environ` / `os.getenv` reads
 *   - Pydantic `Field(env=…)` and `Field("default", env=…)`
 *   - `@property`-decorated getters (with `self.x`, `self.cfg.host`)
 *   - end-to-end fold via `buildResolvedGetters`
 */

import { describe, it, expect } from 'vitest';
import Parser from 'tree-sitter';
import Python from 'tree-sitter-python';
import {
  extractPythonConfigTags,
  extractPythonTrivialGetters,
} from '../../src/core/ingestion/route-extractors/config-tag-python.js';
import {
  buildResolvedGetters,
  getterKey,
} from '../../src/core/ingestion/route-extractors/config-tag-resolver.js';

function parsePy(src: string) {
  const parser = new Parser();
  parser.setLanguage(Python);
  return parser.parse(src);
}

// ─────────────────────────────────────────────────────────────────
// extractPythonConfigTags
// ─────────────────────────────────────────────────────────────────

describe('extractPythonConfigTags', () => {
  it('captures module-level os.environ.get binding', () => {
    const tree = parsePy(`
import os
KOSMOS_URL = os.environ.get("KOSMOS_URL")
`);
    const tags = extractPythonConfigTags(tree.rootNode, 'cfg.py');
    expect(tags).toHaveLength(1);
    expect(tags[0].owner).toBe('*');
    expect(tags[0].field).toBe('KOSMOS_URL');
    expect(tags[0].tags.python).toBe('kosmos');
  });

  it('captures os.environ["KEY"] subscript binding', () => {
    const tree = parsePy(`
import os
WEAVER_HOST = os.environ["WEAVER_HOST"]
`);
    const tags = extractPythonConfigTags(tree.rootNode, 'cfg.py');
    expect(tags).toHaveLength(1);
    expect(tags[0].field).toBe('WEAVER_HOST');
    expect(tags[0].tags.python).toBe('weaver');
  });

  it('captures os.getenv binding', () => {
    const tree = parsePy(`
import os
ABTEST_API_HOST = os.getenv("ABTEST_API_HOST", default="localhost")
`);
    const tags = extractPythonConfigTags(tree.rootNode, 'cfg.py');
    expect(tags[0].tags.python).toBe('abtest');
  });

  it('captures Pydantic BaseSettings field with Field(env="…")', () => {
    const tree = parsePy(`
from pydantic_settings import BaseSettings
from pydantic import Field

class Settings(BaseSettings):
    kosmos_url: str = Field(env="KOSMOS_URL")
    abtest_host: str = Field("default-host", env="ABTEST_HOST")
`);
    const tags = extractPythonConfigTags(tree.rootNode, 'settings.py');
    expect(tags).toHaveLength(2);
    const k = tags.find((t) => t.field === 'kosmos_url');
    expect(k?.owner).toBe('Settings');
    expect(k?.tags.python).toBe('kosmos');
    const a = tags.find((t) => t.field === 'abtest_host');
    expect(a?.tags.python).toBe('abtest');
  });

  it('captures Pydantic Field with alias / validation_alias', () => {
    const tree = parsePy(`
from pydantic import Field
class Settings:
    foo: str = Field(alias="WEAVER_URL")
    bar: str = Field(validation_alias="KOSMOS_HOST")
`);
    const tags = extractPythonConfigTags(tree.rootNode, 's.py');
    expect(tags.find((t) => t.field === 'foo')?.tags.python).toBe('weaver');
    expect(tags.find((t) => t.field === 'bar')?.tags.python).toBe('kosmos');
  });

  it('skips fields without recognisable env binding', () => {
    const tree = parsePy(`
class Settings:
    foo: str = "literal-default"
    bar: str
`);
    const tags = extractPythonConfigTags(tree.rootNode, 's.py');
    expect(tags).toHaveLength(0);
  });

  it('only treats top-level assignments as module-level (not nested)', () => {
    const tree = parsePy(`
import os
def init():
    KOSMOS_URL = os.environ.get("KOSMOS_URL")
`);
    const tags = extractPythonConfigTags(tree.rootNode, 's.py');
    // The assignment inside init() is *function-local* — we don't
    // want to bind it as a module-level constant.
    expect(tags).toHaveLength(0);
  });
});

// ─────────────────────────────────────────────────────────────────
// extractPythonTrivialGetters
// ─────────────────────────────────────────────────────────────────

describe('extractPythonTrivialGetters', () => {
  it('captures @property getter returning self.x', () => {
    const tree = parsePy(`
class Config:
    @property
    def kosmos_url(self):
        return self._kosmos_url
`);
    const getters = extractPythonTrivialGetters(tree.rootNode, 'config.py');
    const g = getters.find((g) => g.name === 'kosmos_url');
    expect(g?.receiver).toBe('Config');
    expect(g?.returnAliases).toEqual([['self', '_kosmos_url']]);
  });

  it('captures @property returning self.attr.attr2', () => {
    const tree = parsePy(`
class Config:
    @property
    def host(self):
        return self.cfg.host
`);
    const getters = extractPythonTrivialGetters(tree.rootNode, 'config.py');
    const g = getters.find((g) => g.name === 'host');
    expect(g?.returnAliases).toEqual([['self', 'cfg', 'host']]);
  });

  it('captures branched @property getter', () => {
    const tree = parsePy(`
class Config:
    @property
    def url(self):
        if self.is_prod:
            return self.prod_cfg.host
        return self.staging_cfg.host
`);
    const getters = extractPythonTrivialGetters(tree.rootNode, 'config.py');
    const g = getters.find((g) => g.name === 'url');
    expect(g?.returnAliases).toContainEqual(['self', 'prod_cfg', 'host']);
    expect(g?.returnAliases).toContainEqual(['self', 'staging_cfg', 'host']);
  });

  it('captures free function `def get_x(): return X`', () => {
    const tree = parsePy(`
KOSMOS_URL = "https://k"
def get_kosmos():
    return KOSMOS_URL
`);
    const getters = extractPythonTrivialGetters(tree.rootNode, 'cfg.py');
    const g = getters.find((g) => g.name === 'get_kosmos');
    expect(g?.receiver).toBeNull();
    expect(g?.returnAliases).toEqual([['KOSMOS_URL']]);
  });

  it('captures method-call alias (one getter calls another)', () => {
    const tree = parsePy(`
class Config:
    def get_alias(self):
        return self.get_real()
    def get_real(self):
        return self._x
`);
    const getters = extractPythonTrivialGetters(tree.rootNode, 'cfg.py');
    const a = getters.find((g) => g.name === 'get_alias');
    expect(a?.returnAliases).toEqual([['self', 'get_real']]);
  });

  it('does not recurse into nested function bodies', () => {
    const tree = parsePy(`
def outer():
    def inner():
        return inner_x
    return outer_x
`);
    const getters = extractPythonTrivialGetters(tree.rootNode, 'cfg.py');
    // Both `outer` and `inner` are emitted, each only with their own returns.
    const o = getters.find((g) => g.name === 'outer');
    expect(o?.returnAliases).toEqual([['outer_x']]);
    const i = getters.find((g) => g.name === 'inner');
    expect(i?.returnAliases).toEqual([['inner_x']]);
  });
});

// ─────────────────────────────────────────────────────────────────
// End-to-end fold
// ─────────────────────────────────────────────────────────────────

describe('Python config-tag fold via buildResolvedGetters', () => {
  it('module-level env binding + free getter resolves to tag', () => {
    const tree = parsePy(`
import os
KOSMOS_URL = os.environ.get("KOSMOS_URL")
def get_kosmos():
    return KOSMOS_URL
`);
    const tags = extractPythonConfigTags(tree.rootNode, 'cfg.py');
    const getters = extractPythonTrivialGetters(tree.rootNode, 'cfg.py');
    const resolved = buildResolvedGetters(tags, getters);

    const v = resolved.get(getterKey(null, 'get_kosmos'));
    expect(v?.has('kosmos')).toBe(true);
  });

  it('Pydantic Field + @property getter resolves through self.field', () => {
    const tree = parsePy(`
from pydantic import Field
class Settings:
    kosmos_url: str = Field(env="KOSMOS_URL")
    @property
    def kosmos(self):
        return self.kosmos_url
`);
    const tags = extractPythonConfigTags(tree.rootNode, 's.py');
    const getters = extractPythonTrivialGetters(tree.rootNode, 's.py');
    const resolved = buildResolvedGetters(tags, getters);

    const v = resolved.get(getterKey('Settings', 'kosmos'));
    expect(v?.has('kosmos')).toBe(true);
  });

  it('chains through multiple getters', () => {
    const tree = parsePy(`
import os
WEAVER_URL = os.getenv("WEAVER_URL")
def primary():
    return WEAVER_URL
def alias():
    return primary()
`);
    const tags = extractPythonConfigTags(tree.rootNode, 'cfg.py');
    const getters = extractPythonTrivialGetters(tree.rootNode, 'cfg.py');
    const resolved = buildResolvedGetters(tags, getters);

    const v = resolved.get(getterKey(null, 'alias'));
    expect(v?.has('weaver')).toBe(true);
  });
});
