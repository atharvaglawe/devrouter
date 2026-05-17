/**
 * Normalized API-endpoint types.
 *
 * Every per-language extractor (Go, Java, TS, Python, …) returns its
 * findings in these shapes so the downstream graph processor — Route
 * node creation, HANDLES_ROUTE / FETCHES edge emission, path-template
 * matching — is written exactly once.
 *
 * Intentionally narrower than the existing {@link ExtractedRoute} /
 * {@link ExtractedFetchCall} pair: we carry an explicit HTTP method,
 * an enclosing-symbol attribution so edges can be emitted at
 * function granularity, and a generic `framework` tag for diagnostics.
 *
 * `pathTemplate` is the route as registered (`/users/:id`,
 * `/users/{id}`); `pathLiteral` on a {@link ClientCall} is the
 * recovered URL string when one is statically discoverable. The
 * downstream matcher normalizes the two before comparing.
 *
 * `providerTag` is the language-/framework-agnostic config join key
 * for cases where the URL is loaded from configuration at runtime
 * (e.g. internal HTTP wrappers that call `GetClient("kosmos")`,
 * Spring `@FeignClient(name="…")`, and so on). The matcher uses it
 * as a fallback resolver when `pathLiteral` is unavailable.
 */

/** HTTP methods. `*` matches any method (Spring `@RequestMapping` w/o method). */
export type HttpMethod =
  | 'GET'
  | 'POST'
  | 'PUT'
  | 'DELETE'
  | 'PATCH'
  | 'HEAD'
  | 'OPTIONS'
  | 'TRACE'
  | 'CONNECT'
  | '*';

export const HTTP_METHODS: ReadonlySet<HttpMethod> = new Set<HttpMethod>([
  'GET',
  'POST',
  'PUT',
  'DELETE',
  'PATCH',
  'HEAD',
  'OPTIONS',
  'TRACE',
  'CONNECT',
  '*',
]);

/** Server-side route registration recovered from source.
 *
 *  Emitted by per-language extractors when they recognise any of
 *  the registration forms documented in their module headers. */
export interface RouteRegistration {
  /** HTTP method, or `*` when the framework allows any method. */
  method: HttpMethod;
  /** Path template as written at the registration site. May still
   *  contain framework-specific parameter syntax — `:id`, `{id}`,
   *  `<id>`, `*` — which the downstream matcher normalises. */
  pathTemplate: string;
  /** Bare name of the handler function/method, when statically
   *  determinable (e.g. `GetCandidatesController`). When the handler
   *  is an inline closure or anonymous function, this is `null` and
   *  the downstream processor falls back to file-level attribution. */
  handlerSymbol: string | null;
  /** Receiver / owner of the handler method, when known
   *  (e.g. `controllers` from `controllers.GetCandidatesController`,
   *  or a Spring `@RestController` class name). */
  handlerReceiver: string | null;
  /** Source file containing the registration call. */
  filePath: string;
  /** Framework family this came from, language-prefixed
   *  (e.g. `go.stdlib`, `go.verb`, `go.tagged`, `spring.mvc`). Used
   *  for diagnostics and confidence scoring; the graph processor
   *  does not branch on it. */
  framework: string;
  /** 0-indexed line of the registration call. */
  lineNumber: number;
  /** Confidence in the extraction. 1.0 for unambiguous static
   *  patterns; lower when the handler is inferred or the path is
   *  recovered through a layer of indirection. */
  confidence: number;
}

/** Deferred provider-tag lookup recorded by an extractor when the
 *  call site's URL/path/host field is non-literal — e.g. the value
 *  is `config.GetXyzApiConfig().ApiPath`.
 *
 *  Phase 3.4 (`config-tag-resolver`) folds every Go file's struct
 *  tags + trivial getters into a getter-name → tag-set map, then
 *  Phase 3.5 backfills `providerTag` on the parent {@link ClientCall}
 *  by looking up these chains and picking the first tag that the
 *  provider-resolver index already knows about (i.e. has a
 *  `serviceDir`). */
export interface PendingGetterLookup {
  /** Receiver type when the call is `recv.Method()` and `recv`'s
   *  type is statically known (e.g. via field-decl or TypeEnv);
   *  `null` for free-function calls or when the receiver type isn't
   *  resolvable at extraction time. The resolver always falls back
   *  to `("*", name)` after trying `(receiver, name)`. */
  receiver: string | null;
  /** Function or method name (e.g. `"GetABTestApiConfig"`). */
  name: string;
}

/** Outbound HTTP / RPC call recovered from source.
 *
 *  Emitted alongside {@link RouteRegistration} by the same extractors
 *  so the matcher can consume both in one pass. */
export interface ClientCall {
  /** HTTP method when statically determinable. `null` when the
   *  caller defers method to a config struct field. */
  method: HttpMethod | null;
  /** Recovered URL or path literal. `null` when the URL is built
   *  dynamically and the caller must rely on `providerTag`. */
  pathLiteral: string | null;
  /** Config join key (`httpclient.GetClient("kosmos")`,
   *  `@FeignClient(name="kosmos")`, etc.). Populated as a fallback
   *  when `pathLiteral` cannot be statically recovered. */
  providerTag: string | null;
  /** Bare name of the enclosing function/method making the call,
   *  when statically determinable. */
  callerSymbol: string | null;
  /** Receiver / owner type of the caller method, when known. */
  callerReceiver: string | null;
  /** File containing the call site. */
  filePath: string;
  /** Framework / wire family (`go.stdlib`, `go.options`, `axios`, …). */
  framework: string;
  /** 0-indexed line of the call site. */
  lineNumber: number;
  /** Confidence in the extraction. */
  confidence: number;
  /** Getter chains the extractor saw inside this call site's
   *  options-bag URL/path/host fields when the RHS was a non-literal
   *  expression containing a function/method call. Phase 3.4
   *  resolves these against struct tags to backfill
   *  {@link providerTag}. Empty / undefined when the URL is a literal
   *  or when no recognisable getter is present. */
  pendingGetterLookups?: PendingGetterLookup[];
}

export interface ExtractedApiEndpoints {
  routes: RouteRegistration[];
  clientCalls: ClientCall[];
}

/** Empty result, useful as a default. */
export const EMPTY_API_ENDPOINTS: ExtractedApiEndpoints = Object.freeze({
  routes: [],
  clientCalls: [],
});

/** Normalise an HTTP method string regardless of case / form.
 *  Returns `null` for inputs that don't look like an HTTP method
 *  (so callers can decide between skipping or stamping `*`). */
export function normalizeHttpMethod(raw: string | null | undefined): HttpMethod | null {
  if (!raw) return null;
  const u = raw.toUpperCase().replace(/^METHOD/, '');
  if (HTTP_METHODS.has(u as HttpMethod)) return u as HttpMethod;
  return null;
}

/** Heuristic — receivers whose `.Get(`/`.Post(` calls are HTTP
 *  client invocations rather than route registrations. Shared
 *  between language extractors that have to disambiguate
 *  verb-as-method registrations from verb-as-method client calls
 *  (Express `app.get('/x', h)` vs. axios `client.get('/x')`). */
export const HTTP_CLIENT_RECEIVER_HINTS: ReadonlySet<string> = new Set([
  'axios',
  'fetch',
  'http',
  'https',
  'client',
  'httpclient',
  'request',
  'requests',
  'session',
  'rpc',
  'grpc',
]);
