/**
 * Integration-flavoured tests for the Python URL → pending-getter
 * path. Mirrors the Go and Java analogues — confirms that
 * non-literal URL args (string concat with module-level constants,
 * settings-attr access, getter calls) all surface pending lookups
 * that the resolver fold can unwind into a provider tag.
 */

import { describe, it, expect } from 'vitest';
import Parser from 'tree-sitter';
import Python from 'tree-sitter-python';
import { extractPythonApiEndpoints } from '../../src/core/ingestion/route-extractors/api-endpoint-python.js';
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

describe('Python URL pending-getter resolution', () => {
  it('captures pending lookup for `requests.get(KOSMOS_URL + …)`', () => {
    const tree = parsePy(`
import os
import requests
KOSMOS_URL = os.environ.get("KOSMOS_URL")
def call():
    requests.get(KOSMOS_URL + "/test")
`);
    const api = extractPythonApiEndpoints(tree.rootNode, 'm.py');
    const c = api.clientCalls.find((c) => c.framework === 'requests');
    expect(c).toBeDefined();
    expect(c?.pathLiteral).toBeNull();
    expect(c?.pendingGetterLookups?.some((l) => l.name === 'KOSMOS_URL')).toBe(true);
  });

  it('end-to-end fold: module env binding + getter resolves to provider tag', () => {
    const tree = parsePy(`
import os
import requests
KOSMOS_URL = os.environ.get("KOSMOS_URL")
def call():
    requests.get(KOSMOS_URL + "/test")
`);
    const api = extractPythonApiEndpoints(tree.rootNode, 'm.py');
    const c = api.clientCalls.find((c) => c.framework === 'requests');
    const tags = extractPythonConfigTags(tree.rootNode, 'm.py');
    const getters = extractPythonTrivialGetters(tree.rootNode, 'm.py');
    const resolved = buildResolvedGetters(tags, getters);

    const all = new Set<string>();
    for (const lk of c!.pendingGetterLookups!) {
      const a = resolved.get(getterKey(lk.receiver, lk.name));
      const b = resolved.get(getterKey(null, lk.name));
      if (a) for (const t of a) all.add(t);
      if (b) for (const t of b) all.add(t);
    }
    expect(all.has('kosmos')).toBe(true);
  });

  it('captures getter-call alias for `requests.get(get_url())`', () => {
    const tree = parsePy(`
import os
import requests
KOSMOS_URL = os.environ.get("KOSMOS_URL")
def get_url():
    return KOSMOS_URL
def call():
    requests.get(get_url())
`);
    const api = extractPythonApiEndpoints(tree.rootNode, 'm.py');
    const c = api.clientCalls.find((c) => c.framework === 'requests');
    expect(c?.pendingGetterLookups?.some((l) => l.name === 'get_url')).toBe(true);

    const tags = extractPythonConfigTags(tree.rootNode, 'm.py');
    const getters = extractPythonTrivialGetters(tree.rootNode, 'm.py');
    const resolved = buildResolvedGetters(tags, getters);
    const all = new Set<string>();
    for (const lk of c!.pendingGetterLookups!) {
      const a = resolved.get(getterKey(null, lk.name));
      if (a) for (const t of a) all.add(t);
    }
    expect(all.has('kosmos')).toBe(true);
  });

  it('captures `settings.attr` style attribute access', () => {
    const tree = parsePy(`
import requests
def call(settings):
    requests.get(settings.weaver_url + "/x")
`);
    const api = extractPythonApiEndpoints(tree.rootNode, 'm.py');
    const c = api.clientCalls.find((c) => c.framework === 'requests');
    expect(c?.pendingGetterLookups?.some((l) =>
      l.receiver === 'settings' && l.name === 'weaver_url',
    )).toBe(true);
  });

  it('Pydantic Field + @property getter chain resolves through call site', () => {
    const tree = parsePy(`
import requests
from pydantic import Field

class Settings:
    abtest_url: str = Field(env="ABTEST_URL")
    @property
    def url(self):
        return self.abtest_url

def call(settings):
    requests.get(settings.url)
`);
    const api = extractPythonApiEndpoints(tree.rootNode, 'm.py');
    const c = api.clientCalls.find((c) => c.framework === 'requests');
    const tags = extractPythonConfigTags(tree.rootNode, 'm.py');
    const getters = extractPythonTrivialGetters(tree.rootNode, 'm.py');
    const resolved = buildResolvedGetters(tags, getters);

    const all = new Set<string>();
    for (const lk of c!.pendingGetterLookups!) {
      const a = resolved.get(getterKey(lk.receiver, lk.name));
      const b = resolved.get(getterKey('Settings', lk.name));
      const z = resolved.get(getterKey(null, lk.name));
      if (a) for (const t of a) all.add(t);
      if (b) for (const t of b) all.add(t);
      if (z) for (const t of z) all.add(t);
    }
    expect(all.has('abtest')).toBe(true);
  });
});
