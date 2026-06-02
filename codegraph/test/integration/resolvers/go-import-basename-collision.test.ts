import { describe, it, expect, beforeAll } from 'vitest';
import path from 'path';
import {
  FIXTURES,
  getRelationships,
  runPipelineFromRepo,
  type PipelineResult,
  type RelEdge,
} from './helpers.js';

// Repro for the mega-repo failovers.go blackout: a file that imports BOTH an
// aliased failover and an unaliased failover (colliding import-path basenames)
// emitted ZERO outbound CALLS — even for receiver calls unrelated to failover.
describe('Go file importing aliased + unaliased packages with the same basename', () => {
  let result: PipelineResult;
  let calls: RelEdge[];

  beforeAll(async () => {
    result = await runPipelineFromRepo(
      path.join(FIXTURES, 'go-import-basename-collision'),
      () => {},
    );
    calls = getRelationships(result, 'CALLS');
  }, 60000);

  const dump = () =>
    JSON.stringify(
      calls.map(
        (c) =>
          `${c.sourceFilePath.split('/').slice(-2).join('/')}:${c.source}->${c.targetFilePath
            .split('/')
            .slice(-2)
            .join('/')}:${c.target}`,
      ),
    );

  it('resolves the cross-file receiver call GetWafBasedFailoverDetails -> getFailoverDetailsAndLog', () => {
    const e = calls.find(
      (c) =>
        c.source === 'GetWafBasedFailoverDetails' && c.target === 'getFailoverDetailsAndLog',
    );
    expect(e, `calls: ${dump()}`).toBeDefined();
  });

  it('resolves the cross-file receiver call getFailoverDetailsAndLog -> loadData', () => {
    const e = calls.find(
      (c) => c.source === 'getFailoverDetailsAndLog' && c.target === 'loadData',
    );
    expect(e, `calls: ${dump()}`).toBeDefined();
  });

  it('resolves the package-qualified call to the unaliased import (GetWafBasedFailoverDetails -> GetWafFailover)', () => {
    const e = calls.find(
      (c) =>
        c.source === 'GetWafBasedFailoverDetails' &&
        c.target === 'GetWafFailover' &&
        c.targetFilePath.includes('pkg/failover'),
    );
    expect(e, `calls: ${dump()}`).toBeDefined();
  });
});
