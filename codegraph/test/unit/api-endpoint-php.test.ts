/**
 * Unit tests for the plain-PHP API-endpoint extractor.
 *
 * The extractor has two responsibilities:
 *   1. Emit AST file-based Route registrations for request handlers
 *      (top-level files that touch superglobals or call response
 *      builtins). Library files must NOT be tagged as routes.
 *   2. Recognise the three plain-PHP outbound HTTP idioms
 *      (`file_get_contents`, `fopen`, cURL) and emit ClientCalls
 *      with the recovered path literal + (where available) HTTP
 *      method.
 */

import { describe, it, expect, beforeAll } from 'vitest';
import Parser from 'tree-sitter';
import PHP from 'tree-sitter-php';
import * as fs from 'node:fs';
import * as path from 'node:path';
import {
  extractPhpApiEndpoints,
  fileToPathTemplate,
  fileToBasenameTemplate,
} from '../../src/core/ingestion/route-extractors/api-endpoint-php.js';
import type {
  RouteRegistration,
  ClientCall,
} from '../../src/core/ingestion/route-extractors/api-endpoint-types.js';

const FIXTURE_DIR = path.resolve(__dirname, '..', 'fixtures', 'api-endpoints', 'php');

function parsePhpFile(file: string): Parser.Tree {
  const parser = new Parser();
  parser.setLanguage(PHP.php_only as Parser.Language);
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

describe('extractPhpApiEndpoints — server (AST file-based fallback)', () => {
  it('emits a route for a top-level handler that touches $_GET + header()', () => {
    const tree = parsePhpFile('server-handler.php');
    const { routes } = extractPhpApiEndpoints(tree.rootNode, 'server-handler.php');
    const r = findRoute(routes, () => true);
    expect(r).toBeDefined();
    expect(r?.framework).toBe('php.fileBased');
    expect(r?.pathTemplate).toBe('/server-handler');
    expect(r?.handlerSymbol).toBe('server-handler');
    expect(r?.method).toBe('*');
  });

  it('narrows method when the handler gates on $_SERVER["REQUEST_METHOD"] === "POST"', () => {
    const tree = parsePhpFile('server-post-only.php');
    const { routes } = extractPhpApiEndpoints(tree.rootNode, 'server-post-only.php');
    const r = findRoute(routes, () => true);
    expect(r).toBeDefined();
    expect(r?.method).toBe('POST');
  });

  it('pure library file (only function/class declarations) emits NO route', () => {
    const tree = parsePhpFile('server-library.php');
    const { routes } = extractPhpApiEndpoints(tree.rootNode, 'server-library.php');
    expect(routes).toHaveLength(0);
  });

  it('fileToPathTemplate: foo/index.php → /foo/', () => {
    expect(fileToPathTemplate('foo/index.php')).toBe('/foo/');
    expect(fileToPathTemplate('index.php')).toBe('/');
    expect(fileToPathTemplate('api/handler.php')).toBe('/api/handler');
  });

  it('fileToBasenameTemplate: cmadserving/scrr.php → /scrr.php; index.php → null', () => {
    expect(fileToBasenameTemplate('cmadserving/scrr.php')).toBe('/scrr.php');
    expect(fileToBasenameTemplate('a/b/c/transfer.php')).toBe('/transfer.php');
    expect(fileToBasenameTemplate('index.php')).toBeNull();
    expect(fileToBasenameTemplate('foo/index.php')).toBeNull();
    expect(fileToBasenameTemplate('not-php.go')).toBeNull();
  });

  it('emits BOTH stripped and basename routes for a non-index handler', () => {
    const tree = parsePhpFile('server-handler.php');
    const { routes } = extractPhpApiEndpoints(
      tree.rootNode,
      'cmadserving/server-handler.php',
    );
    const paths = routes.map((r) => r.pathTemplate).sort();
    expect(paths).toContain('/cmadserving/server-handler');
    expect(paths).toContain('/server-handler.php');
  });

  it('qualifies bootstrap-style files (define CONTROLLER_ID + service-invocation)', () => {
    const tree = parsePhpFile('server-bootstrap.php');
    const { routes } = extractPhpApiEndpoints(
      tree.rootNode,
      'cmadserving/scrr.php',
    );
    // Expect both routes — stripped `/cmadserving/scrr` and
    // basename `/scrr.php` — and both should be tagged php.fileBased.
    const paths = routes.map((r) => r.pathTemplate).sort();
    expect(paths).toContain('/cmadserving/scrr');
    expect(paths).toContain('/scrr.php');
    for (const r of routes) {
      expect(r.framework).toBe('php.fileBased');
      expect(r.filePath).toBe('cmadserving/scrr.php');
    }
  });
});

describe('extractPhpApiEndpoints — client-side (file_get_contents / fopen / cURL)', () => {
  let clientCalls: ClientCall[];

  beforeAll(() => {
    const tree = parsePhpFile('client-calls.php');
    const result = extractPhpApiEndpoints(tree.rootNode, 'client-calls.php');
    clientCalls = result.clientCalls;
  });

  it('file_get_contents("http://…") becomes a GET ClientCall with path stripped of host', () => {
    const c = findCall(clientCalls, (x) => x.callerSymbol === 'fetchProfile');
    expect(c).toBeDefined();
    expect(c?.pathLiteral).toBe('/profile');
    expect(c?.method).toBe('GET');
    expect(c?.framework).toBe('php.stdlib');
  });

  it('fopen("http://…", …) becomes a ClientCall with method=null', () => {
    const c = findCall(clientCalls, (x) => x.callerSymbol === 'streamLogs');
    expect(c).toBeDefined();
    expect(c?.pathLiteral).toBe('/tail');
    expect(c?.method).toBeNull();
  });

  it('cURL with CURLOPT_CUSTOMREQUEST narrows the method', () => {
    const c = findCall(clientCalls, (x) => x.callerSymbol === 'deleteRecord');
    expect(c).toBeDefined();
    expect(c?.method).toBe('DELETE');
    expect(c?.pathLiteral).toBe('/records');
  });

  it('cURL with CURLOPT_POST=true implies method=POST when no CUSTOMREQUEST', () => {
    const c = findCall(clientCalls, (x) => x.callerSymbol === 'postEvent');
    expect(c).toBeDefined();
    expect(c?.method).toBe('POST');
    expect(c?.pathLiteral).toBe('/ingest');
  });
});
