# devrouter agent rules

devrouter's closed-loop tuning and memory-relevance learning only work if
the calling agent actually closes the loop. This file is the canonical
ruleset every consumer repo should embed.

## Where to put it

Paste the block between the `<!-- devrouter:start -->` and
`<!-- devrouter:end -->` markers below into your repo's agent context file:

- Claude Code → `CLAUDE.md`
- Cursor → `.cursor/rules/devrouter.mdc`
- Codex / generic → `AGENTS.md`

Replace `<repo>` with your repo name (the one passed to every devrouter
tool). Keep the markers intact so downstream tooling can sync the block
in-place across consumer repos.

## What the rules require

Three rules, enforced on **every** turn — including follow-ups in the same
session:

1. **Before every question, call `dev_context` first.** Capture the
   `query_id` from the response.
2. **After every task, save 1–3 memories** (`memory_save_file` /
   `_func` / `_flow`, or `decision_save` for deliberate choices).
3. **After every `dev_context`, call `dev_feedback` with the captured
   `query_id`.** This drives both the bandit (intent-level trim/budget
   knobs) and the per-memory false-positive demotion centroids.

Skipping rule 3 freezes the learning loop — `dev_context` returns the same
quality forever. Skipping rule 1 means you miss prior memories and can
contradict saved decisions. The block below spells out the full workflow
and the four most common mistakes.

---

## Copy this block (verbatim)

GitHub renders the block below as a single fenced markdown unit — use the
Copy button at its top-right and paste it (markers included) into your
agent context file. The markers must remain intact so downstream sync
tooling can update the block in-place across consumer repos.

````markdown
<!-- devrouter:start -->

> **READ THIS FIRST: THREE MANDATORY RULES FOR EVERY QUESTION**
>
> These rules apply to:
> - Initial questions
> - **Follow-up questions in the same session** ← critical
> - Clarifications about prior answers
> - Debugging, architecture, refactoring, code review questions
> - Before ANY file reads, greps, or searches
>
> **Violation cost:** Skipping these breaks devrouter's learning loop, makes
> future queries worse, and causes memory saves to be forgotten.

# DevRouter — Persistent Developer Memory

This project has a devrouter MCP server that provides persistent memory across
sessions.

## THREE MANDATORY RULES (STRICT ENFORCEMENT)

### **RULE 1 — BEFORE every question: Call `dev_context` FIRST (with a `plan`)**

**When:** Every time you start a task OR answer a follow-up question
**How:** `dev_context(query="<your question>", repo="<repo>", plan={...})`
**Capture:** The `query_id` field — you will need it in RULE 3

**Always supply a `plan`.** devrouter has no in-process planner LLM — the
structured `plan` you produce is the single highest-leverage thing you
can do to improve retrieval. You have the conversation context that bare-
query keyword extraction never has; use it.

See the `dev_context` section below for the full `plan` schema and two
worked examples.

**What counts as a question requiring dev_context:**
- Initial task ("how does X work?")
- Follow-up question ("what about Y?") ← Don't skip this
- Clarification ("so does that mean...?")
- New angle on same topic ("in production vs dev?")

**What happens if you skip this:**
- You miss saved memories from prior sessions
- `dev_context` retrieval gets worse (no feedback loop)
- You waste time re-exploring code

---

### **RULE 2 — AFTER every task: SAVE MEMORIES (AUTOMATIC, NO PERMISSION NEEDED)**

**When:** Immediately after understanding something new (during the task, not after)

**Save these:**

| You did this | Save this | Example |
|---|---|---|
| Read and understood a file | `memory_save_file` | "consumer/main.go: orchestrates startup" |
| Traced a function's logic | `memory_save_func` | "startConsumerApp: loads projects, calls initiateTopicConsumption" |
| Followed a flow across 2+ files | `memory_save_flow` | "consumer-startup-sequence: 6 phases from main() to goroutines" |
| Made a deliberate decision (architecture, refactoring, optimization, coding standard, constraint, tradeoff) | `decision_save` | "use async readers instead of blocking" |

**Quantity:** 1–3 memories per task (not optional)

**What happens if you skip this:**
- Future queries won't have this context
- The same code gets re-explained in future sessions
- Memories for this repo stay static

---

### **RULE 3 — AFTER every `dev_context`: Call `dev_feedback` BEFORE THE NEXT QUESTION**

**When:** Once you've finished acting on the returned context
**When exactly:** Right before you ask the next question (or move to a new task)

**What to report:**

```
dev_feedback
  query_id="<the UUID from dev_context>"
  additional_files=<count>
  revisited_files=<count>
  file_paths="file1.go,file2.go"
  success=true/false
```

**Fields:**
- `query_id` — the UUID from dev_context (REQUIRED — don't lose it)
- `additional_files` — files you had to read beyond what dev_context returned
- `revisited_files` — files from dev_context you opened more than once
- `file_paths` — comma-separated list of files you actually consulted
- `success` — `true` if context was sufficient, `false` if you had to grep/search

**Frequency:** Call exactly once per `dev_context`. Don't skip on follow-up turns.

**What happens if you skip this:**
- The bandit cannot learn which retrieval settings work
- The per-memory false-positive loop cannot demote irrelevant memories
- Your queries stay just as bad in future sessions
- DevRouter's self-tuning stalls

---

**Scope is auto-detected:** All save tools have an optional `scope` parameter.
Omit it (or pass empty string) to auto-detect: files unchanged vs the
configured release ref (default `origin/release`, configurable per-deployment
via `DEVROUTER_RELEASE_BRANCH` — see [`staleness.md`](staleness.md)) are saved
globally; changed files are scoped to your current branch. This prevents
experimental branch work from polluting shared memory.

## How to use the response

**When PRIMARY CONTEXT is present:**

1. Start from PRIMARY CONTEXT — it is the most reliable source of system
   behavior, captured from prior debugging and exploration.
2. Expand using call chains and symbols returned alongside it.
3. Use code snippets only for validation, not as the primary source.
4. Do NOT ignore PRIMARY CONTEXT unless it is clearly contradicted by current
   code.

**When PRIMARY CONTEXT is missing** (`memory_coverage: "none"`): `dev_context`
still returns symbols, call chains, file paths, and code snippets from the code
graph. Use these FIRST:

1. Read the code snippets returned — they are the most relevant files the
   graph found.
2. Follow the call chain to understand callers and callees.
3. Use symbols and graph relationships (importers, extends, siblings) to find
   related files.
4. Only search or read additional files if the graph context is insufficient.

Do NOT skip the graph context and jump straight to grepping or file
exploration. The graph already found the relevant entry points — start there.

## Available Tools

### `dev_context`
Retrieves structured context for a developer question.

**Input:** `{ "query": "...", "repo": "<repo>", "plan": { ... } }`

`plan` is optional but **strongly recommended** — devrouter has no
in-process planner LLM, so the structured retrieval terms come from you.
Use everything you know about the conversation so far (not just the last
user message) when filling these in.

**Schema:**

| Field | Cap | Meaning |
|---|---|---|
| `must_terms` | 1–2 | Hard-anchor tokens (lowercase, no stop words). Results must match at least one. Prefer short domain abbreviations (`fms`, `kbb`) over generic verbs (`error`, `debug`). |
| `should_terms` | 0–6 | Synonyms / expansions / canonical identifier spellings (lowercase). Do NOT duplicate morphological variants — the indexer already stems. |
| `exclude_terms` | 0–3 | Conventional noise filters (`test`, `mock`, `fixture`). Targeted, not substring contains. |
| `phrases` | 0–3 | Multi-word strings worth matching verbatim. |
| `context_hints` | 0–3 | Likely package or file-path fragments (e.g. `gobackend/fms`). Soft bias only. |

Caps are enforced server-side; over-stuffing fields just wastes tokens
and dilutes scoring. Plan source ends up in `retrieve_debug` output as
`source="agent"` (you supplied it) or `source="auto"` (you didn't —
devrouter auto-anchored the rarest query token instead).

**Example — user just asked about an FMS unmarshalling error:**

```json
{
  "query": "fms unmarshalling error on null fields",
  "repo": "gobackend",
  "plan": {
    "must_terms": ["fms"],
    "should_terms": ["unmarshal", "decode", "parse", "json", "null"],
    "exclude_terms": ["test"],
    "phrases": ["unmarshal error"],
    "context_hints": ["gobackend/fms"]
  }
}
```

**Example — follow-up turn ("why does that fail on empty arrays?"); resolve "that" from prior context to `fms`:**

```json
{
  "query": "why does that fail on empty arrays?",
  "repo": "gobackend",
  "plan": {
    "must_terms": ["fms"],
    "should_terms": ["unmarshal", "decode", "array", "empty", "nil"],
    "phrases": ["empty array"],
    "context_hints": ["gobackend/fms"]
  }
}
```

Returns a structured prompt containing:
- `query_id` — UUID to pass back to `dev_feedback`
- `instructions` — how to use the response (changes based on coverage)
- `intent` — detected query type (debug, explore, trace, refactor, general)
- `primary_context` — agent-written file/func/flow memories most relevant
- `context_confidence` — float 0–1 (honest, derived from cosine similarity)
- `memory_coverage` — `high` / `medium` / `low` / `none`
- `symbols` — matching symbol names from the code graph
- `call_chain` — upstream callers and downstream callees
- `graph` — importers, extends, methods, sibling files
- `impact_radius` — symbols affected by changes
- `code_snippets` — relevant source code
- `model_hint` — recommended model based on memory coverage

The system automatically detects query intent and adjusts what it returns:
debug queries get deeper call chains, trace queries get broader traversal, etc.

### `retrieve_debug`
Debug the retrieval pipeline to understand why specific context was selected
and where time was spent.

**Input:** `{ "query": "...", "repo": "<repo>", "plan": { ... } }`

Same `plan` schema as `dev_context` (above). Returns the same context as
`dev_context` plus stage-by-stage latencies, ranking signals (semantic
similarity, graph proximity, memory coverage), candidate counts in/out at
each stage, the active plan with its `source` (`agent` if you supplied
one, `auto` if devrouter only auto-anchored), and final token estimate.

Use this when:
- Context quality seems unexpected
- You want to understand which signals drove ranking decisions
- Debugging why a particular memory was or wasn't selected
- Measuring retrieval cost and token usage

### `memory_save_file`
Saves what you learned about a source file.

**Input:** `{ "repo": "<repo>", "path": "...", "purpose": "...", "key_symbols": "...", "scope": "..." }`

- `path` (required) — file path relative to repo root
- `purpose` (required) — what this file does, why it exists, its role
- `key_symbols` (optional) — exported functions/types, comma-separated
- `scope` (optional) — `"global"` to share across branches; omit to auto-detect

### `memory_save_func`
Saves what you learned about a function.

**Input:** `{ "repo": "<repo>", "name": "...", "file": "...", "purpose": "...", "callers": "...", "callees": "...", "scope": "..." }`

- `name`, `file`, `purpose` (required)
- `callers`, `callees` (optional, comma-separated)
- `scope` (optional)

### `memory_save_flow`
Saves what you learned about a cross-file flow spanning multiple files.

**Input:** `{ "repo": "<repo>", "name": "...", "purpose": "...", "files": "...", "entry_points": "...", "scope": "..." }`

- `name` (required) — descriptive flow name (e.g. `"add-content-provider"`)
- `purpose` (required) — step-by-step description of the flow
- `files` (optional) — key file paths involved, comma-separated
- `entry_points` (optional) — entry-point functions, comma-separated
- `scope` (optional)

### `memory_populate`
Bootstraps skeleton entries from codegraph for a new repo. Run once when
onboarding.

**Input:** `{ "repo": "<repo>" }`

### `decision_save`
Saves a deliberate developer decision (architecture, refactoring, optimization,
coding standard, constraint, tradeoff). Surfaced in future `dev_context` so
prior choices aren't re-litigated.

**Input:** `{ "repo": "<repo>", "name": "...", "decision_type": "...", "decision": "...", "rationale": "...", "alternatives": "...", "constraint": "...", "decision_scope": "...", "files": "...", "scope": "..." }`

- `name`, `decision_type`, `decision`, `rationale` (required)
- `decision_type` ∈ {`refactor`, `optimization`, `coding_standard`, `architecture`, `constraint`, `tradeoff`}
- everything else optional

### `decision_list`
Lists saved developer decisions, optionally filtered by type, scope, or files.
Output shows `[ACTIVE]` and `[SUPERSEDED]` labels with lineage.

**Input:** `{ "repo": "<repo>", "decision_type": "...", "scope": "...", "files": "..." }`

### `decision_supersede`
Mark an existing decision as superseded by a newer one. The old decision is
preserved as lineage, never deleted.

**Input:** `{ "repo": "<repo>", "old_name": "...", "new_name": "..." }`

Call this AFTER you've saved the new decision with `decision_save`.

### `dev_feedback`
Closes the retrieval loop for one `dev_context` call. The router uses your
feedback to (a) compute a reward signal that drives the self-tuning bandit,
which adapts graph-traversal depth and context-trim caps per intent, and (b)
record per-memory false-positive signals so memories that keep matching the
wrong queries get demoted on future retrievals.

**Input:** `{ "query_id": "...", "additional_files": 0, "revisited_files": 0, "file_paths": "...", "success": true }`

- `query_id` (strongly recommended) — UUID returned at the top of the matching
  `dev_context` response. If you forgot to capture it, omit and the server
  falls back to the most recent `dev_context` from this MCP session.
- `additional_files` (required) — count of files you ended up reading that
  `dev_context` did NOT return.
- `revisited_files` (optional) — count of files from `dev_context` you re-opened
  (signal the initial trim was too aggressive).
- `file_paths` (optional, **comma-separated string**) — files you actually
  consulted; used both to detect over-trim and to drive false-positive
  attribution.
- `success` (optional, default `true`) — `false` if context was insufficient
  and you had to fall back to grep/search.

Call once per `dev_context`. Skipping this is RULE 3.

### `dev_feedback_stats`
Inspect the live heuristics state — active profile per intent, recent rolling
rewards, perturbation/promotion counters, rollback strikes, freeze status.

**Input:** `{}`

### `dev_heuristics_reset`
Force a manual rollback of one (or all) intent profiles to the frozen default.
Use this after a regression to clear bad learning before re-enabling the
bandit.

**Input:** `{ "intent": "debug" }` — one of `debug`, `explore`, `trace`,
`refactor`, `general`, or `"all"`/omitted to reset every intent.

## Complete Workflow (Step-by-Step)

```
STEP 1: Call dev_context
├─ Capture query_id (save it!)
├─ Read code snippets returned
└─ Follow call chains and graph

STEP 2: Do the work
├─ Read files from dev_context
├─ Debug, trace, explore as needed
├─ Save memories (DURING work, not after)
└─ Answer the question

STEP 3: Save memories (AUTOMATIC)
├─ memory_save_file for files you read deeply
├─ memory_save_func for functions you traced
├─ memory_save_flow for cross-file flows
└─ decision_save if you made a decision

CHECKPOINT BEFORE NEXT QUESTION:
   ├─ Did you call dev_context? ✓
   ├─ Did you save 1–3 memories? ✓
   └─ Did you call dev_feedback? → GO TO STEP 4

STEP 4: Call dev_feedback
├─ Include the query_id
├─ Report additional_files count
├─ Report revisited_files count
└─ Report file_paths you consulted

ONLY NOW can you ask the next question
```

## Common Mistakes (And How to Avoid Them)

### MISTAKE 1: Answer a follow-up question without dev_context

```
User: "How does the consumer work?"
You: [call dev_context, answer, save, feedback] ✓

User: "What about in production vs dev?"
You: [immediately answer without dev_context] ✗
     → Lost context from prior session
     → Devrouter can't improve
     → Might contradict saved memories
```

Correct: call `dev_context` again on the follow-up.

### MISTAKE 2: Forget to save memories after tracing code

```
You: [read consumer/main.go, understand startup flow]
You: [trace 5 functions, learn the architecture]
You: [answer the question]
You: [SKIP saving] ✗
     → Next session, same code gets re-traced
     → Future queries don't benefit from this learning
```

Correct: save 1–3 memories during the task.

### MISTAKE 3: Lose the query_id and skip dev_feedback

```
You: [dev_context returns query_id "abc-123"]
You: [explore, save memories]
You: [forget to save query_id]
You: [skip dev_feedback] ✗
     → Bandit doesn't learn if context was good
     → Per-memory FP loop can't demote irrelevant hits
     → Same queries stay slow in future sessions
```

Correct: capture `query_id` immediately when reading the response.

### MISTAKE 4: Skip checkpoints and jump to next question

```
You: [answer question 1]
You: [think: "that was easy, I'll skip dev_feedback"]
User: [asks follow-up question 2]
You: [immediately answer without dev_context] ✗
     → You've now broken TWO rules
     → Feedback loop is broken
     → Next session is worse
```

Correct: complete RULE 1–3 cycle for every turn.

### When NOT to save

- Trivial one-line changes where you didn't learn anything new
- You only glanced at a file without understanding it
- The memory already exists and is not stale (check `dev_context` first)

## How It Works

- Memories live in Redis with vector embeddings (`nomic-embed-text-v1.5`,
  served by the bundled ONNX embedder on `localhost:11435`) and persist
  across sessions.
- `dev_context` runs vector recall through a 4-stage relevance gate —
  cosine-distance floor, structural-field must filter, false-positive demotion,
  should-term re-rank — so only memories that actually match the query make it
  into PRIMARY CONTEXT.
- The system detects query intent (debug/explore/trace/refactor/general) and
  adjusts graph traversal depth and response shaping accordingly.
- Memories are scoped per repo. Save tools auto-detect branch scope.
- Stale memories (where the underlying file has changed since the memory was
  saved) are flagged so you know to re-examine.
- **Decisions are surfaced in `dev_context` responses alongside symbols and
  call chains.** When you query about a codebase area where decisions exist,
  `dev_context` returns those decisions with type-aware instructions
  (e.g. "REFACTOR DECISIONS: Before refactoring, review existing decisions —
  some refactors were explicitly rejected."). This prevents re-litigating
  settled choices.
- `dev_feedback` drives **two** independent learning loops: the per-intent
  bandit that tunes trim/budget knobs, and the per-memory false-positive
  centroid that demotes memories that keep matching the wrong queries.

## CRITICAL REMINDERS

### Before You Ask The Next Question:

Have you done ALL THREE?
- Called `dev_context` for this question?
- Saved 1–3 memories (memory_save_file/func/flow)?
- Called `dev_feedback` with `query_id`?

If not, STOP and complete these steps before proceeding.

**This is not optional.** These rules exist because:
1. **RULE 1 (dev_context first)** ensures you get the best prior context.
2. **RULE 2 (save memories)** means future sessions benefit from your learning.
3. **RULE 3 (dev_feedback)** trains devrouter so queries get faster and more
   accurate.

Without all three, the system degrades and future queries suffer.

<!-- devrouter:end -->
````
