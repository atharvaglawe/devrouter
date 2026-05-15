# Question authoring guide

Each `<repo>.jsonl` file is a JSONL of questions used to score retrieval
adapters on that repo. The runner loads them, asks every adapter the
`query`, and compares each adapter's top-K returned files against
`expected_files`.

## Schema

```json
{
  "id": "<repo>-NNN",
  "repo": "<repo>",
  "intent": "trace | debug | refactor | explore | general",
  "query": "Question phrased the way a developer would actually ask it.",
  "expected_files": ["repo-relative/path/to/file.go", "..."],
  "expected_symbols": ["OptionalQualifiedSymbolName", "..."],
  "notes": "Optional human note explaining why these files are the right answer."
}
```

* `expected_files` MUST use **repo-relative POSIX paths** (no leading `/`,
  no `./`, forward slashes). The scorer joins on exact string match.
* `expected_symbols` is informational; not currently scored.
* `intent` should be one of the five values DevRouter classifies into,
  so per-intent breakdowns line up across systems.

## Quality bar

A good question:

1. **Has a defensible "right answer".** A human reviewing the repo would
   independently agree the listed files are what they'd open to answer it.
2. **Is verifiable.** Author has actually opened each `expected_file` and
   confirmed it contains the relevant code.
3. **Does not include the answer in the prompt.** Asking "Where is
   `FmsController.Get`?" is too easy because the symbol name is in the
   query — every adapter trivially wins. Phrase it the way a developer who
   doesn't yet know the symbol name would ("Which controller serves the
   FMS endpoint?").
4. **Has 1-3 expected files**, not 20. If the answer requires reading 10
   files, it's two questions.
5. **Reflects real intent diversity.** Mix `trace` (where is X served
   from), `debug` (why is Y returning null), `refactor` (which files
   would I touch to add Z), `explore` (how does the cache layer work),
   and `general` (what does this service do).

## Anti-patterns

- **Tautological queries**: query contains the literal filename. The
  question is then "can the system regex its own input?" — uninteresting.
- **Subjective answers**: "What is the best way to implement…?" — no
  ground truth.
- **Repo-internal trivia that no human cares about**: avoid; the
  benchmark should reflect questions agents actually face.
- **Questions whose answer is in CLAUDE.md / AGENTS.md verbatim**: the
  claudemd baseline will trivially win and the result tells us nothing
  about retrieval quality.
