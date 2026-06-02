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

/** Client-side URL-builder form:
 *    urlBuilder.SetPath("/jsonAds")
 *    b.WithPath("/log")
 *
 *  Why this lives here: internal Go services in monorepos commonly
 *  construct outbound URLs through a fluent builder (`urlutil.NewUrlBuilder`,
 *  `pageview.UrlBuilder`, etc.) instead of passing a string literal
 *  to an HTTP-client verb. The string literal sits on the
 *  `SetPath`/`WithPath` call; later `.String()` / `.Build()` consumes
 *  it and hands the result to whichever transport runs the request.
 *
 *  Because that downstream HTTP call is dataflow-invisible to the
 *  static extractor, we accept the literal on the builder call as
 *  the most reliable evidence of an outbound URL and emit a
 *  {@link ClientCall} with `method=null` (the builder doesn't carry
 *  the method — that's set elsewhere). The downstream URL-pattern
 *  matcher in pipeline.ts joins this to {@link Route} nodes the
 *  same way it does for `http.Get("/x")`.
 *
 *  False positives are extremely rare: `.SetPath("/literal")` with
 *  an absolute path is overwhelmingly a URL builder; non-HTTP
 *  builders (filesystem paths, CLI args) don't share the naming. */
/** Resolve a Go type node to its base type name, stripping pointer,
 *  package qualifier, and generic wrappers. Returns null for shapes
 *  we don't resolve (slices, maps, func types, etc.). */
function goBaseTypeName(typeNode: SyntaxNode | null | undefined): string | null {
  let n: SyntaxNode | null | undefined = typeNode;
  let guard = 0;
  while (n && guard++ < 8) {
    switch (n.type) {
      case 'pointer_type':
        n = n.namedChildren?.[0] ?? null;
        continue;
      case 'parenthesized_type':
        n = n.namedChildren?.[0] ?? null;
        continue;
      case 'qualified_type':
        return n.childForFieldName?.('name')?.text ?? null;
      case 'type_identifier':
        return n.text ?? null;
      case 'generic_type': {
        const inner = n.namedChildren?.find(
          (c) => c.type === 'type_identifier' || c.type === 'qualified_type',
        );
        n = inner ?? null;
        continue;
      }
      default:
        return null;
    }
  }
  return null;
}

/** Build a same-file `structType → (field → baseTypeName)` map from a
 *  Go file root. Used by the URL-builder recogniser to resolve a
 *  receiver-field chain (`c.adClickRouteService`) to its declared
 *  type (`AdClickRoute`) so the pending getter lookup is scoped to a
 *  type — preventing a generic method name (`GetPath`) from matching
 *  every same-named getter across the repo at resolve time. */
function collectStructFieldTypes(rootNode: SyntaxNode): Map<string, Map<string, string>> {
  const out = new Map<string, Map<string, string>>();
  const stack: SyntaxNode[] = [rootNode];
  while (stack.length > 0) {
    const n = stack.pop()!;
    if (n.type === 'type_spec') {
      const owner = n.childForFieldName?.('name')?.text;
      const typeNode = n.childForFieldName?.('type');
      if (owner && typeNode?.type === 'struct_type') {
        const fieldList = typeNode.namedChildren?.find(
          (c) => c.type === 'field_declaration_list',
        );
        if (fieldList) {
          let fm = out.get(owner);
          if (!fm) {
            fm = new Map<string, string>();
            out.set(owner, fm);
          }
          for (const fd of fieldList.namedChildren ?? []) {
            if (fd.type !== 'field_declaration') continue;
            const base = goBaseTypeName(fd.childForFieldName?.('type'));
            if (!base) continue;
            const names: SyntaxNode[] = [];
            const explicit = fd.childForFieldName?.('name');
            if (explicit) names.push(explicit);
            else for (const c of fd.namedChildren ?? []) if (c.type === 'field_identifier') names.push(c);
            for (const nm of names) if (nm.text) fm.set(nm.text, base);
          }
        }
      }
    }
    for (const c of n.namedChildren ?? []) stack.push(c);
  }
  return out;
}

/** Receiver variable name + type for the nearest enclosing method.
 *  `(c *ClickUrl)` → `{varName:"c", typeName:"ClickUrl"}`. Null for
 *  free functions or when the receiver shape is unrecognised. */
function findEnclosingReceiverVarType(
  node: SyntaxNode,
): { varName: string; typeName: string } | null {
  let cur: SyntaxNode | null = node.parent;
  while (cur) {
    if (cur.type === 'method_declaration') {
      const recvList = cur.childForFieldName?.('receiver');
      if (recvList) {
        for (const child of recvList.children ?? []) {
          if (child.type !== 'parameter_declaration') continue;
          const nameN = child.childForFieldName?.('name');
          const typeName = goBaseTypeName(child.childForFieldName?.('type'));
          if (nameN?.text && typeName) return { varName: nameN.text, typeName };
          return null;
        }
      }
      return null;
    }
    if (cur.type === 'function_declaration') return null;
    cur = cur.parent;
  }
  return null;
}

/** Flatten a selector / call / identifier into a left-to-right chain
 *  of identifier texts (`c.adClickRouteService.GetPath()` →
 *  `["c","adClickRouteService","GetPath"]`). Calls drop their args;
 *  any non-trivial shape returns null. */
function flattenGoChain(node: SyntaxNode | null | undefined): string[] | null {
  if (!node) return null;
  if (node.type === 'identifier' || node.type === 'field_identifier' || node.type === 'type_identifier') {
    return node.text ? [node.text] : null;
  }
  if (node.type === 'selector_expression') {
    const left = flattenGoChain(node.childForFieldName?.('operand'));
    const field = node.childForFieldName?.('field');
    if (!left || !field?.text) return null;
    return [...left, field.text];
  }
  if (node.type === 'call_expression') {
    return flattenGoChain(node.childForFieldName?.('function'));
  }
  if (node.type === 'parenthesized_expression') {
    return flattenGoChain(node.namedChildren?.[0]);
  }
  return null;
}

/** Find the RHS expression of a `name := <rhs>` / `name = <rhs>`
 *  assignment within `scope`, not descending into nested functions.
 *  Only the trivial single-LHS / single-RHS case. */
function findLocalAssignmentRHS(
  scope: SyntaxNode | null | undefined,
  name: string,
): SyntaxNode | null {
  if (!scope) return null;
  const stack: SyntaxNode[] = [scope];
  while (stack.length > 0) {
    const cur = stack.pop()!;
    if (
      cur !== scope &&
      (cur.type === 'func_literal' ||
        cur.type === 'function_declaration' ||
        cur.type === 'method_declaration')
    ) {
      continue;
    }
    if (cur.type === 'short_var_declaration' || cur.type === 'assignment_statement') {
      const left = cur.childForFieldName?.('left');
      const right = cur.childForFieldName?.('right');
      if (left && right) {
        const lhs = (left.namedChildren ?? []).filter((c) => c.type === 'identifier');
        const rhs = (right.namedChildren ?? []).filter((c) => c.isNamed === true);
        if (lhs.length === 1 && rhs.length === 1 && lhs[0].text === name) {
          return rhs[0];
        }
      }
    }
    for (const c of cur.namedChildren ?? []) stack.push(c);
  }
  return null;
}

function tryClientUrlBuilder(
  callNode: SyntaxNode,
  filePath: string,
  structFieldTypes: Map<string, Map<string, string>>,
): ClientCall[] {
  const fn = callNode.childForFieldName?.('function');
  if (!fn || fn.type !== 'selector_expression') return [];
  const { field } = splitSelector(fn);
  if (!field) return [];
  if (field !== 'SetPath' && field !== 'WithPath' && field !== 'SetHost') return [];
  const args = [...callArguments(callNode)];
  if (args.length === 0) return [];
  const arg = args[0];
  const url = readGoString(arg);
  const enclosing = findEnclosingFuncInfo(callNode);

  if (url !== null) {
    // Literal argument. Only the path setters carry a route literal,
    // and only when absolute. Non-absolute paths (e.g. `"v1/foo"`)
    // are usually relative-resolve idioms inside non-HTTP builders;
    // a literal host on `SetHost` is not a route either.
    if (field === 'SetHost') return [];
    if (!url.startsWith('/')) return [];
    return [
      {
        // The builder doesn't know the HTTP verb — the verb is set
        // wherever the built URL is consumed. Downstream pipeline
        // matches by URL alone; method=null is a valid match input.
        method: null,
        pathLiteral: url,
        providerTag: null,
        callerSymbol: enclosing.symbol,
        callerReceiver: enclosing.receiver,
        filePath,
        framework: 'go.urlbuilder',
        lineNumber: callNode.startPosition.row,
        // Confidence band reflects this is one step removed from
        // a real HTTP call (the builder is plausibly consumed by an
        // HTTP client, but we don't statically prove it).
        confidence: 0.7,
      },
    ];
  }

  // Non-literal argument (e.g. `urlBuilder.SetPath(svc.GetPath())` or
  // `path := svc.GetPath(); urlBuilder.SetPath(path)`). Record the
  // getter chain as a pending lookup so Phase 3.4c can chase it back
  // to an in-code string constant (e.g. `const DefaultPath = "/trf"`)
  // and stamp `fetchURL`. The downstream matcher self-filters: a
  // recovered literal only yields a FETCHES edge when it matches a
  // registered route, so coincidental constants never create edges.
  const body = findEnclosingFuncBody(callNode);

  // Recover the value expression: when the setter arg is a bare local,
  // substitute it for its assignment RHS.
  let valueExpr: SyntaxNode | null = arg;
  if (arg.type === 'identifier' && body) {
    const rhs = findLocalAssignmentRHS(body, arg.text);
    if (rhs) valueExpr = rhs;
  }
  const chain = flattenGoChain(valueExpr);
  if (!chain || chain.length === 0) return [];
  const method = chain[chain.length - 1];

  // Scope the lookup to the receiver TYPE when the chain is rooted at
  // the enclosing method's receiver and every hop is a known struct
  // field — e.g. `c.adClickRouteService.GetPath` with receiver
  // `c *ClickUrl` and field `adClickRouteService *AdClickRoute`
  // resolves the lookup to `{receiver:"AdClickRoute", name:"GetPath"}`.
  // Without a resolvable type the lookup stays receiver-less; the
  // in-code-constant resolver only emits receiver-scoped keys, so an
  // unresolved chain simply produces no edge (no false positives from
  // generic method names).
  let receiverType: string | null = null;
  if (chain.length >= 2) {
    const recv = findEnclosingReceiverVarType(callNode);
    if (recv && chain[0] === recv.varName) {
      let cur: string | null = recv.typeName;
      for (let i = 1; i < chain.length - 1 && cur; i++) {
        cur = structFieldTypes.get(cur)?.get(chain[i]) ?? null;
      }
      receiverType = cur;
    }
  }

  const localAssignments = body ? collectLocalAssignments(body) : {};
  return [
    {
      method: null,
      pathLiteral: null,
      providerTag: null,
      callerSymbol: enclosing.symbol,
      callerReceiver: enclosing.receiver,
      filePath,
      framework: 'go.urlbuilder',
      lineNumber: callNode.startPosition.row,
      // Two+ hops removed from a real HTTP call (builder arg resolved
      // through a getter + constant chain) — band the confidence low.
      confidence: 0.6,
      pendingGetterLookups: [{ receiver: receiverType, name: method }],
      ...(Object.keys(localAssignments).length > 0 ? { localAssignments } : {}),
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
            const key = `${recv ?? '*'}::${field.text}::`;
            if (!seen.has(key)) {
              seen.add(key);
              out.push({ receiver: recv, name: field.text });
            }
          }
        } else if (fn.type === 'identifier') {
          const key = `*::${fn.text}::`;
          if (!seen.has(key)) {
            seen.add(key);
            out.push({ receiver: null, name: fn.text });
          }
        }
      }
    } else if (cur.type === 'selector_expression') {
      // Bare field access on a local: `originConfig.Renderer`.
      // Only capture when it's NOT the function-position of an
      // enclosing call (the call branch above handles those).
      const parent = cur.parent;
      const isCallFn =
        parent?.type === 'call_expression' &&
        parent.childForFieldName?.('function') === cur;
      if (!isCallFn) {
        const operand = cur.childForFieldName?.('operand');
        const field = cur.childForFieldName?.('field');
        if (operand?.type === 'identifier' && field?.text) {
          const recv = operand.text;
          const key = `${recv}::${field.text}::${field.text}`;
          if (!seen.has(key)) {
            seen.add(key);
            // `tail` set signals the URL resolver to substitute the
            // receiver via the call site's localAssignments and then
            // apply `tail` as the field accessor on the substituted
            // chain. The legacy tag resolver also still tries
            // `(receiver, name)` / `(null, name)` for backward compat.
            out.push({ receiver: recv, name: field.text, tail: field.text });
          }
        }
      }
    }
    for (const c of cur.namedChildren ?? []) stack.push(c);
  }
  return out;
}

/** Walk a function body and record short-distance local assignments
 *  of the form `local := pkg.Func(...)` (call shape) or
 *  `local := x.Y.Z` (bare selector chain). Used by the URL resolver
 *  to substitute bare selector receivers back to a getter call so
 *  the YAML key path can be reconstructed across the indirection.
 *
 *  Scoping: stops descending into nested function literals; the
 *  outer body's locals don't leak in and inner-only locals don't
 *  leak out. Re-assignments overwrite earlier ones (last wins). */
function collectLocalAssignments(
  scope: SyntaxNode | null | undefined,
): Record<string, { call?: { receiver: string | null; name: string }; alias?: string[] }> {
  const out: Record<
    string,
    { call?: { receiver: string | null; name: string }; alias?: string[] }
  > = {};
  if (!scope) return out;
  const stack: SyntaxNode[] = [scope];
  while (stack.length > 0) {
    const cur = stack.pop()!;
    // Don't descend into nested function literals — they have their
    // own scope. The outer extractor will visit them separately if
    // they emit ClientCalls of their own.
    if (cur !== scope && (cur.type === 'func_literal' || cur.type === 'function_declaration' || cur.type === 'method_declaration')) {
      continue;
    }
    if (cur.type === 'short_var_declaration' || cur.type === 'assignment_statement') {
      const left = cur.childForFieldName?.('left');
      const right = cur.childForFieldName?.('right');
      if (left && right) {
        const lhsList = (left.namedChildren ?? []).filter((c) => c.type === 'identifier');
        const rhsList = (right.namedChildren ?? []).filter((c) => c.isNamed === true);
        // Only handle the trivial 1:1 case. Multi-return and tuple
        // destructuring are out of scope (typically not config-driven).
        if (lhsList.length === 1 && rhsList.length === 1) {
          const localName = lhsList[0].text;
          const rhs = rhsList[0];
          if (!localName) {
            // nothing
          } else if (rhs.type === 'call_expression') {
            const fn = rhs.childForFieldName?.('function');
            if (fn?.type === 'selector_expression') {
              const operand = fn.childForFieldName?.('operand');
              const field = fn.childForFieldName?.('field');
              if (field?.text) {
                out[localName] = {
                  call: {
                    receiver: operand?.type === 'identifier' ? operand.text : null,
                    name: field.text,
                  },
                };
              }
            } else if (fn?.type === 'identifier') {
              out[localName] = { call: { receiver: null, name: fn.text } };
            }
          } else if (rhs.type === 'selector_expression') {
            const alias = flattenSelectorChain(rhs);
            if (alias && alias.length >= 2) {
              out[localName] = { alias };
            }
          }
        }
      }
    }
    for (const c of cur.namedChildren ?? []) stack.push(c);
  }
  return out;
}

/** Flatten a selector_expression / identifier into a left-to-right
 *  chain of identifier texts. Returns null when any intermediate
 *  node isn't a plain identifier or another selector. */
function flattenSelectorChain(node: SyntaxNode | null | undefined): string[] | null {
  if (!node) return null;
  if (node.type === 'identifier' || node.type === 'field_identifier') {
    return node.text ? [node.text] : null;
  }
  if (node.type === 'selector_expression') {
    const operand = node.childForFieldName?.('operand') ?? null;
    const field = node.childForFieldName?.('field') ?? null;
    const left = flattenSelectorChain(operand);
    if (!left || !field?.text) return null;
    return [...left, field.text];
  }
  return null;
}

/** Find the nearest enclosing function or method body (the `block`
 *  node that holds locals visible to `node`). Returns null when
 *  `node` is at top level. */
function findEnclosingFuncBody(node: SyntaxNode | null): SyntaxNode | null {
  let cur: SyntaxNode | null = node;
  while (cur) {
    if (
      cur.type === 'function_declaration' ||
      cur.type === 'method_declaration' ||
      cur.type === 'func_literal'
    ) {
      return cur.childForFieldName?.('body') ?? null;
    }
    cur = cur.parent;
  }
  return null;
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

  // When a pending lookup carries a `tail` (bare selector shape like
  // `originConfig.Renderer`), the URL resolver needs to substitute
  // the bare receiver back to a getter call. Walk the enclosing
  // function body once to build the local→call map and attach it.
  let localAssignments: Record<
    string,
    { call?: { receiver: string | null; name: string }; alias?: string[] }
  > | undefined;
  const hasBareSelector = pendingLookups.some((l) => l.tail !== undefined);
  if (hasBareSelector) {
    const body = findEnclosingFuncBody(litNode);
    if (body) {
      const all = collectLocalAssignments(body);
      // Trim to only the names actually referenced by pending lookups
      // so we don't bloat the ClientCall with unused entries.
      const needed = new Set(pendingLookups.filter((l) => l.tail).map((l) => l.receiver ?? ''));
      const filtered: Record<
        string,
        { call?: { receiver: string | null; name: string }; alias?: string[] }
      > = {};
      let kept = 0;
      for (const name of needed) {
        if (name && all[name]) {
          filtered[name] = all[name];
          kept++;
        }
      }
      if (kept > 0) localAssignments = filtered;
    }
  }

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
      localAssignments,
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

  // Same-file struct field types — lets the URL-builder recogniser
  // resolve a receiver-field chain to a declared type so its pending
  // getter lookup is type-scoped (see tryClientUrlBuilder).
  const structFieldTypes = collectStructFieldTypes(rootNode);

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
      clientCalls.push(...tryClientUrlBuilder(node, filePath, structFieldTypes));
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
