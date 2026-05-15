# Heuristics — how devrouter self-tunes

devrouter has a small set of dials that control how much code, graph, and
memory it pulls into each response. Those dials are not hand-tuned. They
move on their own based on whether the agent's response actually worked.

This document explains what the dials are, what feedback moves them,
what stops them from running away, and how to inspect or freeze the
system.

> Looking for the canonical, source-linked rules? See
> [`retrieval-rules.md`](retrieval-rules.md) Section 9. This file is the
> overview; that one is the contract.

## What the system actually does

For every `dev_context` call, devrouter picks one of five intents
(`debug`, `explore`, `trace`, `refactor`, `general`) and uses the dial
profile assigned to that intent. After the agent does something with
the response, devrouter scores the call:

- If the agent had to read more files anyway, the score goes down
  (under-retrieval — devrouter held back too much).
- If the response was bloated with snippets and graph context,
  the score goes down (over-retrieval — devrouter padded the prompt).
- If the agent re-asked a near-identical question minutes later, the
  score goes down (the answer didn't stick).

devrouter occasionally tries a slightly different dial setting for the
same intent. If that variant scores meaningfully better over a window
of queries, it becomes the new default for that intent. If it scores
worse, it's discarded. If it produces a few really bad calls in a row,
devrouter rolls back to the original frozen defaults automatically.

The whole system runs without any human in the loop. Every change is
logged to Redis so you can see what moved and why.

## The dials (per intent)

Source: [`internal/heuristics/profile.go`](../internal/heuristics/profile.go).

| Dial | What it controls | Hard min | Hard max |
|------|------------------|---------:|---------:|
| `max_trace` | How many seed symbols get full call-chain tracing | 3 | 8 |
| `caller_hops` | How many "who calls this" hops to follow | 0 | 3 |
| `max_upstream` | Cap on callers shown per symbol | 3 | 25 |
| `max_downstream` | Cap on callees shown per symbol | 2 | 15 |
| `max_importers` | Cap on packages that import a symbol | 3 | 20 |
| `max_methods` | Cap on methods of a struct/interface | 3 | 20 |
| `max_siblings` | Cap on nearby files in the same package/dir | 3 | 30 |
| `max_snippets` | Cap on raw code snippets in the response | 1 | 10 |
| `max_impact` | Cap on refactor blast-radius entries | 5 | 30 |
| `max_symbols` | Cap on symbols in the response | 3 | 25 |
| `max_primary_ctx` | Cap on primary file context entries | 3 | 25 |
| `max_decisions` | Cap on architecture-decision notes shown | 3 | 10 |

Each intent starts with a different default profile (debug favours
deeper traces, explore favours more siblings, etc.). The bandit can
push any dial up or down by ±1 step per try, but never outside the
hard min/max in the table. Even if Redis is corrupted and returns
garbage, every read is clipped back into the safe range before use.

There's also a separate, hand-coded "strong memory shrink" rule that
sits on top: when the response already has 1+ saved memories
attached, devrouter tightens the snippet/sibling caps regardless of
what the bandit says. The reasoning is fixed — strong memory means a
shorter prompt is fine — so we don't make the bandit relearn it every
time.

## How the score is computed

Source: [`internal/heuristics/reward.go`](../internal/heuristics/reward.go).

After the agent reports back via `dev_feedback`, devrouter computes a
raw score between 0 and 1:

```
raw = 1.0
    - 0.15 * additional_files       # agent had to read this many extra files
    - 2e-5 * total_prompt_tokens    # response was this big
    - 0.05 * revisited_files        # agent re-read files devrouter already returned
    - 0.20 * trim_overlap_hit       # devrouter trimmed something the agent ended up reading
```

Then it subtracts the rolling average for that intent so easy queries
don't look like wins and hard queries don't look like losses:

```
adjusted = raw - rolling_mean(intent, last 50 samples)
```

The bandit only consumes `adjusted`. The rolling-mean subtraction is
the single biggest reason this works at all — without it, query
difficulty drowns out any signal from the dial change.

The token-cost coefficient is intentionally tiny: a 5k-token response
costs 0.10, a 20k-token response costs 0.40. Big enough to push back
against bloat, small enough that it never dominates the file-reads
term. Without it, the bandit's optimal strategy would converge on
"return everything" — because under-retrieval is much louder than
mild over-retrieval in any single query.

## Two ways feedback comes in

**1. Explicit (`dev_feedback`)**

The agent calls `dev_feedback({query_id, additional_files, ...})` after
acting on the response. Weighted at 1.0 — full strength.

**2. Implicit (repeat detection)**

devrouter embeds every query and compares it against the last
30 minutes of queries on the same repo. If a near-identical query
shows up again, that's a signal the previous answer didn't land:

| Cosine similarity | Interpretation        | Retro raw score |
|-------------------|-----------------------|-----------------|
| > 0.95            | Near-identical re-ask | 0.0 |
| 0.70 – 0.95       | Paraphrased repeat    | 0.4 |
| 0.50 – 0.70       | Related follow-up     | (no penalty) |

Weighted at 0.5 — half strength. The agent might be drilling down
into a topic rather than retrying a failed answer, so we don't want
implicit signals to overpower explicit ones.

## How a dial actually moves

Source: [`internal/heuristics/bandit.go`](../internal/heuristics/bandit.go).

For each intent, devrouter holds a single "current best" profile. On
10% of queries it generates a candidate by perturbing one dial by ±1
step. The candidate runs alongside the base for the next batch:

- After 20 candidate samples (and at least 5 base samples in the
  same window), compare the means.
- If the candidate's mean adjusted score beats the base by ≥ 0.05,
  promote it. The candidate becomes the new base.
- Otherwise, discard the candidate. The base stays.
- If the candidate produces 3 consecutive raw scores below 0.30,
  abandon the experiment immediately and roll back to the original
  frozen default for that intent.

By default no dials are eligible for movement (Phase 1 — the bandit is
dormant but everything around it is wired up). Turn on movement with
`DEVROUTER_HEURISTICS_BANDIT` (see Settings below).

## Safety

| Mechanism | What it guarantees |
|-----------|--------------------|
| Hard bounds (`Profile.Clip()`) | No dial ever leaves the min/max envelope, even with corrupted state. |
| Frozen default snapshot | The original starting profile for every intent is written once at startup to `heuristics:default:{intent}:*` and never mutates. It's the rollback target. |
| 3-strike rollback | Three bad raw scores in a row from a candidate triggers automatic restore to the frozen default. |
| Freeze mode | Set `DEVROUTER_HEURISTICS_FROZEN=true` and the bandit selects/scores as normal but never updates anything. Use during incidents, regression hunts, or any isolated experiment. |
| Manual reset | `dev_heuristics_reset` MCP tool reverts one intent (or all of them) to the frozen default. Logged. |

The promotion lift threshold (0.05) and rollback strikes count (3) live
as `var`s in `bandit.go` if you ever need to tune the safety/aggression
trade-off; defaults are conservative on purpose.

## Inspecting

`dev_feedback_stats` returns a snapshot per intent:

- The current live profile (every dial value).
- Sample counts today and over the last 7 days.
- Mean / p50 / p95 raw score over the last 7 days.
- Fraction of samples that were explicit vs implicit.
- The five most recent profile-change events (promote / discard /
  rollback / seed) with reasons and timestamps.
- Whether the system is frozen.

For raw inspection in Redis directly:

| Key | Contents |
|-----|----------|
| `heuristics:current:{intent}:*` | The live profile for an intent (JSON). |
| `heuristics:default:{intent}:*` | Frozen default snapshot (rollback target). |
| `heuristics:history:{intent}` | Recent profile-change log (newest first, capped at 200 entries). |
| `heuristics:reward:{intent}:{yyyy-mm-dd}` | Per-day list of reward rows (90-day TTL). |
| `feedback:trace:{query_id}` | Full per-query span with both decision-side and feedback-side fields (30-day TTL). |

## Settings

Both are env vars on the devrouter process. See
[`configuration.md`](configuration.md) for the full list.

| Variable | Default | Effect |
|----------|---------|--------|
| `DEVROUTER_HEURISTICS_FROZEN` | `false` | When `true`, scoring still runs and rows are still written, but the bandit never updates anything. Manual `dev_heuristics_reset` is still allowed. |
| `DEVROUTER_HEURISTICS_BANDIT` | (empty) | Comma-separated list of dial names to enable for ±1 perturbation, or `all` to enable every dial. Empty (default) leaves the bandit dormant — profiles are read normally but never mutated. |

Recommended rollout:

1. Run with both unset for a few days. The system collects rewards
   and writes traces but doesn't touch profiles. You get baseline
   numbers in `dev_feedback_stats`.
2. Turn on a single dial first:
   `DEVROUTER_HEURISTICS_BANDIT=max_trace`. Watch
   `dev_feedback_stats` for promotions / rollbacks. Most promotions
   are small.
3. When comfortable, switch to `DEVROUTER_HEURISTICS_BANDIT=all`.
4. During incidents or any isolated experiment, set
   `DEVROUTER_HEURISTICS_FROZEN=true` so results are reproducible.

## Files

- [`internal/heuristics/profile.go`](../internal/heuristics/profile.go)
  — dial definitions, defaults per intent, hard bounds, clipping,
  strong-memory shrink rules.
- [`internal/heuristics/reward.go`](../internal/heuristics/reward.go)
  — score function, implicit-repeat scoring, weights.
- [`internal/heuristics/bandit.go`](../internal/heuristics/bandit.go)
  — perturbation, promotion, discard, rollback.
- [`internal/heuristics/store.go`](../internal/heuristics/store.go)
  — Redis schema (current / default / history / reward / trace).
- [`internal/heuristics/picker.go`](../internal/heuristics/picker.go)
  — process-wide handle that wires all of the above together; reads
  the env vars listed above.
- [`internal/router/feedback.go`](../internal/router/feedback.go)
  — MCP handlers for `dev_feedback`, `dev_feedback_stats`,
  `dev_heuristics_reset`.

## Related

- [`retrieval-rules.md`](retrieval-rules.md) Section 9 — source-linked
  ruleset that this doc summarizes.
- [`retrieval-rules.md`](retrieval-rules.md) Section 10 — the per-memory
  false-positive feedback loop. That's a separate, complementary
  system: Section 9 tunes how *much* to retrieve, Section 10 tunes which
  *individual memories* should be suppressed.
- [`tools.md`](tools.md) — input schemas for `dev_feedback`,
  `dev_feedback_stats`, `dev_heuristics_reset`.
- [`agent-rules.md`](agent-rules.md) — the call-order rules an agent
  must follow for any of this to actually run.
