/**
 * Generic Java API-endpoint extractor.
 *
 * Detection is by AST shape + annotation name, never by package or
 * class name. Any framework that uses one of the established Java
 * web-annotation conventions is captured without hardcoding the repo.
 *
 * Server-side forms (route registrations):
 *   1. **Spring MVC verb shortcuts** —
 *        `@GetMapping("/x")`, `@PostMapping(value = "/x")`,
 *        `@PutMapping`, `@DeleteMapping`, `@PatchMapping`.
 *      Class-level `@RequestMapping("/prefix")` (or any verb shortcut)
 *      contributes a path prefix to all methods of the class.
 *   2. **Spring `@RequestMapping`** —
 *        `@RequestMapping(value = "/x", method = RequestMethod.POST)`
 *      with `method = ` either a single `RequestMethod.X`, a bare
 *      `X`, an array `{POST, PUT}`, or omitted (in which case `*`).
 *   3. **JAX-RS** —
 *        `@Path("/users")` on the class plus `@GET`/`@POST`/… (and
 *        an optional `@Path("/{id}")`) on each method. Catches
 *        Jersey, RESTEasy, CXF and any other JAX-RS impl.
 *   4. **Spring `@FeignClient`** — interfaces annotated with
 *        `@FeignClient(name = "kosmos", url = "https://…")`. Each
 *      interface method's `@GetMapping`/`@RequestMapping`/… emits a
 *      {@link ClientCall} (these *consume* HTTP, they don't serve it).
 *      The provider tag is captured for join-by-name.
 *
 * Client-side forms (outbound HTTP):
 *   1. **RestTemplate** —
 *        `rt.getForObject(url, …)`, `rt.postForEntity(url, …)`,
 *        `rt.exchange(url, HttpMethod.X, …)`, `rt.execute(url, X, …)`.
 *   2. **Spring 5+ WebClient** — `client.get().uri("/x")`,
 *        `client.method(HttpMethod.POST).uri("/x")`.
 *   3. **java.net.http.HttpClient** — `HttpRequest.newBuilder(URI.create("/x"))`
 *        plus `.GET()` / `.POST(...)` / `.method("POST", ...)`.
 *   4. **OkHttp** — `new Request.Builder().url("/x").get()` / `.post(...)`.
 *   5. **Apache HttpClient** — `new HttpGet("/x")` / `new HttpPost("/x")` etc.
 *   6. **Feign declarations** (covered above).
 *
 * The extractor is intentionally *forgiving*: when a form looks
 * shaped like an HTTP call but a literal path can't be statically
 * recovered, we still emit a {@link ClientCall} with `pathLiteral =
 * null` and `providerTag` populated when available, so the
 * provider-tag resolver can later join it by host.
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
// Constants — shape recognisers
// ─────────────────────────────────────────────────────────────────

/** Spring verb-shortcut annotations → HTTP method. */
const SPRING_VERB_ANNOTATIONS: ReadonlyMap<string, HttpMethod> = new Map([
  ['GetMapping', 'GET'],
  ['PostMapping', 'POST'],
  ['PutMapping', 'PUT'],
  ['DeleteMapping', 'DELETE'],
  ['PatchMapping', 'PATCH'],
]);

/** JAX-RS verb annotations (marker form, no args). */
const JAXRS_VERB_ANNOTATIONS: ReadonlyMap<string, HttpMethod> = new Map([
  ['GET', 'GET'],
  ['POST', 'POST'],
  ['PUT', 'PUT'],
  ['DELETE', 'DELETE'],
  ['PATCH', 'PATCH'],
  ['HEAD', 'HEAD'],
  ['OPTIONS', 'OPTIONS'],
]);

/** Annotation names that contribute a path prefix at the class level. */
const SPRING_PREFIX_ANNOTATIONS: ReadonlySet<string> = new Set<string>([
  'RequestMapping',
  ...SPRING_VERB_ANNOTATIONS.keys(),
]);

/** RestTemplate methods → (HTTP method, URL arg index). */
const REST_TEMPLATE_METHODS: ReadonlyMap<
  string,
  { method: HttpMethod | null; urlIdx: number; methodArgIdx?: number }
> = new Map([
  ['getForObject', { method: 'GET', urlIdx: 0 }],
  ['getForEntity', { method: 'GET', urlIdx: 0 }],
  ['postForObject', { method: 'POST', urlIdx: 0 }],
  ['postForEntity', { method: 'POST', urlIdx: 0 }],
  ['postForLocation', { method: 'POST', urlIdx: 0 }],
  ['put', { method: 'PUT', urlIdx: 0 }],
  ['patchForObject', { method: 'PATCH', urlIdx: 0 }],
  ['delete', { method: 'DELETE', urlIdx: 0 }],
  ['headForHeaders', { method: 'HEAD', urlIdx: 0 }],
  ['optionsForAllow', { method: 'OPTIONS', urlIdx: 0 }],
  // Generic forms — method comes from a later argument.
  ['exchange', { method: null, urlIdx: 0, methodArgIdx: 1 }],
  ['execute', { method: null, urlIdx: 0, methodArgIdx: 1 }],
]);

/** Apache HttpClient request classes → HTTP method. */
const APACHE_REQUEST_CLASSES: ReadonlyMap<string, HttpMethod> = new Map([
  ['HttpGet', 'GET'],
  ['HttpPost', 'POST'],
  ['HttpPut', 'PUT'],
  ['HttpDelete', 'DELETE'],
  ['HttpPatch', 'PATCH'],
  ['HttpHead', 'HEAD'],
  ['HttpOptions', 'OPTIONS'],
  ['HttpTrace', 'TRACE'],
]);

/** OkHttp request-builder verb methods. */
const OKHTTP_VERBS: ReadonlyMap<string, HttpMethod> = new Map([
  ['get', 'GET'],
  ['post', 'POST'],
  ['put', 'PUT'],
  ['delete', 'DELETE'],
  ['patch', 'PATCH'],
  ['head', 'HEAD'],
]);

// ─────────────────────────────────────────────────────────────────
// Small AST helpers
// ─────────────────────────────────────────────────────────────────

/** Read the content of a Java `string_literal`. */
function readJavaString(node: SyntaxNode | null | undefined): string | null {
  if (!node) return null;
  if (node.type === 'string_literal') {
    // string_literal has 0-or-more string_fragment children.
    const fragments: string[] = [];
    for (const child of node.namedChildren ?? []) {
      if (child.type === 'string_fragment') fragments.push(child.text ?? '');
    }
    if (fragments.length > 0) return fragments.join('');
    // Fallback: strip wrapping quotes.
    const raw = node.text ?? '';
    if (raw.length >= 2 && raw.startsWith('"') && raw.endsWith('"')) return raw.slice(1, -1);
    return raw;
  }
  return null;
}

/** Pull all annotations attached to a `modifiers` node. */
function collectAnnotations(modifiers: SyntaxNode | null | undefined): SyntaxNode[] {
  if (!modifiers) return [];
  const out: SyntaxNode[] = [];
  for (const child of modifiers.namedChildren ?? []) {
    if (child.type === 'annotation' || child.type === 'marker_annotation') out.push(child);
  }
  return out;
}

/** Get the bare annotation name (`GetMapping` from `@GetMapping(…)`). */
function annotationName(annotation: SyntaxNode): string | null {
  const ident = annotation.namedChildren?.find((c) => c.type === 'identifier');
  if (!ident) {
    // scoped form: `org.springframework.web.bind.annotation.GetMapping`
    const scoped = annotation.namedChildren?.find((c) => c.type === 'scoped_identifier');
    if (scoped) {
      const parts = scoped.text?.split('.') ?? [];
      return parts.length > 0 ? parts[parts.length - 1] : null;
    }
    return null;
  }
  return ident.text ?? null;
}

/** Resolve the `value` attribute of an annotation, accepting both
 *  positional (`@GetMapping("/x")`) and keyed (`@GetMapping(value = "/x")`)
 *  forms. Returns all path strings when the value is an array
 *  initializer (`@GetMapping({"/a", "/b"})`). */
function annotationStringValues(annotation: SyntaxNode, key = 'value'): string[] {
  const argList = annotation.childForFieldName?.('arguments') ??
    annotation.namedChildren?.find((c) => c.type === 'annotation_argument_list');
  if (!argList) return [];

  const collectFromInitializer = (init: SyntaxNode): string[] => {
    const out: string[] = [];
    for (const child of init.namedChildren ?? []) {
      const s = readJavaString(child);
      if (s !== null) out.push(s);
    }
    return out;
  };

  // Positional: a lone string_literal directly inside argument_list
  // (Spring `@GetMapping("/x")`).
  let sawPair = false;
  for (const child of argList.namedChildren ?? []) {
    if (child.type === 'element_value_pair') {
      sawPair = true;
      const ident = child.namedChildren?.find((c) => c.type === 'identifier');
      if (ident?.text === key) {
        const valueNode = child.namedChildren?.find((c) => c !== ident);
        if (valueNode?.type === 'string_literal') {
          const s = readJavaString(valueNode);
          return s !== null ? [s] : [];
        }
        if (valueNode?.type === 'element_value_array_initializer') {
          return collectFromInitializer(valueNode);
        }
      }
    }
  }
  if (sawPair) return [];

  // No element_value_pair → positional value.
  for (const child of argList.namedChildren ?? []) {
    if (child.type === 'string_literal') {
      const s = readJavaString(child);
      return s !== null ? [s] : [];
    }
    if (child.type === 'element_value_array_initializer') {
      return collectFromInitializer(child);
    }
  }
  return [];
}

/** Resolve the `method = …` attribute of a Spring `@RequestMapping`.
 *  Accepts `RequestMethod.POST`, bare `POST`, or `{POST, PUT}`. */
function annotationMethodValues(annotation: SyntaxNode): HttpMethod[] {
  const argList = annotation.childForFieldName?.('arguments') ??
    annotation.namedChildren?.find((c) => c.type === 'annotation_argument_list');
  if (!argList) return [];

  const fromExpr = (expr: SyntaxNode | null | undefined): HttpMethod[] => {
    if (!expr) return [];
    if (expr.type === 'identifier') {
      const m = normalizeHttpMethod(expr.text);
      return m ? [m] : [];
    }
    if (expr.type === 'field_access') {
      const tail = expr.namedChildren?.[expr.namedChildren.length - 1];
      const m = normalizeHttpMethod(tail?.text ?? null);
      return m ? [m] : [];
    }
    if (expr.type === 'element_value_array_initializer') {
      const out: HttpMethod[] = [];
      for (const child of expr.namedChildren ?? []) {
        out.push(...fromExpr(child));
      }
      return out;
    }
    return [];
  };

  for (const child of argList.namedChildren ?? []) {
    if (child.type !== 'element_value_pair') continue;
    const ident = child.namedChildren?.find((c) => c.type === 'identifier');
    if (ident?.text !== 'method') continue;
    const valueNode = child.namedChildren?.find((c) => c !== ident);
    return fromExpr(valueNode);
  }
  return [];
}

/** Normalise a path string. Leading slash, no trailing slash. */
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

/** Walk every node bottom-up; cheap iterative DFS. */
function* walk(root: SyntaxNode): Generator<SyntaxNode> {
  const stack: SyntaxNode[] = [root];
  while (stack.length > 0) {
    const node = stack.pop()!;
    yield node;
    const children = node.namedChildren ?? [];
    for (let i = children.length - 1; i >= 0; i--) stack.push(children[i]);
  }
}

/** Walk up to the enclosing method or class. Used to attribute
 *  client calls to their caller. */
function findEnclosingMethodInfo(node: SyntaxNode): {
  symbol: string | null;
  receiver: string | null;
} {
  let cur: SyntaxNode | null = node.parent;
  let methodName: string | null = null;
  while (cur) {
    if (
      !methodName &&
      (cur.type === 'method_declaration' || cur.type === 'constructor_declaration')
    ) {
      methodName = cur.childForFieldName?.('name')?.text ?? null;
    }
    if (
      cur.type === 'class_declaration' ||
      cur.type === 'interface_declaration' ||
      cur.type === 'record_declaration' ||
      cur.type === 'enum_declaration'
    ) {
      const owner = cur.childForFieldName?.('name')?.text ?? null;
      return { symbol: methodName, receiver: owner };
    }
    cur = cur.parent;
  }
  return { symbol: methodName, receiver: null };
}

// ─────────────────────────────────────────────────────────────────
// Server-side: Spring MVC + JAX-RS at type level
// ─────────────────────────────────────────────────────────────────

/** Recover the path prefix and `@FeignClient` info contributed by
 *  a class- / interface-level annotation set. Returns:
 *    - prefix: joined `/x` path, or '' if none
 *    - feignProvider: provider name from `@FeignClient(name=…)`, else null
 *    - jaxrs: true if the class is annotated `@Path(…)` (JAX-RS resource)
 *    - skipServer: true when this is a `@FeignClient` (its methods are
 *      client calls, not server routes).
 */
function classLevelInfo(modifiers: SyntaxNode | null): {
  prefix: string;
  feignProvider: string | null;
  jaxrs: boolean;
  skipServer: boolean;
} {
  let prefix = '';
  let feignProvider: string | null = null;
  let jaxrs = false;
  let skipServer = false;
  for (const ann of collectAnnotations(modifiers)) {
    const name = annotationName(ann);
    if (!name) continue;
    if (name === 'FeignClient') {
      // Provider name → priority order: `name`, `value`, `contextId`.
      const candidates =
        annotationStringValues(ann, 'name')[0] ??
        annotationStringValues(ann, 'value')[0] ??
        annotationStringValues(ann, 'contextId')[0] ??
        null;
      if (candidates) feignProvider = candidates;
      // FeignClient may also pin a path via `path = "/api"`.
      const p = annotationStringValues(ann, 'path')[0];
      if (p) prefix = normalizePath(p) ?? '';
      skipServer = true;
    } else if (SPRING_PREFIX_ANNOTATIONS.has(name)) {
      const v = annotationStringValues(ann, 'value')[0] ?? annotationStringValues(ann, 'path')[0];
      const p = normalizePath(v ?? null);
      if (p) prefix = p;
    } else if (name === 'Path') {
      jaxrs = true;
      const v = annotationStringValues(ann, 'value')[0] ?? annotationStringValues(ann)[0];
      const p = normalizePath(v ?? null);
      if (p) prefix = p;
    }
  }
  return { prefix, feignProvider, jaxrs, skipServer };
}

/** Examine a method's annotations and emit one or more {@link
 *  RouteRegistration} (Spring MVC / JAX-RS) or {@link ClientCall}
 *  (Spring `@FeignClient` interface methods). */
function processTypeMember(
  classModifiers: SyntaxNode | null,
  className: string | null,
  member: SyntaxNode,
  filePath: string,
  out: ExtractedApiEndpoints,
): void {
  if (member.type !== 'method_declaration') return;
  const memberModifiers = member.childForFieldName?.('modifiers') ??
    member.namedChildren?.find((c) => c.type === 'modifiers');
  const annotations = collectAnnotations(memberModifiers);
  if (annotations.length === 0) return;

  const handlerSymbol = member.childForFieldName?.('name')?.text ?? null;
  const lineNumber = member.startPosition?.row ?? 0;

  const cls = classLevelInfo(classModifiers);

  for (const ann of annotations) {
    const aName = annotationName(ann);
    if (!aName) continue;

    // Spring verb shortcuts.
    const springVerb = SPRING_VERB_ANNOTATIONS.get(aName);
    if (springVerb) {
      const paths = annotationStringValues(ann, 'value');
      const altPaths = paths.length === 0 ? annotationStringValues(ann, 'path') : paths;
      const collected = altPaths.length > 0 ? altPaths : [''];
      for (const raw of collected) {
        const sub = normalizePath(raw) ?? '';
        const full = normalizePath(joinPaths(cls.prefix, sub) || '/');
        if (full === null) continue;
        if (cls.skipServer) {
          // `@FeignClient` interface method = client call.
          out.clientCalls.push({
            method: springVerb,
            pathLiteral: full,
            providerTag: cls.feignProvider,
            callerSymbol: handlerSymbol,
            callerReceiver: className,
            filePath,
            framework: 'spring.feign',
            lineNumber,
            confidence: 1.0,
          });
        } else {
          out.routes.push({
            method: springVerb,
            pathTemplate: full,
            handlerSymbol,
            handlerReceiver: className,
            filePath,
            framework: 'spring.mvc',
            lineNumber,
            confidence: handlerSymbol ? 1.0 : 0.7,
          });
        }
      }
      continue;
    }

    // Spring `@RequestMapping(value=, method=)`.
    if (aName === 'RequestMapping') {
      const paths = annotationStringValues(ann, 'value');
      const altPaths = paths.length === 0 ? annotationStringValues(ann, 'path') : paths;
      const positional =
        altPaths.length === 0 ? annotationStringValues(ann) : altPaths;
      const collected = positional.length > 0 ? positional : [''];
      const methods = annotationMethodValues(ann);
      const httpMethods: HttpMethod[] = methods.length > 0 ? methods : ['*'];
      for (const raw of collected) {
        const sub = normalizePath(raw) ?? '';
        const full = normalizePath(joinPaths(cls.prefix, sub) || '/');
        if (full === null) continue;
        for (const m of httpMethods) {
          if (cls.skipServer) {
            out.clientCalls.push({
              method: m === '*' ? null : m,
              pathLiteral: full,
              providerTag: cls.feignProvider,
              callerSymbol: handlerSymbol,
              callerReceiver: className,
              filePath,
              framework: 'spring.feign',
              lineNumber,
              confidence: 1.0,
            });
          } else {
            out.routes.push({
              method: m,
              pathTemplate: full,
              handlerSymbol,
              handlerReceiver: className,
              filePath,
              framework: 'spring.mvc',
              lineNumber,
              confidence: handlerSymbol ? 1.0 : 0.7,
            });
          }
        }
      }
      continue;
    }

    // JAX-RS — verb annotation on method, optional `@Path` companion.
    const jaxrsVerb = JAXRS_VERB_ANNOTATIONS.get(aName);
    if (jaxrsVerb) {
      // Look for a sibling `@Path("/sub")` on the same method.
      let methodPath = '';
      for (const sibling of annotations) {
        if (annotationName(sibling) === 'Path') {
          methodPath =
            normalizePath(
              annotationStringValues(sibling, 'value')[0] ?? annotationStringValues(sibling)[0] ??
                null,
            ) ?? '';
          break;
        }
      }
      const full = normalizePath(joinPaths(cls.prefix, methodPath) || '/');
      if (full === null) continue;
      out.routes.push({
        method: jaxrsVerb,
        pathTemplate: full,
        handlerSymbol,
        handlerReceiver: className,
        filePath,
        framework: 'jaxrs',
        lineNumber,
        confidence: handlerSymbol ? 1.0 : 0.7,
      });
    }
  }
}

// ─────────────────────────────────────────────────────────────────
// Shared client-side helpers
// ─────────────────────────────────────────────────────────────────

/** Walk a Java URL-argument expression (string concatenation,
 *  method-invocation chain, ternary, etc.) and harvest every
 *  `method_invocation` it contains as a {@link PendingGetterLookup}.
 *  Phase 3.4 will resolve each chain against `@Value`-bound config
 *  fields to recover a `providerTag` for the call site.
 *
 *  The receiver, when present, is the *identifier text* (not its
 *  resolved type) — Phase 3.4 first tries `(receiver, name)` then
 *  falls back to `("*", name)`, so the approximation is safe. */
function collectJavaGetterLookups(
  node: SyntaxNode | null | undefined,
): PendingGetterLookup[] {
  if (!node) return [];
  const out: PendingGetterLookup[] = [];
  const seen = new Set<string>();
  const stack: SyntaxNode[] = [node];
  while (stack.length > 0) {
    const cur = stack.pop()!;
    if (cur.type === 'method_invocation') {
      const obj = cur.childForFieldName?.('object');
      const name = cur.childForFieldName?.('name');
      if (name?.text) {
        const recv = obj?.type === 'identifier' ? obj.text : null;
        const key = `${recv ?? '*'}::${name.text}`;
        if (!seen.has(key)) {
          seen.add(key);
          out.push({ receiver: recv, name: name.text });
        }
      }
    }
    for (const c of cur.namedChildren ?? []) stack.push(c);
  }
  return out;
}

// ─────────────────────────────────────────────────────────────────
// Client-side recognisers
// ─────────────────────────────────────────────────────────────────

/** RestTemplate-shape: `someTemplate.<method>(url, …)`. */
function tryRestTemplate(
  callNode: SyntaxNode,
  filePath: string,
  restTemplateVars: ReadonlySet<string>,
): ClientCall[] {
  if (callNode.type !== 'method_invocation') return [];
  const name = callNode.childForFieldName?.('name')?.text ?? null;
  if (!name) return [];
  const spec = REST_TEMPLATE_METHODS.get(name);
  if (!spec) return [];
  const objectNode = callNode.childForFieldName?.('object');
  const receiverIdent = objectNode?.type === 'identifier' ? objectNode.text : null;
  const receiverText = objectNode?.text?.toLowerCase() ?? '';
  const looksLikeTemplate =
    (receiverIdent && restTemplateVars.has(receiverIdent)) ||
    receiverText.includes('template') ||
    receiverText.includes('rest') ||
    receiverText.endsWith('client');
  if (!looksLikeTemplate) {
    // Fall back to allow any `*.exchange(url, HttpMethod, …)` form
    // since `exchange` is unambiguous.
    if (name !== 'exchange' && name !== 'execute') return [];
  }
  const args = callNode.childForFieldName?.('arguments');
  const argChildren = (args?.namedChildren ?? []).filter((c) => c.isNamed === true);
  if (argChildren.length <= spec.urlIdx) return [];
  const urlArg = argChildren[spec.urlIdx];
  const url = readJavaString(urlArg);
  // Even when the URL isn't a literal, a non-literal expression
  // containing a JavaBean getter (e.g. `props.getKosmosUrl() + "/path"`)
  // can still resolve to a provider tag via Phase 3.4.
  const pendingLookups = url === null ? collectJavaGetterLookups(urlArg) : [];
  if (url === null && pendingLookups.length === 0) return [];
  let resolvedMethod: HttpMethod | null = spec.method;
  if (spec.method === null && spec.methodArgIdx !== undefined) {
    const arg = argChildren[spec.methodArgIdx];
    if (arg) {
      if (arg.type === 'field_access') {
        const tail = arg.namedChildren?.[arg.namedChildren.length - 1];
        resolvedMethod = normalizeHttpMethod(tail?.text ?? null);
      } else if (arg.type === 'identifier') {
        resolvedMethod = normalizeHttpMethod(arg.text);
      } else if (arg.type === 'string_literal') {
        resolvedMethod = normalizeHttpMethod(readJavaString(arg));
      }
    }
  }
  const enclosing = findEnclosingMethodInfo(callNode);
  return [
    {
      method: resolvedMethod,
      pathLiteral: url,
      providerTag: null,
      callerSymbol: enclosing.symbol,
      callerReceiver: enclosing.receiver,
      filePath,
      framework: 'spring.resttemplate',
      lineNumber: callNode.startPosition?.row ?? 0,
      confidence: url ? (resolvedMethod ? 0.95 : 0.7) : 0.5,
      pendingGetterLookups: pendingLookups.length > 0 ? pendingLookups : undefined,
    },
  ];
}

/** WebClient-shape: walk the method-chain backwards from `.uri(url)`
 *  to the originating verb (`get`, `post`, `method(HttpMethod.X)`). */
function tryWebClient(callNode: SyntaxNode, filePath: string): ClientCall[] {
  if (callNode.type !== 'method_invocation') return [];
  const name = callNode.childForFieldName?.('name')?.text ?? null;
  if (name !== 'uri') return [];
  const args = callNode.childForFieldName?.('arguments');
  const argChildren = (args?.namedChildren ?? []).filter((c) => c.isNamed === true);
  if (argChildren.length === 0) return [];
  const urlArg = argChildren[0];
  const url = readJavaString(urlArg);
  const pendingLookups = url === null ? collectJavaGetterLookups(urlArg) : [];
  if (url === null && pendingLookups.length === 0) return [];
  // Chain: `.uri(...)` is invoked on `prevCall.<verb>()`; the receiver
  // of `uri` is itself a method_invocation.
  const objectNode = callNode.childForFieldName?.('object');
  if (!objectNode || objectNode.type !== 'method_invocation') return [];
  let httpMethod: HttpMethod | null = null;
  let cur: SyntaxNode | null = objectNode;
  while (cur && cur.type === 'method_invocation') {
    const callName = cur.childForFieldName?.('name')?.text ?? '';
    const lower = callName.toLowerCase();
    const verbMatch = OKHTTP_VERBS.get(lower);
    if (verbMatch) {
      httpMethod = verbMatch;
      break;
    }
    if (callName === 'method') {
      // method(HttpMethod.POST)
      const a = cur.childForFieldName?.('arguments');
      const aChildren = (a?.namedChildren ?? []).filter((c) => c.isNamed === true);
      if (aChildren.length > 0) {
        const m0 = aChildren[0];
        if (m0.type === 'field_access') {
          const tail = m0.namedChildren?.[m0.namedChildren.length - 1];
          httpMethod = normalizeHttpMethod(tail?.text ?? null);
        } else if (m0.type === 'identifier') {
          httpMethod = normalizeHttpMethod(m0.text);
        } else if (m0.type === 'string_literal') {
          httpMethod = normalizeHttpMethod(readJavaString(m0));
        }
      }
      break;
    }
    cur = cur.childForFieldName?.('object') ?? null;
  }
  if (!httpMethod) return [];
  const enclosing = findEnclosingMethodInfo(callNode);
  return [
    {
      method: httpMethod,
      pathLiteral: url,
      providerTag: null,
      callerSymbol: enclosing.symbol,
      callerReceiver: enclosing.receiver,
      filePath,
      framework: 'spring.webclient',
      lineNumber: callNode.startPosition?.row ?? 0,
      confidence: url ? 0.9 : 0.55,
      pendingGetterLookups: pendingLookups.length > 0 ? pendingLookups : undefined,
    },
  ];
}

/** OkHttp-shape: `new Request.Builder().url("/x").get()` etc.
 *  Detection: a call_expr whose name is one of the OkHttp verbs and
 *  whose receiver chain (eventually) contains `.url("…")`. */
function tryOkHttp(callNode: SyntaxNode, filePath: string): ClientCall[] {
  if (callNode.type !== 'method_invocation') return [];
  const name = callNode.childForFieldName?.('name')?.text ?? '';
  const verb = OKHTTP_VERBS.get(name);
  if (!verb) return [];
  // `.url("/x")` should appear in the receiver chain.
  let cur: SyntaxNode | null = callNode.childForFieldName?.('object') ?? null;
  let url: string | null = null;
  let urlArg: SyntaxNode | null = null;
  while (cur && cur.type === 'method_invocation') {
    const callName = cur.childForFieldName?.('name')?.text ?? '';
    if (callName === 'url') {
      const a = cur.childForFieldName?.('arguments');
      const aChildren = (a?.namedChildren ?? []).filter((c) => c.isNamed === true);
      if (aChildren.length > 0) {
        urlArg = aChildren[0];
        url = readJavaString(urlArg);
        if (url !== null) break;
        // Non-literal — keep the arg node for getter mining below.
        break;
      }
    }
    cur = cur.childForFieldName?.('object') ?? null;
  }
  const pendingLookups = url === null && urlArg ? collectJavaGetterLookups(urlArg) : [];
  if (url === null && pendingLookups.length === 0) return [];
  const enclosing = findEnclosingMethodInfo(callNode);
  return [
    {
      method: verb,
      pathLiteral: url,
      providerTag: null,
      callerSymbol: enclosing.symbol,
      callerReceiver: enclosing.receiver,
      filePath,
      framework: 'okhttp',
      lineNumber: callNode.startPosition?.row ?? 0,
      confidence: url ? 0.9 : 0.55,
      pendingGetterLookups: pendingLookups.length > 0 ? pendingLookups : undefined,
    },
  ];
}

/** Apache HttpClient-shape: `new HttpGet("/x")` / `new HttpPost("/x")`. */
function tryApacheHttp(callNode: SyntaxNode, filePath: string): ClientCall[] {
  if (callNode.type !== 'object_creation_expression') return [];
  const typeNode = callNode.childForFieldName?.('type');
  const className = typeNode?.text ?? null;
  if (!className) return [];
  // Strip any qualifier — `org.apache.http.client.methods.HttpGet`.
  const tail = className.split('.').pop() ?? className;
  const verb = APACHE_REQUEST_CLASSES.get(tail);
  if (!verb) return [];
  const args = callNode.childForFieldName?.('arguments');
  const argChildren = (args?.namedChildren ?? []).filter((c) => c.isNamed === true);
  if (argChildren.length === 0) return [];
  const urlArg = argChildren[0];
  const url = readJavaString(urlArg);
  const pendingLookups = url === null ? collectJavaGetterLookups(urlArg) : [];
  if (url === null && pendingLookups.length === 0) return [];
  const enclosing = findEnclosingMethodInfo(callNode);
  return [
    {
      method: verb,
      pathLiteral: url,
      providerTag: null,
      callerSymbol: enclosing.symbol,
      callerReceiver: enclosing.receiver,
      filePath,
      framework: 'apache.httpclient',
      lineNumber: callNode.startPosition?.row ?? 0,
      confidence: url ? 0.95 : 0.55,
      pendingGetterLookups: pendingLookups.length > 0 ? pendingLookups : undefined,
    },
  ];
}

/** java.net.http.HttpClient-shape:
 *    `HttpRequest.newBuilder(URI.create("/x"))` chain plus
 *    `.GET()` / `.POST(...)` / `.method("POST", ...)`.
 *
 *  We anchor on the verb call and recover the URL by walking down
 *  the receiver chain to a `URI.create("…")` or `.uri("…")` call. */
function tryJavaNetHttp(callNode: SyntaxNode, filePath: string): ClientCall[] {
  if (callNode.type !== 'method_invocation') return [];
  const name = callNode.childForFieldName?.('name')?.text ?? '';
  let httpMethod: HttpMethod | null = null;
  if (['GET', 'POST', 'PUT', 'DELETE', 'PATCH', 'HEAD'].includes(name)) {
    httpMethod = name as HttpMethod;
  } else if (name === 'method') {
    const a = callNode.childForFieldName?.('arguments');
    const aChildren = (a?.namedChildren ?? []).filter((c) => c.isNamed === true);
    if (aChildren.length > 0 && aChildren[0].type === 'string_literal') {
      httpMethod = normalizeHttpMethod(readJavaString(aChildren[0]));
    }
  }
  if (!httpMethod) return [];
  // Walk backwards through receiver chain to find a URI argument.
  const collectUriFromArgs = (args: SyntaxNode | null | undefined): string | null => {
    if (!args) return null;
    for (const child of (args.namedChildren ?? []).filter((c) => c.isNamed)) {
      if (child.type === 'string_literal') {
        const s = readJavaString(child);
        if (s !== null) return s;
      }
      if (child.type === 'method_invocation') {
        const callName = child.childForFieldName?.('name')?.text ?? '';
        if (callName === 'create') {
          const a = child.childForFieldName?.('arguments');
          const aChildren = (a?.namedChildren ?? []).filter((c) => c.isNamed === true);
          if (aChildren.length > 0) {
            const s = readJavaString(aChildren[0]);
            if (s !== null) return s;
          }
        }
      }
    }
    return null;
  };
  let cur: SyntaxNode | null = callNode.childForFieldName?.('object') ?? null;
  let url: string | null = null;
  let isJavaNet = false;
  while (cur && cur.type === 'method_invocation') {
    const callName = cur.childForFieldName?.('name')?.text ?? '';
    if (callName === 'newBuilder' || callName === 'uri') {
      isJavaNet = true;
      url = collectUriFromArgs(cur.childForFieldName?.('arguments'));
      if (url !== null) break;
    }
    cur = cur.childForFieldName?.('object') ?? null;
  }
  if (!isJavaNet || url === null) return [];
  const enclosing = findEnclosingMethodInfo(callNode);
  return [
    {
      method: httpMethod,
      pathLiteral: url,
      providerTag: null,
      callerSymbol: enclosing.symbol,
      callerReceiver: enclosing.receiver,
      filePath,
      framework: 'java.net.http',
      lineNumber: callNode.startPosition?.row ?? 0,
      confidence: 0.9,
    },
  ];
}

// ─────────────────────────────────────────────────────────────────
// Entry point
// ─────────────────────────────────────────────────────────────────

/** Generic Java API-endpoint extractor. Parameter `rootNode` is the
 *  tree-sitter root for a Java file. */
export function extractJavaApiEndpoints(
  rootNode: SyntaxNode,
  filePath: string,
): ExtractedApiEndpoints {
  const out: ExtractedApiEndpoints = { routes: [], clientCalls: [] };

  // Server-side & Feign declarations: walk every type declaration
  // and drill into its members.
  for (const node of walk(rootNode)) {
    if (
      node.type !== 'class_declaration' &&
      node.type !== 'interface_declaration' &&
      node.type !== 'record_declaration' &&
      node.type !== 'enum_declaration'
    ) {
      continue;
    }
    const classModifiers = node.childForFieldName?.('modifiers') ??
      node.namedChildren?.find((c) => c.type === 'modifiers') ??
      null;
    const className = node.childForFieldName?.('name')?.text ?? null;
    const body = node.childForFieldName?.('body') ??
      node.namedChildren?.find(
        (c) =>
          c.type === 'class_body' ||
          c.type === 'interface_body' ||
          c.type === 'record_body' ||
          c.type === 'enum_body',
      ) ??
      null;
    if (!body) continue;
    for (const member of body.namedChildren ?? []) {
      processTypeMember(classModifiers, className, member, filePath, out);
    }
  }

  // Pre-scan for RestTemplate-typed identifiers so callers like `rt`
  // are recognised even when the receiver name doesn't substring-match
  // "template" / "rest".
  const restTemplateVars = collectTypedVarsByName(rootNode, 'RestTemplate');

  // Client-side: walk every call / object_creation in the file.
  for (const node of walk(rootNode)) {
    if (node.type === 'method_invocation') {
      const matched = [
        ...tryRestTemplate(node, filePath, restTemplateVars),
        ...tryWebClient(node, filePath),
        ...tryOkHttp(node, filePath),
        ...tryJavaNetHttp(node, filePath),
      ];
      out.clientCalls.push(...matched);
    } else if (node.type === 'object_creation_expression') {
      out.clientCalls.push(...tryApacheHttp(node, filePath));
    }
  }

  return out;
}

/** Collect names of identifiers whose declared (or formal-parameter)
 *  type tail matches `typeName`. Cheap structural scan; works for
 *  `RestTemplate rt`, `final RestTemplate rt`, `WebClient client`. */
function collectTypedVarsByName(rootNode: SyntaxNode, typeName: string): Set<string> {
  const out = new Set<string>();
  const matchesType = (typeNode: SyntaxNode | null | undefined): boolean => {
    if (!typeNode) return false;
    if (typeNode.type === 'type_identifier') return typeNode.text === typeName;
    if (typeNode.type === 'scoped_type_identifier') {
      const tail = typeNode.namedChildren?.[typeNode.namedChildren.length - 1];
      return tail?.text === typeName;
    }
    if (typeNode.type === 'generic_type') {
      const inner = typeNode.namedChildren?.find(
        (c) => c.type === 'type_identifier' || c.type === 'scoped_type_identifier',
      );
      return matchesType(inner ?? null);
    }
    return false;
  };
  for (const node of walk(rootNode)) {
    if (node.type === 'formal_parameter') {
      const t = node.childForFieldName?.('type') ??
        node.namedChildren?.find(
          (c) =>
            c.type === 'type_identifier' ||
            c.type === 'scoped_type_identifier' ||
            c.type === 'generic_type',
        );
      const ident = node.childForFieldName?.('name') ??
        node.namedChildren?.find((c) => c.type === 'identifier');
      if (matchesType(t) && ident?.text) out.add(ident.text);
    } else if (node.type === 'local_variable_declaration') {
      const t = node.childForFieldName?.('type') ??
        node.namedChildren?.find(
          (c) =>
            c.type === 'type_identifier' ||
            c.type === 'scoped_type_identifier' ||
            c.type === 'generic_type',
        );
      if (!matchesType(t)) continue;
      for (const child of node.namedChildren ?? []) {
        if (child.type !== 'variable_declarator') continue;
        const name = child.childForFieldName?.('name') ??
          child.namedChildren?.find((c) => c.type === 'identifier');
        if (name?.text) out.add(name.text);
      }
    } else if (node.type === 'field_declaration') {
      const t = node.childForFieldName?.('type') ??
        node.namedChildren?.find(
          (c) =>
            c.type === 'type_identifier' ||
            c.type === 'scoped_type_identifier' ||
            c.type === 'generic_type',
        );
      if (!matchesType(t)) continue;
      for (const child of node.namedChildren ?? []) {
        if (child.type !== 'variable_declarator') continue;
        const name = child.childForFieldName?.('name') ??
          child.namedChildren?.find((c) => c.type === 'identifier');
        if (name?.text) out.add(name.text);
      }
    }
  }
  return out;
}
