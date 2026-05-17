/**
 * Generic Python API-endpoint extractor.
 *
 * Detection is by AST shape and well-known framework idioms — never
 * by repo / package name.
 *
 * Server-side forms (route registrations):
 *   1. **FastAPI / APIRouter decorators** —
 *        `@app.get("/x")`, `@router.post("/x", …)`, etc. on a
 *        function definition. Handles `APIRouter(prefix="/api/v1")`
 *        plus `app.include_router(router, prefix="/y")` for prefix
 *        composition.
 *   2. **Flask / Blueprint decorators** —
 *        `@app.route("/x", methods=["POST", "GET"])`,
 *        `@bp.route("/x")`. Blueprint prefix from
 *        `Blueprint("name", __name__, url_prefix="/v2")`.
 *      `@app.get("/x")` is also handled (Flask 2.x).
 *   3. **Django** —
 *        `urlpatterns = [path("hello/", view), re_path(r"^x$", v), …]`
 *        Method is `*` (Django dispatches by view).
 *   4. **aiohttp** —
 *        `app.router.add_get("/x", handler)`,
 *        `app.router.add_post("/x", handler)` and the
 *        `RouteTableDef` decorator form `@routes.get("/x")`.
 *   5. **Tornado** —
 *        `tornado.web.Application([(r"/x", Handler), …])` /
 *        `URLSpec(r"/x", Handler)`. Method is `*` (Tornado dispatches
 *        on `Handler.get/post/…`).
 *
 * Client-side forms (outbound HTTP):
 *   1. **requests** — `requests.get/post/put/…(url, …)`,
 *        `requests.request("METHOD", url, …)`,
 *        `requests.Session().get(...)`.
 *   2. **httpx** — sync + async (`httpx.get/post/…`,
 *        `httpx.Client().get(...)`, `httpx.AsyncClient().get(...)`).
 *   3. **aiohttp client** — `session.get/post/…(url, …)` where
 *        `session` is typed as / created from `aiohttp.ClientSession`.
 *   4. **urllib3** — `pool.request("METHOD", "/x")`.
 */

import type { SyntaxNode } from '../utils/ast-helpers.js';
import {
  type ExtractedApiEndpoints,
  type RouteRegistration,
  type ClientCall,
  type HttpMethod,
  type PendingGetterLookup,
  normalizeHttpMethod,
} from './api-endpoint-types.js';

// ─────────────────────────────────────────────────────────────────
// Constants — pure-shape recognisers
// ─────────────────────────────────────────────────────────────────

/** HTTP verbs as decorator method names (FastAPI / Flask 2.x / aiohttp / RouteTableDef). */
const VERB_DECORATOR_NAMES: ReadonlyMap<string, HttpMethod> = new Map([
  ['get', 'GET'],
  ['post', 'POST'],
  ['put', 'PUT'],
  ['delete', 'DELETE'],
  ['patch', 'PATCH'],
  ['head', 'HEAD'],
  ['options', 'OPTIONS'],
  ['trace', 'TRACE'],
]);

/** aiohttp-style `add_<verb>` registration on `app.router`. */
const AIOHTTP_ADD_METHODS: ReadonlyMap<string, HttpMethod> = new Map([
  ['add_get', 'GET'],
  ['add_post', 'POST'],
  ['add_put', 'PUT'],
  ['add_delete', 'DELETE'],
  ['add_patch', 'PATCH'],
  ['add_head', 'HEAD'],
  ['add_options', 'OPTIONS'],
  ['add_route', '*'],
]);

/** Django `urlpatterns = […]` callables that register a route by
 *  `(path, view)`. Method is always `*` since Django dispatches at
 *  the view level. */
const DJANGO_URL_FUNCTIONS: ReadonlySet<string> = new Set(['path', 're_path', 'url']);

/** Identifier names that strongly hint at "is a known HTTP-client
 *  module" so `<id>.get(...)`-shaped calls are interpreted as HTTP. */
const CLIENT_MODULE_HINTS: ReadonlySet<string> = new Set([
  'requests',
  'httpx',
  'aiohttp',
  'urllib3',
]);

/** Variables identified at parse time as instances of an HTTP client
 *  via assignments like `session = httpx.Client()`. Populated per-file. */
type ClientVarMap = Map<string, string>; // varName → framework tag

// ─────────────────────────────────────────────────────────────────
// AST helpers
// ─────────────────────────────────────────────────────────────────

/** Read the content of a Python `string` node. */
function readPyString(node: SyntaxNode | null | undefined): string | null {
  if (!node || node.type !== 'string') return null;
  const fragments: string[] = [];
  for (const child of node.namedChildren ?? []) {
    if (child.type === 'string_content') fragments.push(child.text ?? '');
  }
  if (fragments.length > 0) return fragments.join('');
  // Fallback: strip wrapping quotes / prefix.
  let raw = node.text ?? '';
  // Drop string prefix (b/r/f/rb/Rb…).
  raw = raw.replace(/^[bBrRfFuU]+/, '');
  if (raw.length >= 2) {
    const c = raw[0];
    if ((c === '"' || c === "'") && raw.endsWith(c)) return raw.slice(1, -1);
  }
  return null;
}

/** Walk every named node, iterative DFS. */
function* walk(root: SyntaxNode): Generator<SyntaxNode> {
  const stack: SyntaxNode[] = [root];
  while (stack.length > 0) {
    const node = stack.pop()!;
    yield node;
    const children = node.namedChildren ?? [];
    for (let i = children.length - 1; i >= 0; i--) stack.push(children[i]);
  }
}

/** Pull the "receiver.attribute" pair from a Python `attribute` node. */
function splitAttribute(
  node: SyntaxNode,
): { receiver: SyntaxNode | null; attribute: string | null } {
  if (node.type !== 'attribute')
    return { receiver: null, attribute: null };
  return {
    receiver: node.childForFieldName?.('object') ?? node.namedChildren?.[0] ?? null,
    attribute:
      node.childForFieldName?.('attribute')?.text ??
      node.namedChildren?.[node.namedChildren.length - 1]?.text ??
      null,
  };
}

/** Get the function-callable side of a `call`. */
function callFunction(callNode: SyntaxNode): SyntaxNode | null {
  return callNode.childForFieldName?.('function') ?? null;
}

/** Iterate positional + keyword arguments. */
function* callArguments(callNode: SyntaxNode): Generator<SyntaxNode> {
  const args = callNode.childForFieldName?.('arguments');
  if (!args) return;
  for (const child of args.namedChildren ?? []) {
    yield child;
  }
}

/** Find a keyword argument with a given identifier name. */
function getKeywordArg(
  callNode: SyntaxNode,
  name: string,
): SyntaxNode | null {
  for (const arg of callArguments(callNode)) {
    if (arg.type !== 'keyword_argument') continue;
    const ident = arg.namedChildren?.[0];
    if (ident?.type === 'identifier' && ident.text === name) {
      return arg.namedChildren?.[1] ?? null;
    }
  }
  return null;
}

/** First positional arg (skips keyword arguments). */
function firstPositional(callNode: SyntaxNode): SyntaxNode | null {
  for (const arg of callArguments(callNode)) {
    if (arg.type !== 'keyword_argument') return arg;
  }
  return null;
}

/** All positional args, skipping keyword arguments. */
function positionalArgs(callNode: SyntaxNode): SyntaxNode[] {
  const out: SyntaxNode[] = [];
  for (const arg of callArguments(callNode)) {
    if (arg.type !== 'keyword_argument') out.push(arg);
  }
  return out;
}

/** Resolve methods=["POST", "GET"]-style list of verbs into HttpMethods. */
function resolveMethodsList(node: SyntaxNode | null | undefined): HttpMethod[] {
  if (!node) return [];
  if (node.type === 'list' || node.type === 'tuple' || node.type === 'set') {
    const out: HttpMethod[] = [];
    for (const child of node.namedChildren ?? []) {
      const s = readPyString(child);
      const m = normalizeHttpMethod(s);
      if (m) out.push(m);
    }
    return out;
  }
  return [];
}

/** Normalise a path string. */
function normalizePath(raw: string | null | undefined): string | null {
  if (raw == null) return null;
  let p = raw.trim();
  if (!p) return null;
  if (!p.startsWith('/')) p = '/' + p;
  if (p.length > 1 && p.endsWith('/')) p = p.slice(0, -1);
  if (p.includes(' ') || p.includes('\n')) return null;
  return p;
}

function joinPaths(prefix: string, suffix: string): string {
  if (!prefix || prefix === '/') return suffix;
  if (!suffix || suffix === '/') return prefix;
  const a = prefix.endsWith('/') ? prefix.slice(0, -1) : prefix;
  const b = suffix.startsWith('/') ? suffix : '/' + suffix;
  return a + b;
}

/** Walk up to the enclosing `function_definition` or `class_definition`. */
function findEnclosing(node: SyntaxNode): { symbol: string | null; receiver: string | null } {
  let cur: SyntaxNode | null = node.parent;
  let symbol: string | null = null;
  while (cur) {
    if ((cur.type === 'function_definition' || cur.type === 'async_function_definition') && symbol === null) {
      symbol = cur.childForFieldName?.('name')?.text ?? null;
    } else if (cur.type === 'class_definition') {
      const owner = cur.childForFieldName?.('name')?.text ?? null;
      return { symbol, receiver: owner };
    }
    cur = cur.parent;
  }
  return { symbol, receiver: null };
}

// ─────────────────────────────────────────────────────────────────
// Pre-scan: prefixes for routers / blueprints / RouteTableDef + clients
// ─────────────────────────────────────────────────────────────────

interface FilePrefixIndex {
  /** routerVar → prefix string ('' for none). */
  routerPrefixes: Map<string, string>;
  /** Blueprints + APIRouters that were `include_router`'d at a higher prefix. */
  includePrefixes: Map<string, string>;
}

function buildPrefixIndex(rootNode: SyntaxNode): FilePrefixIndex {
  const routerPrefixes = new Map<string, string>();
  const includePrefixes = new Map<string, string>();

  for (const node of walk(rootNode)) {
    // `var = APIRouter(prefix="/api/v1")` / `Blueprint(..., url_prefix="/v2")`
    if (node.type === 'assignment') {
      const lhs = node.childForFieldName?.('left') ?? node.namedChildren?.[0];
      const rhs = node.childForFieldName?.('right') ?? node.namedChildren?.[1];
      if (lhs?.type === 'identifier' && rhs?.type === 'call') {
        const fn = callFunction(rhs);
        const fnName =
          fn?.type === 'identifier'
            ? fn.text
            : fn?.type === 'attribute'
            ? splitAttribute(fn).attribute
            : null;
        if (fnName === 'APIRouter') {
          const prefix =
            readPyString(getKeywordArg(rhs, 'prefix') ?? null) ?? '';
          const norm = normalizePath(prefix) ?? '';
          routerPrefixes.set(lhs.text, norm);
        } else if (fnName === 'Blueprint') {
          const prefix =
            readPyString(getKeywordArg(rhs, 'url_prefix') ?? null) ?? '';
          const norm = normalizePath(prefix) ?? '';
          routerPrefixes.set(lhs.text, norm);
        } else if (fnName === 'RouteTableDef') {
          // RouteTableDef has no prefix; entry exists so the
          // decorator path is recognised below.
          routerPrefixes.set(lhs.text, '');
        }
      }
    }
    // `app.include_router(router, prefix="/y")`
    if (node.type === 'call') {
      const fn = callFunction(node);
      if (!fn || fn.type !== 'attribute') continue;
      const { attribute } = splitAttribute(fn);
      if (attribute !== 'include_router') continue;
      const args = positionalArgs(node);
      const routerArg = args[0];
      if (routerArg?.type !== 'identifier') continue;
      const prefix =
        readPyString(getKeywordArg(node, 'prefix') ?? null) ??
        readPyString(args[1] ?? null) ??
        '';
      const norm = normalizePath(prefix) ?? '';
      const existing = includePrefixes.get(routerArg.text) ?? '';
      includePrefixes.set(routerArg.text, joinPaths(existing, norm));
    }
  }
  return { routerPrefixes, includePrefixes };
}

function effectivePrefixFor(routerVar: string, idx: FilePrefixIndex): string {
  const own = idx.routerPrefixes.get(routerVar) ?? '';
  const include = idx.includePrefixes.get(routerVar) ?? '';
  return joinPaths(include, own);
}

// ─────────────────────────────────────────────────────────────────
// Server-side recognisers
// ─────────────────────────────────────────────────────────────────

/** A `decorated_definition` with `@router.<verb>("/x", …)`. */
function tryFastApiFlaskDecorator(
  decoratedDef: SyntaxNode,
  idx: FilePrefixIndex,
  filePath: string,
): RouteRegistration[] {
  if (decoratedDef.type !== 'decorated_definition') return [];
  const fn = decoratedDef.namedChildren?.find(
    (c) => c.type === 'function_definition' || c.type === 'async_function_definition',
  );
  const handlerSymbol = fn?.childForFieldName?.('name')?.text ?? null;
  const out: RouteRegistration[] = [];

  for (const decorator of decoratedDef.namedChildren ?? []) {
    if (decorator.type !== 'decorator') continue;
    const expr = decorator.namedChildren?.[0];
    if (!expr || expr.type !== 'call') continue;
    const fnNode = callFunction(expr);
    if (!fnNode || fnNode.type !== 'attribute') continue;
    const { receiver, attribute } = splitAttribute(fnNode);
    if (!attribute) continue;
    const receiverVar = receiver?.type === 'identifier' ? receiver.text : null;
    if (!receiverVar) continue;
    const lineNumber = decorator.startPosition?.row ?? 0;

    // FastAPI / Flask 2.x verb shortcut.
    const verb = VERB_DECORATOR_NAMES.get(attribute);
    if (verb) {
      const path = readPyString(firstPositional(expr));
      const norm = normalizePath(path);
      if (norm === null) continue;
      const prefix = effectivePrefixFor(receiverVar, idx);
      const full = normalizePath(joinPaths(prefix, norm) || '/');
      if (full === null) continue;
      out.push({
        method: verb,
        pathTemplate: full,
        handlerSymbol,
        handlerReceiver: null,
        filePath,
        framework: idx.routerPrefixes.has(receiverVar) ? 'python.router' : 'python.app',
        lineNumber,
        confidence: handlerSymbol ? 0.95 : 0.7,
      });
      continue;
    }

    // Flask `@app.route("/x", methods=[…])`.
    if (attribute === 'route') {
      const path = readPyString(firstPositional(expr));
      const norm = normalizePath(path);
      if (norm === null) continue;
      const methods = resolveMethodsList(getKeywordArg(expr, 'methods'));
      const verbs: HttpMethod[] = methods.length > 0 ? methods : ['GET']; // Flask default
      const prefix = effectivePrefixFor(receiverVar, idx);
      const full = normalizePath(joinPaths(prefix, norm) || '/');
      if (full === null) continue;
      for (const m of verbs) {
        out.push({
          method: m,
          pathTemplate: full,
          handlerSymbol,
          handlerReceiver: null,
          filePath,
          framework: 'flask',
          lineNumber,
          confidence: handlerSymbol ? 0.95 : 0.7,
        });
      }
    }
  }
  return out;
}

/** `app.router.add_get("/x", handler)` and friends. */
function tryAiohttpAdd(
  callNode: SyntaxNode,
  filePath: string,
): RouteRegistration[] {
  if (callNode.type !== 'call') return [];
  const fn = callFunction(callNode);
  if (!fn || fn.type !== 'attribute') return [];
  const { attribute } = splitAttribute(fn);
  if (!attribute) return [];
  const verb = AIOHTTP_ADD_METHODS.get(attribute);
  if (!verb) return [];
  const args = positionalArgs(callNode);
  let pathArg: SyntaxNode | null = null;
  let handlerArg: SyntaxNode | null = null;
  let method: HttpMethod = verb;
  if (attribute === 'add_route') {
    if (args.length < 3) return [];
    const m = normalizeHttpMethod(readPyString(args[0]));
    if (!m) return [];
    method = m;
    pathArg = args[1];
    handlerArg = args[2];
  } else {
    if (args.length < 1) return [];
    pathArg = args[0];
    handlerArg = args[1] ?? null;
  }
  const path = normalizePath(readPyString(pathArg));
  if (path === null) return [];
  const handlerSymbol = handlerArg?.type === 'identifier' ? handlerArg.text : null;
  return [
    {
      method,
      pathTemplate: path,
      handlerSymbol,
      handlerReceiver: null,
      filePath,
      framework: 'aiohttp',
      lineNumber: callNode.startPosition?.row ?? 0,
      confidence: handlerSymbol ? 0.95 : 0.7,
    },
  ];
}

/** `path("hello/", view)` / `re_path(r"^x$", view)` inside `urlpatterns`.
 *  Walks the parent chain to confirm the call is part of an
 *  assignment to `urlpatterns` — keeps us from misclassifying any
 *  `path("x", y)` helper. */
function tryDjangoUrl(
  callNode: SyntaxNode,
  filePath: string,
): RouteRegistration[] {
  if (callNode.type !== 'call') return [];
  const fn = callFunction(callNode);
  const fnName = fn?.type === 'identifier' ? fn.text : null;
  if (!fnName || !DJANGO_URL_FUNCTIONS.has(fnName)) return [];
  // Ascend to find an enclosing `urlpatterns = […]` assignment.
  let cur: SyntaxNode | null = callNode.parent;
  let inUrlPatterns = false;
  while (cur) {
    if (cur.type === 'assignment') {
      const lhs = cur.childForFieldName?.('left') ?? cur.namedChildren?.[0];
      if (lhs?.type === 'identifier' && lhs.text === 'urlpatterns') {
        inUrlPatterns = true;
      }
      break;
    }
    cur = cur.parent;
  }
  if (!inUrlPatterns) return [];
  const args = positionalArgs(callNode);
  if (args.length < 2) return [];
  const path = normalizePath(readPyString(args[0]));
  if (path === null) return [];
  const handlerArg = args[1];
  const handlerSymbol = handlerArg?.type === 'identifier' ? handlerArg.text : null;
  return [
    {
      method: '*',
      pathTemplate: path,
      handlerSymbol,
      handlerReceiver: null,
      filePath,
      framework: 'django',
      lineNumber: callNode.startPosition?.row ?? 0,
      confidence: handlerSymbol ? 0.95 : 0.7,
    },
  ];
}

/** Tornado `URLSpec(r"/x", Handler)` / `Application([(r"/x", Handler)])`.
 *  We pick up both forms by anchoring on the literal-tuple shape. */
function tryTornado(
  containerNode: SyntaxNode,
  filePath: string,
): RouteRegistration[] {
  // Tornado forms:
  //   tornado.web.Application([(r"/x", Handler), …])
  //   URLSpec(r"/x", Handler)
  const out: RouteRegistration[] = [];
  if (containerNode.type === 'call') {
    const fn = callFunction(containerNode);
    const tail =
      fn?.type === 'identifier'
        ? fn.text
        : fn?.type === 'attribute'
        ? splitAttribute(fn).attribute
        : null;
    if (tail === 'URLSpec' || tail === 'url') {
      const args = positionalArgs(containerNode);
      if (args.length < 2) return out;
      const path = normalizePath(readPyString(args[0]));
      if (path === null) return out;
      const handlerArg = args[1];
      const handlerSymbol =
        handlerArg?.type === 'identifier' ? handlerArg.text : null;
      out.push({
        method: '*',
        pathTemplate: path,
        handlerSymbol,
        handlerReceiver: null,
        filePath,
        framework: 'tornado',
        lineNumber: containerNode.startPosition?.row ?? 0,
        confidence: handlerSymbol ? 0.9 : 0.6,
      });
    }
    if (tail === 'Application') {
      // First positional arg is a list of tuples.
      const args = positionalArgs(containerNode);
      const list = args[0];
      if (!list) return out;
      if (list.type === 'list' || list.type === 'tuple') {
        for (const el of list.namedChildren ?? []) {
          if (el.type !== 'tuple' && el.type !== 'list') continue;
          const elArgs = el.namedChildren ?? [];
          if (elArgs.length < 2) continue;
          const path = normalizePath(readPyString(elArgs[0]));
          if (path === null) continue;
          const handlerArg = elArgs[1];
          const handlerSymbol =
            handlerArg?.type === 'identifier' ? handlerArg.text : null;
          out.push({
            method: '*',
            pathTemplate: path,
            handlerSymbol,
            handlerReceiver: null,
            filePath,
            framework: 'tornado',
            lineNumber: el.startPosition?.row ?? 0,
            confidence: handlerSymbol ? 0.9 : 0.6,
          });
        }
      }
    }
  }
  return out;
}

// ─────────────────────────────────────────────────────────────────
// Client-side recognisers
// ─────────────────────────────────────────────────────────────────

/** Pre-scan: variables assigned to `requests.Session()`,
 *  `httpx.Client()`, `httpx.AsyncClient()`, `aiohttp.ClientSession()`. */
function buildClientVars(rootNode: SyntaxNode): ClientVarMap {
  const out: ClientVarMap = new Map();
  for (const node of walk(rootNode)) {
    if (node.type !== 'assignment') continue;
    const lhs = node.childForFieldName?.('left') ?? node.namedChildren?.[0];
    const rhs = node.childForFieldName?.('right') ?? node.namedChildren?.[1];
    if (lhs?.type !== 'identifier') continue;
    if (rhs?.type !== 'call') continue;
    const fn = callFunction(rhs);
    let module: string | null = null;
    let name: string | null = null;
    if (fn?.type === 'attribute') {
      const split = splitAttribute(fn);
      module = split.receiver?.type === 'identifier' ? split.receiver.text : null;
      name = split.attribute ?? null;
    } else if (fn?.type === 'identifier') {
      name = fn.text;
    }
    if (!name) continue;
    if (
      (module === 'requests' && name === 'Session') ||
      (module === 'httpx' && (name === 'Client' || name === 'AsyncClient')) ||
      (module === 'aiohttp' && name === 'ClientSession')
    ) {
      out.set(lhs.text, module === 'aiohttp' ? 'aiohttp' : module ?? 'httpx');
    }
  }
  return out;
}

/** Walk a Python URL-argument expression and harvest getter chains
 *  and config-name dereferences as {@link PendingGetterLookup}s.
 *
 *  Captures:
 *    - `call`: function/method names (`get_kosmos()`, `cfg.get_kosmos()`)
 *    - `attribute`: typed-LHS dereferences (`settings.KOSMOS_URL`)
 *    - bare `ALL_CAPS` identifiers (`KOSMOS_URL`)
 *
 *  Phase 3.4 resolves each entry against the config-tag-resolver
 *  fold to recover a provider tag — entries that don't resolve to
 *  a known tag are silently dropped, so over-emission is safe. */
function collectPythonGetterLookups(
  node: SyntaxNode | null | undefined,
): PendingGetterLookup[] {
  if (!node) return [];
  const out: PendingGetterLookup[] = [];
  const seen = new Set<string>();
  const push = (recv: string | null, name: string) => {
    const key = `${recv ?? '*'}::${name}`;
    if (seen.has(key)) return;
    seen.add(key);
    out.push({ receiver: recv, name });
  };
  const stack: SyntaxNode[] = [node];
  while (stack.length > 0) {
    const cur = stack.pop()!;
    if (cur.type === 'call') {
      const fn = cur.childForFieldName?.('function');
      if (fn?.type === 'identifier' && fn.text) {
        push(null, fn.text);
      } else if (fn?.type === 'attribute') {
        const obj = fn.childForFieldName?.('object');
        const attr = fn.childForFieldName?.('attribute');
        if (attr?.text) {
          const recv = obj?.type === 'identifier' ? obj.text : null;
          push(recv, attr.text);
        }
      }
    } else if (cur.type === 'attribute') {
      const obj = cur.childForFieldName?.('object');
      const attr = cur.childForFieldName?.('attribute');
      if (attr?.text) {
        const recv = obj?.type === 'identifier' ? obj.text : null;
        push(recv, attr.text);
      }
    } else if (cur.type === 'identifier') {
      // Only consider ALL_CAPS identifiers — almost always a
      // module-level constant. Lower-case names alias every
      // function-local variable in the file and would explode the
      // candidate list.
      if (cur.text && /^[A-Z][A-Z0-9_]*$/.test(cur.text)) {
        push(null, cur.text);
      }
    }
    for (const c of cur.namedChildren ?? []) stack.push(c);
  }
  return out;
}

function tryHttpClientCall(
  callNode: SyntaxNode,
  clientVars: ClientVarMap,
  filePath: string,
): ClientCall[] {
  if (callNode.type !== 'call') return [];
  const fn = callFunction(callNode);
  if (!fn || fn.type !== 'attribute') return [];
  const { receiver, attribute } = splitAttribute(fn);
  if (!attribute) return [];

  // 1. Module-level: `requests.get(...)`, `httpx.post(...)`.
  const verb = VERB_DECORATOR_NAMES.get(attribute);
  if (verb && receiver?.type === 'identifier' && CLIENT_MODULE_HINTS.has(receiver.text)) {
    const urlArg = firstPositional(callNode);
    const url = readPyString(urlArg);
    const pendingLookups = url === null ? collectPythonGetterLookups(urlArg) : [];
    if (url === null && pendingLookups.length === 0) return [];
    const enclosing = findEnclosing(callNode);
    return [
      {
        method: verb,
        pathLiteral: url,
        providerTag: null,
        callerSymbol: enclosing.symbol,
        callerReceiver: enclosing.receiver,
        filePath,
        framework: receiver.text,
        lineNumber: callNode.startPosition?.row ?? 0,
        confidence: url ? 0.95 : 0.55,
        pendingGetterLookups: pendingLookups.length > 0 ? pendingLookups : undefined,
      },
    ];
  }

  // 2. `<sessionVar>.get(...)` where sessionVar was created above.
  if (verb && receiver?.type === 'identifier' && clientVars.has(receiver.text)) {
    const urlArg = firstPositional(callNode);
    const url = readPyString(urlArg);
    const pendingLookups = url === null ? collectPythonGetterLookups(urlArg) : [];
    if (url === null && pendingLookups.length === 0) return [];
    const enclosing = findEnclosing(callNode);
    return [
      {
        method: verb,
        pathLiteral: url,
        providerTag: null,
        callerSymbol: enclosing.symbol,
        callerReceiver: enclosing.receiver,
        filePath,
        framework: clientVars.get(receiver.text) ?? 'httpx',
        lineNumber: callNode.startPosition?.row ?? 0,
        confidence: url ? 0.9 : 0.55,
        pendingGetterLookups: pendingLookups.length > 0 ? pendingLookups : undefined,
      },
    ];
  }

  // 3. Inline-instantiated: `httpx.AsyncClient().get(...)` — receiver
  //    is a `call` whose function is `<module>.<ClientCtor>`.
  if (verb && receiver?.type === 'call') {
    const innerFn = callFunction(receiver);
    if (innerFn?.type === 'attribute') {
      const innerSplit = splitAttribute(innerFn);
      const innerModule =
        innerSplit.receiver?.type === 'identifier' ? innerSplit.receiver.text : null;
      const innerName = innerSplit.attribute;
      const isClientCtor =
        (innerModule === 'requests' && innerName === 'Session') ||
        (innerModule === 'httpx' && (innerName === 'Client' || innerName === 'AsyncClient')) ||
        (innerModule === 'aiohttp' && innerName === 'ClientSession');
      if (isClientCtor) {
        const urlArg = firstPositional(callNode);
        const url = readPyString(urlArg);
        const pendingLookups = url === null ? collectPythonGetterLookups(urlArg) : [];
        if (url === null && pendingLookups.length === 0) return [];
        const enclosing = findEnclosing(callNode);
        return [
          {
            method: verb,
            pathLiteral: url,
            providerTag: null,
            callerSymbol: enclosing.symbol,
            callerReceiver: enclosing.receiver,
            filePath,
            framework: innerModule ?? 'httpx',
            lineNumber: callNode.startPosition?.row ?? 0,
            confidence: url ? 0.9 : 0.55,
            pendingGetterLookups: pendingLookups.length > 0 ? pendingLookups : undefined,
          },
        ];
      }
    }
  }

  // 4. Generic `request("METHOD", "/x")` form (requests/httpx/urllib3).
  if (
    attribute === 'request' &&
    receiver?.type === 'identifier' &&
    (CLIENT_MODULE_HINTS.has(receiver.text) || clientVars.has(receiver.text))
  ) {
    const args = positionalArgs(callNode);
    if (args.length < 2) return [];
    const method = normalizeHttpMethod(readPyString(args[0]));
    const url = readPyString(args[1]);
    const pendingLookups = url === null ? collectPythonGetterLookups(args[1]) : [];
    if (!method) return [];
    if (url === null && pendingLookups.length === 0) return [];
    const enclosing = findEnclosing(callNode);
    return [
      {
        method,
        pathLiteral: url,
        providerTag: null,
        callerSymbol: enclosing.symbol,
        callerReceiver: enclosing.receiver,
        filePath,
        framework:
          clientVars.get(receiver.text) ?? receiver.text,
        lineNumber: callNode.startPosition?.row ?? 0,
        confidence: url ? 0.9 : 0.55,
        pendingGetterLookups: pendingLookups.length > 0 ? pendingLookups : undefined,
      },
    ];
  }

  return [];
}

// ─────────────────────────────────────────────────────────────────
// Entry point
// ─────────────────────────────────────────────────────────────────

export function extractPythonApiEndpoints(
  rootNode: SyntaxNode,
  filePath: string,
): ExtractedApiEndpoints {
  const out: ExtractedApiEndpoints = { routes: [], clientCalls: [] };
  const idx = buildPrefixIndex(rootNode);
  const clientVars = buildClientVars(rootNode);

  for (const node of walk(rootNode)) {
    // Server-side
    if (node.type === 'decorated_definition') {
      out.routes.push(...tryFastApiFlaskDecorator(node, idx, filePath));
    } else if (node.type === 'call') {
      out.routes.push(...tryAiohttpAdd(node, filePath));
      out.routes.push(...tryDjangoUrl(node, filePath));
      out.routes.push(...tryTornado(node, filePath));
      out.clientCalls.push(...tryHttpClientCall(node, clientVars, filePath));
    }
  }

  return out;
}
