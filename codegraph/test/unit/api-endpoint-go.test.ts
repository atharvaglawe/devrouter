/**
 * Unit tests for the generic Go API-endpoint extractor.
 *
 * The tests parse the fixture files in
 * `codegraph/test/fixtures/api-endpoints/go/` with tree-sitter and
 * assert that each of the eight recognised forms (3 server +
 * 5 client) emits the expected normalized record. No real repo
 * names appear in fixtures or assertions — the extractor must
 * detect by AST shape alone.
 */

import { describe, it, expect, beforeAll } from 'vitest';
import Parser from 'tree-sitter';
import Go from 'tree-sitter-go';
import * as fs from 'node:fs';
import * as path from 'node:path';
import { extractGoApiEndpoints } from '../../src/core/ingestion/route-extractors/api-endpoint-go.js';
import type {
  RouteRegistration,
  ClientCall,
} from '../../src/core/ingestion/route-extractors/api-endpoint-types.js';

const FIXTURE_DIR = path.resolve(__dirname, '..', 'fixtures', 'api-endpoints', 'go');

function parseGoFile(file: string) {
  const parser = new Parser();
  parser.setLanguage(Go);
  const src = fs.readFileSync(path.join(FIXTURE_DIR, file), 'utf-8');
  return parser.parse(src);
}

function findRoute(
  routes: RouteRegistration[],
  predicate: (r: RouteRegistration) => boolean,
): RouteRegistration | undefined {
  return routes.find(predicate);
}

function findCall(
  calls: ClientCall[],
  predicate: (c: ClientCall) => boolean,
): ClientCall | undefined {
  return calls.find(predicate);
}

// ─────────────────────────────────────────────────────────────────
// Server-side
// ─────────────────────────────────────────────────────────────────

describe('extractGoApiEndpoints — server forms', () => {
  let routes: RouteRegistration[];

  beforeAll(() => {
    const tree = parseGoFile('server.go');
    const result = extractGoApiEndpoints(tree.rootNode, 'server.go');
    routes = result.routes;
  });

  it('Form 1: stdlib HandleFunc(path, handler)', () => {
    const r = findRoute(
      routes,
      (x) => x.framework === 'go.stdlib' && x.pathTemplate === '/health',
    );
    expect(r).toBeDefined();
    expect(r?.method).toBe('*');
    expect(r?.handlerSymbol).toBe('healthHandler');
  });

  it('Form 1: stdlib Handle(path, handler)', () => {
    const r = findRoute(
      routes,
      (x) => x.framework === 'go.stdlib' && x.pathTemplate === '/static',
    );
    expect(r).toBeDefined();
    expect(r?.handlerSymbol).toBe('staticHandler');
  });

  it('Form 2: verb-as-method GET', () => {
    const r = findRoute(
      routes,
      (x) => x.framework === 'go.verb' && x.pathTemplate === '/users' && x.method === 'GET',
    );
    expect(r).toBeDefined();
    expect(r?.handlerSymbol).toBe('listUsers');
  });

  it('Form 2: verb-as-method POST', () => {
    const r = findRoute(
      routes,
      (x) => x.framework === 'go.verb' && x.pathTemplate === '/users' && x.method === 'POST',
    );
    expect(r).toBeDefined();
    expect(r?.handlerSymbol).toBe('createUser');
  });

  it('Form 2: verb-as-method DELETE with path param', () => {
    const r = findRoute(
      routes,
      (x) => x.framework === 'go.verb' && x.method === 'DELETE',
    );
    expect(r).toBeDefined();
    expect(r?.pathTemplate).toBe('/users/:id');
    expect(r?.handlerSymbol).toBe('deleteUser');
  });

  it('Form 3: tagged-register single path', () => {
    const r = findRoute(
      routes,
      (x) =>
        x.framework === 'go.tagged' && x.pathTemplate === '/orders' && x.method === 'POST',
    );
    expect(r).toBeDefined();
    expect(r?.handlerSymbol).toBe('createOrder');
  });

  it('Form 3: tagged-register splits a multi-path slice into one route per element', () => {
    const list = routes.filter((x) => x.handlerSymbol === 'listOrders');
    expect(list).toHaveLength(2);
    const paths = list.map((r) => r.pathTemplate).sort();
    expect(paths).toEqual(['/orders', '/orders/list']);
    for (const r of list) {
      expect(r.method).toBe('GET');
      expect(r.framework).toBe('go.tagged');
    }
  });

  it('group prefix is applied to verb-as-method routes', () => {
    const r = findRoute(routes, (x) => x.handlerSymbol === 'listItems');
    expect(r).toBeDefined();
    expect(r?.pathTemplate).toBe('/v1/items');
    expect(r?.method).toBe('GET');
  });

  it('group prefix is applied to tagged-register routes', () => {
    const r = findRoute(routes, (x) => x.handlerSymbol === 'makePayment');
    expect(r).toBeDefined();
    expect(r?.pathTemplate).toBe('/api/payments');
    expect(r?.method).toBe('POST');
  });

  it('transitive (nested) group prefixes are concatenated', () => {
    const r = findRoute(routes, (x) => x.handlerSymbol === 'healthHandler2');
    expect(r).toBeDefined();
    expect(r?.pathTemplate).toBe('/api/v2/health');
  });

  it('does not double-emit: every registration appears exactly once', () => {
    const seen = new Set<string>();
    for (const r of routes) {
      const key = `${r.framework}:${r.method}:${r.pathTemplate}:${r.lineNumber}:${r.handlerSymbol}`;
      expect(seen.has(key)).toBe(false);
      seen.add(key);
    }
  });
});

// ─────────────────────────────────────────────────────────────────
// Client-side
// ─────────────────────────────────────────────────────────────────

describe('extractGoApiEndpoints — client forms', () => {
  let calls: ClientCall[];

  beforeAll(() => {
    const tree = parseGoFile('client.go');
    const result = extractGoApiEndpoints(tree.rootNode, 'client.go');
    calls = result.clientCalls;
  });

  it('Form 1: stdlib http.Get', () => {
    const c = findCall(
      calls,
      (x) => x.framework === 'go.stdlib' && x.method === 'GET' && x.pathLiteral === '/v1/health',
    );
    expect(c).toBeDefined();
    expect(c?.callerSymbol).toBe('fetchHealth');
  });

  it('Form 1: stdlib http.Post', () => {
    const c = findCall(
      calls,
      (x) => x.framework === 'go.stdlib' && x.method === 'POST' && x.pathLiteral === '/v1/events',
    );
    expect(c).toBeDefined();
  });

  it('Form 1: stdlib http.Head', () => {
    const c = findCall(
      calls,
      (x) => x.framework === 'go.stdlib' && x.method === 'HEAD',
    );
    expect(c?.pathLiteral).toBe('/v1/ping');
  });

  it('Form 2: verb-as-method on a client receiver', () => {
    const c = findCall(
      calls,
      (x) => x.framework === 'go.client' && x.method === 'GET',
    );
    expect(c).toBeDefined();
    expect(c?.pathLiteral).toBe('/v1/users/123');
    expect(c?.callerSymbol).toBe('fetchUser');
  });

  it('Form 3: request builder NewRequest', () => {
    const c = findCall(
      calls,
      (x) =>
        x.framework === 'go.builder' && x.method === 'POST' && x.pathLiteral === '/v1/orders',
    );
    expect(c).toBeDefined();
    expect(c?.callerSymbol).toBe('sendOrder');
  });

  it('Form 3: request builder NewRequestWithContext (literal-string method)', () => {
    const c = findCall(
      calls,
      (x) => x.framework === 'go.builder' && x.method === 'DELETE',
    );
    expect(c).toBeDefined();
    expect(c?.pathLiteral).toBe('/v1/orders/42');
  });

  it('Form 4: options-bag with literal URL + method + provider tag', () => {
    const c = findCall(
      calls,
      (x) => x.framework === 'go.options' && x.pathLiteral === '/v1/inventory',
    );
    expect(c).toBeDefined();
    expect(c?.method).toBe('GET');
    expect(c?.providerTag).toBe('INVENTORY_API');
    expect(c?.callerSymbol).toBe('fetchInventory');
  });

  it('Form 4: options-bag with provider-tag-only URL still emits when method is recoverable', () => {
    const c = findCall(
      calls,
      (x) =>
        x.framework === 'go.options' &&
        x.providerTag === 'pricing-svc' &&
        x.pathLiteral === null,
    );
    expect(c).toBeDefined();
    expect(c?.method).toBe('POST');
    expect(c?.callerSymbol).toBe('fetchPricing');
  });

  it('Form 5: gRPC stub call', () => {
    const c = findCall(
      calls,
      (x) => x.framework === 'go.grpc' && x.pathLiteral === '/Orders/Place',
    );
    expect(c).toBeDefined();
    expect(c?.method).toBe('POST');
    expect(c?.providerTag).toBe('orders');
    expect(c?.callerSymbol).toBe('placeOrder');
  });

  it('Form 4 variant: path-on-config-struct with host companion is recognised', () => {
    const c = findCall(
      calls,
      (x) => x.framework === 'go.options' && x.pathLiteral === '/v1/shipments',
    );
    expect(c).toBeDefined();
    expect(c?.callerSymbol).toBe('buildShipmentConfig');
  });

  it('Form 6: provider-tag factory emits a tag-only call', () => {
    const c = findCall(
      calls,
      (x) => x.framework === 'go.factory' && x.providerTag === 'kosmos',
    );
    expect(c).toBeDefined();
    expect(c?.pathLiteral).toBeNull();
    expect(c?.method).toBeNull();
    expect(c?.callerSymbol).toBe('fetchByTag');
  });

  it('Form 4 variant: bare Path field WITHOUT host/method companion is NOT emitted', () => {
    const c = findCall(
      calls,
      (x) => x.callerSymbol === 'openFile' || x.pathLiteral === '/etc/foo.conf',
    );
    expect(c).toBeUndefined();
  });

  it('does not double-emit: every client call appears exactly once', () => {
    const seen = new Set<string>();
    for (const c of calls) {
      const key = `${c.framework}:${c.method}:${c.pathLiteral}:${c.providerTag}:${c.lineNumber}`;
      expect(seen.has(key)).toBe(false);
      seen.add(key);
    }
  });
});
