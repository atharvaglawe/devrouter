/**
 * Unit tests for the generic Java API-endpoint extractor.
 *
 * Fixtures live in `codegraph/test/fixtures/api-endpoints/java/`
 * and intentionally avoid any real repo names — extraction must
 * succeed by AST + annotation shape alone.
 */

import { describe, it, expect, beforeAll } from 'vitest';
import Parser from 'tree-sitter';
import Java from 'tree-sitter-java';
import * as fs from 'node:fs';
import * as path from 'node:path';
import { extractJavaApiEndpoints } from '../../src/core/ingestion/route-extractors/api-endpoint-java.js';
import type {
  RouteRegistration,
  ClientCall,
} from '../../src/core/ingestion/route-extractors/api-endpoint-types.js';

const FIXTURE_DIR = path.resolve(__dirname, '..', 'fixtures', 'api-endpoints', 'java');

function parseJavaFile(file: string) {
  const parser = new Parser();
  parser.setLanguage(Java);
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
// Server-side
// ─────────────────────────────────────────────────────────────────

describe('extractJavaApiEndpoints — server (Spring MVC + JAX-RS)', () => {
  let routes: RouteRegistration[];
  let clientCalls: ClientCall[];

  beforeAll(() => {
    const tree = parseJavaFile('Server.java');
    const result = extractJavaApiEndpoints(tree.rootNode, 'Server.java');
    routes = result.routes;
    clientCalls = result.clientCalls;
  });

  it('Spring verb shortcut: @GetMapping joins class-level prefix', () => {
    const r = findRoute(
      routes,
      (x) => x.framework === 'spring.mvc' && x.pathTemplate === '/api/v1/users' && x.method === 'GET',
    );
    expect(r).toBeDefined();
    expect(r?.handlerSymbol).toBe('listUsers');
    expect(r?.handlerReceiver).toBe('Server');
  });

  it('Spring verb shortcut: @PostMapping(value=...)', () => {
    const r = findRoute(
      routes,
      (x) => x.framework === 'spring.mvc' && x.pathTemplate === '/api/v1/users' && x.method === 'POST',
    );
    expect(r).toBeDefined();
    expect(r?.handlerSymbol).toBe('createUser');
  });

  it('Spring @PutMapping recognises path template', () => {
    const r = findRoute(
      routes,
      (x) => x.method === 'PUT' && x.pathTemplate === '/api/v1/users/{id}',
    );
    expect(r).toBeDefined();
    expect(r?.handlerSymbol).toBe('updateUser');
  });

  it('Spring @DeleteMapping with positional path', () => {
    const r = findRoute(
      routes,
      (x) => x.method === 'DELETE' && x.pathTemplate === '/api/v1/users/{id}',
    );
    expect(r).toBeDefined();
    expect(r?.handlerSymbol).toBe('deleteUser');
  });

  it('Spring @PatchMapping path-array splits into multiple routes', () => {
    const a = findRoute(
      routes,
      (x) => x.method === 'PATCH' && x.pathTemplate === '/api/v1/users/{id}',
    );
    const b = findRoute(
      routes,
      (x) => x.method === 'PATCH' && x.pathTemplate === '/api/v1/people/{id}',
    );
    expect(a).toBeDefined();
    expect(b).toBeDefined();
  });

  it('Spring @RequestMapping(value=, method=) recovers method enum', () => {
    const r = findRoute(
      routes,
      (x) => x.method === 'POST' && x.pathTemplate === '/api/v1/legacy',
    );
    expect(r).toBeDefined();
    expect(r?.handlerSymbol).toBe('legacy');
  });

  it('Spring @RequestMapping with method-array emits one route per method', () => {
    const a = findRoute(
      routes,
      (x) => x.method === 'GET' && x.pathTemplate === '/api/v1/multi',
    );
    const b = findRoute(
      routes,
      (x) => x.method === 'HEAD' && x.pathTemplate === '/api/v1/multi',
    );
    expect(a).toBeDefined();
    expect(b).toBeDefined();
  });

  it('JAX-RS marker @GET joins class @Path("/jaxrs")', () => {
    const r = findRoute(
      routes,
      (x) => x.framework === 'jaxrs' && x.method === 'GET' && x.pathTemplate === '/jaxrs',
    );
    expect(r).toBeDefined();
    expect(r?.handlerSymbol).toBe('list');
  });

  it('JAX-RS marker @POST + sibling @Path("/{id}")', () => {
    const r = findRoute(
      routes,
      (x) => x.framework === 'jaxrs' && x.method === 'POST' && x.pathTemplate === '/jaxrs/{id}',
    );
    expect(r).toBeDefined();
    expect(r?.handlerSymbol).toBe('update');
  });

  it('@FeignClient interface methods are emitted as ClientCalls, not Routes', () => {
    const route = findRoute(routes, (x) => x.pathTemplate === '/test/candidates');
    expect(route).toBeUndefined();
    const call = findCall(
      clientCalls,
      (x) => x.framework === 'spring.feign' && x.pathLiteral === '/test/candidates',
    );
    expect(call).toBeDefined();
    expect(call?.providerTag).toBe('kosmos');
    expect(call?.method).toBe('GET');
  });

  it('@FeignClient @PostMapping → POST client call with provider tag', () => {
    const call = findCall(
      clientCalls,
      (x) => x.framework === 'spring.feign' && x.pathLiteral === '/match',
    );
    expect(call).toBeDefined();
    expect(call?.method).toBe('POST');
    expect(call?.providerTag).toBe('kosmos');
  });
});

// ─────────────────────────────────────────────────────────────────
// Client-side
// ─────────────────────────────────────────────────────────────────

describe('extractJavaApiEndpoints — client forms', () => {
  let calls: ClientCall[];

  beforeAll(() => {
    const tree = parseJavaFile('Client.java');
    const result = extractJavaApiEndpoints(tree.rootNode, 'Client.java');
    calls = result.clientCalls;
  });

  it('RestTemplate.getForObject(url, …)', () => {
    const c = findCall(
      calls,
      (x) => x.framework === 'spring.resttemplate' && x.pathLiteral === '/api/items' && x.method === 'GET',
    );
    expect(c).toBeDefined();
    expect(c?.callerSymbol).toBe('useRestTemplate');
    expect(c?.callerReceiver).toBe('ApiClients');
  });

  it('RestTemplate.postForEntity → POST', () => {
    const c = findCall(
      calls,
      (x) => x.framework === 'spring.resttemplate' && x.pathLiteral === '/api/items' && x.method === 'POST',
    );
    expect(c).toBeDefined();
  });

  it('RestTemplate.exchange recovers method from HttpMethod.X', () => {
    const c = findCall(
      calls,
      (x) => x.framework === 'spring.resttemplate' && x.pathLiteral === '/api/users' && x.method === 'PUT',
    );
    expect(c).toBeDefined();
  });

  it('RestTemplate.delete → DELETE', () => {
    const c = findCall(
      calls,
      (x) => x.framework === 'spring.resttemplate' && x.pathLiteral === '/api/users/{id}' && x.method === 'DELETE',
    );
    expect(c).toBeDefined();
  });

  it('WebClient.get().uri("/x")', () => {
    const c = findCall(
      calls,
      (x) => x.framework === 'spring.webclient' && x.pathLiteral === '/api/health' && x.method === 'GET',
    );
    expect(c).toBeDefined();
  });

  it('WebClient.method(HttpMethod.POST).uri("/x")', () => {
    const c = findCall(
      calls,
      (x) => x.framework === 'spring.webclient' && x.pathLiteral === '/api/orders' && x.method === 'POST',
    );
    expect(c).toBeDefined();
  });

  it('OkHttp Builder().url("/x").get()', () => {
    const c = findCall(
      calls,
      (x) => x.framework === 'okhttp' && x.pathLiteral === '/api/feed' && x.method === 'GET',
    );
    expect(c).toBeDefined();
  });

  it('OkHttp Builder().url("/x").post(...)', () => {
    const c = findCall(
      calls,
      (x) => x.framework === 'okhttp' && x.pathLiteral === '/api/feed' && x.method === 'POST',
    );
    expect(c).toBeDefined();
  });

  it('Apache new HttpGet("/x")', () => {
    const c = findCall(
      calls,
      (x) => x.framework === 'apache.httpclient' && x.pathLiteral === '/api/legacy' && x.method === 'GET',
    );
    expect(c).toBeDefined();
  });

  it('Apache new HttpPost("/x")', () => {
    const c = findCall(
      calls,
      (x) => x.framework === 'apache.httpclient' && x.pathLiteral === '/api/submit' && x.method === 'POST',
    );
    expect(c).toBeDefined();
  });

  it('java.net.http: newBuilder(URI.create("/x")).GET()', () => {
    const c = findCall(
      calls,
      (x) => x.framework === 'java.net.http' && x.pathLiteral === '/api/data' && x.method === 'GET',
    );
    expect(c).toBeDefined();
  });

  it('java.net.http: .uri(URI.create("/x")).method("POST", …)', () => {
    const c = findCall(
      calls,
      (x) => x.framework === 'java.net.http' && x.pathLiteral === '/api/upload' && x.method === 'POST',
    );
    expect(c).toBeDefined();
  });
});
