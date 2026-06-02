/**
 * Integration: Go startup/cron background-task lifecycle recovery
 * (Phase 3.5d, startup-task recognizer).
 *
 * Fixture shape mirrors goserving's cron registration:
 *   - main → initialiseStartupTasks → getStartupTasks builds a
 *     `[]startup.StartupTaskInterface{ refreshtask.GetTask() }` slice.
 *   - startup.RegisterTasks dispatches StartupRun/PeriodicRun through the
 *     interface (the hop static CALLS resolution cannot follow).
 *   - refreshtask.StartupRun/PeriodicRun → refresh() → builds a URL with
 *     SetPath("/refresh"), which router.go registers as a Route.
 *   - decoytask defines an identically-named GetTask + StartupRun but is
 *     NEVER registered — it must not be linked.
 *
 * Expected: the recognizer synthesizes CALLS edges from getStartupTasks
 * to refreshtask's StartupRun AND PeriodicRun (reason
 * `startup-task-lifecycle`), reconnecting the cron work to main, while
 * leaving the decoy package untouched.
 */

import { describe, it, expect, beforeAll } from 'vitest';
import path from 'path';
import {
  FIXTURES,
  getRelationships,
  getNodesByLabel,
  runPipelineFromRepo,
  type PipelineResult,
} from './helpers.js';

describe('Go startup/cron task → lifecycle CALLS recovery', () => {
  let result: PipelineResult;

  beforeAll(async () => {
    result = await runPipelineFromRepo(
      path.join(FIXTURES, 'go-startup-task-cron'),
      () => {},
    );
  }, 60000);

  it('synthesizes lifecycle CALLS edges from getStartupTasks to the registered task', () => {
    const calls = getRelationships(result, 'CALLS');
    const lifecycle = calls.filter(
      (e) =>
        e.source === 'getStartupTasks' &&
        e.rel.reason === 'startup-task-lifecycle' &&
        e.targetFilePath.endsWith('refreshtask/task.go'),
    );
    const methods = lifecycle.map((e) => e.target).sort();
    expect(methods).toEqual(['PeriodicRun', 'StartupRun']);
  });

  it('does NOT link the unregistered decoy package', () => {
    const calls = getRelationships(result, 'CALLS');
    const decoyEdges = calls.filter(
      (e) =>
        e.rel.reason === 'startup-task-lifecycle' &&
        e.targetFilePath.endsWith('decoytask/task.go'),
    );
    expect(decoyEdges).toEqual([]);
  });

  it('keeps the normal call chain main → initialiseStartupTasks → getStartupTasks', () => {
    const calls = getRelationships(result, 'CALLS');
    const has = (src: string, tgt: string) =>
      calls.some((e) => e.source === src && e.target === tgt);
    expect(has('main', 'initialiseStartupTasks')).toBe(true);
    expect(has('initialiseStartupTasks', 'getStartupTasks')).toBe(true);
  });

  it('registers the /refresh route the cron task fetches', () => {
    const routes = getNodesByLabel(result, 'Route');
    expect(routes).toContain('/refresh');
  });

  it('connects the task lifecycle to the /refresh fetch (StartupRun → refresh → FETCHES)', () => {
    const calls = getRelationships(result, 'CALLS');
    // StartupRun/PeriodicRun both call the package-private refresh().
    const toRefresh = calls.filter(
      (e) =>
        (e.source === 'StartupRun' || e.source === 'PeriodicRun') &&
        e.target === 'refresh' &&
        e.sourceFilePath.endsWith('refreshtask/task.go'),
    );
    expect(toRefresh.length).toBeGreaterThan(0);

    const fetches = getRelationships(result, 'FETCHES');
    const fetchToRefresh = fetches.find(
      (e) => e.target === '/refresh' && e.sourceFilePath.endsWith('refreshtask/task.go'),
    );
    expect(fetchToRefresh).toBeDefined();
  });
});
