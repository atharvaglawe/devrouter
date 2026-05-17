/**
 * Integration test: end-to-end pipeline emits structural IMPLEMENTS edges
 * for languages without an explicit `implements` keyword (Go) and for
 * TypeScript types that satisfy an interface by shape alone (no
 * `implements` clause).
 */
import { describe, it, expect, beforeAll } from 'vitest';
import path from 'path';
import { runPipelineFromRepo } from '../../src/core/ingestion/pipeline.js';
import type { PipelineResult } from '../../src/types/pipeline.js';

const FIXTURE_ROOT = path.resolve(__dirname, '..', 'fixtures', 'structural-implements');

describe('structural IMPLEMENTS — end-to-end', () => {
  describe('Go', () => {
    let result: PipelineResult;

    beforeAll(async () => {
      result = await runPipelineFromRepo(path.join(FIXTURE_ROOT, 'go'), () => {});
    }, 60000);

    it('FileStore implements Reader, Closer, and ReadCloser (all structural)', () => {
      const implByConcrete = collectImplements(result);

      expect(implByConcrete.get('FileStore')).toEqual(
        expect.arrayContaining(['Reader', 'Closer', 'ReadCloser']),
      );
    });

    it('extracts interface method signatures (Reader.Read, Closer.Close)', () => {
      const interfaceMethodNames = new Set<string>();
      for (const rel of result.graph.iterRelationships()) {
        if (rel.type !== 'HAS_METHOD') continue;
        const owner = result.graph.getNode(rel.sourceId);
        const method = result.graph.getNode(rel.targetId);
        if (!owner || !method || owner.label !== 'Interface') continue;
        interfaceMethodNames.add(`${owner.properties.name}.${method.properties.name}`);
      }
      expect(interfaceMethodNames).toContain('Reader.Read');
      expect(interfaceMethodNames).toContain('Closer.Close');
    });

    it('MemoryStore implements Reader but NOT Closer or ReadCloser', () => {
      const implByConcrete = collectImplements(result);

      const targets = implByConcrete.get('MemoryStore') ?? [];
      expect(targets).toContain('Reader');
      expect(targets).not.toContain('Closer');
      expect(targets).not.toContain('ReadCloser');
    });

    it('emits METHOD_IMPLEMENTS edges from concrete methods to interface methods', () => {
      const methodImpls = result.graph.relationships.filter(
        (r) => r.type === 'METHOD_IMPLEMENTS',
      );
      expect(methodImpls.length).toBeGreaterThan(0);

      // FileStore.Read → Reader.Read
      const readPair = methodImpls.find((r) => {
        const src = result.graph.getNode(r.sourceId);
        const tgt = result.graph.getNode(r.targetId);
        return (
          src?.label === 'Method' &&
          src.properties.name === 'Read' &&
          src.properties.filePath?.toString().includes('storage.go') &&
          tgt?.label === 'Method' &&
          tgt.properties.name === 'Read'
        );
      });
      expect(readPair).toBeDefined();
    });
  });

  describe('TypeScript', () => {
    let result: PipelineResult;

    beforeAll(async () => {
      result = await runPipelineFromRepo(path.join(FIXTURE_ROOT, 'typescript'), () => {});
    }, 60000);

    it('ConsoleLogger implements Logger structurally (no `implements` clause)', () => {
      const implByConcrete = collectImplements(result);
      expect(implByConcrete.get('ConsoleLogger')).toContain('Logger');
    });

    it('FileLogger implements both Logger and Closeable structurally', () => {
      const implByConcrete = collectImplements(result);
      const targets = implByConcrete.get('FileLogger') ?? [];
      expect(targets).toEqual(expect.arrayContaining(['Logger', 'Closeable']));
    });

    it('WeirdLogger only implements Logger (Closeable.close has wrong arity)', () => {
      const implByConcrete = collectImplements(result);
      const targets = implByConcrete.get('WeirdLogger') ?? [];
      expect(targets).toContain('Logger');
      expect(targets).not.toContain('Closeable');
    });
  });
});

/** Build a {concreteName -> interfaceNames[]} index from the graph. */
function collectImplements(result: PipelineResult): Map<string, string[]> {
  const implByConcrete = new Map<string, string[]>();
  for (const rel of result.graph.iterRelationships()) {
    if (rel.type !== 'IMPLEMENTS') continue;
    const src = result.graph.getNode(rel.sourceId);
    const tgt = result.graph.getNode(rel.targetId);
    if (!src || !tgt) continue;
    const list = implByConcrete.get(src.properties.name) ?? [];
    list.push(tgt.properties.name);
    implByConcrete.set(src.properties.name, list);
  }
  return implByConcrete;
}
