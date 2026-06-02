import { describe, it, expect, beforeAll } from 'vitest';
import path from 'path';
import {
  FIXTURES,
  getRelationships,
  runPipelineFromRepo,
  type PipelineResult,
  type RelEdge,
} from './helpers.js';

describe('Go methods in files that do NOT declare their receiver type', () => {
  let result: PipelineResult;
  let calls: RelEdge[];

  beforeAll(async () => {
    result = await runPipelineFromRepo(path.join(FIXTURES, 'go-app-multifile'), () => {});
    calls = getRelationships(result, 'CALLS');
  }, 60000);

  const dump = () =>
    JSON.stringify(calls.map((c) => `${c.sourceFilePath.split('/').slice(-2).join('/')}:${c.source}->${c.targetFilePath.split('/').slice(-2).join('/')}:${c.target}`));

  it('emits intra-package method call where the target lives in the type-declaring file (getDetailsAndLog -> loadData)', () => {
    const e = calls.find(
      (c) =>
        c.sourceFilePath.includes('cmd/app/') &&
        c.source === 'getDetailsAndLog' &&
        c.target === 'loadData',
    );
    expect(e, `calls: ${dump()}`).toBeDefined();
    expect(e!.targetFilePath).toContain('cmd/app/app.go');
  });

  it('emits package-qualified sibling call edge (GetCurlTimeoutData -> wrapper.New)', () => {
    const e = calls.find(
      (c) =>
        c.sourceFilePath.includes('cmd/app/') &&
        c.source === 'GetCurlTimeoutData' &&
        c.target === 'New' &&
        c.targetFilePath.includes('wrapper'),
    );
    expect(e, `calls: ${dump()}`).toBeDefined();
  });

  it('resolves a cross-file method to the CALLER package despite a same-named type in another package', () => {
    // Regression: getDetailsAndLog and the App struct live in different files
    // (phantom ownerId), AND internal/dup declares a homonym App with the same
    // methods. The directory-scoped owner fallback must pick the caller's own
    // package (cmd/app), never the dup homonym, and never collapse to ambiguous.
    const e = calls.find(
      (c) =>
        c.sourceFilePath.includes('cmd/app/') &&
        c.source === 'GetCurlTimeoutData' &&
        c.target === 'getDetailsAndLog',
    );
    expect(e, `calls: ${dump()}`).toBeDefined();
    expect(e!.targetFilePath).toContain('cmd/app/failovers.go');
    expect(e!.targetFilePath).not.toContain('internal/dup');
  });

  it('still resolves the homonym package internally (dup.GetCurlTimeoutData -> dup.getDetailsAndLog)', () => {
    const e = calls.find(
      (c) =>
        c.sourceFilePath.includes('internal/dup/') &&
        c.source === 'GetCurlTimeoutData' &&
        c.target === 'getDetailsAndLog',
    );
    expect(e, `calls: ${dump()}`).toBeDefined();
    expect(e!.targetFilePath).toContain('internal/dup');
  });
});
