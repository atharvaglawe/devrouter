# Automated git-based staleness detection

devrouter compares against git in three places, automatically, to
keep memories and the codegraph index honest. No user action is
required for any of these — they run as part of the normal save
and retrieval paths.

| Surface | When it runs | Compared against | Action on mismatch |
|---------|--------------|------------------|--------------------|
| Memory git-blob-hash drift | Every memory hit at retrieval time | The blob hash recorded at save time | Damp the entry's confidence ×0.6 and flag `stale: true` |
| Codegraph index commit drift | `codegraph status` and at the top of every `analyze` | Repo's current `HEAD` commit vs `repo.meta.lastCommit` | Print `⚠️ stale (re-run codegraph analyze)`; full rebuild on `analyze` (with embedding cache reuse) |
| Release-branch scope diff | Every memory save | The configured release ref (`DEVROUTER_RELEASE_BRANCH`, default `origin/release`) for the referenced file(s) | Pin the new memory to the current branch (`scope=<branch>`) instead of `global` |

## Memory git-blob-hash drift — retrieve-time check

When the agent calls `memory_save_file` or `memory_save_func`,
`Store.SaveFile` / `SaveFunc` records the current git blob hash for
the referenced file in the memory's `git_hash` field
(`internal/memory/store.go:464` / `:512`):

```go
if m.RepoPath != "" {
    if h := GitFileHash(m.RepoPath, m.Path); h != "" {
        fields["git_hash"] = h
    }
}
```

`GitFileHash` is a thin wrapper over `git log -1 --format=%H -- <file>`
(`internal/memory/freshness.go`). Untracked files / non-git repos
return `""` → "can't determine, treat as fresh".

At retrieval time, every file/func memory hit goes through `isStale`
(`internal/router/router.go:1793`):

```go
func isStale(repoPath, filePath, savedHash string) bool {
    if repoPath == "" || filePath == "" || savedHash == "" {
        return false
    }
    currentHash := memory.GitFileHash(repoPath, filePath)
    if currentHash == "" {
        return false
    }
    return currentHash != savedHash
}
```

A stale memory is **damped, not dropped**. `confidence`
(`router.go:3077`) multiplies the cosine similarity by 0.6:

```go
func confidence(sim float64, stale bool) float64 {
    c := sim
    if c < 0 { c = 0 }
    if c > 1 { c = 1 }
    if stale {
        c *= 0.6
    }
    return c
}
```

The damped value lands in `PrimaryContextEntry.Confidence`, and the
boolean flag is exposed as `PrimaryContextEntry.Stale: true`. Damp
instead of drop because a stale memory is still frequently correct
(file body changed, purpose didn't); the agent gets both the numeric
penalty and the explicit boolean and can validate against code.

`coverageLevel` (`internal/prompt/builder.go:91`) only counts non-stale
entries when computing `MemoryCoverage`, so a query whose hits are
all stale reports `low` coverage and the agent's prompt rules fall
back to graph context first.

### Known gap: flows

`isFlowStale` exists at `router.go:1807` but `SaveFlow` does not
populate `git_hash` for flow memories today (no `RepoPath` plumbing
on `FlowMemory`), so flow staleness is currently unreachable. Flows
*do* get correct branch scoping via the release-diff path below,
which walks every file in the comma-separated list.

## Codegraph index commit drift — analyze-time check

The codegraph CLI persists the indexed commit in `repo.meta.lastCommit`
when `analyze` finishes successfully. `runFullAnalysis` short-circuits
when the commit hasn't moved (`codegraph/src/core/run-analyze.ts:139`):

```typescript
if (existingMeta && !options.force && existingMeta.lastCommit === currentCommit) {
  if (currentCommit !== '') {
    return { …, alreadyUpToDate: true };
  }
}
```

`codegraph status` does the same comparison for human consumption
(`codegraph/src/cli/status.ts:34`):

```typescript
const isUpToDate = currentCommit === repo.meta.lastCommit;
```

When commits diverge the rebuild is full (no per-file incremental
graph merge yet), but two mitigations keep this cheap:

- **Embedding cache reuse** — before tearing down the old DB,
  `runFullAnalysis` snapshots `(nodeId, embedding)` pairs from
  `CodeEmbedding`. Nodes whose IDs survive the rebuild reuse their
  embedding, so the expensive ONNX pass only re-embeds renamed /
  signature-changed symbols.
- **Non-git repos rebuild every analyze** — with no commit to
  compare, the early-return is skipped. Intentional, because we
  can't safely detect "no changes" without a commit ref.

## Release-branch scope diff — save-time check

This is the second git-based comparison. It runs at memory **save**
time and answers a different question than the blob-hash check:
*should this memory be visible from other branches, or only from the
branch that wrote it?*

`internal/memory/store.go:937-991` defines three variants — all
automatically invoked from the four `memory_save_*` MCP tools when
the agent doesn't supply an explicit `scope`:

| Function | Used by | Scope-determining input |
|----------|---------|-------------------------|
| `ScopeForFile` | `memory_save_file`, `memory_save_func` | one file path |
| `ScopeForFiles` | `memory_save_flow` | comma-separated paths; ANY diff → branch scope |
| `ScopeForDecision` | `memory_save_decision` | files (if provided), else `git rev-list --count <release-ref>..HEAD` |

The core check (`store.go`):

```go
func ScopeForFile(repoPath, filePath string) string {
    if repoPath == "" || filePath == "" {
        return "global"
    }
    fetchRelease(repoPath)
    ref := ReleaseRef() // DEVROUTER_RELEASE_BRANCH or "origin/release"
    err := exec.Command("git", "-C", repoPath,
        "diff", "--quiet", ref, "--", filePath).Run()
    if err != nil {
        // exit 1 = file differs from the release ref
        return CurrentBranch(repoPath)
    }
    return "global"
}
```

A best-effort `git fetch <remote> <branch>` runs first
(`fetchRelease` in `store.go`, parsed out of the configured ref) so
the comparison is against an up-to-date baseline. The result is
written into the memory's `scope` field once at save time and never
recomputed; at retrieval, `SearchAll(..., branch)` filters to
`scope IN ("global", <currentBranch>)`, so a memory saved on
`feature/foo` with `scope=feature/foo` is invisible from
`feature/bar`.

The agent can override the auto-detection with an explicit
`scope: "global"` parameter on any of the save tools.

### Configuring the baseline ref

The default baseline is `origin/release`. Override via
`DEVROUTER_RELEASE_BRANCH` if your team's "shipped truth" lives
somewhere else:

```bash
DEVROUTER_RELEASE_BRANCH=origin/main      ./devrouter  # GitHub trunk-based
DEVROUTER_RELEASE_BRANCH=origin/master    ./devrouter  # legacy default branch
DEVROUTER_RELEASE_BRANCH=upstream/trunk   ./devrouter  # forks pointing at upstream
DEVROUTER_RELEASE_BRANCH=main             ./devrouter  # local ref, no fetch
```

Any ref `git diff --quiet <ref> -- <file>` understands works. When
the value is `<remote>/<branch>`, devrouter runs a best-effort
`git fetch <remote> <branch>` before each scope check so the
comparison is against the latest baseline. Local refs (no `/`) skip
the fetch.

### Sharp edge: missing baseline ref

If the configured ref doesn't exist (e.g. `DEVROUTER_RELEASE_BRANCH`
unset on a repo without `origin/release`, or set to a typo), behaviour
splits across the three helpers:

- `ScopeForFile` / `ScopeForFiles` — `git diff --quiet <ref>` exits
  128 on the missing-ref error, the helpers treat that as "file
  diverges" and return the current branch as scope. Conservative.
- `ScopeForDecision` with **no files** — `git rev-list <ref>..HEAD`
  also exits 128 on the missing ref, but the helper falls back to
  `scope=global` rather than branch (it has no per-file evidence to
  fall back on). So a decision saved with empty `files` and no valid
  baseline becomes globally visible.

The fix in both cases is to point `DEVROUTER_RELEASE_BRANCH` at a ref
that actually exists for your repo (most commonly `origin/main` or
`origin/master`). The mismatch between the two fallbacks is by
design — file-level diffs have the file path as a fingerprint, so
branch-scoping is cheap and reversible; decisions without files are
intentionally globally-applicable claims, so global-scoping on
missing baseline matches the agent's likely intent.

## How to inspect

```bash
# Codegraph index status (commit drift).
codegraph status

# Memory blob-hash drift — pull the saved hash and compare.
redis-cli HGET 'mem:<repo>:file:<sanitized-path>' git_hash
git -C /abs/path/to/repo log -1 --format='%H' -- <file>

# Branch scope on a memory (save-time release diff).
redis-cli HGET 'mem:<repo>:file:<sanitized-path>' scope

# Replay the release diff manually for one file.
# Substitute $DEVROUTER_RELEASE_BRANCH (default origin/release).
ref="${DEVROUTER_RELEASE_BRANCH:-origin/release}"
remote="${ref%%/*}"; branch="${ref#*/}"
git -C /abs/path/to/repo fetch "$remote" "$branch"
git -C /abs/path/to/repo diff --quiet "$ref" -- <file>; echo $?
# exit 0 → would save as scope=global; exit 1 → as scope=<branch>
```
