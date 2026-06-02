/**
 * Unit tests for Go package import resolution, focused on the multi-module
 * (mega-index) directory-suffix fallback.
 *
 * In a repo that symlinks several go.mod modules under one root, only one
 * `goModule.modulePath` is configured. Imports from sibling modules fall
 * through to a generic single-file suffix match that can't address a package
 * directory and silently picks the wrong same-named package. The dir-suffix
 * fallback resolves the longest matching package directory instead, returning
 * the whole package's files.
 */
import { describe, it, expect } from 'vitest';
import { resolveGoImport } from '../../src/core/ingestion/import-resolvers/go.js';
import { buildSuffixIndex } from '../../src/core/ingestion/import-resolvers/utils.js';
import type { ResolveCtx, ImportConfigs } from '../../src/core/ingestion/import-resolvers/types.js';
import type { GoModuleConfig } from '../../src/core/ingestion/language-config.js';

function makeCtx(files: string[], goModule: GoModuleConfig | null): ResolveCtx {
  const normalized = files.map((f) => f.replace(/\\/g, '/'));
  const configs: ImportConfigs = {
    tsconfigPaths: null,
    goModule,
    composerConfig: null,
    swiftPackageConfig: null,
    csharpConfigs: [],
  };
  return {
    allFilePaths: new Set(files),
    allFileList: files,
    normalizedFileList: normalized,
    index: buildSuffixIndex(normalized, files),
    resolveCache: new Map(),
    configs,
  };
}

describe('resolveGoImport — multi-module directory-suffix fallback', () => {
  it('disambiguates same-named packages across modules by longest dir suffix', () => {
    // Two `viewstatus` packages in different modules. The configured module is
    // the "common" one, so the smartcacheserving import must use the fallback.
    const files = [
      'goserving/cmpkg/statuses/viewstatus/viewstatus.go',
      'goserving/smartcacheserving/app/pkg/viewstatus/viewstatus.go',
      'goserving/smartcacheserving/app/pkg/viewstatus/helper.go',
    ];
    const ctx = makeCtx(files, { modulePath: 'goserving', goVersion: '1.21' } as GoModuleConfig);

    const result = resolveGoImport('smartcacheserving/app/pkg/viewstatus', 'caller.go', ctx);

    expect(result).not.toBeNull();
    expect(result!.kind).toBe('package');
    // Must resolve to the smartcacheserving package — and to ALL its files,
    // never the cmpkg/statuses homonym.
    expect(result!.files.sort()).toEqual(
      [
        'goserving/smartcacheserving/app/pkg/viewstatus/viewstatus.go',
        'goserving/smartcacheserving/app/pkg/viewstatus/helper.go',
      ].sort(),
    );
    expect(result!.files).not.toContain('goserving/cmpkg/statuses/viewstatus/viewstatus.go');
  });

  it('resolves a renamed-directory package (dir basename differs from import tail)', () => {
    const files = [
      'goserving/smartcacheserving/app/pkg/nerrping2/nerrpingurl.go',
      'goserving/other/health/health.go',
    ];
    const ctx = makeCtx(files, null);

    const result = resolveGoImport('smartcacheserving/app/pkg/nerrping2', 'caller.go', ctx);
    expect(result).not.toBeNull();
    expect(result!.kind).toBe('package');
    expect(result!.files).toEqual([
      'goserving/smartcacheserving/app/pkg/nerrping2/nerrpingurl.go',
    ]);
  });

  it('excludes _test.go files from the resolved package', () => {
    const files = [
      'app/pkg/widget/widget.go',
      'app/pkg/widget/widget_test.go',
    ];
    const ctx = makeCtx(files, null);

    const result = resolveGoImport('app/pkg/widget', 'caller.go', ctx);
    expect(result!.files).toEqual(['app/pkg/widget/widget.go']);
  });

  it('the ≥2-segment guard prevents a bare trailing segment from matching as a package', () => {
    // External import whose final segment (`util`) collides with an in-repo dir.
    // The dir-suffix fallback must NOT manufacture a package match from the bare
    // `util` segment. (Whatever the generic single-file fallback does afterward
    // is pre-existing behavior, out of scope here.)
    const files = ['internal/util/util.go'];
    const ctx = makeCtx(files, { modulePath: 'myrepo', goVersion: '1.21' } as GoModuleConfig);

    const result = resolveGoImport('github.com/some/external/util', 'caller.go', ctx);
    expect(result?.kind).not.toBe('package');
  });

  it('honours the configured module path before the dir-suffix fallback', () => {
    const files = ['myrepo/internal/auth/auth.go', 'other/internal/billing/billing.go'];
    const ctx = makeCtx(files, { modulePath: 'myrepo', goVersion: '1.21' } as GoModuleConfig);

    const result = resolveGoImport('myrepo/internal/auth', 'caller.go', ctx);
    expect(result!.kind).toBe('package');
    expect(result!.files).toContain('myrepo/internal/auth/auth.go');
    expect(result!.files).not.toContain('other/internal/billing/billing.go');
  });
});
