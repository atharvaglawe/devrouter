# Retrieval rules

How devrouter turns a free-form developer query into a ranked response.
This file is the canonical reference. Read it top-to-bottom — each
stage feeds the next.

The reference implementation is `Router.HandleQuery` in
[`internal/router/router.go`](../internal/router/router.go). Source
pointers below name the file and function so they survive line-number
churn.

## Pipeline overview

```
                          query
                            │
   Section 1    tokenize            stoplist + light stemming
                            │
   Section 2    detect path         explicit file/package short-circuit
                            │
   Section 3    plan                agent-supplied, with "rarest token" auto-anchor fallback
                            │
   Section 4    intent              keyword-only, no LLM
                            │
   Section 5    code search         cypher cascade, IDF-scored
                            │
   Section 6    memory recall       KNN → 4-stage relevance gate
                            │       (floor / must / FP / rerank)
   Section 7    graph expansion     per-intent budget
                            │
   Section 8    trim & assemble     per-intent caps, memory shrink
                            │
   Section 11   honest signals      cosine-derived confidence
                            │
                        response  ──►  agent acts
                                            │
                                       dev_feedback
                                            │
                                            ▼
   Section 9   self-tuning  ◄── reward ──   Section 10  per-memory FP learning
   (heuristics.md)                          (suppresses wrong memories)
```

---

## Section 1 — Tokenize the query

Source: `codegraph.SplitQueryWords`
([`internal/codegraph/client.go`](../internal/codegraph/client.go)).

1. Split on whitespace; strip surrounding punctuation `.,;:!?"'()[]{}`.
2. Lowercase.
3. Drop tokens with `len < 2`, or in the stoplist (`the`, `and`, `of`,
   `how`, `why`, …).
4. Stem if `len > 6`: trim one of `ings | ing | ers | ed | es | s`. If
   the result still ends in `ll`, drop one `l` so British
   "unmarshalling" → "unmarshall" → "unmarshal".
5. Dedupe.

The `len < 2` floor (rather than the historical `len < 5/6`) is
deliberate: domain abbreviations like `fms`, `kbb`, `rs4c` were the
single biggest source of missed retrieval.

When the caller supplies a `plan` (Section 3), its `MustTerms` and
`ShouldTerms` are appended to the raw query string before tokenization
(`buildEffectiveQuery` in
[`internal/router/router.go`](../internal/router/router.go)). Hybrid
`/api/search` sees the raw + extras; everything downstream sees the
tokenized union.

---

## Section 2 — Detect file or package paths

Highest-precision signal available without touching the graph. Runs
before search.

| Pattern | Treated as | Effect |
|---------|-----------|--------|
| Token with **both** `/` and `.` (e.g. `kosmos/matchengine/rule.go`) | Explicit file path | `SearchByFilePath` runs first and short-circuits all other text search |
| Token with `/` but **no** `.` (e.g. `cmpkg/abtestv2`) | Package path | Seeds an early `SearchByFilePath` and re-ranks results via `boostByPath` |

`SearchByFilePath` itself
([`internal/codegraph/client.go`](../internal/codegraph/client.go))
tries three matches, stops at the first non-empty:

1. `n.filePath = "<path>"` — exact.
2. `n.filePath ENDS WITH "<path>"` — handles partial paths.
3. `n.filePath CONTAINS "<path>"` — last-resort substring.

---

## Section 3 — Plan the query

Source:
[`internal/router/planner.go`](../internal/router/planner.go),
`Router.HandleQueryWithPlan`.

The router accepts an optional structured `QueryPlan` from the MCP
caller via `dev_context`'s `plan` argument. Plans give the
deterministic retrieval rules in Section 5 and Section 6 better inputs
(synonyms, exclude conventions, path bias) without changing their
logic. The plan is **always optional** — when omitted, the auto-anchor
fallback (below) takes over and the system still works.

There is **no in-process planner LLM**. The agent (Claude/Cursor/etc.)
is responsible for producing the plan; devrouter only sanitizes and
applies it.

### `QueryPlan` schema

```go
type QueryPlan struct {
    MustTerms    []string  // file-level filter (Section 5, Section 6.2)
    ShouldTerms  []string  // synonym/expansion vocab (Section 1, Section 5)
    ExcludeTerms []string  // conventional drop rules (Section 5, Section 6.2)
    Phrases      []string  // multi-word strings; logged, reserved
    ContextHints []string  // soft path bias (Section 5)
}
```

On the MCP wire the field is `plan` on the `dev_context` /
`retrieve_debug` tool arguments. See
[`agent-rules.md`](agent-rules.md) for the full schema, semantics, and
worked examples agents should use.

### Sanitization caps (server-side, non-negotiable)

`SanitizePlan` (`internal/router/planner.go`) runs on every supplied
plan before it touches retrieval:

- Lowercases, trims, dedupes every entry.
- Drops empty / overly long tokens, strips multi-word strings out of
  `*Terms` slots (keeps them in `Phrases`), only allows `/` in
  `ContextHints` (so package paths like `gobackend/fms` pass through).
- Enforces hard per-field caps: `must_terms ≤ 2`, `should_terms ≤ 6`,
  `exclude_terms ≤ 3`, `phrases ≤ 3`, `context_hints ≤ 3`. These caps
  are advertised in the MCP tool schema, but server-side enforcement
  means a frontier model that over-produces can't bypass them.
- Too many must terms collapse recall (the file-level filter
  intersects across them) — capping at 2 is what keeps Section 5/6
  scoring stable.
- `exclude_terms` are conventional, not literal. `"test"` → applies
  the targeted rules in Section 5 (`_test.go`, `Test*` prefix), so
  `requestSettings` is safe.

### Auto-anchor fallback

`ensureMustAnchor` in
[`internal/router/router.go`](../internal/router/router.go) — when
`MustTerms` is empty for any reason (no plan supplied, agent supplied
an empty plan, or the agent's must_terms got sanitized away), the
rarest non-stopword query token is promoted to `MustTerms`. "Rarest"
= lowest non-zero `count(n) WHERE name CONTAINS` — one cypher per
token via `Client.NameHitCount`. Guarantees a hard anchor with zero
LLM involvement.

Logged as `[router] auto-anchored must="fms" (hits=64)`. Surfaces in
`PlanDebug.AutoAnchored = true` and as `plan.source = "auto"` in
`retrieve_debug`.

### Where each plan field flows

| Field | Used by | How |
|-------|---------|-----|
| `MustTerms` | Section 5 (filter+score), Section 6.2 (memory must-filter) | File-level filter on symbol search; structural-only filter on memory text |
| `ShouldTerms` | Section 1 → Section 5 | Folded into `effectiveQuery`, tokenized with stoplist+stem, then per-term name CONTAINS + IDF |
| `ExcludeTerms` | Section 5, Section 6.2 | `shouldExclude` drop pass on symbols; conventional path/name patterns on memories |
| `ContextHints` | Section 5 | Score multiplier (1×/2×/3×) by filePath substring |
| `Phrases` | (logged only) | Reserved for future content-scoring |

### Settings

No env vars. Plan provenance is purely on the wire — the caller either
sends a `plan` object or it doesn't.

---

## Section 4 — Intent classification (keyword-only)

Source: `DetectIntent`
([`internal/router/intent.go`](../internal/router/intent.go)).

Match the lowercased query against curated keyword lists per intent
(`debug` / `trace` / `refactor` / `explore`). On match, return
immediately (microseconds). On no match, return `IntentGeneral` and
the rest of the pipeline runs with the general-intent profile.

There is **no LLM fallback**. An earlier version routed keyword-misses
to a 5-second sidecar LLM call; the call paid its full timeout budget
on ~5–10% of queries while contributing little signal that the
downstream profile (graph budget + trim caps) actually needed.
`general` defaults are reasonable when intent is genuinely ambiguous.

The intent picks one of five `Profile`s in Section 9. If you want to
override how a given intent shapes the response, change the profile —
not the classifier.

---

## Section 5 — Code search cascade

Each step runs only if the previous one returned zero results.

| # | Step | Source | What it does |
|---|------|--------|--------------|
| 1 | File-path search | Section 2 | Hard-precision short circuit |
| 2 | Package-path search | Section 2 | Same, looser |
| 3 | Hybrid `/api/search` | `Client.Search` | codegraph FTS + embedding hybrid |
| 4 | **Cypher name search** | `SearchByNameWithOpts` | IDF-ranked, plan-aware — the heart of the pipeline |
| 5 | Path boost | `boostByPath` | Reorders results matching `pkgPath` |

Step 4 deserves its own breakdown.

### Inside `SearchByNameWithOpts`

Source:
[`internal/codegraph/client.go`](../internal/codegraph/client.go).

**Per-term name CONTAINS.** For every tokenized word:

```cypher
MATCH (n) WHERE toLower(n.name) CONTAINS "<word>" AND n.startLine IS NOT NULL
RETURN n.id, n.name, n.filePath, n.startLine, n.endLine, n.content
LIMIT <limit*3>
```

Each hit records its strongest matching surface (`name` > `filePath`
> `content`). Per-term hit counts accumulate for IDF.

**Content fallback for the rarest term.** If total hits across all
terms < `limit`, run **one** extra cypher on `n.content CONTAINS
"<rarest>"` (lowest hit count seen so far). Only the rarest, to keep
it cheap — common terms like `error` would pull in thousands of
irrelevant nodes.

**Exclude rules** (`ExcludeTerms`). For every exclude term, drop a
result if any of these hold:

- `filePath` ends with `_<term>.go` (catches `_test.go`, `_mock.go`).
- `filePath` contains `/<term>/` or `/<term>s/` (catches `/test/`,
  `/mocks/`).
- `name` has title-case prefix `<Term>`: lowercased name starts with
  the term AND the next character is uppercase or end-of-string
  (catches `TestFoo`, `BenchmarkBar`, `MockClient` but **not**
  `requestSettings`, `latestVersion`).

Targeted on purpose — substring CONTAINS would over-match.

**Must-term anchor** (`MustTerms`). Build the union of files where
ANY must term hits in `name`, `filePath`, or `content`:

```cypher
MATCH (n)
WHERE (toLower(n.filePath) CONTAINS "<term>"
       OR toLower(n.name)    CONTAINS "<term>"
       OR toLower(n.content) CONTAINS "<term>")
  AND n.filePath IS NOT NULL
RETURN DISTINCT n.filePath LIMIT 500
```

Drop any result whose file isn't in that union. **Soft anchor, not
hard fail**: if the cypher errors or returns zero, the filter is
skipped entirely so a bad must term never blackholes the query.

**IDF + surface scoring**:

```
score(node) = Σ_t  termIDF(hits[t]) * surfaceWeight(matched_surface[t][node])
```

| Surface | Weight |
|---------|-------:|
| `name` | 1.0 |
| `filePath` | 0.7 |
| `content` | 0.4 |

```
termIDF(c) = min(8, log(1 + 1000 / (c+1)))
```

The `1000` is a notional corpus size — only the relative ordering
across terms matters. `fms` (~30 hits) ≈ 3.5; `error` (~10000 hits)
≈ 0.1. Rare anchors dominate, generic words round out the score
without drowning anything.

**Context-hint boost** (`ContextHints`). After IDF scoring, multiply
by:

| filePath substring matches | Multiplier |
|---:|---:|
| 0 | 1.0 |
| 1 | 2.0 |
| 2+ | 3.0 (capped) |

Soft bias only. A wrong hint can never demote results below 1.0 — the
filter side is exclude-only.

**Sort + take top N.** Standard descending sort by score. `limit`
(default 10) caps the returned slice.

---

## Section 6 — Memory retrieval: KNN + 4-stage relevance gate

Source: `Store.SearchAll`
([`internal/memory/store.go`](../internal/memory/store.go)) and the
memory pipeline in `Router.HandleQuery`.

Memory provides pre-digested insights ("FMS silently swallows
unmarshal errors") that often beat raw code at answering debug-style
questions. But pure embedding similarity surfaces semantically
adjacent yet topically wrong memories — e.g. a
`kosmos-rule-dnf-evaluation` memory ranks high for
`"fms unmarshalling error"`, or the regression case that motivated
this section: `seat-provider-mapping-resolution` ranking high for
`"error logging in tag-based clearing"` because its purpose mentioned
`"if FM value is incomplete or error"` in passing.

The pipeline is a four-stage funnel applied between `SearchAll` and
`buildAllMemories`:

```
KNN recall  →  6.1 floor  →  6.2 must-filter  →  6.3 FP demotion  →  6.4 should-rerank
```

Each stage is independent and individually loggable, so a regression
can be attributed to one stage without re-running the whole query.

### 6.0 KNN recall (high-recall, low-precision)

1. Embed the **raw** query string via `nomic-embed-text-v1.5` (768-dim).
2. Cosine search against the Redis-stored memory index, scoped
   optionally by repo, sliced by `mem_type`.
3. Return top-5 per type (file / func / flow / decision) → up to 20
   hits.

No keyword filter, no IDF, no surface weighting. Recall-maximising on
purpose. The cosine distance from `FT.SEARCH` is preserved on every
hit as `MemoryHit.Score` and is what every later stage relies on
being honest.

### 6.1 Cosine-distance floor

Source: `dropBelowFloor` + `memoryMaxDistance`.

`FT.SEARCH KNN` always returns top-K regardless of how weak the
matches are. Drop everything above a configurable distance ceiling.
For `nomic-embed-text-v1.5` the empirical bands are:

| Cosine distance | Interpretation |
|-----------------|----------------|
| < 0.20 | paraphrase / near-duplicate |
| 0.20 – 0.40 | same topic |
| 0.40 – 0.60 | weak topical relation |
| > 0.60 | incidental |

Default ceiling: `0.60` (= ≥ 0.40 cosine similarity). Override via
`DEVROUTER_MEMORY_MAX_DISTANCE`. Hits with `Score == 0` (lexical
hits, future graph-derived hits) bypass the floor.

Logged as `[router] memory floor dropped 8/20 hits (max_distance=0.60)`.

### 6.2 Must-term anchor on **structural** fields only

Source: `filterMemoriesByPlan` + `memoryStructuralText`.

Memory hits are split into two text views:

- **Structural** — `name`, `path`, `file`, `files`, `key_symbols`,
  `entry_points`, `scope`. Substring match here reliably signals
  "this memory is about this thing."
- **Free-text** — `purpose`, `rationale`, `decision`. Common
  technical words appear incidentally (`error`, `cache`, `update`).

Must terms match **structural only**. A memory whose only mention of
the must term lives in its prose `purpose` is dropped. This was the
seat-provider regression: `purpose: "if FM value is incomplete or
error"` no longer counts as an "error" anchor.

Empty `MustTerms` → no-op (the auto-anchor in Section 3 always runs
when no must terms are supplied, so this branch is rarely hit in
practice).

The conventional `ExcludeTerms` filter (`shouldExcludeMemory`) — same
list as before for `test` / `mock` / `fixture` — runs at the same
stage; it has always been structural-only and is unchanged.

Logged as `[router] memory plan-filter dropped 5/12 hits (must=[error] exclude=[])`.

### 6.3 FP demotion (memory-relevance learning)

Source: `applyFPPenalties`,
`Store.BatchFalsePositiveSimilarity`
([`internal/memory/falsepositives.go`](../internal/memory/falsepositives.go)).

For each surviving candidate, the router consults the false-positive
store (built from prior `dev_feedback` calls — see Section 10) and adds a
cosine-distance penalty when the current query embedding is close to
the memory's accumulated FP centroid:

```
penalty = 0.20 * min(1, count / 3) * sim   if sim ≥ 0.70
        = 0                                  otherwise
```

A memory that has been an FP three or more times for queries
near-identical to the current one (sim ≥ 0.70) gets penalised by the
full 0.20 — enough to push a borderline candidate (distance ≈ 0.45)
past the default floor of 0.60.

After demotion the floor (6.1) is **re-applied** so demoted hits
that crossed the threshold are removed entirely rather than just
re-ranked.

Logged per demoted hit:

```
[router] FP demote key=mem:goserving:flow:seat-provider-… sim=0.873 count=4 penalty=0.175 new_score=0.620
[router] memory FP demoted=2 post-demotion=3
```

### 6.4 Should-term re-rank

Source: `rankByPlan`.

Surviving hits are re-scored:

```
rank = cosine_similarity
     + 0.10 * #should_terms_in_structural
     + 0.05 * #should_terms_in_freetext
```

The structural bonus dominates: a memory that **names** the right
symbols outranks one that just happens to be embedding-adjacent.
Stable sort — equal ranks preserve `FT.SEARCH` order.

This is the only stage where free-text counts (and only as a
tiebreaker). The cosine score the planner is competing against
already came from an embedding of the prose, so we don't want to
over-weight the same signal twice.

### 6.5 Filter consequences are honest

The pipeline doesn't try to preserve `memory_coverage`. If a query
had 3 hits and 2 get filtered, the surviving count of 1 flows through
unchanged into:

- the graph-budget decision (Section 7) — loosens to compensate by pulling
  in more graph data (e.g. `imports=true`);
- `signals.memory_coverage` (Section 11) — accurately reports `partial`
  instead of overstating as `high`;
- `model_hint` — bumps `haiku` → `sonnet` because there's less
  verified context to lean on.

Better to use a stronger model with more graph data than a weaker
model anchored on a wrong memory.

---

## Section 7 — Graph expansion budget

Source: `graphBudgetFromMemory`.

The bandit in Section 9 owns these numbers now (each one is a tunable dial),
but the **shape** of the decision is:

> graph budget = f(memory strength, intent)

### Memory strength → narrow graph

| `memCount` | maxTrace | callerHops | callees | extends | methods | imports |
|---:|---:|---:|:---:|:---:|:---:|:---:|
| ≥ 3 | 2 | 1 | yes | no | no | no |
| ≥ 2 | 3 | 1 | yes | no | no | yes |
| < 2 | 5 | 2 | yes | yes | yes | yes |

Strong agent memory means we don't need to brute-force the graph.
`memCount` is **post-filter** (Section 6.1, Section 6.2 already pruned), so noisy
memories can't artificially shrink the budget.

### Intent overrides

| Intent | Override |
|--------|----------|
| `debug` | `callerHops=2`, `callees=true`, `maxTrace ≥ 4` |
| `trace` | `callerHops=2`, `callees=true`, `imports=true`, `maxTrace ≥ 4` |
| `refactor` | `imports=true`, `methods=true`, `extends=true` |
| `explore` | shrink `maxTrace=2` if memory ≥ 2 |

Debug and trace queries get a wider call-chain regardless of memory
strength because that's what they're asking for.

### Per-symbol traversal

For each of the top-`maxTrace` seed symbols (search results + auto
memory hints):

| Edge | Source | When |
|------|--------|------|
| `CALLS` upstream d=1 | `CallersWithPath` | always |
| `CALLS` upstream d=2 | `UpstreamChain` | `callerHops ≥ 2` |
| `CALLS` downstream d=1 | `CalleesWithPath` | `fetchCallees` |
| `EXTENDS` | `Extends` | `fetchExtends` |
| `HAS_METHOD` | `Methods` | `fetchMethods` |
| `IMPORTS` by name | `Importers` | `fetchImports` |
| `IMPORTS` by package | `ImportersByPackage` | `fetchImports` and term len ≥ 3 |

`ImportersByPackage` runs once per query keyword (len ≥ 3 cutoff lets
`fms` qualify).

### Always-on related-files

For every keyword (len ≥ 3), `RelatedFiles` runs:

```cypher
MATCH (n) WHERE toLower(n.filePath) CONTAINS "<word>" AND n.filePath IS NOT NULL
RETURN DISTINCT n.filePath LIMIT 100
```

Result feeds `Graph.Siblings` in the prompt.

---

## Section 8 — Trim caps

Source: `trimCaps`, `trimResponse`.

After assembly, every section is capped per intent and memory
strength. Defaults:

| | maxUpstream | maxDownstream | maxImporters | maxSiblings | maxSnippets | maxImpact | maxSymbols | maxPrimaryCtx |
|---|---:|---:|---:|---:|---:|---:|---:|---:|
| general | 10 | 5 | 10 | 15 | 5 | 15 | 20 | 10 |
| debug | 20 | 10 | 10 | 15 | 7 | 15 | 20 | 10 |
| explore | 5 | 3 | 10 | 25 | 5 | 15 | 20 | 10 |
| trace | 20 | 10 | 15 | 15 | 5 | 15 | 20 | 10 |
| refactor | 10 | 5 | 15 | 15 | 5 | 25 | 20 | 10 |

Memory-strength shrink (always applied, on top of the bandit):

- `memCount ≥ 1` → `maxSymbols ≤ 5`, `maxSnippets ≤ 2`, `maxSiblings ≤ 5`
- `memCount ≥ 3` → `maxSnippets ≤ 1`, `maxSiblings ≤ 3`, `maxSymbols ≤ 3`

Strong memory should produce concise prompts; weak memory should
expose more context to compensate. This rule is hand-coded and lives
outside the bandit so we don't make it relearn that strong memory
means a tighter prompt.

---

## Section 9 — Self-tuning (per-intent dial profile)

The constants in Section 7 and Section 8 are no longer hand-tuned. They live in a
per-intent `Profile` that mutates online based on agent feedback,
within hard min/max bounds and with automatic rollback on regression.

**Full design lives in [`heuristics.md`](heuristics.md).** Headline:

- One profile per intent (5 intents × 12 dials).
- Score per query =
  `1 - 0.15·additional_files - 2e-5·prompt_tokens - 0.05·revisits - 0.20·trim_overlap`,
  clipped to `[0, 1]`.
- `adjusted = score - rolling_mean(intent, last 50)` for variance
  reduction. The bandit consumes `adjusted`.
- ε = 0.10 perturbation: 10% of queries get a candidate (one dial
  ±1). Promote on +0.05 lift over 20 candidate samples; rollback to
  frozen default on 3 consecutive raw scores < 0.30.
- Two feedback paths:
  - **Explicit `dev_feedback`** — weight 1.0.
  - **Implicit repeat detection** — embed every query, cosine against
    the last 30 minutes of queries on the same repo. Cosine > 0.95 →
    raw score 0.0; 0.70–0.95 → 0.4; 0.50–0.70 → no penalty. Weight
    0.5 (noisier — agent might be drilling down rather than retrying).
- Per-query span at `feedback:trace:{query_id}` joins decision-side
  and feedback-side fields for replay / regression / per-knob
  attribution. TTL 30 days.
- Daily reward rows at `heuristics:reward:{intent}:{yyyy-mm-dd}`,
  TTL 90 days.
- Freeze mode (`DEVROUTER_HEURISTICS_FROZEN=true`): selection still
  runs and rows are still written, but `Update` is a no-op. Use
  during incidents and benchmarks.
- Inspect via `dev_feedback_stats`. Reset via `dev_heuristics_reset`.

### Settings

| Variable | Default | Effect |
|----------|---------|--------|
| `DEVROUTER_HEURISTICS_FROZEN` | `false` | Bandit `Update` is a no-op when `true`. Selection still runs. |
| `DEVROUTER_HEURISTICS_BANDIT` | empty | Comma-separated dial names to enable for ε-perturbation, or `all`. Empty = no perturbation (baseline). |

### Reserved for v2 — `(intent, query_shape) → profile`

Profile keys are namespaced as `heuristics:current:{intent}:{shape}`
with `shape="*"` as the v1 default. The next-step extension is to
fingerprint the query shape (`debug+stacktrace`, `refactor+cross-package`)
and maintain a profile per `(intent, shape)` bucket. Key shape is
already compatible — no migration needed.

---

## Section 10 — Per-memory FP learning

Source:
[`internal/memory/falsepositives.go`](../internal/memory/falsepositives.go),
`Router.attributeFalsePositives`
([`internal/router/feedback.go`](../internal/router/feedback.go)),
`applyFPPenalties`.

The bandit in Section 9 tunes how MUCH to retrieve. It cannot tell us WHICH
memories are wrong-for-this-query, because every reward signal is
profile-keyed, not memory-keyed. Section 10 closes that gap with a second,
orthogonal learning loop keyed by `(memory, query embedding)`.

### Schema

One Redis hash per memory that has ever been an FP:

```
mem:fp:{memKey}    HASH
  cent  → 768×float32 little-endian   running mean of FP query embeddings
  count → integer                     number of FPs accumulated
  TTL: 14 days, refreshed on each FP write
```

Storage cost: ~3 KB per FP'd memory. A 500-memory repo at 50% FP rate
≈ 750 KB total. The TTL means a memory that gets re-curated and stops
false-positiving naturally falls back to a clean slate within two
weeks.

### Recording an FP (in `dev_feedback`)

Triggered after the bandit reward is computed, when ALL of:

- `additional_files > 0` — agent had to look beyond what was returned.
- `file_paths` supplied — we know what the agent actually read.
- trace has `memory_keys` — call returned at least one memory.
- trace has `query` — re-embeddable.

For each returned memory: load its files (`path` / `file` / `files`)
via one pipelined `HMGET`, check whether any of those files overlaps
with `splitCSV(file_paths)` (substring match in either direction,
with line-suffix and leading-slash normalisation). Zero overlap →
record the FP:

```
new_centroid = (count * old_centroid + query_embedding) / (count + 1)
new_count    = count + 1
```

Re-embedding the query takes ~80 ms against the bundled ONNX embedder; that cost lands once
per task on the feedback handshake, not on every retrieval.

Logged as `[feedback] joined=explicit ... fp_recorded=2`.

### Constants

| Constant | Value | Meaning |
|----------|------:|---------|
| `FPDemoteSimThreshold` | 0.70 | Below this, the FP centroid is too dissimilar to act on |
| `FPMaxDistancePenalty` | 0.20 | Cap added to a hit's cosine distance |
| `FPSaturationCount` | 3 | Number of FPs at which the penalty hits the cap |
| `FPTTL` | 14 days | Idle TTL on the FP record |

### Two important properties

- **Linear ramp on count.** A single FP doesn't fully suppress a
  memory — it shaves ~33% of the cap. Three+ FPs hit the cap. One
  anomalous misfire can't kill an otherwise-good memory.
- **Self-cleaning.** After demotion the floor (Section 6.1) re-runs.
  Memories that crossed the ceiling drop out; ones that just got
  re-ranked stay in but lose the ranking competition. Either way,
  the next query against the same FP centroid will find the memory
  pre-demoted automatically.

### What the loop does NOT do

- Does **not** delete or modify the memory record. Curation is still
  a human/agent decision; the FP store is a per-query relevance
  signal layer on top.
- Does **not** propagate to other memories. Each memory's FP centroid
  is independent. A memory that was wrong for "cache clearing" can
  still be returned for "auth flow" if its file paths overlap there.
- Does **not** override the must filter. Must-term filtering (Section
  6.2) happens *before* FP demotion — a memory with the must term in
  a structural field still gets to compete; the FP penalty just makes
  it compete from a worse starting position.

### Manual reset

`Store.ResetFalsePositives(ctx, memKey)` deletes the FP record for
one memory. Use after deliberately re-curating the memory's files /
purpose so it isn't held back by stale grudges from the prior
version. Not yet exposed as an MCP tool; for now an admin path.

---

## Section 11 — Honest signals

Source: `agentSimilarityStats`, `graphProximityFromTrace`,
`confidence`.

`retrieval_trace.signals` and per-entry `confidence` used to be
hardcoded constants (`semantic_similarity = 0.8`,
`primary_context_match = 0.85`, agent-source confidence = `0.9`).
Every `dev_context` looked high-quality regardless of actual match
strength. They are now derived from the real cosine numbers that
drove ranking:

| Field | Computation |
|-------|-------------|
| `signals.semantic_similarity` | Top cosine similarity across kept agent hits |
| `signals.primary_context_match` | Mean cosine similarity of returned `primary_context` entries |
| `signals.memory_coverage` | `min(1, kept_count / 10)` (count-based, capped) |
| `signals.graph_proximity` | `traced_symbols / seed_symbols` from the graph stage trace |
| `signals.decision_relevance` | `0.9` (decisions are agent-curated; will become real once decision cosine is wired) |
| `PrimaryContextEntry.confidence` | `cosine_similarity * (0.6 if stale else 1.0)` |
| `DevPrompt.context_confidence` | Mean of per-entry confidence |

Why this matters: the FP loop in Section 10 only works because the upstream
signals stopped lying. If `context_confidence` were still hardcoded
to `0.9`, the bandit and the FP loop would both be optimising
against a constant. With per-entry confidence equal to real cosine
similarity, a trace that says `context_confidence: 0.42` is itself
a useful FP-suspect signal — something a future version could read
directly without waiting for explicit `dev_feedback`.

---

## Section 12 — Plan echo (debugging surface)

Source: `prompt.PlanDebug`
([`internal/prompt/types.go`](../internal/prompt/types.go)).

The active `QueryPlan` (post-sanitization, post-auto-anchor) is echoed
on every `dev_context` response as a top-level `query_plan` field:

```json
{
  "query_plan": {
    "source":        "agent",
    "must_terms":    ["fms"],
    "should_terms":  ["unmarshal", "decode", "parse", "json"],
    "exclude_terms": ["test"],
    "phrases":       ["unmarshal error"],
    "context_hints": ["gobackend/fms"],
    "auto_anchored": false
  }
}
```

| Field | Meaning |
|-------|---------|
| `source` | `"agent"` if the caller supplied a `plan` in dev_context args; `"auto"` if no plan was supplied and retrieval used only the auto-anchor. |
| `must_terms` … `context_hints` | The five `QueryPlan` slots used downstream. |
| `auto_anchored` | `true` iff the must-term came from Section 3's auto-anchor rather than the caller's `plan`. Distinguishes "agent set this anchor" from "fallback rarest-token logic did". |

Always emitted (~30–80 tokens). Inner term lists are `omitempty`-gated,
so a caller that supplies no plan and gets no auto-anchor hit renders
as just `{"source": "auto"}`.

---

## End-to-end example

Query: `fms unmarshalling error`, repo: `goserving`, agent supplies a plan.

1. **Tokenize** (Section 1) → `[unmarshal, debug, error, fms]`.
2. **Path detect** (Section 2) — none.
3. **Plan** (Section 3) — agent sends, on the `dev_context` call:
   ```json
   {"must_terms":["fms"],
    "should_terms":["unmarshal","decode","parse","json"],
    "exclude_terms":["test"],
    "phrases":["unmarshal error"],
    "context_hints":["gobackend/fms"]}
   ```
   Router `SanitizePlan` runs (microseconds): lowercase, dedupe, caps
   already met → plan unchanged.
   `effectiveQuery` = `"fms unmarshalling error fms unmarshal decode parse json"`
   → tokenized to `[fms, unmarshal, decode, parse, json, debug, error]`.
4. **Intent** (Section 4) → `debug` (matched on `error`).
5. **Code search** (Section 5) — hybrid `/api/search` returns `[]`;
   `SearchByNameWithOpts` runs across 7 tokens, drops `_test.go` /
   `Test*`, anchors on `fms`, IDF-scored. Top:
   `GetFmsController, Fms, AdomainFmsValue` (capped to 3 by trim
   shrink for `memCount ≥ 3`).
6. **Memory** (Section 6) — 20 KNN hits → 12 after floor (Section 6.1)
   → 7 after must-filter (Section 6.2; `kosmos-rule-dnf` and friends
   dropped because `fms` was only in their `purpose` prose) → 6 after
   FP demotion (Section 6.3) → 6 re-ranked (Section 6.4).
7. **Graph** (Section 7) — debug + `memCount ≥ 3` budget → 2 trace
   symbols, `callerHops=1`, no extends/methods/imports.
8. **Trim** (Section 8) — debug shrunk-by-memory caps applied.
9. **Honest signals** (Section 11) — `context_confidence ≈ 0.78`,
   `memory_coverage = 0.6`.
10. **Plan echo** (Section 12) — `source: "agent"`,
    `auto_anchored: false`, full plan attached.

Stderr summary:

```
[router] plan(agent): must=[fms] should=[unmarshal decode parse json] exclude=[test] hints=[gobackend/fms] phrases=[unmarshal error]
[router] memory floor dropped 8/20 hits (max_distance=0.60)
[router] memory plan-filter dropped 5/12 hits (must=[fms] exclude=[test])
[router] memory FP demoted=1 post-demotion=6
[router] graph budget: maxTrace=4 callerHops=2 callees=true
[router] tracing symbol: "GetFmsController"
```

**No-plan variant:** identical except step 3's plan is
`{must:[fms]}` (auto-anchor picks the rarest token; `source: "auto"`,
`auto_anchored: true`). Recall is narrower (4 tokens vs 7) and exclude
rules don't fire, but the deterministic backbone in Section 1, Section
2, Section 5 is doing the same work either way.
