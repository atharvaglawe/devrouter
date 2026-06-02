import { describe, it, expect } from 'vitest';
import {
  processStructuralImplements,
  buildStructuralImplementorFiles,
} from '../../src/core/ingestion/structural-implements-processor.js';
import { createKnowledgeGraph } from '../../src/core/graph/graph.js';
import type { KnowledgeGraph } from '../../src/core/graph/types.js';
import { generateId } from '../../src/lib/utils.js';

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

type ConcreteLabel = 'Class' | 'Struct' | 'Trait' | 'Record';
type AnyTypeLabel = ConcreteLabel | 'Interface';

function addType(
  graph: KnowledgeGraph,
  name: string,
  label: AnyTypeLabel,
  language: string,
  filePath?: string,
): string {
  const id = generateId(label, name);
  graph.addNode({
    id,
    label,
    properties: {
      name,
      filePath: filePath ?? `src/${name}.${language === 'go' ? 'go' : 'ts'}`,
      language,
    },
  });
  return id;
}

function addMethod(
  graph: KnowledgeGraph,
  ownerName: string,
  ownerLabel: AnyTypeLabel,
  methodName: string,
  arity: number | undefined,
  language: string,
): string {
  const ownerId = generateId(ownerLabel, ownerName);
  const arityKey = arity ?? 'V';
  const methodId = generateId('Method', `${ownerName}.${methodName}#${arityKey}`);
  graph.addNode({
    id: methodId,
    label: 'Method',
    properties: {
      name: methodName,
      filePath: `src/${ownerName}.${language === 'go' ? 'go' : 'ts'}`,
      language,
      ...(arity !== undefined ? { parameterCount: arity } : {}),
    },
  });
  graph.addRelationship({
    id: generateId('HAS_METHOD', `${ownerId}->${methodId}`),
    sourceId: ownerId,
    targetId: methodId,
    type: 'HAS_METHOD',
    confidence: 1.0,
    reason: '',
  });
  return methodId;
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

describe('processStructuralImplements', () => {
  it('Go struct that satisfies an interface gets an IMPLEMENTS edge', () => {
    const graph = createKnowledgeGraph();
    addType(graph, 'Reader', 'Interface', 'go');
    addType(graph, 'FileReader', 'Struct', 'go');

    addMethod(graph, 'Reader', 'Interface', 'Read', 1, 'go');
    const fileRead = addMethod(graph, 'FileReader', 'Struct', 'Read', 1, 'go');
    addMethod(graph, 'FileReader', 'Struct', 'Close', 0, 'go');

    const result = processStructuralImplements(graph);

    expect(result.implementsEdges).toBe(1);
    expect(result.methodImplementsEdges).toBe(1);

    const impls = graph.relationships.filter((r) => r.type === 'IMPLEMENTS');
    expect(impls).toHaveLength(1);
    expect(impls[0]).toMatchObject({
      sourceId: generateId('Struct', 'FileReader'),
      targetId: generateId('Interface', 'Reader'),
      reason: 'structural',
    });
    expect(impls[0].confidence).toBeCloseTo(0.85);

    const methodImpls = graph.relationships.filter((r) => r.type === 'METHOD_IMPLEMENTS');
    expect(methodImpls.some((r) => r.sourceId === fileRead)).toBe(true);
  });

  it('skips interfaces with no methods (would match every type)', () => {
    const graph = createKnowledgeGraph();
    addType(graph, 'Any', 'Interface', 'go');
    addType(graph, 'Foo', 'Struct', 'go');
    addMethod(graph, 'Foo', 'Struct', 'Bar', 0, 'go');

    const result = processStructuralImplements(graph);

    expect(result.skippedEmptyInterfaces).toBe(1);
    expect(result.implementsEdges).toBe(0);
  });

  it('does not match when concrete type is missing a required method', () => {
    const graph = createKnowledgeGraph();
    addType(graph, 'ReadWriter', 'Interface', 'go');
    addType(graph, 'OnlyReader', 'Struct', 'go');

    addMethod(graph, 'ReadWriter', 'Interface', 'Read', 1, 'go');
    addMethod(graph, 'ReadWriter', 'Interface', 'Write', 1, 'go');
    addMethod(graph, 'OnlyReader', 'Struct', 'Read', 1, 'go');

    const result = processStructuralImplements(graph);

    expect(result.implementsEdges).toBe(0);
  });

  it('arity mismatch on a single method blocks the match', () => {
    const graph = createKnowledgeGraph();
    addType(graph, 'Doer', 'Interface', 'go');
    addType(graph, 'BadDoer', 'Struct', 'go');

    addMethod(graph, 'Doer', 'Interface', 'Do', 2, 'go');
    addMethod(graph, 'BadDoer', 'Struct', 'Do', 0, 'go');

    const result = processStructuralImplements(graph);

    expect(result.implementsEdges).toBe(0);
  });

  it('emits exactly one IMPLEMENTS per (concrete, interface) pair on multiple runs', () => {
    const graph = createKnowledgeGraph();
    addType(graph, 'Stringer', 'Interface', 'go');
    addType(graph, 'Greeting', 'Struct', 'go');
    addMethod(graph, 'Stringer', 'Interface', 'String', 0, 'go');
    addMethod(graph, 'Greeting', 'Struct', 'String', 0, 'go');

    processStructuralImplements(graph);
    const after1 = graph.relationships.filter((r) => r.type === 'IMPLEMENTS').length;
    processStructuralImplements(graph);
    const after2 = graph.relationships.filter((r) => r.type === 'IMPLEMENTS').length;

    expect(after1).toBe(1);
    expect(after2).toBe(1);
  });

  it('does not duplicate an IMPLEMENTS edge already emitted by heritage-processor', () => {
    const graph = createKnowledgeGraph();
    const ifaceId = addType(graph, 'Foo', 'Interface', 'typescript');
    const classId = addType(graph, 'FooImpl', 'Class', 'typescript');
    addMethod(graph, 'Foo', 'Interface', 'doIt', 0, 'typescript');
    addMethod(graph, 'FooImpl', 'Class', 'doIt', 0, 'typescript');

    // Pretend heritage-processor already emitted an explicit IMPLEMENTS.
    graph.addRelationship({
      id: generateId('IMPLEMENTS', `${classId}->${ifaceId}`),
      sourceId: classId,
      targetId: ifaceId,
      type: 'IMPLEMENTS',
      confidence: 0.95,
      reason: '',
    });

    const result = processStructuralImplements(graph);

    expect(result.implementsEdges).toBe(0);
    const impls = graph.relationships.filter((r) => r.type === 'IMPLEMENTS');
    expect(impls).toHaveLength(1);
    expect(impls[0].confidence).toBeCloseTo(0.95);
  });

  it('does not match across languages', () => {
    const graph = createKnowledgeGraph();
    addType(graph, 'Reader', 'Interface', 'go');
    addType(graph, 'TsReader', 'Class', 'typescript');
    addMethod(graph, 'Reader', 'Interface', 'Read', 1, 'go');
    addMethod(graph, 'TsReader', 'Class', 'Read', 1, 'typescript');

    const result = processStructuralImplements(graph);

    expect(result.implementsEdges).toBe(0);
  });

  it('falls back to name-only match (lower confidence) when interface arity is unknown', () => {
    const graph = createKnowledgeGraph();
    addType(graph, 'Closer', 'Interface', 'go');
    addType(graph, 'File', 'Struct', 'go');
    // Interface method without parameterCount → arity unknown.
    addMethod(graph, 'Closer', 'Interface', 'Close', undefined, 'go');
    addMethod(graph, 'File', 'Struct', 'Close', 0, 'go');

    const result = processStructuralImplements(graph);

    expect(result.implementsEdges).toBe(1);
    const impls = graph.relationships.filter((r) => r.type === 'IMPLEMENTS');
    expect(impls[0].confidence).toBeCloseTo(0.7);
  });

  it('one interface, multiple structural implementors all get IMPLEMENTS', () => {
    const graph = createKnowledgeGraph();
    addType(graph, 'Stringer', 'Interface', 'go');
    addType(graph, 'A', 'Struct', 'go');
    addType(graph, 'B', 'Struct', 'go');
    addType(graph, 'C', 'Struct', 'go');

    addMethod(graph, 'Stringer', 'Interface', 'String', 0, 'go');
    addMethod(graph, 'A', 'Struct', 'String', 0, 'go');
    addMethod(graph, 'B', 'Struct', 'String', 0, 'go');
    addMethod(graph, 'C', 'Struct', 'String', 0, 'go');

    const result = processStructuralImplements(graph);

    expect(result.implementsEdges).toBe(3);
    expect(result.methodImplementsEdges).toBe(3);

    const ifaceId = generateId('Interface', 'Stringer');
    const targets = graph.relationships
      .filter((r) => r.type === 'IMPLEMENTS' && r.targetId === ifaceId)
      .map((r) => r.sourceId)
      .sort();
    expect(targets).toEqual(
      [generateId('Struct', 'A'), generateId('Struct', 'B'), generateId('Struct', 'C')].sort(),
    );
  });

  it('skips METHOD_IMPLEMENTS when concrete has overloaded methods (ambiguous mapping)', () => {
    const graph = createKnowledgeGraph();
    addType(graph, 'Doer', 'Interface', 'typescript');
    addType(graph, 'Multi', 'Class', 'typescript');

    addMethod(graph, 'Doer', 'Interface', 'do', 1, 'typescript');
    // Two `do` methods with same arity — overloads, ambiguous.
    addMethod(graph, 'Multi', 'Class', 'do', 1, 'typescript');
    // Force a second method node with a different ID by faking a different type-tag.
    const overloadId = generateId('Method', 'Multi.do#1~overload');
    graph.addNode({
      id: overloadId,
      label: 'Method',
      properties: { name: 'do', filePath: 'src/Multi.ts', parameterCount: 1, language: 'typescript' },
    });
    graph.addRelationship({
      id: generateId('HAS_METHOD', `${generateId('Class', 'Multi')}->${overloadId}`),
      sourceId: generateId('Class', 'Multi'),
      targetId: overloadId,
      type: 'HAS_METHOD',
      confidence: 1.0,
      reason: '',
    });

    const result = processStructuralImplements(graph);

    // Class still implements interface (method-set is satisfied), but
    // METHOD_IMPLEMENTS is skipped because the mapping is ambiguous.
    expect(result.implementsEdges).toBe(1);
    expect(result.methodImplementsEdges).toBe(0);
  });

  it('propagates interface methods through EXTENDS / IMPLEMENTS edges (e.g. Go interface embedding)', () => {
    const graph = createKnowledgeGraph();
    addType(graph, 'Reader', 'Interface', 'go');
    addType(graph, 'Closer', 'Interface', 'go');
    addType(graph, 'ReadCloser', 'Interface', 'go');
    addType(graph, 'FileStore', 'Struct', 'go');

    addMethod(graph, 'Reader', 'Interface', 'Read', 1, 'go');
    addMethod(graph, 'Closer', 'Interface', 'Close', 0, 'go');
    addMethod(graph, 'FileStore', 'Struct', 'Read', 1, 'go');
    addMethod(graph, 'FileStore', 'Struct', 'Close', 0, 'go');

    // ReadCloser embeds Reader and Closer — emitted by heritage-processor as
    // IMPLEMENTS (resolveExtendsType maps "parent is Interface" to IMPLEMENTS).
    const readCloserId = generateId('Interface', 'ReadCloser');
    const readerId = generateId('Interface', 'Reader');
    const closerId = generateId('Interface', 'Closer');
    graph.addRelationship({
      id: generateId('IMPLEMENTS', `${readCloserId}->${readerId}`),
      sourceId: readCloserId,
      targetId: readerId,
      type: 'IMPLEMENTS',
      confidence: 1.0,
      reason: 'embedding',
    });
    graph.addRelationship({
      id: generateId('IMPLEMENTS', `${readCloserId}->${closerId}`),
      sourceId: readCloserId,
      targetId: closerId,
      type: 'IMPLEMENTS',
      confidence: 1.0,
      reason: 'embedding',
    });

    const result = processStructuralImplements(graph);

    // FileStore should now structurally implement ReadCloser (and Reader, Closer).
    expect(result.implementsEdges).toBeGreaterThanOrEqual(3);
    const fileStoreId = generateId('Struct', 'FileStore');
    const targets = graph.relationships
      .filter((r) => r.type === 'IMPLEMENTS' && r.sourceId === fileStoreId)
      .map((r) => r.targetId)
      .sort();
    expect(targets).toEqual([readCloserId, readerId, closerId].sort());
  });

  it('disables structural matching for JVM languages (Java/Kotlin/C#/PHP)', () => {
    // Even with a perfect, multi-method match, the processor must skip
    // structural detection in JVM-style languages — heritage already
    // captures every real `implements` and structural-only matches collide
    // with shared CRUD method names across controllers and services.
    const graph = createKnowledgeGraph();
    addType(graph, 'Repository', 'Interface', 'java');
    addType(graph, 'OrderRepoImpl', 'Class', 'java');

    addMethod(graph, 'Repository', 'Interface', 'save', 1, 'java');
    addMethod(graph, 'Repository', 'Interface', 'findById', 1, 'java');
    addMethod(graph, 'OrderRepoImpl', 'Class', 'save', 1, 'java');
    addMethod(graph, 'OrderRepoImpl', 'Class', 'findById', 1, 'java');

    const result = processStructuralImplements(graph);

    expect(result.implementsEdges).toBe(0);
    expect(result.skippedDisabledLanguage).toBe(1);
  });

  it.each([
    ['kotlin'],
    ['csharp'],
    ['php'],
  ])('disables structural matching for %s as well', (lang) => {
    const graph = createKnowledgeGraph();
    addType(graph, 'Repo', 'Interface', lang);
    addType(graph, 'RepoImpl', 'Class', lang);
    addMethod(graph, 'Repo', 'Interface', 'save', 1, lang);
    addMethod(graph, 'Repo', 'Interface', 'find', 1, lang);
    addMethod(graph, 'RepoImpl', 'Class', 'save', 1, lang);
    addMethod(graph, 'RepoImpl', 'Class', 'find', 1, lang);

    const result = processStructuralImplements(graph);

    expect(result.implementsEdges).toBe(0);
    expect(result.skippedDisabledLanguage).toBe(1);
  });

  it('still allows single-method interfaces in Go (Reader / Stringer / Closer pattern)', () => {
    const graph = createKnowledgeGraph();
    addType(graph, 'Stringer', 'Interface', 'go');
    addType(graph, 'Greeting', 'Struct', 'go');
    addMethod(graph, 'Stringer', 'Interface', 'String', 0, 'go');
    addMethod(graph, 'Greeting', 'Struct', 'String', 0, 'go');

    const result = processStructuralImplements(graph);

    expect(result.implementsEdges).toBe(1);
    expect(result.skippedSmallInterfaces).toBe(0);
  });

  it('a concrete type may implement multiple interfaces simultaneously', () => {
    const graph = createKnowledgeGraph();
    addType(graph, 'Reader', 'Interface', 'go');
    addType(graph, 'Closer', 'Interface', 'go');
    addType(graph, 'ReadCloser', 'Struct', 'go');

    addMethod(graph, 'Reader', 'Interface', 'Read', 1, 'go');
    addMethod(graph, 'Closer', 'Interface', 'Close', 0, 'go');
    addMethod(graph, 'ReadCloser', 'Struct', 'Read', 1, 'go');
    addMethod(graph, 'ReadCloser', 'Struct', 'Close', 0, 'go');

    const result = processStructuralImplements(graph);

    expect(result.implementsEdges).toBe(2);
    const sourceId = generateId('Struct', 'ReadCloser');
    const targets = graph.relationships
      .filter((r) => r.type === 'IMPLEMENTS' && r.sourceId === sourceId)
      .map((r) => r.targetId)
      .sort();
    expect(targets).toEqual(
      [generateId('Interface', 'Reader'), generateId('Interface', 'Closer')].sort(),
    );
  });
});

// ---------------------------------------------------------------------------
// buildStructuralImplementorFiles — the interface-dispatch index consumed by
// call resolution (interface name → implementor file paths). Mirrors the
// structural matching but keyed for HeritageMap.getImplementorFiles.
// ---------------------------------------------------------------------------

/** Add a method node owned by `ownerName` but declared in an arbitrary file. */
function addMethodInFile(
  graph: KnowledgeGraph,
  ownerName: string,
  ownerLabel: AnyTypeLabel,
  methodName: string,
  arity: number | undefined,
  language: string,
  filePath: string,
): string {
  const ownerId = generateId(ownerLabel, ownerName);
  const methodId = generateId('Method', `${ownerName}.${methodName}#${arity ?? 'V'}@${filePath}`);
  graph.addNode({
    id: methodId,
    label: 'Method',
    properties: {
      name: methodName,
      filePath,
      language,
      ...(arity !== undefined ? { parameterCount: arity } : {}),
    },
  });
  graph.addRelationship({
    id: generateId('HAS_METHOD', `${ownerId}->${methodId}`),
    sourceId: ownerId,
    targetId: methodId,
    type: 'HAS_METHOD',
    confidence: 1.0,
    reason: '',
  });
  return methodId;
}

describe('buildStructuralImplementorFiles', () => {
  it('maps a Go interface name to its structural implementor files', () => {
    const graph = createKnowledgeGraph();
    // addType/addMethod default both the struct node and its method to
    // src/<Name>.go, so the implementor file is src/CleanupJob.go.
    addType(graph, 'Task', 'Interface', 'go');
    addType(graph, 'CleanupJob', 'Struct', 'go');

    addMethod(graph, 'Task', 'Interface', 'Execute', 0, 'go');
    addMethod(graph, 'CleanupJob', 'Struct', 'Execute', 0, 'go');

    const index = buildStructuralImplementorFiles(graph);

    const files = index.get('Task');
    expect(files, 'Task should have implementor files').toBeDefined();
    expect([...files!]).toContain('src/CleanupJob.go');
  });

  it('includes every file holding an implementor method (Go split-file packages)', () => {
    const graph = createKnowledgeGraph();
    addType(graph, 'Server', 'Interface', 'go', 'src/iface.go');
    // Struct declared in src/HTTPServer.go (addType default), its two methods
    // spread across two more files.
    addType(graph, 'HTTPServer', 'Struct', 'go');
    addMethod(graph, 'Server', 'Interface', 'Start', 0, 'go');
    addMethod(graph, 'Server', 'Interface', 'Stop', 0, 'go');
    addMethodInFile(graph, 'HTTPServer', 'Struct', 'Start', 0, 'go', 'src/server_start.go');
    addMethodInFile(graph, 'HTTPServer', 'Struct', 'Stop', 0, 'go', 'src/server_stop.go');

    const index = buildStructuralImplementorFiles(graph);
    const files = index.get('Server');
    expect(files).toBeDefined();
    // Both method files plus the struct declaration file must be present, so a
    // subsequent lookupExactAll(file, 'Start'/'Stop') can find the method
    // regardless of which file it lives in.
    expect([...files!].sort()).toEqual(
      ['src/HTTPServer.go', 'src/server_start.go', 'src/server_stop.go'].sort(),
    );
  });

  it('does not index an interface when no concrete type satisfies it', () => {
    const graph = createKnowledgeGraph();
    addType(graph, 'ReadWriter', 'Interface', 'go');
    addType(graph, 'OnlyReader', 'Struct', 'go');
    addMethod(graph, 'ReadWriter', 'Interface', 'Read', 1, 'go');
    addMethod(graph, 'ReadWriter', 'Interface', 'Write', 1, 'go');
    addMethod(graph, 'OnlyReader', 'Struct', 'Read', 1, 'go');

    const index = buildStructuralImplementorFiles(graph);
    expect(index.has('ReadWriter')).toBe(false);
  });

  it('skips zero-method interfaces (would match every type)', () => {
    const graph = createKnowledgeGraph();
    addType(graph, 'Any', 'Interface', 'go');
    addType(graph, 'Foo', 'Struct', 'go');
    addMethod(graph, 'Foo', 'Struct', 'Bar', 0, 'go');

    const index = buildStructuralImplementorFiles(graph);
    expect(index.has('Any')).toBe(false);
  });

  it('does not match across languages', () => {
    const graph = createKnowledgeGraph();
    addType(graph, 'Reader', 'Interface', 'go', 'src/reader.go');
    addType(graph, 'TsReader', 'Class', 'typescript', 'src/reader.ts');
    addMethod(graph, 'Reader', 'Interface', 'Read', 1, 'go');
    addMethod(graph, 'TsReader', 'Class', 'Read', 1, 'typescript');

    const index = buildStructuralImplementorFiles(graph);
    expect(index.has('Reader')).toBe(false);
  });

  it('records all implementors of an interface under one name', () => {
    const graph = createKnowledgeGraph();
    addType(graph, 'Stringer', 'Interface', 'go', 'src/iface.go');
    addType(graph, 'A', 'Struct', 'go');
    addType(graph, 'B', 'Struct', 'go');
    addMethod(graph, 'Stringer', 'Interface', 'String', 0, 'go');
    addMethod(graph, 'A', 'Struct', 'String', 0, 'go');
    addMethod(graph, 'B', 'Struct', 'String', 0, 'go');

    const index = buildStructuralImplementorFiles(graph);
    const files = index.get('Stringer');
    expect(files).toBeDefined();
    expect([...files!].sort()).toEqual(['src/A.go', 'src/B.go'].sort());
  });
});
