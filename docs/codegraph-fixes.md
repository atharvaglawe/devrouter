# codegraph fixes — extractor + structural-edge work

> **Historical — describes the previous (GitNexus fork) engine.**
> DevRouter now vendors the MIT-licensed `colbymchenry/codegraph` engine
> (see [`codegraph/MIGRATION.md`](../codegraph/MIGRATION.md)); the custom
> extractor patches below were **not** ported — the MIT engine ships its
> own extraction/resolution pipeline. How the four strands map onto the
> new engine:
>
> 1. **API-endpoint / route extraction** — provided natively but
>    **framework-dependent** (the engine's resolvers detect Gin, Laravel,
>    etc.); generic in-house HTTP wrappers may not be recognised. Best
>    re-validated per repo.
> 2. **Provider / config-tag resolution** (runtime URL recovery) — **not**
>    carried over; the MIT engine has no equivalent config-tag matcher.
> 3. **Structural IMPLEMENTS for Go/TS** — **covered natively** (the
>    engine's `goImplementsEdges` synthesises Go implicit `implements` by
>    method-set matching; validated on real repos).
> 4. **Go switch-case constant ACCESSES edges** — call edges inside
>    switch/type-switch branches are captured; the dedicated *constant*
>    ACCESSES edge from a `case` arm is **not** specifically reproduced.
>
> The rest of this file is retained for historical context only.

This doc summarises the codegraph improvements that landed in the
working tree on top of the initial commit. Four independent but
related strands:

1. **Generic API-endpoint extraction** for Go / Java / Python — every
   recognisable HTTP / RPC idiom emits a normalised `RouteRegistration`
   (server) or `ClientCall` (client) regardless of repo or framework.
2. **Provider / config-tag resolution** — recovers a URL when the
   path is loaded from configuration at runtime (e.g.
   `httpclient.GetClient("kosmos")`, `@FeignClient(name="kosmos")`,
   `config.GetABTestApiConfig().ApiPath`).
3. **Structural implements processor** — emits IMPLEMENTS edges for
   languages where tree-sitter's heritage capture doesn't (Go has no
   `implements` keyword; TypeScript misses duck-typed satisfaction).
4. **Go switch-case constant references** — emits ACCESSES edges from
   a switch's enclosing function to every constant named in a `case`
   arm, so dispatch sites are visible to "who reads this symbol?"
   queries.

For the higher-level picture of what codegraph stores and exposes,
see [`codegraph.md`](codegraph.md). For the retrieval-shaping rules
that consume these edges, see [`codegraph-heuristics.md`](codegraph-heuristics.md).

## Why this work was needed

Before these fixes:

- **Routes were missed** whenever a framework wasn't in the small
  hardcoded allow-list. Every internal HTTP wrapper, custom mux, or
  in-house option-struct convention produced zero `Route` /
  `HANDLES_ROUTE` edges.
- **Cross-service call chains were broken** for any service whose
  client URL was loaded from YAML / JSON / properties / .env at
  runtime. The static analyser only saw a string tag (`"kosmos"`)
  and could not match it to the receiving service.
- **IMPLEMENTS edges were absent for Go** entirely, and incomplete
  for TypeScript (`const x: Y = { … }`, `interface Foo` aliases,
  duck-typed assignments). The graph reported "no implementers"
  for interfaces that had several at runtime.
- **Go switch-case constants were invisible to the graph.** A `switch`
  that dispatched on `shortnames.ZeroClickAffinityProvider` produced
  no edge from the enclosing function to the constant, so the
  dispatch site never showed up as a reader of the constant — even
  though it's typically the most important consumer.

The retrieval pipeline relies on these edges for trace / explore
intents — so the gaps showed up directly as missing call chains and
empty `extends` / `implementers` / `accessed-by` buckets in
`dev_context` responses.

## 1. API-endpoint extractors

### Shared types

`codegraph/src/core/ingestion/route-extractors/api-endpoint-types.ts`
defines the cross-language vocabulary:

- `HttpMethod` — `GET | POST | PUT | DELETE | PATCH | HEAD | OPTIONS | TRACE | CONNECT | *`. The `*` matches "any method" (Spring `@RequestMapping` without a `method=` clause; Django; Tornado).
- `RouteRegistration` — server-side: `{ method, pathTemplate, framework, handlerSymbol, sourceLocation }`. The `pathTemplate` is the route as registered (`/users/:id`, `/users/{id}`); the matcher normalises styles before comparing.
- `ClientCall` — client-side: `{ method, pathLiteral?, providerTag?, framework, callerSymbol, sourceLocation }`. `pathLiteral` is the recovered URL string when statically discoverable; `providerTag` is the language-/framework-agnostic config join key when it isn't.

Every per-language extractor returns these shapes. The downstream
graph processor — `Route` node creation, `HANDLES_ROUTE` /
`FETCHES` edge emission, path-template matching — is written exactly
once, in `call-processor.ts`.

### Go (`api-endpoint-go.ts`)

Detection is by AST shape, not symbol name. Anything that follows a
recognised Go idiom is captured without hardcoding the package.

**Server-side forms:**

| Form | Captures | Example shape |
|------|----------|---------------|
| stdlib `Handle` / `HandleFunc` | `net/http`, `*http.ServeMux`, any custom mux on the same surface | `<recv>.HandleFunc(path, h)` |
| Verb-as-method | gin, echo, chi, fiber, gorilla/mux, anything in this convention | `<recv>.<HTTP-VERB>(path, ...handlers)` |
| Tagged register | chi `r.Method`, gorilla `r.HandleFunc(...).Methods(...)`, any internal router | `<recv>.<RegName>(method, path, handler)` where `RegName ∈ {Handle, AddRoute, Register, Route, Method, Any}` |

**Client-side forms:**

| Form | Captures |
|------|----------|
| stdlib verbs | `http.Get/Post/PostForm/Head(url, …)` |
| Verb-as-method on a client | `<recv>.Get/Post/Do(url, …)` when receiver name signals HTTP-client usage |
| Request builder | `http.NewRequest(method, url, …)` — method+URL pair captured at the builder; the subsequent `client.Do(req)` joins via shared variable name |
| Options-bag literal | Any internal HTTP wrapper using `<Type>{Url|URL|Endpoint|Path: <str>, Method: <expr>, …}` |
| gRPC stub | `pb.New<X>Client(conn).<Method>(ctx, req)` |

HTTP method resolution accepts a literal `"POST"`, an `http.MethodPost`
selector, and a bare `MethodPost` identifier — so callers can stamp
constants without us caring how they spelled it.

A diagnostic probe lives at `codegraph/scripts/probe-go-api-extractor.ts`
for ad-hoc inspection of what the extractor sees on a given file.

### Java (`api-endpoint-java.ts`)

Detection is by AST shape + annotation name, never by package or
class name.

**Server-side:**

- **Spring MVC verb shortcuts** — `@GetMapping("/x")`, `@PostMapping(value="/x")`, `@PutMapping`, `@DeleteMapping`, `@PatchMapping`. Class-level `@RequestMapping("/prefix")` (or any verb shortcut) contributes a path prefix.
- **Spring `@RequestMapping`** — `@RequestMapping(value="/x", method=RequestMethod.POST)` accepts a single `RequestMethod.X`, a bare `X`, an array `{POST, PUT}`, or omitted (in which case `*`).
- **JAX-RS** — `@Path("/users")` on the class plus `@GET` / `@POST` / … (with optional `@Path("/{id}")`) on each method. Catches Jersey, RESTEasy, CXF.
- **Spring `@FeignClient`** — interfaces annotated with `@FeignClient(name="kosmos", url="https://…")`. Each interface method's `@GetMapping` / `@RequestMapping` emits a `ClientCall` with the provider tag captured for join-by-name.

**Client-side:**

- **RestTemplate** — `rt.getForObject(url, …)`, `rt.postForEntity(url, …)`, etc.
- **WebClient** — fluent `.get().uri(url)…retrieve()` / `.exchangeToMono(…)`.
- **OkHttp** — `Request.Builder().url(...).get()/post(body)…`.

### Python (`api-endpoint-python.ts`)

Detection is by AST shape and well-known framework idioms.

**Server-side:**

- **FastAPI / APIRouter** — `@app.get("/x")`, `@router.post("/x", …)`. Handles `APIRouter(prefix="/api/v1")` plus `app.include_router(router, prefix="/y")` for prefix composition.
- **Flask / Blueprint** — `@app.route("/x", methods=["POST", "GET"])`, `@bp.route("/x")`. Blueprint prefix from `Blueprint("name", __name__, url_prefix="/v2")`. `@app.get("/x")` (Flask 2.x) also handled.
- **Django** — `urlpatterns = [path("hello/", view), re_path(r"^x$", v), …]`. Method is `*` (Django dispatches by view).
- **aiohttp** — `app.router.add_get("/x", handler)`, `app.router.add_post("/x", handler)`, `RouteTableDef` decorator form `@routes.get("/x")`.
- **Tornado** — `tornado.web.Application([(r"/x", Handler), …])`, `URLSpec(r"/x", Handler)`. Method is `*` (handler dispatches on `get/post/…`).

**Client-side:**

- `requests` — `requests.get/post/…(url, …)`.
- `httpx` — both sync `httpx.get(url)` and async `client.get(url)`.
- `aiohttp.ClientSession` — `session.get(url, …)` / `session.post(...)`.

## 2. Provider-tag and config-tag resolution

### Provider-tag resolver (`route-extractors/provider-resolver.ts`)

When an internal HTTP wrapper defers the path to runtime config —
`httpclient.GetClient("kosmos")`, `@FeignClient(name="kosmos")`,
etc. — we statically see only the tag. The resolver scans the
common config-file shapes (YAML / JSON / properties / .env) plus the
repo's directory layout and produces, per tag:

- `hosts` — bare hostnames (`kosmos.internal`)
- `urls` — full base URLs (`https://kosmos.internal/api`)
- `serviceDirs` — directory hints (`services/kosmos`, `cmd/kosmos`) signalling "files under this prefix belong to provider `kosmos`"

Tag extraction is intentionally repo-agnostic: it pulls candidate
tags from the *key path* of a leaf URL value (last 1–2 segments
before a `url` / `host` / `baseUrl` field). So a YAML
`services: { kosmos: { url: "..." } }`, a properties line
`kosmos.url=…`, and an env var `KOSMOS_URL=…` all collapse to the
same tag `kosmos`. Casing is normalised to lower-case.

Downstream a `ClientCall.providerTag`-only call joins via either:

1. a `RouteRegistration` whose `filePath` lives under one of the
   resolved `serviceDirs`, or
2. a `Route` node whose URL host matches a resolved `hosts` entry
   (when the indexer ever stamps host on Route nodes).

### Config-tag resolver (`route-extractors/config-tag-resolver.ts`)

Bridges the gap when the URL is bound through a Go getter chain
like `config.GetABTestApiConfig().ApiPath`, where:

1. `GetABTestApiConfig()` is a trivial getter returning a field,
2. that field has a struct tag `yaml:"abtestapi"`,
3. `abtestapi` is the YAML key the provider-resolver indexed.

Three pieces:

| Function | Pass | What it produces |
|----------|------|------------------|
| `extractGoConfigTags` | per-file AST | `{owner, field, tags}` for every `field_declaration` with a struct tag |
| `extractGoTrivialGetters` | per-file AST | `{name, receiverType, returnAliases}` for every function whose body is nothing but `return …` (any number of branches) |
| `buildResolvedGetters` | repo-wide | Folds the per-file outputs into `(receiver|"*"::name) → Set<tag>` by chasing alias chains through the field-tag index, with a cycle guard |

Generic across `yaml:`, `json:`, `mapstructure:`, `env:`. Returns
*raw* tag values; the call-site consumer cross-checks them against
the YAML-derived provider-resolver index.

### Java / Python config-tag adapters

`config-tag-java.ts` and `config-tag-python.ts` collect the same
shape of "this symbol resolves to this provider tag" facts from
language-specific idioms:

- Java: `@Value("${kosmos.url}")` injection sites, `@ConfigurationProperties(prefix="kosmos")` POJOs, Spring `Environment.getProperty("kosmos.url")` calls.
- Python: `os.environ["KOSMOS_URL"]`, `settings.KOSMOS_URL` dataclass attributes, `pydantic.BaseSettings` with `env_prefix="KOSMOS_"`.

Both feed into the same downstream join surface as the Go resolver.

## 3. Structural implements processor

`codegraph/src/core/ingestion/structural-implements-processor.ts`
closes the IMPLEMENTS-edge gap for languages where the tree-sitter
`implements` capture is incomplete or absent:

- **Go** — heritage processor never emits IMPLEMENTS (no keyword).
- **TypeScript** — heritage processor only sees `class X implements Y { … }`, missing `const x: Y = { … }`, `interface Foo extends Bar`, and assignment-style satisfaction.
- **Python / Ruby protocols** — duck-typed `include` of method modules, structural protocol matches.

### Algorithm

1. Index `Interface → required-method-name set`. Skip 0-method
   interfaces (they would match every concrete type and add only noise).
2. Index `concrete-type (Class | Struct | Trait | Record) → owned-method-name set`.
3. For each concrete type, gather candidate interfaces via an
   **inverted method-name index** (avoids the O(types × interfaces)
   cross-product that an exhaustive scan would do).
4. For each (concrete, interface) candidate, accept the match when
   every interface method has a same-name method on the concrete type
   with a compatible arity. Emit:
   - `IMPLEMENTS: concrete → interface`
   - `METHOD_IMPLEMENTS: concreteMethod → interfaceMethod` — only when the mapping is unambiguous (exactly one concrete method matches).

### Idempotency and confidence

- Existing IMPLEMENTS / METHOD_IMPLEMENTS edges (from the heritage
  processor or a prior pass of this one) are deduped using the same
  `generateId(...)` scheme used everywhere else in ingestion.
- Confidence scale on emitted edges:
  - **0.85** — name + arity match on every interface method.
  - **0.70** — name-only fallback when both sides report unknown arity (e.g. interface methods extracted without parameter info).

End-to-end coverage in `codegraph/test/integration/structural-implements-e2e.test.ts`.

## 4. Go switch-case constant references

A pre-existing gap in the Go ingestion path: a `switch` arm that
referenced a named constant produced no graph edge from the
enclosing function to that constant. So a typical Go dispatch
site like:

```go
func (r *Resolver) Resolve(p Provider) {
    switch p {
    case shortnames.ZeroClickAffinityProvider:
        r.handleZeroClick()
    case shortnames.MapsAffinityProvider:
        r.handleMaps()
    }
}
```

would emit `CALLS` edges for `r.handleZeroClick()` /
`r.handleMaps()`, but the constants `ZeroClickAffinityProvider` and
`MapsAffinityProvider` had **zero inbound edges from `Resolve`**.
"Who reads this constant?" queries (the bread-and-butter of
trace-intent retrieval) missed the dispatch — typically the most
informative reader.

### What the fix does

For Go files, both `case <pkg>.<Const>:` (qualified) and
`case <Const>:` (bare) forms now emit an `ACCESSES` edge from the
enclosing function (or the file node when the switch is top-level)
to the constant's symbol, with `reason: 'switch-case'` and
confidence `1.0`.

Three pieces, mirroring the call/assignment pipeline:

| Stage | File | What it does |
|-------|------|--------------|
| Tree-sitter query | `tree-sitter-queries.ts` (Go block, lines 474-484) | Two captures on `expression_case` — qualified `selector_expression` (`@switch_case.receiver` + `@switch_case.name`) and bare `identifier` (`@switch_case.name`). |
| Per-file extraction | `workers/parse-worker.ts` (`ExtractedSwitchCase` interface and the capture-handler around line 1572) | Walks the captures, finds the enclosing function via `findEnclosingFunctionId`, and emits `{filePath, sourceId, constantName, receiverName?}`. |
| Cross-file resolution | `call-processor.ts` (`processSwitchCasesFromExtracted`, lines 2901-2939) | Resolves `constantName` via the same `ResolutionContext` used for calls/assignments. Tries qualified resolution first when `receiverName` is present (treats it as an import alias); falls back to bare resolution. Emits `ACCESSES` with `reason: 'switch-case'`. |

`pipeline.ts` collects per-chunk `switchCases` into a deferred
buffer and runs the resolution pass once symbol resolution is
complete (line 1085-1086), so cross-package targets are reachable.

### Scope

Go-only today — `expression_case` is the Go tree-sitter grammar's
switch-arm node. Java / C# already had switch-case type-narrowing
support in their resolver tests; the missing piece was constant-
reference edges for Go's enum-like patterns.

The same machinery (`ExtractedSwitchCase` + the resolver) is
language-agnostic — adding Java `SwitchExpression` / `SwitchLabel`
and C# `SwitchSection` captures to the respective tree-sitter
query blocks is a straightforward extension if a future repo needs
it.

## 5. Pipeline integration

The extractors plug into the existing ingestion stages:

| Stage | File | What changed |
|-------|------|--------------|
| Per-file parse | `parsing-processor.ts` (+159 lines) | Calls the per-language API-endpoint extractor, config-tag extractor, and the Go switch-case extractor in the same pass that already produced symbols; attaches results (`apiEndpoints`, `configTags`, `switchCases`) to the parsed-file payload. |
| Worker | `workers/parse-worker.ts` (+112 lines) | Carries the new payload fields across the worker-pool boundary, including the `ExtractedSwitchCase` interface and the merge helpers in `mergeWorkerData`. |
| Cross-file resolution | `pipeline.ts` (+327 lines) | Adds repo-wide passes that run after symbol resolution: (a) build the provider-tag index from config files + dir layout; (b) build the resolved-getters index from per-file config-tag outputs; (c) run `processSwitchCasesFromExtracted` over the deferred switch-case buffer. |
| Edge emission | `call-processor.ts` (+233 lines) | Promotes `RouteRegistration` → `Route` nodes + `HANDLES_ROUTE` edges; promotes `ClientCall` → `FETCHES` edges, joining by `pathLiteral` first, falling back through `providerTag` → `serviceDirs` / `hosts`; emits `ACCESSES` edges (reason `switch-case`) for Go switch-case constants. |
| Structural implements | `pipeline.ts` | Runs `structural-implements-processor` after the heritage processor as a backstop pass. |
| Tree-sitter queries | `tree-sitter-queries.ts` (+17 lines) | New captures needed by the extractors (decorator targets, struct-tag bodies, and Go `expression_case` constants — both qualified and bare). |
| Per-language adapters | `languages/{csharp,java,kotlin,php}.ts` (+3-4 lines each) | Wire-up to surface the new captures. |
| Shared graph types | `_shared/graph/types.ts` (+14 lines) | New edge kinds (`HANDLES_ROUTE`, `FETCHES`, `METHOD_IMPLEMENTS`) and node-property fields (`pathTemplate`, `providerTag`, etc.). |

## 6. Test coverage

| Component | Tests |
|-----------|-------|
| API endpoint — Go | `test/unit/api-endpoint-go.test.ts`, `test/unit/api-endpoint-go-pending-getter.test.ts` |
| API endpoint — Java | `test/unit/api-endpoint-java.test.ts`, `test/unit/api-endpoint-java-pending-getter.test.ts` |
| API endpoint — Python | `test/unit/api-endpoint-python.test.ts`, `test/unit/api-endpoint-python-pending-getter.test.ts` |
| Config tag — Java / Python / Go | `test/unit/config-tag-java.test.ts`, `test/unit/config-tag-python.test.ts`, `test/unit/config-tag-resolver.test.ts` |
| Provider resolver | `test/unit/provider-resolver.test.ts` |
| Structural implements | `test/unit/structural-implements-processor.test.ts` (algorithm), `test/integration/structural-implements-e2e.test.ts` (full pipeline) |
| Go switch-case constants | Covered end-to-end via the `call-processor.ts` resolver path; regression coverage lives in `test/unit/call-processor.test.ts` (+63 lines, new edge kinds) and is exercised in any repo-level fixture that contains a Go `switch` over named constants. |
| Call processor regressions | `test/unit/call-processor.test.ts` (existing, +63 lines for new edge kinds) |

Fixture corpora:

- `test/fixtures/api-endpoints/` — minimal repos exercising every framework idiom listed above for each language.
- `test/fixtures/structural-implements/` — Go and TypeScript scenarios that the heritage processor doesn't cover, plus negative cases (unrelated method-set overlap).

## How to verify on a real repo

```bash
# Re-index with the new extractors active.
./devrouter analyze --embeddings --force /abs/path/to/repo

# Spot-check route extraction for a known endpoint.
curl -s 'http://localhost:4747/api/query' \
  -H 'content-type: application/json' \
  -d '{"repo":"<name>","cypher":"MATCH (r:Route) RETURN r.path, r.method, r.framework LIMIT 20"}' | jq

# Spot-check IMPLEMENTS edges that the heritage processor missed.
curl -s 'http://localhost:4747/api/query' \
  -H 'content-type: application/json' \
  -d '{"repo":"<name>","cypher":"MATCH (c)-[r:IMPLEMENTS {confidence: 0.85}]->(i:Interface) RETURN c.name, i.name LIMIT 20"}' | jq

# Spot-check provider-tag resolution.
curl -s 'http://localhost:4747/api/query' \
  -H 'content-type: application/json' \
  -d '{"repo":"<name>","cypher":"MATCH ()-[f:FETCHES]->(r:Route) WHERE f.providerTag IS NOT NULL RETURN f.providerTag, r.path LIMIT 20"}' | jq

# Spot-check Go switch-case ACCESSES edges (the function on the
# left should be the dispatch site; the constant on the right
# should be the case-arm value).
curl -s 'http://localhost:4747/api/query' \
  -H 'content-type: application/json' \
  -d '{"repo":"<name>","cypher":"MATCH (fn)-[a:ACCESSES {reason: \"switch-case\"}]->(c) RETURN fn.name, c.name LIMIT 20"}' | jq
```

If any of the above return zero rows on a repo where you'd expect
matches, run the per-extractor probe (`scripts/probe-go-api-extractor.ts`
for Go; the unit-test fixtures are the equivalent for the other
languages) and compare what the AST visitor sees against the failing
file.
