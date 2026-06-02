/**
 * Unit tests for the Apache `mod_rewrite` mini-parser used by the
 * plain-PHP route extractor. Covers the narrow shape we actually
 * derive routes from:
 *
 *   - REQUEST_URI conds paired with a php-target RewriteRule
 *   - External redirects (`[R]`) and static-asset rewrites — skipped
 *   - Pattern cleaning (anchors, escapes, capture groups)
 *   - Dedup across multiple `.htaccess*` variants
 */

import { describe, it, expect } from 'vitest';
import * as fs from 'node:fs';
import * as os from 'node:os';
import * as path from 'node:path';
import {
  parseHtaccess,
  buildHtaccessIndex,
  type HtaccessRoute,
} from '../../src/core/ingestion/route-extractors/htaccess-parser.js';

describe('parseHtaccess — single rules', () => {
  it('REQUEST_URI cond + RewriteRule yields one route with cleaned URL', () => {
    const src = [
      'RewriteCond %{REQUEST_URI}?%{QUERY_STRING} ^/trf\\?(.*)',
      'RewriteRule ^(.*)$ /transfer.php?%1  [UnsafeAllow3F,BCTLS,L]',
    ].join('\n');
    const out = parseHtaccess('.htaccess-dev', src);
    expect(out).toHaveLength(1);
    expect(out[0].urlPattern).toBe('/trf');
    expect(out[0].targetFile).toBe('transfer.php');
    expect(out[0].flags.has('l')).toBe(true);
    expect(out[0].flags.has('unsafeallow3f')).toBe(true);
  });

  it('REQUEST_URI cond without query glue still works', () => {
    const src = [
      'RewriteCond %{REQUEST_URI} ^/jsonAds$',
      'RewriteRule ^(.*)$ /xmlAds.php [L]',
    ].join('\n');
    const out = parseHtaccess('.htaccess', src);
    expect(out).toHaveLength(1);
    expect(out[0].urlPattern).toBe('/jsonAds');
    expect(out[0].targetFile).toBe('xmlAds.php');
  });

  it('multiple RewriteConds preceding one RewriteRule — most recent REQUEST_URI wins', () => {
    const src = [
      'RewriteCond %{HTTP_HOST} ^api\\.example\\.com$',
      'RewriteCond %{REQUEST_URI} ^/api/v1/foo',
      'RewriteRule ^(.*)$ /endpoints/foo.php [L]',
    ].join('\n');
    const out = parseHtaccess('.htaccess', src);
    expect(out).toHaveLength(1);
    expect(out[0].urlPattern).toBe('/api/v1/foo');
    expect(out[0].targetFile).toBe('endpoints/foo.php');
  });

  it('cleans regex anchors, escaped slashes, and trailing capture groups', () => {
    const src = [
      'RewriteCond %{REQUEST_URI} ^/dynamiclander\\/?\\?(.*)',
      'RewriteRule ^(.*)$ /dynads/landing.php?%1 [L]',
    ].join('\n');
    const out = parseHtaccess('.htaccess', src);
    expect(out).toHaveLength(1);
    expect(out[0].urlPattern).toBe('/dynamiclander/');
  });
});

describe('parseHtaccess — skip rules', () => {
  it('external redirect ([R] flag) is skipped', () => {
    const src = [
      'RewriteCond %{REQUEST_URI} ^/old$',
      'RewriteRule ^(.*)$ https://other.example.com/new [R=301,L]',
    ].join('\n');
    const out = parseHtaccess('.htaccess', src);
    expect(out).toHaveLength(0);
  });

  it('non-.php target (static asset) is skipped', () => {
    const src = [
      'RewriteCond %{REQUEST_URI} ^/main\\.js$',
      'RewriteRule ^(.*)$ /static/main.js [L]',
    ].join('\n');
    const out = parseHtaccess('.htaccess', src);
    expect(out).toHaveLength(0);
  });

  it('catch-all RewriteRule with no preceding REQUEST_URI cond is skipped', () => {
    const src = ['RewriteRule ^(.*)$ /fallback.php [L]'].join('\n');
    const out = parseHtaccess('.htaccess', src);
    expect(out).toHaveLength(0);
  });

  it('Apache no-op target (-) is skipped', () => {
    const src = [
      'RewriteCond %{REQUEST_URI} ^/api/(.*)$',
      'RewriteRule ^(.*)$ - [L]',
    ].join('\n');
    const out = parseHtaccess('.htaccess', src);
    expect(out).toHaveLength(0);
  });

  it('rule whose target contains a substitution (%1, $1) is skipped', () => {
    const src = [
      'RewriteCond %{REQUEST_URI} ^/dynamic/(.*)$',
      'RewriteRule ^(.*)$ /$1.php [L]',
    ].join('\n');
    const out = parseHtaccess('.htaccess', src);
    expect(out).toHaveLength(0);
  });

  it('comment lines and blank lines are ignored', () => {
    const src = [
      '# auth bypass',
      '',
      'RewriteCond %{REQUEST_URI} ^/healthz$',
      '# the real rule',
      'RewriteRule ^(.*)$ /health.php [L]',
    ].join('\n');
    const out = parseHtaccess('.htaccess', src);
    expect(out).toHaveLength(1);
    expect(out[0].urlPattern).toBe('/healthz');
  });
});

describe('buildHtaccessIndex — async repo scan', () => {
  it('returns empty Map for a repo with no htaccess files', async () => {
    const tmp = fs.mkdtempSync(path.join(os.tmpdir(), 'htaccess-empty-'));
    fs.writeFileSync(path.join(tmp, 'index.php'), '<?php echo "hi";');
    try {
      const idx = await buildHtaccessIndex(tmp);
      expect(idx.size).toBe(0);
    } finally {
      fs.rmSync(tmp, { recursive: true, force: true });
    }
  });

  it('merges multiple .htaccess variants and dedupes (urlPattern,targetFile)', async () => {
    const tmp = fs.mkdtempSync(path.join(os.tmpdir(), 'htaccess-multi-'));
    const ruleA = [
      'RewriteCond %{REQUEST_URI}?%{QUERY_STRING} ^/trf\\?(.*)',
      'RewriteRule ^(.*)$ /transfer.php?%1 [L]',
    ].join('\n');
    const ruleB = [
      'RewriteCond %{REQUEST_URI} ^/healthz$',
      'RewriteRule ^(.*)$ /health.php [L]',
    ].join('\n');
    fs.writeFileSync(path.join(tmp, '.htaccess-dev'), ruleA + '\n' + ruleB);
    // Same /trf → transfer.php pair lives in the env-serp variant —
    // must dedupe to a single index entry.
    fs.writeFileSync(path.join(tmp, '.htaccess-dev-serp'), ruleA);
    try {
      const idx = await buildHtaccessIndex(tmp);
      expect(idx.size).toBe(2);
      const transfer = idx.get('transfer.php');
      expect(transfer).toHaveLength(1);
      expect(transfer![0].urlPattern).toBe('/trf');
      const health = idx.get('health.php');
      expect(health).toHaveLength(1);
      expect(health![0].urlPattern).toBe('/healthz');
    } finally {
      fs.rmSync(tmp, { recursive: true, force: true });
    }
  });

  it('discovers .htaccess in a child repo dir (mega-index shape)', async () => {
    const root = fs.mkdtempSync(path.join(os.tmpdir(), 'htaccess-mega-'));
    const child = path.join(root, 'cmadserving');
    fs.mkdirSync(child);
    fs.writeFileSync(
      path.join(child, '.htaccess'),
      ['RewriteCond %{REQUEST_URI} ^/x$', 'RewriteRule ^(.*)$ /handler.php [L]'].join('\n'),
    );
    try {
      const idx = await buildHtaccessIndex(root);
      // Target file path is prefixed with the child dir so it
      // matches the repo-relative path used downstream.
      const expectedKey = 'cmadserving/handler.php';
      const entry = idx.get(expectedKey);
      expect(entry).toBeDefined();
      expect(entry![0].urlPattern).toBe('/x');
    } finally {
      fs.rmSync(root, { recursive: true, force: true });
    }
  });

  it('returns each rule once even if the same target file is rewritten from multiple URLs', async () => {
    const tmp = fs.mkdtempSync(path.join(os.tmpdir(), 'htaccess-multiurl-'));
    fs.writeFileSync(
      path.join(tmp, '.htaccess'),
      [
        'RewriteCond %{REQUEST_URI} ^/trf$',
        'RewriteRule ^(.*)$ /transfer.php [L]',
        'RewriteCond %{REQUEST_URI} ^/transfer$',
        'RewriteRule ^(.*)$ /transfer.php [L]',
      ].join('\n'),
    );
    try {
      const idx = await buildHtaccessIndex(tmp);
      const rules = idx.get('transfer.php') as HtaccessRoute[];
      expect(rules).toBeDefined();
      // Different urlPattern → two distinct rules pointing at the
      // same handler. Dedup is on (urlPattern, targetFile), so both
      // survive.
      const patterns = new Set(rules.map((r) => r.urlPattern));
      expect(patterns).toEqual(new Set(['/trf', '/transfer']));
    } finally {
      fs.rmSync(tmp, { recursive: true, force: true });
    }
  });
});
