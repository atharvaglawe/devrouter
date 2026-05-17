/**
 * Generic Go API-endpoint extractor.
 *
 * Detection is by AST shape, not symbol name — so any internal HTTP
 * router or option-struct wrapper that follows a recognised Go idiom
 * is captured without hardcoding its package or type names.
 *
 * Server-side forms (route registrations):
 *   1. **stdlib Handle*** — `<recv>.HandleFunc(path, h)`,
 *      `<recv>.Handle(path, h)`. Covers `net/http`, `*http.ServeMux`
 *      and any custom mux exposing the same surface.
 *   2. **Verb-as-method** — `<recv>.<HTTP-VERB>(path, ...handlers)`.
 *      Covers gin, echo, chi, fiber, gorilla/mux helper variants and
 *      any internal router that follows the convention.
 *   3. **Tagged register** — `<recv>.<RegName>(method, path[s], handler)`
 *      where `RegName ∈ {Handle, AddRoute, Register, Route, Method,
 *      Any}`. Covers chi `r.Method`, gorilla `r.HandleFunc(...).Methods(...)`,
 *      and arbitrary internal routers in this shape.
 *
 * Client-side forms (outbound HTTP / RPC):
 *   1. **stdlib verbs** — `http.Get/Post/PostForm/Head(url, …)`.
 *   2. **Verb-as-method on a client** — `<recv>.Get/Post/Do(url, …)`
 *      where the receiver name strongly hints at HTTP-client usage.
 *   3. **Request builder** — `http.NewRequest(method, url, …)` whose
 *      method+URL pair is captured at the builder site (subsequent
 *      `client.Do(req)` is found via the shared variable name).
 *   4. **Options-bag literal** — `<Type>{Url|URL|Endpoint|Path: <str>,
 *      Method: <expr>, …}`. Catches any internal HTTP wrapper that
 *      uses the options-struct convention.
 *   5. **gRPC stub** — `pb.New<X>Client(conn).<Method>(ctx, req)`.
 *
 * HTTP method resolution accepts any of: a literal `"POST"` string,
 * a `http.MethodPost` selector, or a bare `MethodPost` identifier —
 * so callers can stamp `MethodPost` constants without us caring how
 * they spell it.
 */

import type { SyntaxNode } from '../utils/ast-helpers.js';
import {
  type ExtractedApiEndpoints,
  type RouteRegistration,
  type ClientCall,
  type HttpMethod,
  type PendingGetterLookup,
  normalizeHttpMethod,
  HTTP_CLIENT_RECEIVER_HINTS,
} from './api-endpoint-types.js';

// ─────────────────────────────────────────────────────────────────
// Constants — pure-shape recognisers, no repo names
// ─────────────────────────────────────────────────────────────────

/** Go method names that act like HTTP verbs in routers. We accept
 *  upper-case (idiomatic gin / echo / chi) and the stdlib `Any`
 *  / `All` aliases. Lower-case verbs would clash with HTTP-client
 *  call sites in Go (`client.get(...)`) — those are caught by the
 *  client-side extractor instead. */
const VERB_METHOD_NAMES: ReadonlySet<string> = new Set([
  'GET',
  'POST',
  'PUT',
  'DELETE',
  'PATCH',
  'HEAD',
  'OPTIONS',
  'TRACE',
  'CONNECT',
  'ANY',
  'ALL',
]);

/** Stdlib-shape register methods that take `(path, handler)`. */
const STDLIB_REGISTER_METHODS: ReadonlySet<string> = new Set(['HandleFunc', 'Handle']);

/** Tagged-register methods that take `(method, path[s], handler)`.
 *  These are *names commonly used* by Go router APIs in this shape,
 *  not specific frameworks: `Handle` clashes with the stdlib form
 *  above and is disambiguated by arity at extraction time. */
const TAGGED_REGISTER_METHODS: ReadonlySet<string> = new Set([
  'AddRoute',
  'Register',
  'Route',
  'Method',
  'Match',
  'Add',
]);

/** Field names that hint at "this struct is HTTP request options".
 *  Both full-URL fields (`Url`, `Endpoint`) and path-only fields
 *  (`Path`, `Route`) — many Go HTTP wrappers split URL into
 *  `{HostName, Path}` pairs in a config struct, so a literal
 *  `Path:` value carries the routable component. */
const OPTIONS_URL_FIELDS: ReadonlySet<string> = new Set([
  'Url',
  'URL',
  'Endpoint',
  'Path',
  'Route',
  'Address',
]);

/** Field names that, when present alongside a path/URL field,
 *  *strengthen* the case that a struct literal is HTTP-shaped —
 *  e.g. an `ApiConfig{HostName, Path}` is unambiguously a URL,
 *  whereas a `Foo{Path}` standing alone is ambiguous. */
const OPTIONS_HOST_FIELDS: ReadonlySet<string> = new Set([
  'Host',
  'HostName',
  'BaseURL',
  'BaseUrl',
  'Scheme',
  'Protocol',
  'Server',
]);

/** Names of router-group factory methods whose first string arg is
 *  the prefix for all routes registered on the returned receiver.
 *  Generic across Go web frameworks (`echo.Group`, `gin.Group`,
 *  `chi.Route`, custom `NewGroup`/`Subrouter`/`WithPrefix`). */
const GROUP_FACTORY_METHODS: ReadonlySet<string> = new Set([
  'Group',
  'NewGroup',
  'Route',
  'Subrouter',
  'WithPrefix',
  'PathPrefix',
]);
const OPTIONS_METHOD_FIELDS: ReadonlySet<string> = new Set(['Method', 'HttpMethod', 'HTTPMethod']);
const OPTIONS_PROVIDER_FIELDS: ReadonlySet<string> = new Set([
  'ProviderShortName',
  'Provider',
  'Tag',
  'Name',
  'ServiceName',
]);

/** Stdlib HTTP verb functions — `http.Get`, `http.Post`, etc. */
const STDLIB_VERB_FUNCTIONS: ReadonlySet<string> = new Set([
  'Get',
  'Post',
  'Put',
  'Delete',
  'Patch',
  'Head',
  'PostForm',
]);

/** Receiver names whose verb-as-method calls (`x.Get(url)`,
 *  `x.Post(url, body)`) are almost certainly HTTP-client calls
 *  rather than route registrations. Adds a Go-specific superset
 *  on top of {@link HTTP_CLIENT_RECEIVER_HINTS}. */
const GO_CLIENT_RECEIVER_HINTS: ReadonlySet<string> = new Set([
  ...HTTP_CLIENT_RECEIVER_HINTS,
  'cli',
  'restclient',
  'apiclient',
  'httpapi',
  'transport',
  'roundtripper',
]);

// ─────────────────────────────────────────────────────────────────
// Small AST helpers
// ─────────────────────────────────────────────────────────────────

/** Read the content of a Go `interpreted_string_literal` /
 *  `raw_string_literal`. Strips surrounding quotes / backticks. */
function readGoString(node: SyntaxNode | null | undefined): string | null {
  if (!node) return null;
  if (node.type === 'interpreted_string_literal' || node.type === 'raw_string_literal') {
    const raw = node.text;
    if (!raw) return null;
    // Strip first + last char (the quotes / backticks).
    return raw.length >= 2 ? raw.slice(1, -1) : raw;
  }
  return null;
}

/** True for nodes that are positional argument elements inside a
 *  Go `argument_list`. Filters out commas / parens. */
function isArgumentNode(node: SyntaxNode): boolean {
  return node.isNamed === true;
}

/** Iterate the named arguments of a `call_expression`. */
function* callArguments(callNode: SyntaxNode): Generator<SyntaxNode> {
  const args = callNode.childForFieldName?.('arguments');
  if (!args) return;
  for (const child of args.children ?? []) {
    if (isArgumentNode(child)) yield child;
  }
}

/** Pull the receiver + field of `(selector_expression)`. */
function splitSelector(node: SyntaxNode): { receiver: SyntaxNode | null; field: string | null } {
  if (node.type !== 'selector_expression') return { receiver: null, field: null };
  return {
    receiver: node.childForFieldName?.('operand') ?? null,
    field: node.childForFieldName?.('field')?.text ?? null,
  };
}

/** Extract a bare name from things that *look* like an identifier
 *  reference: `foo`, `pkg.Foo`, `pkg.sub.Foo`. Returns just the tail
 *  (`Foo`) when applicable, or `null` for closures / call results. */
function extractCallableName(node: SyntaxNode | null): string | null {
  if (!node) return null;
  switch (node.type) {
    case 'identifier':
      return node.text || null;
    case 'selector_expression': {
      const { field } = splitSelector(node);
      return field;
    }
    default:
      return null;
  }
}

/** Same as above but returns receiver + tail when the source is a
 *  qualified reference (`controllers.GetX` → `{receiver:"controllers",
 *  symbol:"GetX"}`). For bare identifiers `receiver` is `null`. */
function extractCallableReference(node: SyntaxNode | null): {
  receiver: string | null;
  symbol: string | null;
} {
  if (!node) return { receiver: null, symbol: null };
  if (node.type === 'identifier') return { receiver: null, symbol: node.text || null };
  if (node.type === 'selector_expression') {
    const { receiver, field } = splitSelector(node);
    return {
      receiver: receiver?.text ?? null,
      symbol: field,
    };
  }
  return { receiver: null, symbol: null };
}

/** Resolve any of: literal `"POST"`, `http.MethodPost`, bare
 *  `MethodPost`. Returns the normalised HTTP method or `null`. */
function resolveHttpMethod(node: SyntaxNode | null): HttpMethod | null {
  if (!node) return null;
  if (node.type === 'interpreted_string_literal' || node.type === 'raw_string_literal') {
    return normalizeHttpMethod(readGoString(node));
  }
  if (node.type === 'identifier') {
    return normalizeHttpMethod(node.text);
  }
  if (node.type === 'selector_expression') {
    const field = node.childForFieldName?.('field')?.text;
    return normalizeHttpMethod(field);
  }
  return null;
}

/** A path arg can be either a single string literal or a slice
 *  literal of strings. Returns all recovered path strings. Order
 *  is preserved so callers can emit one route per element. */
function resolvePathArg(node: SyntaxNode | null): string[] {
  if (!node) return [];
  const direct = readGoString(node);
  if (direct !== null) return [direct];
  // []string{"/a", "/b"} — composite_literal of slice_type
  if (node.type === 'composite_literal') {
    const body = node.childForFieldName?.('body');
    if (!body) return [];
    const out: string[] = [];
    for (const child of body.children ?? []) {
      // body holds literal_element nodes (named) which wrap strings
      const inner = child.type === 'literal_element' ? child.children?.[0] : child;
      const s = readGoString(inner ?? null);
      if (s !== null) out.push(s);
    }
    return out;
  }
  return [];
}

/** Map of local variable name → URL prefix, recovered from
 *  `<var> := <recv>.Group("/prefix", …)` style assignments at any
 *  scope. The same map handles transitive grouping
 *  (`v2 := api.Group("/v2")` where `api` is itself a group). */
type GroupPrefixMap = Map<string, string>;

/** Drill into the value side of a short-var-decl / assignment to
 *  see whether it's a router-group factory call, and if so, what
 *  prefix it carries. Returns null when the expression is not a
 *  group-factory call we recognise. */
function asGroupFactoryCall(node: SyntaxNode | null): {
  parentVar: string | null;
  prefix: string;
} | null {
  if (!node || node.type !== 'call_expression') return null;
  const fn = node.childForFieldName?.('function');
  if (!fn || fn.type !== 'selector_expression') return null;
  const { receiver, field } = splitSelector(fn);
  if (!field || !GROUP_FACTORY_METHODS.has(field)) return null;
  const args = [...callArguments(node)];
  if (args.length === 0) return null;
  const prefix = readGoString(args[0]);
  if (prefix === null) return null;
  // The parent group is the receiver of the call, when it's a bare
  // identifier (so chains like `api.Group("/v1").Group("/v2")` are
  // resolved by recursive lookup at the call site).
  const parentVar = receiver?.type === 'identifier' ? receiver.text : null;
  return { parentVar, prefix };
}

/** Pre-scan a function body (or the whole file) to collect every
 *  router-group variable and resolve its full prefix transitively. */
function collectGroupPrefixes(rootNode: SyntaxNode): GroupPrefixMap {
  const direct: Map<string, { parent: string | null; prefix: string }> = new Map();

  // Walk for both `:=` (short_var_declaration) and `=`
  // (assignment_statement) plus `var = …` (var_spec). Each captures
  // a (lhs-name, rhs-call) pair.
  const stack: SyntaxNode[] = [rootNode];
  while (stack.length > 0) {
    const node = stack.pop()!;

    let lhsNode: SyntaxNode | null = null;
    let rhsNode: SyntaxNode | null = null;
    if (node.type === 'short_var_declaration') {
      lhsNode = node.childForFieldName?.('left') ?? null;
      rhsNode = node.childForFieldName?.('right') ?? null;
    } else if (node.type === 'assignment_statement') {
      lhsNode = node.childForFieldName?.('left') ?? null;
      rhsNode = node.childForFieldName?.('right') ?? null;
    } else if (node.type === 'var_spec') {
      // var x = …
      const name = node.childForFieldName?.('name');
      const value = node.childForFieldName?.('value');
      if (name?.type === 'identifier') lhsNode = name;
      if (value) rhsNode = value;
    }

    if (lhsNode && rhsNode) {
      // Both sides may be expression_lists for tuple assignments;
      // pair them positionally.
      const lhsList =
        lhsNode.type === 'expression_list' || lhsNode.type === 'identifier_list'
          ? (lhsNode.children ?? []).filter((c) => c.type === 'identifier')
          : lhsNode.type === 'identifier'
          ? [lhsNode]
          : [];
      const rhsList =
        rhsNode.type === 'expression_list'
          ? (rhsNode.children ?? []).filter((c) => c.isNamed === true)
          : [rhsNode];
      for (let i = 0; i < lhsList.length && i < rhsList.length; i++) {
        const factory = asGroupFactoryCall(rhsList[i]);
        if (!factory) continue;
        const varName = lhsList[i].text;
        if (!varName) continue;
        direct.set(varName, { parent: factory.parentVar, prefix: factory.prefix });
      }
    }

    const children = node.children ?? [];
    for (let i = children.length - 1; i >= 0; i--) {
      stack.push(children[i]);
    }
  }

  // Resolve transitive parents into full prefixes. Iterative until
  // a fixed point; simple cycle guard.
  const out: GroupPrefixMap = new Map();
  for (const [name] of direct) {
    const visited = new Set<string>();
    const parts: string[] = [];
    let cur: string | null = name;
    while (cur && direct.has(cur) && !visited.has(cur)) {
      visited.add(cur);
      const entry = direct.get(cur)!;
      parts.unshift(entry.prefix);
      cur = entry.parent;
    }
    out.set(name, joinPrefixSegments(parts));
  }
  return out;
}

function joinPrefixSegments(parts: string[]): string {
  let acc = '';
  for (const p of parts) {
    if (!p) continue;
    const norm = p.startsWith('/') ? p : '/' + p;
    acc = acc + (acc.endsWith('/') ? norm.slice(1) : norm);
  }
  if (acc.length > 1 && acc.endsWith('/')) acc = acc.slice(0, -1);
  return acc;
}

/** Apply a recovered group prefix (when the receiver of a route
 *  registration call is a known group variable) to a child path. */
function applyGroupPrefix(
  callNode: SyntaxNode,
  pathTemplate: string,
  groups: GroupPrefixMap,
): string {
  if (groups.size === 0) return pathTemplate;
  const fn = callNode.childForFieldName?.('function');
  if (!fn || fn.type !== 'selector_expression') return pathTemplate;
  const recv = fn.childForFieldName?.('operand');
  if (!recv || recv.type !== 'identifier') return pathTemplate;
  const prefix = groups.get(recv.text);
  if (!prefix) return pathTemplate;
  if (pathTemplate === '/') return prefix || '/';
  return joinPrefixSegments([prefix, pathTemplate]);
}

/** Walk up the AST until we find a func / method declaration and
 *  return its bare name and (for methods) receiver type name.
 *  Used for caller attribution on client calls. */
function findEnclosingFuncInfo(node: SyntaxNode): {
  symbol: string | null;
  receiver: string | null;
} {
  let cur: SyntaxNode | null = node.parent;
  while (cur) {
    if (cur.type === 'function_declaration') {
      return { symbol: cur.childForFieldName?.('name')?.text ?? null, receiver: null };
    }
    if (cur.type === 'method_declaration') {
      const name = cur.childForFieldName?.('name')?.text ?? null;
      // receiver: (parameter_list (parameter_declaration type: …))
      const recvList = cur.childForFieldName?.('receiver');
      let receiver: string | null = null;
      if (recvList) {
        // Drill into the first parameter_declaration's type.
        for (const child of recvList.children ?? []) {
          if (child.type !== 'parameter_declaration') continue;
          const t = child.childForFieldName?.('type');
          if (!t) continue;
          if (t.type === 'pointer_type') {
            const inner = t.children?.find((c) => c.type === 'type_identifier');
            receiver = inner?.text ?? null;
          } else if (t.type === 'type_identifier') {
            receiver = t.text;
          }
          break;
        }
      }
      return { symbol: name, receiver };
    }
    cur = cur.parent;
  }
  return { symbol: null, receiver: null };
}

/** Normalise a recovered path string. Drops trailing slashes for
 *  non-root paths; ensures leading slash. Empty / non-routey
 *  strings return `null` so callers skip them. */
function normalizePath(raw: string | null | undefined): string | null {
  if (raw == null) return null;
  let p = raw.trim();
  if (!p) return null;
  // Strip wrapping single quotes (some grammars expose them).
  if ((p.startsWith("'") && p.endsWith("'")) || (p.startsWith('"') && p.endsWith('"'))) {
    p = p.slice(1, -1);
  }
  if (!p.startsWith('/')) p = '/' + p;
  if (p.length > 1 && p.endsWith('/')) p = p.slice(0, -1);
  // Reject obvious non-routes (URLs are handled separately).
  if (p.includes(' ') || p.includes('\n')) return null;
  return p;
}

// ─────────────────────────────────────────────────────────────────
// Server-side recognisers
// ─────────────────────────────────────────────────────────────────

/** Server-side stdlib `Handle*` form:
 *    mux.Handle(path, handler) | mux.HandleFunc(path, handler)
 *  Tagged-register form is matched separately when arity >= 3. */
function tryServerStdlibHandle(
  callNode: SyntaxNode,
  filePath: string,
  groups: GroupPrefixMap,
): RouteRegistration[] {
  const fn = callNode.childForFieldName?.('function');
  if (!fn || fn.type !== 'selector_expression') return [];
  const { field } = splitSelector(fn);
  if (!field || !STDLIB_REGISTER_METHODS.has(field)) return [];
  const args = [...callArguments(callNode)];
  if (args.length !== 2) return []; // tagged form is handled below
  const path = normalizePath(readGoString(args[0]));
  if (path === null) return [];
  const fullPath = applyGroupPrefix(callNode, path, groups);
  const { receiver: handlerReceiver, symbol: handlerSymbol } = extractCallableReference(args[1]);
  return [
    {
      method: '*',
      pathTemplate: fullPath,
      handlerSymbol,
      handlerReceiver,
      filePath,
      framework: 'go.stdlib',
      lineNumber: callNode.startPosition.row,
      confidence: handlerSymbol ? 1.0 : 0.7,
    },
  ];
}

/** Server-side verb-as-method form:
 *    router.GET(path, …handlers) | router.POST(path, …handlers) | …
 *  Disambiguated against client-side `client.Get(url)` by:
 *    - method name must be UPPER-CASE (`GET`, `POST`, …); idiomatic
 *      Go HTTP clients use mixed case (`http.Get`, `client.Get`).
 *    - first arg must be a string literal that looks like a path
 *      (starts with `/`).
 *  Anything else falls through to client-side detection. */
function tryServerVerbMethod(
  callNode: SyntaxNode,
  filePath: string,
  groups: GroupPrefixMap,
): RouteRegistration[] {
  const fn = callNode.childForFieldName?.('function');
  if (!fn || fn.type !== 'selector_expression') return [];
  const { receiver, field } = splitSelector(fn);
  if (!field || !VERB_METHOD_NAMES.has(field)) return [];
  // Require all-uppercase to keep this disjoint from client verbs.
  if (field !== field.toUpperCase()) return [];
  const args = [...callArguments(callNode)];
  if (args.length < 2) return [];
  const path = normalizePath(readGoString(args[0]));
  if (path === null) return [];
  const fullPath = applyGroupPrefix(callNode, path, groups);
  const handlerArg = args[args.length - 1];
  const { receiver: handlerReceiver, symbol: handlerSymbol } =
    extractCallableReference(handlerArg);
  // Skip when receiver looks like a client (`httpclient.GET(...)` is
  // unusual but defensive).
  const recvName = receiver?.text?.toLowerCase() ?? '';
  if (GO_CLIENT_RECEIVER_HINTS.has(recvName)) return [];
  // Stamp `*` if the verb is one of the catch-alls.
  const method: HttpMethod =
    field === 'ANY' || field === 'ALL' ? '*' : (field as HttpMethod);
  return [
    {
      method,
      pathTemplate: fullPath,
      handlerSymbol,
      handlerReceiver,
      filePath,
      framework: 'go.verb',
      lineNumber: callNode.startPosition.row,
      confidence: handlerSymbol ? 1.0 : 0.7,
    },
  ];
}

/** Server-side tagged-register form:
 *    router.AddRoute(method, paths, handler)
 *    router.Handle(method, path, handler)            // arity 3
 *    router.Method(method, path, handler)            // chi
 *    …
 *  Path arg may be a single string or a `[]string{…}` slice. */
function tryServerTaggedRegister(
  callNode: SyntaxNode,
  filePath: string,
  groups: GroupPrefixMap,
): RouteRegistration[] {
  const fn = callNode.childForFieldName?.('function');
  if (!fn || fn.type !== 'selector_expression') return [];
  const { field } = splitSelector(fn);
  if (!field || !TAGGED_REGISTER_METHODS.has(field)) return [];
  const args = [...callArguments(callNode)];
  if (args.length < 3) return [];
  const method = resolveHttpMethod(args[0]);
  if (!method) return [];
  const paths = resolvePathArg(args[1]);
  if (paths.length === 0) return [];
  const handlerArg = args[2];
  const { receiver: handlerReceiver, symbol: handlerSymbol } =
    extractCallableReference(handlerArg);
  const out: RouteRegistration[] = [];
  for (const raw of paths) {
    const p = normalizePath(raw);
    if (p === null) continue;
    const fullPath = applyGroupPrefix(callNode, p, groups);
    out.push({
      method,
      pathTemplate: fullPath,
      handlerSymbol,
      handlerReceiver,
      filePath,
      framework: 'go.tagged',
      lineNumber: callNode.startPosition.row,
      confidence: handlerSymbol ? 1.0 : 0.7,
    });
  }
  return out;
}

// ─────────────────────────────────────────────────────────────────
// Client-side recognisers
// ─────────────────────────────────────────────────────────────────

/** Client-side stdlib verb form:
 *    http.Get(url) | http.Post(url, …) | http.Head(url) | … */
function tryClientStdlibVerb(
  callNode: SyntaxNode,
  filePath: string,
): ClientCall[] {
  const fn = callNode.childForFieldName?.('function');
  if (!fn || fn.type !== 'selector_expression') return [];
  const { receiver, field } = splitSelector(fn);
  if (!field || !STDLIB_VERB_FUNCTIONS.has(field)) return [];
  const recvName = receiver?.type === 'identifier' ? receiver.text : null;
  // Require the receiver name to look like an HTTP namespace.
  if (recvName?.toLowerCase() !== 'http') return [];
  const args = [...callArguments(callNode)];
  if (args.length === 0) return [];
  const url = readGoString(args[0]);
  if (url === null) return [];
  const enclosing = findEnclosingFuncInfo(callNode);
  return [
    {
      method: normalizeHttpMethod(field === 'PostForm' ? 'POST' : field),
      pathLiteral: url,
      providerTag: null,
      callerSymbol: enclosing.symbol,
      callerReceiver: enclosing.receiver,
      filePath,
      framework: 'go.stdlib',
      lineNumber: callNode.startPosition.row,
      confidence: 1.0,
    },
  ];
}

/** Client-side verb-as-method form:
 *    httpClient.Get(url) | api.Post(url, body) | …
 *  Strict gating: receiver name must hint at a client; method
 *  must be one of the canonical Go verb names (mixed case);
 *  first arg must be a string literal. */
function tryClientVerbMethod(
  callNode: SyntaxNode,
  filePath: string,
): ClientCall[] {
  const fn = callNode.childForFieldName?.('function');
  if (!fn || fn.type !== 'selector_expression') return [];
  const { receiver, field } = splitSelector(fn);
  if (!field) return [];
  // Only canonical Go verb names; PostForm and Do are handled
  // elsewhere (PostForm by stdlib, Do by the request-builder).
  if (!STDLIB_VERB_FUNCTIONS.has(field) || field === 'PostForm') return [];
  const recvName = receiver?.type === 'identifier' ? receiver.text.toLowerCase() : '';
  // `http.Get(...)` / `https.Get(...)` are stdlib calls — ceded to
  // {@link tryClientStdlibVerb} so we don't double-emit.
  if (recvName === 'http' || recvName === 'https') return [];
  if (!GO_CLIENT_RECEIVER_HINTS.has(recvName)) {
    // Heuristic fallback: any receiver containing 'client'.
    if (!recvName.includes('client')) return [];
  }
  const args = [...callArguments(callNode)];
  if (args.length === 0) return [];
  const url = readGoString(args[0]);
  if (url === null) return [];
  const enclosing = findEnclosingFuncInfo(callNode);
  return [
    {
      method: normalizeHttpMethod(field),
      pathLiteral: url,
      providerTag: null,
      callerSymbol: enclosing.symbol,
      callerReceiver: enclosing.receiver,
      filePath,
      framework: 'go.client',
      lineNumber: callNode.startPosition.row,
      confidence: 0.85,
    },
  ];
}

/** Client-side request-builder form:
 *    http.NewRequest(method, url, body) | http.NewRequestWithContext(ctx, method, url, body)
 *  We capture method+URL at the builder site directly. The
 *  subsequent `client.Do(req)` is a separate concern (function-level
 *  attribution already follows from the enclosing scope). */
function tryClientRequestBuilder(
  callNode: SyntaxNode,
  filePath: string,
): ClientCall[] {
  const fn = callNode.childForFieldName?.('function');
  if (!fn || fn.type !== 'selector_expression') return [];
  const { receiver, field } = splitSelector(fn);
  if (!field) return [];
  const recvName = receiver?.type === 'identifier' ? receiver.text : null;
  if (recvName?.toLowerCase() !== 'http') return [];
  let methodIdx = -1;
  let urlIdx = -1;
  if (field === 'NewRequest') {
    methodIdx = 0;
    urlIdx = 1;
  } else if (field === 'NewRequestWithContext') {
    methodIdx = 1;
    urlIdx = 2;
  } else {
    return [];
  }
  const args = [...callArguments(callNode)];
  if (args.length <= urlIdx) return [];
  const method = resolveHttpMethod(args[methodIdx]);
  const url = readGoString(args[urlIdx]);
  if (!url) return [];
  const enclosing = findEnclosingFuncInfo(callNode);
  return [
    {
      method,
      pathLiteral: url,
      providerTag: null,
      callerSymbol: enclosing.symbol,
      callerReceiver: enclosing.receiver,
      filePath,
      framework: 'go.builder',
      lineNumber: callNode.startPosition.row,
      confidence: method ? 1.0 : 0.7,
    },
  ];
}

/** Client-side gRPC stub form:
 *    pb.NewKosmosClient(conn).Match(ctx, req)
 *
 *  Detection: the call's function field is a `selector_expression`
 *  whose operand is itself a `call_expression` to `New<X>Client` /
 *  `New<X>ServiceClient`. We treat the outer field as the RPC
 *  method, the inner factory's tail as the service name, and emit
 *  a synthetic path `/<Service>/<Method>` matching Go gRPC's
 *  internal full-method-name format. */
function tryClientGrpcStub(
  callNode: SyntaxNode,
  filePath: string,
): ClientCall[] {
  const fn = callNode.childForFieldName?.('function');
  if (!fn || fn.type !== 'selector_expression') return [];
  const { receiver, field: rpcMethod } = splitSelector(fn);
  if (!rpcMethod || !receiver || receiver.type !== 'call_expression') return [];
  const factoryFn = receiver.childForFieldName?.('function');
  const factoryName = extractCallableName(factoryFn ?? null);
  if (!factoryName) return [];
  const m = factoryName.match(/^New(.+?)(Service)?Client$/);
  if (!m) return [];
  const serviceName = m[1];
  const enclosing = findEnclosingFuncInfo(callNode);
  return [
    {
      method: 'POST', // gRPC tunnels over HTTP/2 POST
      pathLiteral: `/${serviceName}/${rpcMethod}`,
      providerTag: serviceName.toLowerCase(),
      callerSymbol: enclosing.symbol,
      callerReceiver: enclosing.receiver,
      filePath,
      framework: 'go.grpc',
      lineNumber: callNode.startPosition.row,
      confidence: 0.9,
    },
  ];
}

/** Walk a value subtree and record every distinct call-expression
 *  observed as a {@link PendingGetterLookup}. Phase 3.4 will resolve
 *  each chain against config-tag bindings to recover a `providerTag`.
 *
 *  We capture both:
 *    - free function calls — `GetXyz()` → `{receiver: null, name: "GetXyz"}`
 *    - method calls       — `recv.GetXyz()` → `{receiver: "recv", name: "GetXyz"}`
 *
 *  The `receiver` here is the *identifier text*, not the resolved type
 *  — Phase 3.4 first tries `(receiver, name)` as a method-on-type key,
 *  then falls back to `("*", name)` so this approximation never costs
 *  recall: if `recv` is a package alias rather than a typed local,
 *  the `("*", name)` fallback still finds the getter. */
function collectGetterLookups(node: SyntaxNode | null | undefined): PendingGetterLookup[] {
  if (!node) return [];
  const out: PendingGetterLookup[] = [];
  const seen = new Set<string>();
  const stack: SyntaxNode[] = [node];
  while (stack.length > 0) {
    const cur = stack.pop()!;
    if (cur.type === 'call_expression') {
      const fn = cur.childForFieldName?.('function') ?? null;
      if (fn) {
        if (fn.type === 'selector_expression') {
          const operand = fn.childForFieldName?.('operand');
          const field = fn.childForFieldName?.('field');
          if (field?.text) {
            const recv = operand?.type === 'identifier' ? operand.text : null;
            const key = `${recv ?? '*'}::${field.text}`;
            if (!seen.has(key)) {
              seen.add(key);
              out.push({ receiver: recv, name: field.text });
            }
          }
        } else if (fn.type === 'identifier') {
          const key = `*::${fn.text}`;
          if (!seen.has(key)) {
            seen.add(key);
            out.push({ receiver: null, name: fn.text });
          }
        }
      }
    }
    for (const c of cur.namedChildren ?? []) stack.push(c);
  }
  return out;
}

/** Client-side options-bag form:
 *    SomeOpts{Url: <str>, Method: <expr>, …}
 *  Catches any composite literal whose keyed elements include both
 *  a URL-ish field and (optionally) a Method-ish field. Provider
 *  tag and caller attribution are pulled from the same struct so
 *  the matcher can use either as a fallback when the URL is
 *  dynamic. */
function tryClientOptionsBag(
  litNode: SyntaxNode,
  filePath: string,
): ClientCall[] {
  if (litNode.type !== 'composite_literal') return [];
  const body = litNode.childForFieldName?.('body');
  if (!body || body.type !== 'literal_value') return [];

  let url: string | null = null;
  let urlIsPathOnly = false;
  let method: HttpMethod | null = null;
  let provider: string | null = null;
  let sawAnyOptionsField = false;
  let sawHostField = false;
  let sawMethodField = false;
  // Pending getter lookups harvested from non-literal URL/host/path
  // fields — fed to Phase 3.4 so the resolver can fold them into a
  // providerTag using struct-tag bindings.
  const pendingLookups: PendingGetterLookup[] = [];
  const pendingSeen = new Set<string>();
  const pushPending = (lk: PendingGetterLookup) => {
    const key = `${lk.receiver ?? '*'}::${lk.name}`;
    if (pendingSeen.has(key)) return;
    pendingSeen.add(key);
    pendingLookups.push(lk);
  };

  for (const child of body.children ?? []) {
    if (child.type !== 'keyed_element') continue;
    const namedChildren = (child.children ?? []).filter((c) => c.isNamed === true);
    if (namedChildren.length < 2) continue;
    const keyOuter = namedChildren[0];
    const valOuter = namedChildren[1];
    const keyNode = keyOuter.type === 'literal_element' ? keyOuter.children?.[0] : keyOuter;
    const valNode = valOuter.type === 'literal_element' ? valOuter.children?.[0] : valOuter;
    if (!keyNode || !valNode) continue;
    if (keyNode.type !== 'identifier') continue;
    const key = keyNode.text;
    if (OPTIONS_URL_FIELDS.has(key)) {
      sawAnyOptionsField = true;
      const u = readGoString(valNode);
      if (u !== null) {
        url = u;
        urlIsPathOnly = key === 'Path' || key === 'Route';
      } else {
        // Non-literal URL/path — record any embedded getter chains
        // so Phase 3.4 can resolve them to a provider tag.
        for (const lk of collectGetterLookups(valNode)) pushPending(lk);
        // Even if we can't recover the literal, the *presence* of a
        // path/route field still helps the HTTP-shape discriminator
        // below — but only when paired with a host or method. Track
        // the path-only bit accordingly so the existing combinator
        // logic still applies.
        if (key === 'Path' || key === 'Route') urlIsPathOnly = true;
      }
    } else if (OPTIONS_METHOD_FIELDS.has(key)) {
      sawAnyOptionsField = true;
      sawMethodField = true;
      const m = resolveHttpMethod(valNode);
      if (m) method = m;
    } else if (OPTIONS_PROVIDER_FIELDS.has(key)) {
      const s = readGoString(valNode);
      if (s !== null) provider = s;
    } else if (OPTIONS_HOST_FIELDS.has(key)) {
      sawHostField = true;
      sawAnyOptionsField = true;
      // Host fields are most often the same getter chain as the
      // path field (`GetXyz().Host` vs `GetXyz().Path`). Mining them
      // gives us a second chance at recovering the tag if the path
      // field resolved to a literal but the host didn't, or vice versa.
      const h = readGoString(valNode);
      if (h === null) {
        for (const lk of collectGetterLookups(valNode)) pushPending(lk);
      }
    }
  }

  // Discriminate between an HTTP-options struct and an arbitrary
  // struct that happens to have a `Path:` field. We require *one*
  // of these positive signals:
  //   - a Method field is present (URL field optional), OR
  //   - both a URL/path field and a host/scheme companion field
  //     are present, OR
  //   - a full URL field (`Url`/`URL`/`Endpoint`/`Address`) is
  //     present (these names alone are unambiguous), OR
  //   - a provider tag is recoverable (call is config-driven), OR
  //   - a pending getter lookup is recoverable (call is config-driven
  //     via a chained accessor like `GetXyz().Path`).
  const hasFullUrlField = url !== null && !urlIsPathOnly;
  if (!sawAnyOptionsField) return [];
  const isHttpShaped =
    sawMethodField ||
    hasFullUrlField ||
    (urlIsPathOnly && sawHostField) ||
    provider !== null ||
    pendingLookups.length > 0;
  if (!isHttpShaped) return [];
  // Reject when nothing — literal URL, provider tag, or pending
  // getter lookup — is recoverable. A pending lookup that fails to
  // resolve in Phase 3.4 is dropped silently then.
  if (url === null && provider === null && pendingLookups.length === 0) return [];

  const enclosing = findEnclosingFuncInfo(litNode);
  return [
    {
      method,
      pathLiteral: url,
      providerTag: provider,
      callerSymbol: enclosing.symbol,
      callerReceiver: enclosing.receiver,
      filePath,
      framework: 'go.options',
      lineNumber: litNode.startPosition.row,
      // Confidence floor for pending-lookup-only calls — Phase 3.4
      // either upgrades them by stamping a provider tag (in which
      // case the matcher promotes confidence via `processProviderTagFetches`)
      // or drops them entirely.
      confidence: url ? (method ? 0.95 : 0.85) : provider ? 0.7 : 0.5,
      pendingGetterLookups: pendingLookups.length > 0 ? pendingLookups : undefined,
    },
  ];
}

// ─────────────────────────────────────────────────────────────────
// Entry point
// ─────────────────────────────────────────────────────────────────

/** Client-side provider-tag factory:
 *    pkg.GetClient("kosmos") | pkg.NewClient("kosmos") | pkg.For("kosmos")
 *
 *  Internal HTTP wrappers commonly expose a factory whose only
 *  argument is a logical service tag; the returned client is then
 *  `.Do()`'d / `.Call()`'d with a request whose URL was loaded from
 *  configuration. We can't statically recover the URL, but we can
 *  capture the tag itself — the provider-tag resolver fans the call
 *  out to every Route living under the resolved service directory.
 *
 *  Detection is conservative:
 *    - call_expression on a selector,
 *    - field name in a small whitelist of factory verbs,
 *    - exactly 1 string-literal argument that looks tag-shaped. */
function tryClientProviderFactory(
  callNode: SyntaxNode,
  filePath: string,
): ClientCall[] {
  const fn = callNode.childForFieldName?.('function');
  if (!fn || fn.type !== 'selector_expression') return [];
  const { field } = splitSelector(fn);
  if (!field) return [];
  // Whitelist — generic across internal HTTP wrappers.
  const FACTORY_NAMES = new Set([
    'GetClient',
    'NewClient',
    'Client',
    'For',
    'ForService',
    'Provider',
  ]);
  if (!FACTORY_NAMES.has(field)) return [];
  const args = [...callArguments(callNode)];
  if (args.length !== 1) return [];
  const tag = readGoString(args[0]);
  if (tag === null) return [];
  // Tag must look service-name-shaped: lower/upper alphanumeric +
  // dashes/underscores, 2-64 chars; rejects URL-like or empty values.
  if (!/^[a-zA-Z][a-zA-Z0-9_-]{1,63}$/.test(tag)) return [];
  const enclosing = findEnclosingFuncInfo(callNode);
  return [
    {
      method: null,
      pathLiteral: null,
      providerTag: tag.toLowerCase(),
      callerSymbol: enclosing.symbol,
      callerReceiver: enclosing.receiver,
      filePath,
      framework: 'go.factory',
      lineNumber: callNode.startPosition.row,
      confidence: 0.6,
    },
  ];
}

/** Walk a Go AST and return every recognised route registration
 *  and outbound HTTP / RPC call. Idempotent: extracts cleanly even
 *  when the same expression is matched by overlapping forms (e.g.
 *  a 2-arg `Handle(path, h)` is stdlib; a 3-arg `Handle(method,
 *  path, h)` is tagged-register). */
export function extractGoApiEndpoints(
  rootNode: SyntaxNode,
  filePath: string,
): ExtractedApiEndpoints {
  const routes: RouteRegistration[] = [];
  const clientCalls: ClientCall[] = [];

  // First pass: recover router-group variables and their prefixes
  // so subsequent route registrations can prepend the right prefix.
  // Cheap — same iterative walk shape as the main pass.
  const groupPrefixes = collectGroupPrefixes(rootNode);

  // Main pass.
  const stack: SyntaxNode[] = [rootNode];
  while (stack.length > 0) {
    const node = stack.pop()!;

    if (node.type === 'call_expression') {
      // Server forms — emit before client forms. Tagged-register
      // takes precedence over stdlib `Handle` when arity matches.
      const tagged = tryServerTaggedRegister(node, filePath, groupPrefixes);
      if (tagged.length > 0) {
        routes.push(...tagged);
      } else {
        const stdlib = tryServerStdlibHandle(node, filePath, groupPrefixes);
        if (stdlib.length > 0) {
          routes.push(...stdlib);
        } else {
          const verb = tryServerVerbMethod(node, filePath, groupPrefixes);
          routes.push(...verb);
        }
      }

      // Client forms — these can co-exist with server matches when
      // (very rare in practice) a single call is ambiguous; the
      // matcher de-dupes downstream.
      clientCalls.push(...tryClientStdlibVerb(node, filePath));
      clientCalls.push(...tryClientVerbMethod(node, filePath));
      clientCalls.push(...tryClientRequestBuilder(node, filePath));
      clientCalls.push(...tryClientGrpcStub(node, filePath));
      clientCalls.push(...tryClientProviderFactory(node, filePath));
    } else if (node.type === 'composite_literal') {
      clientCalls.push(...tryClientOptionsBag(node, filePath));
    }

    // Continue traversal.
    const children = node.children ?? [];
    for (let i = children.length - 1; i >= 0; i--) {
      stack.push(children[i]);
    }
  }

  return { routes, clientCalls };
}
