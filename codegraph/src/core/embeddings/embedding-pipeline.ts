/**
 * Embedding Pipeline Module
 *
 * Orchestrates the background embedding process:
 * 1. Query embeddable nodes from LadybugDB
 * 2. Generate text representations
 * 3. Batch embed using transformers.js
 * 4. Update LadybugDB with embeddings
 * 5. Create vector index for semantic search
 */

import {
  initEmbedder,
  embedBatch,
  embedText,
  embeddingToArray,
  isEmbedderReady,
} from './embedder.js';
import { generateBatchEmbeddingTexts } from './text-generator.js';
import {
  type EmbeddingProgress,
  type EmbeddingConfig,
  type EmbeddableNode,
  type SemanticSearchResult,
  type ModelProgress,
  DEFAULT_EMBEDDING_CONFIG,
  EMBEDDABLE_LABELS,
} from './types.js';

const isDev = process.env.NODE_ENV === 'development';

/**
 * Progress callback type
 */
export type EmbeddingProgressCallback = (progress: EmbeddingProgress) => void;

/**
 * Query all embeddable nodes from LadybugDB
 * Uses table-specific queries (File has different schema than code elements)
 */
const queryEmbeddableNodes = async (
  executeQuery: (cypher: string) => Promise<any[]>,
): Promise<EmbeddableNode[]> => {
  const allNodes: EmbeddableNode[] = [];

  // Query each embeddable table with table-specific columns
  for (const label of EMBEDDABLE_LABELS) {
    try {
      let query: string;

      if (label === 'File') {
        // File nodes don't have startLine/endLine
        query = `
          MATCH (n:File)
          RETURN n.id AS id, n.name AS name, 'File' AS label, 
                 n.filePath AS filePath, n.content AS content
        `;
      } else {
        // Code elements have startLine/endLine
        query = `
          MATCH (n:${label})
          RETURN n.id AS id, n.name AS name, '${label}' AS label, 
                 n.filePath AS filePath, n.content AS content,
                 n.startLine AS startLine, n.endLine AS endLine
        `;
      }

      const rows = await executeQuery(query);
      for (const row of rows) {
        allNodes.push({
          id: row.id ?? row[0],
          name: row.name ?? row[1],
          label: row.label ?? row[2],
          filePath: row.filePath ?? row[3],
          content: row.content ?? row[4] ?? '',
          startLine: row.startLine ?? row[5],
          endLine: row.endLine ?? row[6],
        });
      }
    } catch (error) {
      // Table might not exist or be empty, continue
      if (isDev) {
        console.warn(`Query for ${label} nodes failed:`, error);
      }
    }
  }

  return allNodes;
};

/**
 * Batch INSERT embeddings into separate CodeEmbedding table
 * Using a separate lightweight table avoids copy-on-write overhead
 * that occurs when UPDATEing nodes with large content fields
 */
const batchInsertEmbeddings = async (
  executeWithReusedStatement: (
    cypher: string,
    paramsList: Array<Record<string, any>>,
  ) => Promise<void>,
  updates: Array<{ id: string; embedding: number[] }>,
): Promise<void> => {
  // INSERT into separate embedding table - much more memory efficient!
  const cypher = `CREATE (e:CodeEmbedding {nodeId: $nodeId, embedding: $embedding})`;
  const paramsList = updates.map((u) => ({ nodeId: u.id, embedding: u.embedding }));
  await executeWithReusedStatement(cypher, paramsList);
};

/**
 * Create the vector index for semantic search
 * Now indexes the separate CodeEmbedding table
 */
let vectorExtensionLoaded = false;

const createVectorIndex = async (
  executeQuery: (cypher: string) => Promise<any[]>,
): Promise<void> => {
  // LadybugDB v0.15+ requires explicit VECTOR extension loading (once per session).
  // We tolerate "already loaded" but NOT a genuine load failure — without the
  // extension CREATE_VECTOR_INDEX would silently no-op and we'd ship a repo
  // with embeddings rows but no index, which is exactly the bug that wedged
  // /api/search (mode=hybrid|semantic) into HTTP 500 on every query for the
  // entire mall benchmark (see bench notes 2026-05-14).
  if (!vectorExtensionLoaded) {
    try {
      await executeQuery('INSTALL VECTOR');
      await executeQuery('LOAD EXTENSION VECTOR');
      vectorExtensionLoaded = true;
    } catch (error) {
      const msg = error instanceof Error ? error.message : String(error);
      // "already loaded" / "already installed" are benign and idempotent;
      // anything else (no internet for INSTALL, permission errors, version
      // mismatch) we surface so callers can decide rather than ship a
      // half-indexed repo.
      if (/already (loaded|installed)/i.test(msg)) {
        vectorExtensionLoaded = true;
      } else {
        throw new Error(`VECTOR extension load failed: ${msg}`);
      }
    }
  }

  const cypher = `
    CALL CREATE_VECTOR_INDEX('CodeEmbedding', 'code_embedding_idx', 'embedding', metric := 'cosine')
  `;

  let createError: unknown;
  try {
    await executeQuery(cypher);
  } catch (error) {
    createError = error;
  }

  // Verify regardless of whether CREATE succeeded or threw. A successful
  // CREATE without verification was the old behaviour and it's what let the
  // mall index go missing for an entire bench day. Verification is one
  // SHOW_INDEXES() call — cheap, deterministic, and idempotent.
  let indexExists = false;
  try {
    const rows = await executeQuery(`CALL SHOW_INDEXES() RETURN *`);
    indexExists = rows.some((row: any) => {
      // LadybugDB's SHOW_INDEXES returns snake_case column names —
      // `index_name`, `table_name`, `index_type`, `extension_loaded`,
      // etc. (verified empirically against a known-working repo on
      // 2026-05-14). Earlier versions of this verify checked for
      // 'index name' (with a space) / camelCase / bare 'name' / 'table',
      // none of which match what LadybugDB actually emits — that mismatch
      // produced false-positive "index was not created" errors on airflow
      // even after the index was registered correctly. We keep the
      // camelCase/space variants as fallbacks in case the column naming
      // changes again in a future LadybugDB release, but the primary
      // probe is the one that LadybugDB actually returns today.
      const name =
        row?.index_name ??
        row?.['index name'] ??
        row?.indexName ??
        row?.name ??
        '';
      const table =
        row?.table_name ??
        row?.['table name'] ??
        row?.tableName ??
        row?.table ??
        '';
      return name === 'code_embedding_idx' && table === 'CodeEmbedding';
    });
  } catch (verifyError) {
    // SHOW_INDEXES itself failed (very rare) — we can't tell either way, so
    // we report the original CREATE error if there was one, else the verify
    // error. Either way we throw rather than ship silent breakage.
    throw new Error(
      `Vector index verification failed: ${
        verifyError instanceof Error ? verifyError.message : String(verifyError)
      }${createError ? ` (create-index also threw: ${
        createError instanceof Error ? createError.message : String(createError)
      })` : ''}`,
    );
  }

  if (indexExists) {
    // The "create errored AND index exists" case is exactly the benign
    // "already exists" path — index is healthy, error was idempotent.
    if (createError && isDev) {
      console.warn(
        'Vector index already existed (create-index threw, verified present):',
        createError,
      );
    }
    return;
  }

  // Index is missing. Whether or not CREATE threw, this is a hard failure —
  // every subsequent /api/search hybrid|semantic call will 500.
  const detail = createError
    ? createError instanceof Error
      ? createError.message
      : String(createError)
    : 'CREATE_VECTOR_INDEX returned without error but the index was not registered';
  throw new Error(
    `Vector index 'code_embedding_idx' was not created on CodeEmbedding — semantic search will fail. ` +
      `Root cause: ${detail}`,
  );
};

/**
 * Run the embedding pipeline
 *
 * @param executeQuery - Function to execute Cypher queries against LadybugDB
 * @param executeWithReusedStatement - Function to execute with reused prepared statement
 * @param onProgress - Callback for progress updates
 * @param config - Optional configuration override
 * @param skipNodeIds - Optional set of node IDs that already have embeddings (incremental mode)
 */
export const runEmbeddingPipeline = async (
  executeQuery: (cypher: string) => Promise<any[]>,
  executeWithReusedStatement: (
    cypher: string,
    paramsList: Array<Record<string, any>>,
  ) => Promise<void>,
  onProgress: EmbeddingProgressCallback,
  config: Partial<EmbeddingConfig> = {},
  skipNodeIds?: Set<string>,
): Promise<void> => {
  const finalConfig = { ...DEFAULT_EMBEDDING_CONFIG, ...config };

  try {
    // Phase 1: Load embedding model
    onProgress({
      phase: 'loading-model',
      percent: 0,
      modelDownloadPercent: 0,
    });

    if (!isEmbedderReady()) {
      await initEmbedder((modelProgress: ModelProgress) => {
        const downloadPercent = modelProgress.progress ?? 0;
        onProgress({
          phase: 'loading-model',
          percent: Math.round(downloadPercent * 0.2),
          modelDownloadPercent: downloadPercent,
        });
      }, finalConfig);
    }

    onProgress({
      phase: 'loading-model',
      percent: 20,
      modelDownloadPercent: 100,
    });

    if (isDev) {
      console.log('🔍 Querying embeddable nodes...');
    }

    // Phase 2: Query embeddable nodes
    let nodes = await queryEmbeddableNodes(executeQuery);

    // Incremental mode: filter out nodes that already have embeddings
    if (skipNodeIds && skipNodeIds.size > 0) {
      const beforeCount = nodes.length;
      nodes = nodes.filter((n) => !skipNodeIds.has(n.id));
      if (isDev) {
        console.log(
          `📦 Incremental embeddings: ${beforeCount} total, ${skipNodeIds.size} cached, ${nodes.length} to embed`,
        );
      }
    }

    const totalNodes = nodes.length;

    if (isDev) {
      console.log(`📊 Found ${totalNodes} embeddable nodes`);
    }

    if (totalNodes === 0) {
      onProgress({
        phase: 'ready',
        percent: 100,
        nodesProcessed: 0,
        totalNodes: 0,
      });
      return;
    }

    // Phase 3: Batch embed nodes
    const batchSize = finalConfig.batchSize;
    const totalBatches = Math.ceil(totalNodes / batchSize);
    let processedNodes = 0;

    onProgress({
      phase: 'embedding',
      percent: 20,
      nodesProcessed: 0,
      totalNodes,
      currentBatch: 0,
      totalBatches,
    });

    for (let batchIndex = 0; batchIndex < totalBatches; batchIndex++) {
      const start = batchIndex * batchSize;
      const end = Math.min(start + batchSize, totalNodes);
      const batch = nodes.slice(start, end);

      // Generate texts for this batch
      const texts = generateBatchEmbeddingTexts(batch, finalConfig);

      // Embed the batch
      const embeddings = await embedBatch(texts);

      // Update LadybugDB with embeddings
      const updates = batch.map((node, i) => ({
        id: node.id,
        embedding: embeddingToArray(embeddings[i]),
      }));

      await batchInsertEmbeddings(executeWithReusedStatement, updates);

      processedNodes += batch.length;

      // Report progress (20-90% for embedding phase)
      const embeddingProgress = 20 + (processedNodes / totalNodes) * 70;
      onProgress({
        phase: 'embedding',
        percent: Math.round(embeddingProgress),
        nodesProcessed: processedNodes,
        totalNodes,
        currentBatch: batchIndex + 1,
        totalBatches,
      });
    }

    // Phase 4: Create vector index
    onProgress({
      phase: 'indexing',
      percent: 90,
      nodesProcessed: totalNodes,
      totalNodes,
    });

    if (isDev) {
      console.log('📇 Creating vector index...');
    }

    await createVectorIndex(executeQuery);

    // Complete
    onProgress({
      phase: 'ready',
      percent: 100,
      nodesProcessed: totalNodes,
      totalNodes,
    });

    if (isDev) {
      console.log('✅ Embedding pipeline complete!');
    }
  } catch (error) {
    const errorMessage = error instanceof Error ? error.message : 'Unknown error';

    if (isDev) {
      console.error('❌ Embedding pipeline error:', error);
    }

    onProgress({
      phase: 'error',
      percent: 0,
      error: errorMessage,
    });

    throw error;
  }
};

/**
 * Perform semantic search using the vector index
 *
 * Uses CodeEmbedding table and queries each node table to get metadata
 *
 * @param executeQuery - Function to execute Cypher queries
 * @param query - Search query text
 * @param k - Number of results to return (default: 10)
 * @param maxDistance - Maximum distance threshold (default: 0.5)
 * @returns Array of search results ordered by relevance
 */
export const semanticSearch = async (
  executeQuery: (cypher: string) => Promise<any[]>,
  query: string,
  k: number = 10,
  maxDistance: number = 0.5,
): Promise<SemanticSearchResult[]> => {
  if (!isEmbedderReady()) {
    throw new Error('Embedding model not initialized. Run embedding pipeline first.');
  }

  // Embed the query
  const queryEmbedding = await embedText(query);
  const queryVec = embeddingToArray(queryEmbedding);
  const queryVecStr = `[${queryVec.join(',')}]`;

  // Query the vector index on CodeEmbedding to get nodeIds and distances
  const vectorQuery = `
    CALL QUERY_VECTOR_INDEX('CodeEmbedding', 'code_embedding_idx', 
      CAST(${queryVecStr} AS FLOAT[${queryVec.length}]), ${k})
    YIELD node AS emb, distance
    WITH emb, distance
    WHERE distance < ${maxDistance}
    RETURN emb.nodeId AS nodeId, distance
    ORDER BY distance
  `;

  const embResults = await executeQuery(vectorQuery);

  if (embResults.length === 0) {
    return [];
  }

  // Group results by label for batched metadata queries
  const byLabel = new Map<string, Array<{ nodeId: string; distance: number }>>();
  for (const embRow of embResults) {
    const nodeId = embRow.nodeId ?? embRow[0];
    const distance = embRow.distance ?? embRow[1];
    const labelEndIdx = nodeId.indexOf(':');
    const label = labelEndIdx > 0 ? nodeId.substring(0, labelEndIdx) : 'Unknown';
    if (!byLabel.has(label)) byLabel.set(label, []);
    byLabel.get(label)!.push({ nodeId, distance });
  }

  // Batch-fetch metadata per label
  const results: SemanticSearchResult[] = [];

  for (const [label, items] of byLabel) {
    const idList = items.map((i) => `'${i.nodeId.replace(/'/g, "''")}'`).join(', ');
    try {
      let nodeQuery: string;
      if (label === 'File') {
        nodeQuery = `
          MATCH (n:File) WHERE n.id IN [${idList}]
          RETURN n.id AS id, n.name AS name, n.filePath AS filePath
        `;
      } else {
        nodeQuery = `
          MATCH (n:${label}) WHERE n.id IN [${idList}]
          RETURN n.id AS id, n.name AS name, n.filePath AS filePath,
                 n.startLine AS startLine, n.endLine AS endLine
        `;
      }
      const nodeRows = await executeQuery(nodeQuery);
      const rowMap = new Map<string, any>();
      for (const row of nodeRows) {
        const id = row.id ?? row[0];
        rowMap.set(id, row);
      }
      for (const item of items) {
        const nodeRow = rowMap.get(item.nodeId);
        if (nodeRow) {
          results.push({
            nodeId: item.nodeId,
            name: nodeRow.name ?? nodeRow[1] ?? '',
            label,
            filePath: nodeRow.filePath ?? nodeRow[2] ?? '',
            distance: item.distance,
            startLine: label !== 'File' ? (nodeRow.startLine ?? nodeRow[3]) : undefined,
            endLine: label !== 'File' ? (nodeRow.endLine ?? nodeRow[4]) : undefined,
          });
        }
      }
    } catch {
      // Table might not exist, skip
    }
  }

  // Re-sort by distance since batch queries may have mixed order
  results.sort((a, b) => a.distance - b.distance);

  return results;
};

/**
 * Semantic search with graph expansion (flattened results)
 *
 * Note: With multi-table schema, graph traversal is simplified.
 * Returns semantic matches with their metadata.
 * For full graph traversal, use execute_vector_cypher tool directly.
 *
 * @param executeQuery - Function to execute Cypher queries
 * @param query - Search query text
 * @param k - Number of initial semantic matches (default: 5)
 * @param _hops - Unused (kept for API compatibility).
 * @returns Semantic matches with metadata
 */
export const semanticSearchWithContext = async (
  executeQuery: (cypher: string) => Promise<any[]>,
  query: string,
  k: number = 5,
  _hops: number = 1,
): Promise<any[]> => {
  // For multi-table schema, just return semantic search results
  // Graph traversal is complex with separate tables - use execute_vector_cypher instead
  const results = await semanticSearch(executeQuery, query, k, 0.5);

  return results.map((r) => ({
    matchId: r.nodeId,
    matchName: r.name,
    matchLabel: r.label,
    matchPath: r.filePath,
    distance: r.distance,
    connectedId: null,
    connectedName: null,
    connectedLabel: null,
    relationType: null,
  }));
};
