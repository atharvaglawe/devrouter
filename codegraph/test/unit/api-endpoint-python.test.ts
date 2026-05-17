/**
 * Unit tests for the generic Python API-endpoint extractor.
 *
 * Fixtures live in `codegraph/test/fixtures/api-endpoints/python/`.
 */

import { describe, it, expect, beforeAll } from 'vitest';
import Parser from 'tree-sitter';
import Python from 'tree-sitter-python';
import * as fs from 'node:fs';
import * as path from 'node:path';
import { extractPythonApiEndpoints } from '../../src/core/ingestion/route-extractors/api-endpoint-python.js';
import type {
  RouteRegistration,
  ClientCall,
} from '../../src/core/ingestion/route-extractors/api-endpoint-types.js';

const FIXTURE_DIR = path.resolve(__dirname, '..', 'fixtures', 'api-endpoints', 'python');

function parsePyFile(file: string) {
  const parser = new Parser();
  parser.setLanguage(Python);
  const src = fs.readFileSync(path.join(FIXTURE_DIR, file), 'utf-8');
  return parser.parse(src);
}

const findRoute = (
  routes: RouteRegistration[],
  predicate: (r: RouteRegistration) => boolean,
) => routes.find(predicate);

const findCall = (
  calls: ClientCall[],
  predicate: (c: ClientCall) => boolean,
) => calls.find(predicate);

// ─────────────────────────────────────────────────────────────────
// Server
// ─────────────────────────────────────────────────────────────────

describe('extractPythonApiEndpoints — server forms', () => {
  let routes: RouteRegistration[];

  beforeAll(() => {
    const tree = parsePyFile('server.py');
    const result = extractPythonApiEndpoints(tree.rootNode, 'server.py');
    routes = result.routes;
  });

  it('FastAPI @app.get("/items")', () => {
    const r = findRoute(
      routes,
      (x) => x.framework === 'python.app' && x.pathTemplate === '/items' && x.method === 'GET',
    );
    expect(r).toBeDefined();
    expect(r?.handlerSymbol).toBe('list_items');
  });

  it('FastAPI @app.post on a sync function', () => {
    const r = findRoute(
      routes,
      (x) => x.framework === 'python.app' && x.pathTemplate === '/items' && x.method === 'POST',
    );
    expect(r).toBeDefined();
    expect(r?.handlerSymbol).toBe('create_item');
  });

  it('APIRouter prefix joins the decorator path', () => {
    // The router was both `APIRouter(prefix="/api/v1")` AND
    // `app.include_router(router, prefix="/v2-extra")` so the
    // effective prefix is "/v2-extra/api/v1".
    const r = findRoute(
      routes,
      (x) => x.method === 'GET' && x.pathTemplate === '/v2-extra/api/v1/users/{id}',
    );
    expect(r).toBeDefined();
    expect(r?.handlerSymbol).toBe('get_user');
  });

  it('APIRouter @router.delete with composed prefix', () => {
    const r = findRoute(
      routes,
      (x) => x.method === 'DELETE' && x.pathTemplate === '/v2-extra/api/v1/users/{id}',
    );
    expect(r).toBeDefined();
  });

  it('Flask @app.route with explicit methods', () => {
    const get = findRoute(
      routes,
      (x) => x.framework === 'flask' && x.pathTemplate === '/legacy' && x.method === 'GET',
    );
    const post = findRoute(
      routes,
      (x) => x.framework === 'flask' && x.pathTemplate === '/legacy' && x.method === 'POST',
    );
    expect(get).toBeDefined();
    expect(post).toBeDefined();
  });

  it('Flask @app.route defaults to GET when no methods=…', () => {
    const r = findRoute(
      routes,
      (x) => x.framework === 'flask' && x.pathTemplate === '/default-method' && x.method === 'GET',
    );
    expect(r).toBeDefined();
  });

  it('Flask Blueprint applies url_prefix', () => {
    const r = findRoute(
      routes,
      (x) => x.framework === 'flask' && x.pathTemplate === '/v2/things' && x.method === 'GET',
    );
    expect(r).toBeDefined();
  });

  it('aiohttp router.add_get("/x", h)', () => {
    const r = findRoute(
      routes,
      (x) => x.framework === 'aiohttp' && x.pathTemplate === '/health' && x.method === 'GET',
    );
    expect(r).toBeDefined();
  });

  it('aiohttp router.add_post', () => {
    const r = findRoute(
      routes,
      (x) => x.framework === 'aiohttp' && x.pathTemplate === '/upload' && x.method === 'POST',
    );
    expect(r).toBeDefined();
  });

  it('aiohttp router.add_route("PATCH", "/x", h)', () => {
    const r = findRoute(
      routes,
      (x) => x.framework === 'aiohttp' && x.pathTemplate === '/patch-me' && x.method === 'PATCH',
    );
    expect(r).toBeDefined();
  });

  it('Tornado Application([(r"/x", H), …])', () => {
    const a = findRoute(
      routes,
      (x) => x.framework === 'tornado' && x.pathTemplate === '/tornado/x',
    );
    const b = findRoute(
      routes,
      (x) => x.framework === 'tornado' && x.pathTemplate === '/tornado/y/{id}',
    );
    expect(a).toBeDefined();
    expect(b).toBeDefined();
  });

  it('Django path("hello/", view) inside urlpatterns', () => {
    const r = findRoute(
      routes,
      (x) => x.framework === 'django' && x.pathTemplate === '/hello',
    );
    expect(r).toBeDefined();
    expect(r?.method).toBe('*');
    expect(r?.handlerSymbol).toBe('hello_view');
  });

  it('Django re_path inside urlpatterns', () => {
    const r = findRoute(
      routes,
      (x) => x.framework === 'django' && x.pathTemplate.includes('legacy'),
    );
    expect(r).toBeDefined();
  });
});

// ─────────────────────────────────────────────────────────────────
// Client
// ─────────────────────────────────────────────────────────────────

describe('extractPythonApiEndpoints — client forms', () => {
  let calls: ClientCall[];

  beforeAll(() => {
    const tree = parsePyFile('client.py');
    const result = extractPythonApiEndpoints(tree.rootNode, 'client.py');
    calls = result.clientCalls;
  });

  it('requests.get("/x")', () => {
    const c = findCall(
      calls,
      (x) => x.framework === 'requests' && x.method === 'GET' && x.pathLiteral === '/api/items',
    );
    expect(c).toBeDefined();
    expect(c?.callerSymbol).toBe('use_requests');
  });

  it('requests.post("/x", …)', () => {
    const c = findCall(
      calls,
      (x) => x.framework === 'requests' && x.method === 'POST' && x.pathLiteral === '/api/items',
    );
    expect(c).toBeDefined();
  });

  it('requests.delete("/x")', () => {
    const c = findCall(
      calls,
      (x) => x.framework === 'requests' && x.method === 'DELETE' && x.pathLiteral === '/api/items/1',
    );
    expect(c).toBeDefined();
  });

  it('requests.request("PUT", "/x")', () => {
    const c = findCall(
      calls,
      (x) => x.framework === 'requests' && x.method === 'PUT' && x.pathLiteral === '/api/items/1',
    );
    expect(c).toBeDefined();
  });

  it('requests.Session().get from sessionVar', () => {
    const c = findCall(
      calls,
      (x) => x.framework === 'requests' && x.pathLiteral === '/api/users' && x.method === 'GET',
    );
    expect(c).toBeDefined();
    expect(c?.callerSymbol).toBe('use_requests_session');
  });

  it('httpx.get("/x")', () => {
    const c = findCall(
      calls,
      (x) => x.framework === 'httpx' && x.method === 'GET' && x.pathLiteral === '/api/health',
    );
    expect(c).toBeDefined();
  });

  it('httpx.AsyncClient().delete("/x")', () => {
    const c = findCall(
      calls,
      (x) => x.framework === 'httpx' && x.method === 'DELETE' && x.pathLiteral === '/api/x',
    );
    expect(c).toBeDefined();
  });

  it('aiohttp ClientSession().get from sessionVar', () => {
    const c = findCall(
      calls,
      (x) => x.framework === 'aiohttp' && x.method === 'GET' && x.pathLiteral === '/api/aio',
    );
    expect(c).toBeDefined();
  });
});
