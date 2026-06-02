/**
 * Integration: plain-PHP routes derived from .htaccess + AST fallback
 * filtering + cross-file FETCHES → Route join.
 *
 * The fixture mimics the cmadserving shape: a doc-root `.htaccess`
 * that rewrites public URLs to .php handler files, plus one client
 * file that hits one of the routes through plain-PHP outbound HTTP
 * (`file_get_contents`). Library .php files must NOT pollute the
 * route registry.
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

describe('PHP plain routes from htaccess', () => {
  let result: PipelineResult;

  beforeAll(async () => {
    result = await runPipelineFromRepo(path.join(FIXTURES, 'php-htaccess-routes'), () => {});
  }, 60000);

  it('creates Route nodes for each .htaccess RewriteRule that targets a .php file', () => {
    const routes = getNodesByLabel(result, 'Route');
    expect(routes).toContain('/trf');
    expect(routes).toContain('/healthz');
    expect(routes).toContain('/ads');
  });

  it('skips external redirects ([R] flag) and static-asset rewrites', () => {
    const routes = getNodesByLabel(result, 'Route');
    // /old has [R=301] → external redirect, must not appear.
    expect(routes).not.toContain('/old');
    // /main.js rewrites to a .js file — must not appear.
    expect(routes).not.toContain('/main.js');
  });

  it('does NOT create a Route for the pure library file lib.php', () => {
    const routes = getNodesByLabel(result, 'Route');
    expect(routes).not.toContain('/lib');
  });

  it('drops AST file-based fallback for .php files already covered by htaccess', () => {
    // transfer.php, health.php, ads.php are all htaccess targets; the
    // per-file AST extractor would otherwise emit /transfer, /health,
    // /ads pathTemplates pointing at the same handler files. With
    // htaccess present, the pipeline filters those out so the public
    // URL contract (the htaccess one) is the only Route.
    const routes = getNodesByLabel(result, 'Route');
    expect(routes).not.toContain('/transfer');
    expect(routes).not.toContain('/health');
    expect(routes).not.toContain('/ads.php');
  });

  it('creates HANDLES_ROUTE edges binding handler files to their htaccess URLs', () => {
    const edges = getRelationships(result, 'HANDLES_ROUTE');
    const healthEdge = edges.find((e) => e.target === '/healthz');
    expect(healthEdge).toBeDefined();
    expect(healthEdge!.sourceFilePath).toContain('health.php');

    const trfEdge = edges.find((e) => e.target === '/trf');
    expect(trfEdge).toBeDefined();
    expect(trfEdge!.sourceFilePath).toContain('transfer.php');
  });

  it('creates a FETCHES edge from client.php to the /healthz Route', () => {
    const edges = getRelationships(result, 'FETCHES');
    const fetchToHealthz = edges.find(
      (e) => e.target === '/healthz' && e.sourceFilePath.endsWith('client.php'),
    );
    expect(fetchToHealthz).toBeDefined();
  });
});
