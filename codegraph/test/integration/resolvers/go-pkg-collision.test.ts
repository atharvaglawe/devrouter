import { describe, it, expect, beforeAll } from 'vitest';
import path from 'path';
import {
  FIXTURES,
  getRelationships,
  runPipelineFromRepo,
  type PipelineResult,
  type RelEdge,
} from './helpers.js';

describe('Go same-basename package collision in one caller file', () => {
  let result: PipelineResult;
  let calls: RelEdge[];

  beforeAll(async () => {
    result = await runPipelineFromRepo(path.join(FIXTURES, 'go-pkg-collision'), () => {});
    calls = getRelationships(result, 'CALLS');
  }, 60000);

  it('resolves the plain import to its package despite a basename-colliding aliased import', () => {
    const waf = calls.filter((c) => c.target === 'GetWafFailover');
    const fromMain = waf.find((c) => c.source === 'main');
    expect(
      fromMain,
      `expected main → GetWafFailover; got ${JSON.stringify(waf.map((w) => w.targetFilePath))}`,
    ).toBeDefined();
    expect(fromMain!.targetFilePath).toContain('svc/failover/failover.go');
    expect(fromMain!.targetFilePath).not.toContain('other/failover');
  });

  it('resolves the explicitly-aliased colliding import too', () => {
    const cleanup = calls.find((c) => c.source === 'main' && c.target === 'Cleanup');
    expect(cleanup).toBeDefined();
    expect(cleanup!.targetFilePath).toContain('common/failover/failover.go');
  });
});
