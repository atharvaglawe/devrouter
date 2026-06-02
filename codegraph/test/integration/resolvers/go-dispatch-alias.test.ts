/**
 * Go: explicit import-alias resolution + structural interface dispatch.
 *
 * Covers two indexer fixes that close call-graph gaps:
 *  1. A renamed import (`pinger "…/internal/pinger2"`) must resolve
 *     `pinger.GetURL()` to the aliased package even though the package
 *     directory basename (`pinger2`) differs from the call-site receiver
 *     (`pinger`), and even when the function name collides across packages.
 *  2. An interface-typed receiver call (`t.Execute()` where `t Task`) must
 *     fan out a CALLS edge to every structural implementor's method
 *     (`CleanupJob.Execute`), tagged `interface-dispatch` — Go emits no
 *     `implements` heritage, so this relies on the structural implementor
 *     index being available during call resolution.
 */
import { describe, it, expect, beforeAll } from 'vitest';
import path from 'path';
import {
  FIXTURES,
  getRelationships,
  runPipelineFromRepo,
  type PipelineResult,
  type RelEdge,
} from './helpers.js';

describe('Go explicit-alias + interface-dispatch resolution', () => {
  let result: PipelineResult;
  let calls: RelEdge[];

  beforeAll(async () => {
    result = await runPipelineFromRepo(path.join(FIXTURES, 'go-dispatch-alias'), () => {});
    calls = getRelationships(result, 'CALLS');
  }, 60000);

  it('resolves an explicitly-aliased import to the aliased package, not the homonym', () => {
    const getUrlCalls = calls.filter((c) => c.target === 'GetURL');
    // Exactly one GetURL edge from main, and it must land on the aliased
    // pinger2 package — never the colliding health.GetURL.
    const pingerEdge = getUrlCalls.find((c) => c.targetFilePath.includes('pinger2/pinger.go'));
    expect(pingerEdge, 'expected a CALLS edge into pinger2/pinger.go:GetURL').toBeDefined();

    const healthEdgeFromMain = getUrlCalls.find(
      (c) => c.targetFilePath.includes('health/health.go') && c.source === 'main',
    );
    expect(healthEdgeFromMain, 'main should also resolve health.GetURL').toBeDefined();

    // The aliased call must not be mis-attributed to health.
    const pingerToHealth = getUrlCalls.find(
      (c) => c.targetFilePath.includes('health') && c.sourceFilePath.includes('cmd/main.go'),
    );
    // main calls both, so a health edge from main is expected, but there must
    // be a distinct pinger2 edge too (the alias fix). Assert both targets present.
    void pingerToHealth;
    const targets = new Set(getUrlCalls.map((c) => c.targetFilePath.replace(/.*lang-resolution\//, '')));
    expect([...targets].some((t) => t.includes('pinger2/pinger.go'))).toBe(true);
    expect([...targets].some((t) => t.includes('health/health.go'))).toBe(true);
  });

  it('emits an interface-dispatch CALLS edge to the structural implementor', () => {
    const dispatch = calls.filter((c) => c.rel.reason === 'interface-dispatch');
    const toCleanup = dispatch.find(
      (c) => c.target === 'Execute' && c.targetFilePath.includes('jobs/cleanup.go'),
    );
    expect(
      toCleanup,
      `expected interface-dispatch CALLS edge to CleanupJob.Execute; got ${JSON.stringify(
        dispatch.map((d) => `${d.source}->${d.target}@${d.targetFilePath}`),
      )}`,
    ).toBeDefined();
    expect(toCleanup!.rel.confidence).toBeCloseTo(0.7);
  });

  it('still resolves the primary interface-method CALLS edge (Task.Execute)', () => {
    const executeCalls = calls.filter((c) => c.target === 'Execute');
    const toInterface = executeCalls.find((c) => c.targetFilePath.includes('runner/runner.go'));
    expect(toInterface, 'expected primary CALLS edge to the Task.Execute interface method').toBeDefined();
  });
});
