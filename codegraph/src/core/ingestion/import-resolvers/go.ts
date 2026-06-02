/**
 * Go package import resolution.
 * Handles Go module path-based package imports.
 */

import { SupportedLanguages } from '../../../_shared/index.js';
import type { ImportResult, ResolveCtx } from './types.js';
import { resolveStandard } from './standard.js';
import type { GoModuleConfig } from '../language-config.js';

/**
 * Extract the package directory suffix from a Go import path.
 * Returns the suffix string (e.g., "/internal/auth/") or null if invalid.
 */
export function resolveGoPackageDir(importPath: string, goModule: GoModuleConfig): string | null {
  if (!importPath.startsWith(goModule.modulePath)) return null;
  const relativePkg = importPath.slice(goModule.modulePath.length + 1);
  if (!relativePkg) return null;
  return '/' + relativePkg + '/';
}

/**
 * Resolve a Go internal package import to all .go files in the package directory.
 * Returns an array of file paths.
 */
export function resolveGoPackage(
  importPath: string,
  goModule: GoModuleConfig,
  normalizedFileList: string[],
  allFileList: string[],
): string[] {
  if (!importPath.startsWith(goModule.modulePath)) return [];

  // Strip module path to get relative package path
  const relativePkg = importPath.slice(goModule.modulePath.length + 1); // e.g., "internal/auth"
  if (!relativePkg) return [];

  const pkgSuffix = '/' + relativePkg + '/';
  const matches: string[] = [];

  for (let i = 0; i < normalizedFileList.length; i++) {
    // Prepend '/' so paths like "internal/auth/service.go" match suffix "/internal/auth/"
    const normalized = '/' + normalizedFileList[i];
    // File must be directly in the package directory (not a subdirectory)
    if (
      normalized.includes(pkgSuffix) &&
      normalized.endsWith('.go') &&
      !normalized.endsWith('_test.go')
    ) {
      const afterPkg = normalized.substring(normalized.indexOf(pkgSuffix) + pkgSuffix.length);
      if (!afterPkg.includes('/')) {
        matches.push(allFileList[i]);
      }
    }
  }

  return matches;
}

/**
 * Resolve a Go import as a package directory by its longest matching directory
 * suffix, returning all `.go` files in that package.
 *
 * This is the multi-module fallback for repos that contain several go.mod
 * modules (e.g. a mega-index that symlinks `goserving`, `smartcacheserving`,
 * etc. under one root). The single configured `goModule.modulePath` can only
 * cover one module, so imports from sibling modules (`smartcacheserving/app/
 * pkg/viewstatus`) otherwise fall through to the generic single-file suffix
 * resolver. That resolver can't match a package directory (dir vs file) and
 * degrades to the bare package basename (`viewstatus.go`), which is ambiguous
 * across same-named packages in different modules and silently picks the wrong
 * file — dropping every cross-package CALLS edge into the real package.
 *
 * Matching the *longest* directory suffix of the import path against the repo's
 * directory index disambiguates these: `smartcacheserving/app/pkg/viewstatus`
 * only matches the one true package dir, and we return its full file set
 * (correct Go package semantics). We require at least two path segments so a
 * bare final segment can't accidentally match an unrelated same-named dir from
 * an external dependency.
 */
function resolveGoPackageByDirSuffix(rawImportPath: string, ctx: ResolveCtx): ImportResult {
  const parts = rawImportPath.split('/').filter(Boolean);
  // Need ≥2 segments so we never match on a bare single segment (e.g. a repo
  // dir colliding with the tail of an external import path).
  for (let i = 0; i <= parts.length - 2; i++) {
    const dirSuffix = parts.slice(i).join('/');
    const files = ctx.index.getFilesInDir(dirSuffix, '.go');
    if (files.length === 0) continue;
    const nonTest = files.filter((f) => !f.endsWith('_test.go'));
    const chosen = nonTest.length > 0 ? nonTest : files;
    return { kind: 'package', files: chosen, dirSuffix: '/' + dirSuffix + '/' };
  }
  return null;
}

/** Go: package-level imports via go.mod module path. */
export function resolveGoImport(
  rawImportPath: string,
  filePath: string,
  ctx: ResolveCtx,
): ImportResult {
  const goModule = ctx.configs.goModule;
  if (goModule && rawImportPath.startsWith(goModule.modulePath)) {
    const pkgSuffix = resolveGoPackageDir(rawImportPath, goModule);
    if (pkgSuffix) {
      const pkgFiles = resolveGoPackage(
        rawImportPath,
        goModule,
        ctx.normalizedFileList,
        ctx.allFileList,
      );
      if (pkgFiles.length > 0) {
        return { kind: 'package', files: pkgFiles, dirSuffix: pkgSuffix };
      }
    }
    // Fall through if no files found (package might be external)
  }

  // Multi-module fallback: resolve as a package directory by longest dir
  // suffix before the generic single-file resolver (which mis-resolves
  // same-named packages across modules in a mega-index).
  const dirResult = resolveGoPackageByDirSuffix(rawImportPath, ctx);
  if (dirResult) return dirResult;

  return resolveStandard(rawImportPath, filePath, ctx, SupportedLanguages.Go);
}
