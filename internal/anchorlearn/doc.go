// Package anchorlearn turns the static service-entry-point anchor list
// in the router into a self-improving, per-repo bandit. The router asks
// for "what should we anchor for this query against this repo?" and the
// learner returns a ranked list of candidate file paths drawn from
//
//	1. a static portfolio (cold-start prior, language-agnostic),
//	2. a per-repo learned weight table keyed on (repo, pattern), and
//	3. an exploration arm that probes untried patterns with probability ε
//
// blended into a single score. Outcomes are observed via two existing
// MCP signals — dev_feedback (explicit) and memory_save_* (implicit) —
// and back-propagated into the weight tables, so the system gets better
// at picking anchors the longer it runs in a given repo.
//
// Architecture parallels memory.Store's FP-centroid system intentionally:
// store under Redis hashes via the shared client, do incremental online
// updates on observed events, never block dev_context on missing
// learning data (cold start = static prior, identical to v0 behaviour).
//
// Phase rollout (all in one tree):
//
//	Phase 1 — observation logging       (RecordObservation)
//	Phase 2 — per-repo posterior scoring (Decide consults stats)
//	Phase 3 — file-level discovery       (RewardMemorySave promotes paths)
//	Phase 4 — ε-greedy bandit policy     (Decide injects an explore slot)
//
// All phases ship together; phases 2-4 are no-ops on cold-start repos
// and gradually take over as observation data accumulates.
package anchorlearn
