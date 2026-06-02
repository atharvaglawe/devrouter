/**
 * Integration: cross-repo URL-grounded handler join.
 *
 * Fixture shape mirrors the goserving ↔ cmadserving bridge case:
 *   - goservice/config/setup/production.yaml binds
 *     `origins.cmserving.renderer: "/scrr.php"`.
 *   - goservice/config/config.go has an OriginConfig struct with
 *     `Renderer string \`yaml:"renderer"\`` and a trivial getter
 *     `GetOriginConfig() OriginConfig`.
 *   - goservice/callsite/scrrmodulemanager.go calls a composite
 *     literal `cmorigin.Request{Path: originConfig.Renderer}`.
 *   - cmadserving/scrr.php is a bootstrap-style PHP entry point.
 *
 * Expected: the URL resolver (Phase 3.4b) walks the alias chain via
 * the trivial getter, joins against `byKeyPath` to recover the
 * literal `/scrr.php`, the PHP basename-preserving route emits a
 * `Route(/scrr.php)`, and the existing URL matcher emits a FETCHES
 * edge from the Go call site to the PHP file.
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

describe('Cross-repo URL-grounded handler join (Go YAML/getter → PHP basename Route)', () => {
  let result: PipelineResult;

  beforeAll(async () => {
    result = await runPipelineFromRepo(
      path.join(FIXTURES, 'cross-repo-url-getter'),
      () => {},
    );
  }, 60000);

  it('emits a basename Route /scrr.php for cmadserving/scrr.php', () => {
    const routes = getNodesByLabel(result, 'Route');
    expect(routes).toContain('/scrr.php');
  });

  it('emits a FETCHES edge from the Go call site to the PHP file via the recovered URL', () => {
    const edges = getRelationships(result, 'FETCHES');
    const fetchToScrr = edges.find(
      (e) =>
        e.target === '/scrr.php' &&
        e.sourceFilePath.endsWith('scrrmodulemanager.go'),
    );
    expect(fetchToScrr).toBeDefined();
  });
});
