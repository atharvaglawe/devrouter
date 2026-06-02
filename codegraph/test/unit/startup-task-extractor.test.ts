/**
 * Unit tests for the Go startup/cron background-task recognizer.
 *
 * Two layers under test:
 *   1. `extractGoStartupTaskRegistrations` — finds
 *      `[]startup.StartupTaskInterface{ pkg.GetTask(...) }` slice
 *      literals and captures the enclosing function + constructor /
 *      direct-type elements.
 *   2. `buildStartupTaskEdges` — folds registrations + a symbol lookup
 *      into synthetic CALLS edges from the registration function to the
 *      co-located lifecycle methods (StartupRun/PeriodicRun) of each
 *      registered task package, with conservative disambiguation.
 */

import { describe, it, expect } from 'vitest';
import Parser from 'tree-sitter';
import Go from 'tree-sitter-go';
import {
  extractGoStartupTaskRegistrations,
  buildStartupTaskEdges,
  type StartupTaskSymbolLookup,
} from '../../src/core/ingestion/route-extractors/startup-task-extractor.js';

function parseGo(src: string) {
  const parser = new Parser();
  parser.setLanguage(Go);
  return parser.parse(src);
}

// ─────────────────────────────────────────────────────────────────
// extractGoStartupTaskRegistrations
// ─────────────────────────────────────────────────────────────────

describe('extractGoStartupTaskRegistrations', () => {
  it('captures constructor-call elements with package qualifiers', () => {
    const tree = parseGo(`
package common
func GetStartupTasks() []startup.StartupTaskInterface {
	return []startup.StartupTaskInterface{
		jsversiontask.GetTask(cfg),
		plutotask.GetTask(client, cache),
	}
}
`);
    const regs = extractGoStartupTaskRegistrations(tree.rootNode, 'startuptasks.go');
    expect(regs).toHaveLength(1);
    const reg = regs[0];
    expect(reg.sourceId).toBe('Function:startuptasks.go:GetStartupTasks');
    expect(reg.constructorRefs).toEqual([
      { calleeName: 'GetTask', qualifier: 'jsversiontask' },
      { calleeName: 'GetTask', qualifier: 'plutotask' },
    ]);
    expect(reg.typeRefs).toEqual([]);
  });

  it('captures inline registration in main with direct composite elements', () => {
    const tree = parseGo(`
package main
func main() {
	startup.RegisterTasks([]startup.StartupTaskInterface{
		&SprigTask{},
		backgroundtasks.FMSTask{},
	})
}
`);
    const regs = extractGoStartupTaskRegistrations(tree.rootNode, 'main.go');
    expect(regs).toHaveLength(1);
    expect(regs[0].sourceId).toBe('Function:main.go:main');
    expect(regs[0].typeRefs).toEqual([
      { typeName: 'SprigTask' },
      { typeName: 'FMSTask', qualifier: 'backgroundtasks' },
    ]);
  });

  it('ignores composite literals of unrelated slice types', () => {
    const tree = parseGo(`
package common
func GetThings() []otherpkg.OtherInterface {
	return []otherpkg.OtherInterface{
		foo.GetTask(),
	}
}
`);
    const regs = extractGoStartupTaskRegistrations(tree.rootNode, 'things.go');
    expect(regs).toEqual([]);
  });

  it('does not match the inner argument composites, only the task slice', () => {
    const tree = parseGo(`
package common
func GetStartupTasks() []startup.StartupTaskInterface {
	return []startup.StartupTaskInterface{
		logtask.GetTask("x", genericlogdata.AppParams{AppName: "SCS"}),
	}
}
`);
    const regs = extractGoStartupTaskRegistrations(tree.rootNode, 'startuptasks.go');
    expect(regs).toHaveLength(1);
    // The genericlogdata.AppParams{} composite is an argument, not a slice
    // element — it must not be captured as a task type.
    expect(regs[0].typeRefs).toEqual([]);
    expect(regs[0].constructorRefs).toEqual([
      { calleeName: 'GetTask', qualifier: 'logtask' },
    ]);
  });

  it('skips registrations enclosed in a method (id is not synthesizable here)', () => {
    const tree = parseGo(`
package x
func (r *Registry) load() {
	r.tasks = []startup.StartupTaskInterface{ foo.GetTask() }
}
`);
    const regs = extractGoStartupTaskRegistrations(tree.rootNode, 'x.go');
    expect(regs).toEqual([]);
  });
});

// ─────────────────────────────────────────────────────────────────
// buildStartupTaskEdges
// ─────────────────────────────────────────────────────────────────

/** Build a fake symbol lookup from a list of {name, filePath, nodeId}. */
function fakeSymbols(
  defs: Array<{ name: string; filePath: string; nodeId: string; kind?: 'callable' | 'class' }>,
): StartupTaskSymbolLookup {
  return {
    lookupCallableByName: (name) =>
      defs.filter((d) => d.name === name && d.kind !== 'class'),
    lookupClassByName: (name) =>
      defs.filter((d) => d.name === name && d.kind === 'class'),
  };
}

describe('buildStartupTaskEdges', () => {
  it('links the registration fn to co-located lifecycle methods (qualifier→dir)', () => {
    const edges = buildStartupTaskEdges(
      [
        {
          filePath: 'common/startuptasks.go',
          sourceId: 'Function:common/startuptasks.go:GetStartupTasks',
          constructorRefs: [{ calleeName: 'GetTask', qualifier: 'jsversiontask' }],
          typeRefs: [],
        },
      ],
      fakeSymbols([
        { name: 'GetTask', filePath: 'app/pkg/jsversiontask/task.go', nodeId: 'Function:app/pkg/jsversiontask/task.go:GetTask' },
        { name: 'StartupRun', filePath: 'app/pkg/jsversiontask/task.go', nodeId: 'Method:app/pkg/jsversiontask/task.go:jsVersionTask.StartupRun#0' },
        { name: 'PeriodicRun', filePath: 'app/pkg/jsversiontask/task.go', nodeId: 'Method:app/pkg/jsversiontask/task.go:jsVersionTask.PeriodicRun#0' },
      ]),
    );
    const targets = edges.map((e) => e.targetId).sort();
    expect(targets).toEqual([
      'Method:app/pkg/jsversiontask/task.go:jsVersionTask.PeriodicRun#0',
      'Method:app/pkg/jsversiontask/task.go:jsVersionTask.StartupRun#0',
    ]);
    for (const e of edges) {
      expect(e.sourceId).toBe('Function:common/startuptasks.go:GetStartupTasks');
      expect(e.type).toBe('CALLS');
      expect(e.reason).toBe('startup-task-lifecycle');
      expect(e.confidence).toBeGreaterThan(0);
      expect(e.confidence).toBeLessThan(1);
    }
  });

  it('does NOT cross-link a same-named constructor in a different package', () => {
    const edges = buildStartupTaskEdges(
      [
        {
          filePath: 'common/startuptasks.go',
          sourceId: 'Function:common/startuptasks.go:GetStartupTasks',
          constructorRefs: [{ calleeName: 'GetTask', qualifier: 'plutotask' }],
          typeRefs: [],
        },
      ],
      fakeSymbols([
        // Two GetTask in different packages; only plutotask should match.
        { name: 'GetTask', filePath: 'app/pkg/plutotask/task.go', nodeId: 'Function:app/pkg/plutotask/task.go:GetTask' },
        { name: 'GetTask', filePath: 'app/pkg/jsversiontask/task.go', nodeId: 'Function:app/pkg/jsversiontask/task.go:GetTask' },
        { name: 'StartupRun', filePath: 'app/pkg/plutotask/task.go', nodeId: 'Method:app/pkg/plutotask/task.go:task.StartupRun#0' },
        { name: 'StartupRun', filePath: 'app/pkg/jsversiontask/task.go', nodeId: 'Method:app/pkg/jsversiontask/task.go:jsVersionTask.StartupRun#0' },
      ]),
    );
    expect(edges.map((e) => e.targetId)).toEqual([
      'Method:app/pkg/plutotask/task.go:task.StartupRun#0',
    ]);
  });

  it('skips unqualified calls with multiple definitions (ambiguous)', () => {
    const edges = buildStartupTaskEdges(
      [
        {
          filePath: 'common/startuptasks.go',
          sourceId: 'Function:common/startuptasks.go:GetStartupTasks',
          constructorRefs: [{ calleeName: 'GetTask' }], // no qualifier
          typeRefs: [],
        },
      ],
      fakeSymbols([
        { name: 'GetTask', filePath: 'app/pkg/a/task.go', nodeId: 'Function:app/pkg/a/task.go:GetTask' },
        { name: 'GetTask', filePath: 'app/pkg/b/task.go', nodeId: 'Function:app/pkg/b/task.go:GetTask' },
        { name: 'StartupRun', filePath: 'app/pkg/a/task.go', nodeId: 'Method:app/pkg/a/task.go:a.StartupRun#0' },
        { name: 'StartupRun', filePath: 'app/pkg/b/task.go', nodeId: 'Method:app/pkg/b/task.go:b.StartupRun#0' },
      ]),
    );
    expect(edges).toEqual([]);
  });

  it('resolves direct composite types via the class index', () => {
    const edges = buildStartupTaskEdges(
      [
        {
          filePath: 'weaver/main.go',
          sourceId: 'Function:weaver/main.go:getStartupTasks',
          constructorRefs: [],
          typeRefs: [{ typeName: 'SprigTask', qualifier: 'backgroundtasks' }],
        },
      ],
      fakeSymbols([
        { name: 'SprigTask', filePath: 'app/pkg/backgroundtasks/sprig.go', nodeId: 'Struct:app/pkg/backgroundtasks/sprig.go:SprigTask', kind: 'class' },
        { name: 'StartupRun', filePath: 'app/pkg/backgroundtasks/sprig.go', nodeId: 'Method:app/pkg/backgroundtasks/sprig.go:SprigTask.StartupRun#0' },
      ]),
    );
    expect(edges.map((e) => e.targetId)).toEqual([
      'Method:app/pkg/backgroundtasks/sprig.go:SprigTask.StartupRun#0',
    ]);
  });

  it('follows wrapper indirection via the package subtree (serpjs shape)', () => {
    // `serpjs.GetStartupTasks()` lives in app/pkg/serpjs and returns
    // `cron.GetTask()` — the concrete task + lifecycle live one package
    // deeper at app/pkg/serpjs/internal/cron. The resolved wrapper dir has
    // no lifecycle of its own, so the subtree fallback must reach the
    // subpackage's StartupRun/PeriodicRun.
    const edges = buildStartupTaskEdges(
      [
        {
          filePath: 'app/cmd/serp/startup.go',
          sourceId: 'Function:app/cmd/serp/startup.go:GetStartupTasks',
          constructorRefs: [{ calleeName: 'GetStartupTasks', qualifier: 'serpjs' }],
          typeRefs: [],
        },
      ],
      fakeSymbols([
        { name: 'GetStartupTasks', filePath: 'app/pkg/serpjs/serpjs.go', nodeId: 'Function:app/pkg/serpjs/serpjs.go:GetStartupTasks' },
        { name: 'StartupRun', filePath: 'app/pkg/serpjs/internal/cron/cron.go', nodeId: 'Method:app/pkg/serpjs/internal/cron/cron.go:serpJsCronTask.StartupRun#0' },
        { name: 'PeriodicRun', filePath: 'app/pkg/serpjs/internal/cron/cron.go', nodeId: 'Method:app/pkg/serpjs/internal/cron/cron.go:serpJsCronTask.PeriodicRun#0' },
      ]),
    );
    expect(edges.map((e) => e.targetId).sort()).toEqual([
      'Method:app/pkg/serpjs/internal/cron/cron.go:serpJsCronTask.PeriodicRun#0',
      'Method:app/pkg/serpjs/internal/cron/cron.go:serpJsCronTask.StartupRun#0',
    ]);
    // Subtree-fallback edges are emitted at the lower confidence band.
    for (const e of edges) expect(e.confidence).toBeLessThan(0.75);
  });

  it('does not use the subtree fallback when the package has direct lifecycle methods', () => {
    // refreshtask has its OWN StartupRun, plus an unrelated subpackage
    // task. The direct hit must win and the sibling subpackage must NOT
    // be pulled in.
    const edges = buildStartupTaskEdges(
      [
        {
          filePath: 'common/startuptasks.go',
          sourceId: 'Function:common/startuptasks.go:GetStartupTasks',
          constructorRefs: [{ calleeName: 'GetTask', qualifier: 'refreshtask' }],
          typeRefs: [],
        },
      ],
      fakeSymbols([
        { name: 'GetTask', filePath: 'app/pkg/refreshtask/task.go', nodeId: 'Function:app/pkg/refreshtask/task.go:GetTask' },
        { name: 'StartupRun', filePath: 'app/pkg/refreshtask/task.go', nodeId: 'Method:app/pkg/refreshtask/task.go:refreshTask.StartupRun#0' },
        { name: 'StartupRun', filePath: 'app/pkg/refreshtask/internal/helper/h.go', nodeId: 'Method:app/pkg/refreshtask/internal/helper/h.go:helper.StartupRun#0' },
      ]),
    );
    expect(edges.map((e) => e.targetId)).toEqual([
      'Method:app/pkg/refreshtask/task.go:refreshTask.StartupRun#0',
    ]);
    expect(edges[0].confidence).toBe(0.75);
  });

  it('emits nothing when the task package defines no lifecycle methods', () => {
    const edges = buildStartupTaskEdges(
      [
        {
          filePath: 'common/startuptasks.go',
          sourceId: 'Function:common/startuptasks.go:GetStartupTasks',
          constructorRefs: [{ calleeName: 'GetTask', qualifier: 'plaintask' }],
          typeRefs: [],
        },
      ],
      fakeSymbols([
        { name: 'GetTask', filePath: 'app/pkg/plaintask/task.go', nodeId: 'Function:app/pkg/plaintask/task.go:GetTask' },
        // No StartupRun/PeriodicRun in this dir (uses embedded default).
      ]),
    );
    expect(edges).toEqual([]);
  });
});
