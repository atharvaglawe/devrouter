/**
 * Integration: Go URL-builder path recovered through a getter +
 * interface-field + constant chain (Phase 3.4c).
 *
 * Fixture shape mirrors the oscar click-URL case:
 *   - router/router.go registers `mux.HandleFunc("/trf", …)` → Route(/trf).
 *   - clickurl/clickurl.go builds a URL via `urlBuilder.SetPath(path)`
 *     where `path := c.adClickRouteService.GetPath()` — the path
 *     literal is never written at the call site.
 *   - adclickroute.GetPath delegates to an interface field
 *     (`a.pathSelector.GetPath()`).
 *   - defaultpath.GetPath returns the package const `DefaultPath = "/trf"`.
 *   - An unrelated otherpath.GetPath returns `"/notroute"` (a non-route
 *     constant) to exercise the multi-candidate route-preference.
 *
 * Expected: the URL-const resolver chases the getter chain to "/trf",
 * stamps the call's fetchURL, and the secondary URL matcher emits a
 * FETCHES edge originating at `getUrl` to the `/trf` Route node. The
 * `/notroute` constant must NOT create an edge.
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

describe('Go URL-builder → constant route (getter + interface-field + const chain)', () => {
  let result: PipelineResult;

  beforeAll(async () => {
    result = await runPipelineFromRepo(
      path.join(FIXTURES, 'go-urlbuilder-const-route'),
      () => {},
    );
  }, 60000);

  it('registers a Route /trf', () => {
    const routes = getNodesByLabel(result, 'Route');
    expect(routes).toContain('/trf');
  });

  it('emits a FETCHES edge originating at getUrl to the /trf route', () => {
    const edges = getRelationships(result, 'FETCHES');
    const fetchToTrf = edges.find(
      (e) =>
        e.target === '/trf' &&
        e.sourceFilePath.endsWith('clickurl.go') &&
        e.source === 'getUrl',
    );
    expect(fetchToTrf).toBeDefined();
  });

  it('does not create a Route or FETCHES edge for the non-route constant /notroute', () => {
    const routes = getNodesByLabel(result, 'Route');
    expect(routes).not.toContain('/notroute');
    const edges = getRelationships(result, 'FETCHES');
    expect(edges.find((e) => e.target === '/notroute')).toBeUndefined();
  });
});
