/**
 * Plain-PHP API-endpoint extractor (no framework support).
 *
 * Detection model
 * ───────────────
 * Server-side routes come from two sources:
 *
 *   1. **htaccess** (authoritative, applied in pipeline.ts) — built
 *      by {@link buildHtaccessIndex} from `.htaccess*` files at the
 *      repo root. This module does NOT emit htaccess routes; pipeline.ts
 *      runs that scan once per analyze and feeds the same
 *      `allExtractedRoutes` array.
 *
 *   2. **AST file-based fallback** (this module) — emitted when the
 *      file's top-level shows the AST shape of a request handler:
 *      reads `$_GET` / `$_POST` / `$_SERVER` / `$_REQUEST` / `$_COOKIE`,
 *      calls `header()` / `echo` / `print_r`, or sets cookies. For
 *      repos that DO have an htaccess registry, pipeline.ts filters
 *      these fallback routes out for files the htaccess already
 *      registers (htaccess is the more accurate URL). For repos
 *      without any htaccess, the fallback IS the only route source.
 *
 * Client-side outbound calls are emitted unconditionally for plain
 * PHP's three idioms:
 *
 *   - `file_get_contents("http://…")`
 *   - `fopen("http://…", …)`
 *   - `curl_setopt($ch, CURLOPT_URL, "http://…")` (method narrowed by
 *      sibling `CURLOPT_CUSTOMREQUEST` settings within the same
 *      function body)
 *
 * Frameworks (Laravel, Slim, Symfony) are explicitly out of scope —
 * those repos have their own registration calls and don't need an
 * htaccess shim.
 */

import type { SyntaxNode } from '../utils/ast-helpers.js';
import {
  type ExtractedApiEndpoints,
  type RouteRegistration,
  type ClientCall,
  type HttpMethod,
  normalizeHttpMethod,
} from './api-endpoint-types.js';

/* ─────────────────────────────────────────────────────────────────
 * Constants
 * ────────────────────────────────────────────────────────────── */

/** PHP superglobals whose presence at top level marks a request handler. */
const REQUEST_SUPERGLOBALS: ReadonlySet<string> = new Set([
  '$_GET',
  '$_POST',
  '$_REQUEST',
  '$_COOKIE',
  '$_FILES',
  '$_SERVER',
  'php://input',
]);

/** Built-in functions whose presence at top level strongly suggests
 *  the file is producing a response (not a pure library). */
const RESPONSE_FUNCS: ReadonlySet<string> = new Set([
  'header',
  'setcookie',
  'http_response_code',
  'json_encode',
  'echo',
  'print',
  'print_r',
  'var_dump',
  'readfile',
]);

/** Default framework tag stamped on each extraction. */
const FRAMEWORK_PHP_FILE_BASED = 'php.fileBased';
const FRAMEWORK_PHP_STDLIB = 'php.stdlib';

/** Confidence bands.
 *  - 0.6 for AST fallback (file shape only; URL is inferred from path).
 *  - 0.9 for client calls where the URL is a static literal. */
const CONF_FILE_BASED = 0.6;
const CONF_CLIENT = 0.9;

/* ─────────────────────────────────────────────────────────────────
 * AST helpers (PHP tree-sitter)
 * ────────────────────────────────────────────────────────────── */

/** Strip surrounding quotes from a PHP `string` node. Handles
 *  single-quoted and bare double-quoted strings (no interpolation).
 *  Returns `null` for non-string nodes, encapsed strings with
 *  variable interpolation, or heredocs. */
function readPhpString(node: SyntaxNode | null | undefined): string | null {
  if (!node) return null;
  if (node.type === 'string') {
    // tree-sitter-php exposes a `string_value` child holding the
    // raw content; fall back to slicing the literal if that's
    // absent (older grammars).
    const value = node.namedChildren?.find((c: SyntaxNode) => c.type === 'string_value');
    if (value && typeof value.text === 'string') return value.text;
    const raw = node.text ?? '';
    if (raw.length >= 2) {
      const c = raw[0];
      if ((c === '"' || c === "'") && raw.endsWith(c)) {
        // Unescape common PHP single-quote escapes.
        const inner = raw.slice(1, -1);
        return c === "'" ? inner.replace(/\\\\/g, '\\').replace(/\\'/g, "'") : inner;
      }
    }
    return null;
  }
  if (node.type === 'encapsed_string') {
    // Only honour encapsed strings that are pure literal (no var
    // interpolation). Reject when any child is `variable_name` or
    // `embedded_*`.
    const hasInterp = (node.namedChildren ?? []).some(
      (c: SyntaxNode) =>
        c.type === 'variable_name' || c.type.startsWith('embedded_') || c.type === 'escape_sequence',
    );
    if (hasInterp) return null;
    const value = (node.namedChildren ?? [])
      .filter((c: SyntaxNode) => c.type === 'string_value')
      .map((c: SyntaxNode) => c.text ?? '')
      .join('');
    if (value.length > 0) return value;
    const raw = node.text ?? '';
    if (raw.length >= 2 && raw.startsWith('"') && raw.endsWith('"')) return raw.slice(1, -1);
    return null;
  }
  return null;
}

/** PHP `argument` nodes wrap an expression. Unwrap to inner. */
function argInner(arg: SyntaxNode): SyntaxNode {
  if (arg.type !== 'argument') return arg;
  const named = arg.namedChildren ?? [];
  return named[named.length - 1] ?? arg;
}

/** Iterate the argument expressions inside a `function_call_expression`
 *  or `member_call_expression`. */
function* callArgs(call: SyntaxNode): IterableIterator<SyntaxNode> {
  const args = call.childForFieldName?.('arguments');
  if (!args) return;
  for (const child of args.namedChildren ?? []) {
    if (child.type === 'argument') yield argInner(child);
    // Some grammars place bare expressions directly under arguments.
    else if (child.isNamed) yield child;
  }
}

/** Get the called function's bare name for a `function_call_expression`.
 *  Returns `null` for member / scoped calls and dynamic invocations. */
function calleeName(call: SyntaxNode): string | null {
  if (call.type !== 'function_call_expression') return null;
  const fn = call.childForFieldName?.('function');
  if (!fn) return null;
  if (fn.type === 'name') return fn.text ?? null;
  if (fn.type === 'qualified_name') {
    const last = fn.namedChildren?.[fn.namedChildren.length - 1];
    return last?.text ?? null;
  }
  return null;
}

/** Walk every named descendant (iterative DFS). */
function* walk(root: SyntaxNode): IterableIterator<SyntaxNode> {
  const stack: SyntaxNode[] = [root];
  while (stack.length > 0) {
    const node = stack.pop()!;
    yield node;
    const children = node.namedChildren ?? [];
    for (let i = children.length - 1; i >= 0; i--) stack.push(children[i]);
  }
}

/** Node types that introduce a new local scope and therefore bound
 *  per-scope analyses (curl handle var lifetime). */
const SCOPE_BOUNDARIES: ReadonlySet<string> = new Set([
  'function_definition',
  'method_declaration',
  'arrow_function',
  'anonymous_function_creation_expression',
]);

/** Walk a node's descendants but never cross into a nested function /
 *  method body. Yields `root` itself first. Used by per-scope passes
 *  so that statements inside an inner function don't leak into the
 *  outer scope's analysis. */
function* walkLocal(root: SyntaxNode): IterableIterator<SyntaxNode> {
  const stack: SyntaxNode[] = [root];
  while (stack.length > 0) {
    const node = stack.pop()!;
    yield node;
    if (node !== root && SCOPE_BOUNDARIES.has(node.type)) continue;
    const children = node.namedChildren ?? [];
    for (let i = children.length - 1; i >= 0; i--) stack.push(children[i]);
  }
}

/** Find the nearest enclosing named function / method declaration of
 *  `node`. Returns `{name, receiver}` where `receiver` is the class
 *  name when inside a method. */
function findEnclosing(node: SyntaxNode | null): { name: string | null; receiver: string | null } {
  let cur: SyntaxNode | null = node;
  while (cur) {
    if (cur.type === 'function_definition') {
      const name = cur.childForFieldName?.('name')?.text ?? null;
      return { name, receiver: null };
    }
    if (cur.type === 'method_declaration') {
      const name = cur.childForFieldName?.('name')?.text ?? null;
      // Walk up to the enclosing class.
      let p: SyntaxNode | null = cur.parent;
      while (p) {
        if (
          p.type === 'class_declaration' ||
          p.type === 'interface_declaration' ||
          p.type === 'trait_declaration'
        ) {
          const cname = p.childForFieldName?.('name')?.text ?? null;
          return { name, receiver: cname };
        }
        p = p.parent;
      }
      return { name, receiver: null };
    }
    cur = cur.parent;
  }
  return { name: null, receiver: null };
}

/* ─────────────────────────────────────────────────────────────────
 * Server-side: AST file-based fallback
 * ────────────────────────────────────────────────────────────── */

/** Method-name shapes that, when invoked at top level on a freshly
 *  instantiated object, suggest the file is a controller-bootstrap
 *  entry point (e.g. `new XxxService(); ->printXxx()` style). We
 *  match common verbs used to "run" / "emit" / "render" a response
 *  rather than library-style configuration calls. */
const BOOTSTRAP_INVOCATION_VERBS = [
  'print',
  'render',
  'handle',
  'serve',
  'execute',
  'process',
  'run',
  'do',
  'dispatch',
  'output',
  'emit',
  'respond',
  'main',
];
const BOOTSTRAP_VERB_RE = new RegExp(
  '^(' + BOOTSTRAP_INVOCATION_VERBS.join('|') + ')[A-Z_0-9]?[A-Za-z0-9_]*$',
);

/** Decide whether the file's TOP-LEVEL statements look like a request
 *  handler. We deliberately ignore code that lives inside function /
 *  class bodies — that's library code.
 *
 *  A top-level statement counts when it:
 *    - references a request superglobal anywhere in its subtree
 *    - calls a response-emitting builtin (`header`, `echo`, …)
 *    - is a bare `echo_statement`
 *    - declares `define('CONTROLLER_ID', …)` (Apache mod_php
 *      bootstrap convention)
 *    - invokes a service-style method whose name matches the
 *      bootstrap verb set on a freshly instantiated service
 *      (e.g. `(new ScrrRenderingService())->printScrrCode()` or
 *      `$svc = new RendererService(); $svc->render();`).
 *
 *  Returns `null` when the file is a pure library, otherwise the
 *  narrowed HTTP method (default `*`). */
function detectFileBasedHandlerMethod(rootNode: SyntaxNode): HttpMethod | null {
  // tree-sitter-php exposes a top-level `program` whose children are
  // `php_tag`-wrapped statements; walk one level into the program.
  const topLevel: SyntaxNode[] = [];
  for (const child of rootNode.namedChildren ?? []) {
    if (
      child.type === 'function_definition' ||
      child.type === 'class_declaration' ||
      child.type === 'interface_declaration' ||
      child.type === 'trait_declaration' ||
      child.type === 'namespace_definition' ||
      child.type === 'enum_declaration' ||
      child.type === 'use_declaration' ||
      child.type === 'const_declaration' ||
      child.type === 'comment'
    ) {
      // Namespace bodies may contain executable code — recurse one
      // level into namespace_definition.
      if (child.type === 'namespace_definition') {
        const body = child.childForFieldName?.('body');
        if (body) {
          for (const inner of body.namedChildren ?? []) {
            topLevel.push(inner);
          }
        }
      }
      continue;
    }
    topLevel.push(child);
  }

  let handlerLike = false;
  let narrowedMethod: HttpMethod | null = null;
  // Bootstrap-pattern signals — collected separately because both
  // pieces (instantiation + verb invocation OR a `CONTROLLER_ID`
  // define) must be present to count, so we OR them at the end.
  let sawControllerIdDefine = false;
  let sawServiceInstantiation = false;
  let sawBootstrapInvocation = false;

  for (const stmt of topLevel) {
    // Quick wins: echo_statement and any expression touching a
    // superglobal / response func.
    if (stmt.type === 'echo_statement') {
      handlerLike = true;
      continue;
    }
    for (const desc of walk(stmt)) {
      // Superglobal reference: variable_name with text matching
      // `$_GET` / `$_POST` / `$_SERVER` / `$_REQUEST` / `$_COOKIE` /
      // `$_FILES`. tree-sitter-php represents these as variable_name.
      if (desc.type === 'variable_name' && REQUEST_SUPERGLOBALS.has(desc.text ?? '')) {
        handlerLike = true;
        // Look for `$_SERVER['REQUEST_METHOD'] === 'VERB'` for method narrowing.
        const m = narrowMethodFromServerCheck(desc);
        if (m) narrowedMethod = narrowedMethod ? narrowedMethod : m;
        continue;
      }
      // Response-emitting builtin call.
      if (desc.type === 'function_call_expression') {
        const name = calleeName(desc);
        if (!name) continue;
        if (RESPONSE_FUNCS.has(name)) handlerLike = true;
        // `define('CONTROLLER_ID', "SCRR")` — Apache mod_php
        // bootstrap convention used by some plain-PHP frameworks.
        if (name === 'define') {
          const args = [...callArgs(desc)];
          if (args.length >= 1) {
            const key = readPhpString(args[0]);
            if (key && /^CONTROLLER(_ID)?$/i.test(key)) {
              sawControllerIdDefine = true;
            }
          }
        }
      }
      // `new XxxService(...)` somewhere in this top-level statement.
      if (desc.type === 'object_creation_expression') {
        sawServiceInstantiation = true;
      }
      // `$svc->printXxx(...)` or `(new X())->renderXxx(...)` — any
      // top-level member-call whose method name matches the
      // bootstrap verb set qualifies as the "invoke a service to
      // produce a response" pattern.
      if (desc.type === 'member_call_expression') {
        const nameNode = desc.childForFieldName?.('name');
        const methodName = nameNode?.text ?? '';
        if (methodName && BOOTSTRAP_VERB_RE.test(methodName)) {
          sawBootstrapInvocation = true;
        }
      }
    }
  }

  // Bootstrap pattern qualifies when:
  //   - define('CONTROLLER_ID', …) at top level, OR
  //   - a service instantiation + a bootstrap-verb member call in
  //     top-level scope (the `new X(); ->printXxx()` shape).
  if (
    sawControllerIdDefine ||
    (sawServiceInstantiation && sawBootstrapInvocation)
  ) {
    handlerLike = true;
  }

  if (!handlerLike) return null;
  return narrowedMethod ?? '*';
}

/** Given a `$_SERVER` variable_name node, see if its enclosing
 *  expression is a `$_SERVER['REQUEST_METHOD'] === 'POST'`-shaped
 *  equality check and return the recovered method. Negated checks
 *  (`!==`) tell us what the method isn't, not what it is — skipped. */
function narrowMethodFromServerCheck(serverVar: SyntaxNode): HttpMethod | null {
  if (serverVar.text !== '$_SERVER') return null;
  const sub = serverVar.parent;
  if (!sub || sub.type !== 'subscript_expression') return null;
  const indexChild = (sub.namedChildren ?? []).find((c: SyntaxNode) => c !== serverVar);
  const indexText = readPhpString(indexChild ?? null);
  if (indexText !== 'REQUEST_METHOD') return null;
  const bin = sub.parent;
  if (!bin || bin.type !== 'binary_expression') return null;
  const op = bin.childForFieldName?.('operator')?.text;
  if (op !== '==' && op !== '===') return null;
  const left = bin.childForFieldName?.('left');
  const right = bin.childForFieldName?.('right');
  const lit = readPhpString(left) ?? readPhpString(right);
  return normalizeHttpMethod(lit);
}

/** Derive the URL path template from a file path.
 *
 *  Rules:
 *    - `foo.php` → `/foo`
 *    - `foo/index.php` → `/foo/`
 *    - `index.php` → `/`
 *
 *  Path separators are POSIX-normalised. */
export function fileToPathTemplate(filePath: string): string {
  const norm = filePath.replace(/\\/g, '/');
  // Strip a leading single-component repo prefix is NOT our job —
  // callers index files repo-relative already, but in a polyglot
  // mega-index the first segment is the child repo name. We keep it
  // because htaccess routes also live with the repo prefix.
  const stripped = norm.replace(/\.php$/i, '');
  // index → directory route.
  if (stripped.endsWith('/index')) return '/' + stripped.slice(0, -'/index'.length) + '/';
  if (stripped === 'index') return '/';
  return '/' + stripped;
}

/** Basename-preserving route template: keeps the `.php` extension so
 *  Apache mod_php-served files (Go callers reference them by the bare
 *  filename, e.g. `renderer: "/scrr.php"`) can join via URL matching.
 *
 *  Returns null for `index.php` shapes, which the stripped form
 *  ({@link fileToPathTemplate}) already covers. */
export function fileToBasenameTemplate(filePath: string): string | null {
  const norm = filePath.replace(/\\/g, '/');
  const base = norm.split('/').pop() ?? '';
  if (!base || !/\.php$/i.test(base)) return null;
  if (/^index\.php$/i.test(base)) return null;
  return '/' + base;
}

/* ─────────────────────────────────────────────────────────────────
 * Client-side: outbound HTTP calls
 * ────────────────────────────────────────────────────────────── */

/** Pull the path component out of an `http://host/path` literal,
 *  preserving the query string. Returns the input untouched when it
 *  doesn't look like an absolute URL. */
function pathFromUrl(url: string): string {
  const m = url.match(/^https?:\/\/[^/]+(\/.*)?$/i);
  if (!m) return url;
  return m[1] ?? '/';
}

/** Within a function body, walk every `curl_setopt` call to track:
 *    - the URL set via `CURLOPT_URL`
 *    - the method narrowed via `CURLOPT_CUSTOMREQUEST`
 *    - implicit GET/POST via `CURLOPT_POST=true`
 *  keyed by the handle variable (the first argument). The returned
 *  map is per-scope so multiple curl handles in the same function
 *  don't collide. */
interface CurlBinding {
  url: string | null;
  urlLine: number;
  method: HttpMethod | null;
}
function collectCurlBindings(scope: SyntaxNode): Map<string, CurlBinding> {
  const out = new Map<string, CurlBinding>();
  for (const node of walkLocal(scope)) {
    if (node.type !== 'function_call_expression') continue;
    if (calleeName(node) !== 'curl_setopt') continue;
    const args = [...callArgs(node)];
    if (args.length < 3) continue;
    const handle = args[0];
    if (handle.type !== 'variable_name') continue;
    const optionName = args[1];
    const value = args[2];
    const handleName = handle.text ?? '';
    const binding = out.get(handleName) ?? { url: null, urlLine: 0, method: null };

    // The CURLOPT_* names appear as bare `name` nodes (PHP constants).
    const optText = optionName.type === 'name' ? optionName.text : null;
    if (optText === 'CURLOPT_URL') {
      const u = readPhpString(value);
      if (u !== null) {
        binding.url = u;
        binding.urlLine = node.startPosition?.row ?? 0;
      }
    } else if (optText === 'CURLOPT_CUSTOMREQUEST') {
      const m = readPhpString(value);
      const norm = normalizeHttpMethod(m);
      if (norm) binding.method = norm;
    } else if (optText === 'CURLOPT_POST') {
      if (!binding.method) binding.method = 'POST';
    } else if (optText === 'CURLOPT_PUT') {
      if (!binding.method) binding.method = 'PUT';
    }
    out.set(handleName, binding);
  }
  return out;
}

/** Emit ClientCall entries for every cURL handle whose URL was
 *  resolved within `scope`. */
function emitCurlClientCalls(scope: SyntaxNode, filePath: string, out: ClientCall[]): void {
  const bindings = collectCurlBindings(scope);
  if (bindings.size === 0) return;
  const enc = findEnclosing(scope);
  for (const binding of bindings.values()) {
    if (!binding.url) continue;
    out.push({
      method: binding.method,
      pathLiteral: pathFromUrl(binding.url),
      providerTag: null,
      callerSymbol: enc.name,
      callerReceiver: enc.receiver,
      filePath,
      framework: FRAMEWORK_PHP_STDLIB,
      lineNumber: binding.urlLine,
      confidence: CONF_CLIENT,
    });
  }
}

/** Emit ClientCall entries for `file_get_contents` and `fopen` calls
 *  whose first argument is a literal HTTP URL. */
function emitSimpleClientCalls(rootNode: SyntaxNode, filePath: string, out: ClientCall[]): void {
  for (const node of walk(rootNode)) {
    if (node.type !== 'function_call_expression') continue;
    const name = calleeName(node);
    if (!name) continue;
    if (name !== 'file_get_contents' && name !== 'fopen') continue;
    const args = [...callArgs(node)];
    if (args.length === 0) continue;
    const url = readPhpString(args[0]);
    if (url === null) continue;
    if (!/^https?:\/\//i.test(url)) continue;
    const enc = findEnclosing(node);
    out.push({
      method: name === 'fopen' ? null : 'GET',
      pathLiteral: pathFromUrl(url),
      providerTag: null,
      callerSymbol: enc.name,
      callerReceiver: enc.receiver,
      filePath,
      framework: FRAMEWORK_PHP_STDLIB,
      lineNumber: node.startPosition?.row ?? 0,
      confidence: CONF_CLIENT,
    });
  }
}

/* ─────────────────────────────────────────────────────────────────
 * Public entry point
 * ────────────────────────────────────────────────────────────── */

/** Extract route registrations + outbound client calls from one
 *  parsed PHP file. Sync, pure with respect to the file system —
 *  htaccess scanning lives in {@link buildHtaccessIndex} which the
 *  pipeline calls once per analyze. */
export function extractPhpApiEndpoints(
  rootNode: SyntaxNode,
  filePath: string,
): ExtractedApiEndpoints {
  const routes: RouteRegistration[] = [];
  const clientCalls: ClientCall[] = [];

  // Server-side: AST file-based fallback. The htaccess phase in
  // pipeline.ts dedupes against this when the repo has htaccess.
  const method = detectFileBasedHandlerMethod(rootNode);
  if (method !== null) {
    const handlerName = filePath.split('/').pop()?.replace(/\.php$/i, '') ?? null;
    routes.push({
      method,
      pathTemplate: fileToPathTemplate(filePath),
      handlerSymbol: handlerName,
      handlerReceiver: null,
      filePath,
      framework: FRAMEWORK_PHP_FILE_BASED,
      lineNumber: 0,
      confidence: CONF_FILE_BASED,
    });
    // Apache mod_php-served files are also reachable at their bare
    // basename — `/scrr.php`, `/transfer.php`, etc. Emit a parallel
    // route so cross-repo callers that hold a literal URL like
    // `renderer: "/scrr.php"` (recovered from YAML via the URL
    // resolver) join up here. Skipped for index.php since the
    // stripped form (`/foo/`) already handles that case.
    const basenameTemplate = fileToBasenameTemplate(filePath);
    if (basenameTemplate && basenameTemplate !== fileToPathTemplate(filePath)) {
      routes.push({
        method,
        pathTemplate: basenameTemplate,
        handlerSymbol: handlerName,
        handlerReceiver: null,
        filePath,
        framework: FRAMEWORK_PHP_FILE_BASED,
        lineNumber: 0,
        confidence: CONF_FILE_BASED,
      });
    }
  }

  // Client-side: always run, regardless of htaccess presence.
  emitSimpleClientCalls(rootNode, filePath, clientCalls);
  // cURL bindings are scope-local; walk every function/method body
  // so handle variables (`$ch`) defined in different functions
  // don't smear into one another. Plus a final pass over file-level
  // scope to catch top-level cURL usage (common in plain-PHP
  // handlers that build a request then echo the response).
  const scopes: SyntaxNode[] = [rootNode];
  for (const node of walk(rootNode)) {
    if (node.type === 'function_definition' || node.type === 'method_declaration') {
      scopes.push(node);
    }
  }
  for (const scope of scopes) emitCurlClientCalls(scope, filePath, clientCalls);

  return { routes, clientCalls };
}
