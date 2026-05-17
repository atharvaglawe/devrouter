/**
 * Unit tests for the provider-tag resolver.
 *
 * The resolver is repo-agnostic: it joins a logical service tag
 * (`kosmos`, `weaver`, …) recovered from per-language extractors to
 * (1) a host / URL recovered from a config file, or
 * (2) a service directory hint from the repo layout.
 */

import { describe, it, expect } from 'vitest';
import {
  parseConfigFile,
  ingestConfigFile,
  ingestDirectoryHints,
  crossLinkDirsByHost,
  finalizeIndex,
  resolveTag,
  type ProviderResolverIndex,
} from '../../src/core/ingestion/route-extractors/provider-resolver.js';

function newIdx(): ProviderResolverIndex {
  return { byTag: new Map() };
}

describe('parseConfigFile', () => {
  it('parses YAML with nested service blocks', () => {
    const yaml = `
services:
  kosmos:
    url: https://kosmos.internal/api
    timeout: 30
  weaver:
    host: weaver.internal
    port: 8080
`;
    const leaves = parseConfigFile('services.yaml', yaml);
    expect(leaves).not.toBeNull();
    expect(leaves!.length).toBeGreaterThan(0);
    const urls = leaves!.filter((l) => l.value.includes('kosmos'));
    expect(urls.some((l) => l.keyPath.join('.') === 'services.kosmos.url')).toBe(true);
  });

  it('parses Spring application.properties dot-keys', () => {
    const props = `
# Spring config
kosmos.url=https://kosmos.internal/api
weaver.host=weaver.internal
feign.client.config.kosmos.url=https://kosmos.internal/feign
`;
    const leaves = parseConfigFile('application.properties', props);
    expect(leaves).not.toBeNull();
    const kosmos = leaves!.find((l) => l.keyPath.join('.') === 'kosmos.url');
    expect(kosmos?.value).toBe('https://kosmos.internal/api');
  });

  it('parses .env file with UPPER_SNAKE keys', () => {
    const env = `
# api hosts
KOSMOS_URL=https://kosmos.internal
WEAVER_URL="https://weaver.internal"
export MALL_HOST='mall.internal'
`;
    const leaves = parseConfigFile('.env', env);
    expect(leaves).not.toBeNull();
    const kosmos = leaves!.find((l) => l.keyPath.includes('kosmos'));
    expect(kosmos?.value).toBe('https://kosmos.internal');
    const mall = leaves!.find((l) => l.keyPath.includes('mall'));
    expect(mall?.value).toBe('mall.internal');
  });

  it('parses JSON service registry', () => {
    const json = JSON.stringify({
      services: { kosmos: { url: 'https://kosmos.internal' } },
    });
    const leaves = parseConfigFile('services.json', json);
    expect(leaves).not.toBeNull();
    const kosmos = leaves!.find(
      (l) => l.keyPath.join('.') === 'services.kosmos.url',
    );
    expect(kosmos?.value).toBe('https://kosmos.internal');
  });

  it('returns null for unrecognised filenames', () => {
    expect(parseConfigFile('main.go', 'package main')).toBeNull();
    expect(parseConfigFile('foo.txt', 'hello')).toBeNull();
  });
});

describe('ingestConfigFile + resolveTag', () => {
  it('joins YAML service blocks → tag → URL/host', () => {
    const idx = newIdx();
    const leaves = parseConfigFile(
      'services.yaml',
      `
services:
  kosmos:
    url: https://kosmos.internal/api
  weaver:
    host: weaver.internal
`,
    )!;
    ingestConfigFile(idx, 'services.yaml', leaves);
    finalizeIndex(idx);

    const k = resolveTag('kosmos', idx);
    expect(k).toBeDefined();
    expect([...k!.urls]).toContain('https://kosmos.internal/api');
    expect([...k!.hosts]).toContain('kosmos.internal');

    const w = resolveTag('weaver', idx);
    expect(w).toBeDefined();
    expect([...w!.hosts]).toContain('weaver.internal');
  });

  it('joins Spring properties dotted keys → tag', () => {
    const idx = newIdx();
    const leaves = parseConfigFile(
      'application.properties',
      `kosmos.url=https://kosmos.internal/api`,
    )!;
    ingestConfigFile(idx, 'application.properties', leaves);
    finalizeIndex(idx);

    const k = resolveTag('kosmos', idx);
    expect(k).toBeDefined();
    expect([...k!.urls]).toContain('https://kosmos.internal/api');
  });

  it('joins UPPER_SNAKE env keys → lower tag', () => {
    const idx = newIdx();
    const leaves = parseConfigFile('.env', `KOSMOS_URL=https://kosmos.internal`)!;
    ingestConfigFile(idx, '.env', leaves);
    finalizeIndex(idx);

    const k = resolveTag('kosmos', idx);
    expect(k).toBeDefined();
    expect([...k!.hosts]).toContain('kosmos.internal');
  });

  it('skips entries whose value is not URL-shaped', () => {
    const idx = newIdx();
    const leaves = parseConfigFile(
      'application.properties',
      `kosmos.timeout=30
kosmos.description=this is just text
kosmos.url=https://kosmos.internal`,
    )!;
    ingestConfigFile(idx, 'application.properties', leaves);
    finalizeIndex(idx);

    const k = resolveTag('kosmos', idx);
    expect(k).toBeDefined();
    expect(k!.urls.size).toBe(1);
  });

  it('treats stop-words like "url"/"host" as non-tags', () => {
    const idx = newIdx();
    // The leaf's *parent* is the tag, not "url" itself.
    const leaves = parseConfigFile(
      'application.properties',
      `kosmos.url=https://kosmos.internal`,
    )!;
    ingestConfigFile(idx, 'application.properties', leaves);
    finalizeIndex(idx);

    expect(resolveTag('url', idx)).toBeNull();
    expect(resolveTag('kosmos', idx)).not.toBeNull();
  });

  it('returns null for unknown tags', () => {
    const idx = newIdx();
    finalizeIndex(idx);
    expect(resolveTag('unknown', idx)).toBeNull();
  });
});

describe('ingestDirectoryHints', () => {
  it('binds services/<name>/… → tag <name>', () => {
    const idx = newIdx();
    ingestDirectoryHints(idx, [
      'services/kosmos/main.go',
      'services/kosmos/internal/handler.go',
      'services/weaver/main.go',
    ]);
    finalizeIndex(idx);

    const k = resolveTag('kosmos', idx);
    expect(k).toBeDefined();
    expect([...k!.serviceDirs]).toContain('services/kosmos');

    const w = resolveTag('weaver', idx);
    expect(w).toBeDefined();
    expect([...w!.serviceDirs]).toContain('services/weaver');
  });

  it('also recognises cmd/, apps/, pkg/ layouts', () => {
    const idx = newIdx();
    ingestDirectoryHints(idx, [
      'cmd/kosmos/main.go',
      'apps/weaver/main.ts',
      'pkg/mall/util.go',
    ]);
    finalizeIndex(idx);

    expect(resolveTag('kosmos', idx)?.serviceDirs.has('cmd/kosmos')).toBe(true);
    expect(resolveTag('weaver', idx)?.serviceDirs.has('apps/weaver')).toBe(true);
    expect(resolveTag('mall', idx)?.serviceDirs.has('pkg/mall')).toBe(true);
  });

  it('does nothing for files outside known layouts', () => {
    const idx = newIdx();
    ingestDirectoryHints(idx, ['random/somewhere/file.go', 'README.md']);
    finalizeIndex(idx);

    expect(idx.byTag.size).toBe(0);
  });
});

describe('confidence scoring', () => {
  it('1.0 when both config + dir hints present', () => {
    const idx = newIdx();
    ingestConfigFile(
      idx,
      'application.properties',
      parseConfigFile('application.properties', `kosmos.url=https://kosmos.internal`)!,
    );
    ingestDirectoryHints(idx, ['services/kosmos/main.go']);
    finalizeIndex(idx);

    const k = resolveTag('kosmos', idx);
    expect(k?.confidence).toBe(1.0);
  });

  it('0.5 when only config present', () => {
    const idx = newIdx();
    ingestConfigFile(
      idx,
      'application.properties',
      parseConfigFile('application.properties', `kosmos.url=https://kosmos.internal`)!,
    );
    finalizeIndex(idx);

    const k = resolveTag('kosmos', idx);
    expect(k?.confidence).toBe(0.5);
  });

  it('0.5 when only directory hint present', () => {
    const idx = newIdx();
    ingestDirectoryHints(idx, ['services/kosmos/main.go']);
    finalizeIndex(idx);

    const k = resolveTag('kosmos', idx);
    expect(k?.confidence).toBe(0.5);
  });

  it('0 → resolveTag returns null', () => {
    const idx = newIdx();
    // Force-create an entry with zero signal — confidence stays 0,
    // resolveTag should refuse to return it.
    idx.byTag.set('ghost', {
      hosts: new Set(),
      urls: new Set(),
      serviceDirs: new Set(),
      sourceFiles: new Set(),
      confidence: 0,
    });
    finalizeIndex(idx);

    expect(resolveTag('ghost', idx)).toBeNull();
  });
});

describe('crossLinkDirsByHost', () => {
  it('binds a tag to a top-level dir when its host shares a name fragment', () => {
    // Mirrors goserving: `abtestapi.host = "kosmos-neg.goapps.svc.cluster.local"`,
    // and the actual handler routes live under `kosmos/web/...`.
    const idx = newIdx();
    ingestConfigFile(
      idx,
      'oscar/config/setup/oscar/c22-sc/production.yaml',
      parseConfigFile(
        'production.yaml',
        `abtestapi:\n  host: "kosmos-neg.goapps.svc.cluster.local"\n  path: "/test/evaluate"`,
      )!,
    );
    crossLinkDirsByHost(idx, [
      'kosmos/web/routes/route.go',
      'kosmos/main.go',
      'oscar/app/main.go',
    ]);
    finalizeIndex(idx);

    const v = resolveTag('abtestapi', idx);
    expect(v).not.toBeNull();
    expect(v?.serviceDirs.has('kosmos')).toBe(true);
  });

  it('does not bind dirs whose names are not present in the file list', () => {
    const idx = newIdx();
    ingestConfigFile(
      idx,
      'config.yaml',
      parseConfigFile('config.yaml', `foo:\n  host: "ghost-svc.cluster.local"`)!,
    );
    crossLinkDirsByHost(idx, ['kosmos/main.go', 'oscar/main.go']);
    finalizeIndex(idx);
    const v = resolveTag('foo', idx);
    // No `ghost/` dir exists → no binding.
    expect(v?.serviceDirs.has('ghost')).toBe(false);
  });

  it('binds via flat-layout fallback for known config tags', () => {
    // Tag known from config + flat-layout dir → bound.
    const idx = newIdx();
    ingestConfigFile(
      idx,
      'config.yaml',
      parseConfigFile('config.yaml', `kosmos:\n  url: "https://kosmos.internal"`)!,
    );
    ingestDirectoryHints(idx, ['kosmos/web/routes/route.go', 'kosmos/main.go']);
    finalizeIndex(idx);
    const v = resolveTag('kosmos', idx);
    expect(v?.serviceDirs.has('kosmos')).toBe(true);
  });
});
