package prompt

// DevPrompt is the structured context returned to Claude via MCP.
// Field order matters — Claude attends more to fields it sees first.
//
// QueryID is surfaced as a top-level field (mirroring RetrievalTrace.QueryID)
// so the agent can echo it back via dev_feedback without digging through
// retrieval_trace. This is the join key for the heuristics feedback loop.
type DevPrompt struct {
	QueryID           string                 `json:"query_id,omitempty"`
	Instructions      string                 `json:"instructions,omitempty"`
	Intent            string                 `json:"intent,omitempty"`
	ContextConfidence float64                `json:"context_confidence,omitempty"`
	MemoryCoverage    string                 `json:"memory_coverage,omitempty"`
	PrimaryContext    []PrimaryContextEntry  `json:"primary_context,omitempty"`
	Decisions         []DecisionContextEntry `json:"decisions,omitempty"`
	Symbols           []string               `json:"symbols,omitempty"`
	CallChain         *CallChain             `json:"call_chain,omitempty"`
	Graph             *GraphLinks            `json:"graph,omitempty"`
	ImpactRadius      []string               `json:"impact_radius,omitempty"`
	CodeSnippets      []Snippet              `json:"code_snippets,omitempty"`
	Documentation     []DocEntry             `json:"documentation,omitempty"`
	ModelHint         *ModelHint             `json:"model_hint,omitempty"`
	QueryPlan         *PlanDebug             `json:"query_plan,omitempty"`
	RetrievalTrace    *RetrievalTrace        `json:"retrieval_trace,omitempty"` // debug/observability only
}

// PlanDebug exposes the structured QueryPlan that drove retrieval. It's
// populated for debugging/observability so the must/should/exclude/
// hints/phrases — and whether the must-term was auto-anchored from the
// rarest query token rather than supplied by the caller — are visible
// in the MCP response without grepping stderr. Distinct from
// router.QueryPlan (different package) to avoid an import cycle.
//
// Source records where the plan came from:
//
//	"agent"  — the MCP caller (e.g. Claude) filled in dev_context's
//	           `plan` argument; that plan was sanitized and used.
//	"auto"   — no plan was supplied; must_terms (if any) came purely
//	           from ensureMustAnchor's rarest-token promotion.
//
// Empty plans surface as Source="agent" with no terms (caller supplied
// an empty object) vs Source="auto" with no terms (caller skipped the
// field entirely) — useful when debugging why retrieval is weak.
type PlanDebug struct {
	Source       string   `json:"source"`
	MustTerms    []string `json:"must_terms,omitempty"`
	ShouldTerms  []string `json:"should_terms,omitempty"`
	ExcludeTerms []string `json:"exclude_terms,omitempty"`
	Phrases      []string `json:"phrases,omitempty"`
	ContextHints []string `json:"context_hints,omitempty"`
	AutoAnchored bool     `json:"auto_anchored,omitempty"`
}

// PrimaryContextEntry is a flat, high-signal memory entry.
// Replaces the nested StructuredMemories for easier Claude consumption.
type PrimaryContextEntry struct {
	Type       string  `json:"type"`
	Name       string  `json:"name"`
	File       string  `json:"file,omitempty"`
	Summary    string  `json:"summary"`
	Details    string  `json:"details,omitempty"`
	Confidence float64 `json:"confidence"`
	Stale      bool    `json:"stale,omitempty"`
}

// ModelHint recommends which model to use for this query.
type ModelHint struct {
	Model  string `json:"model"`
	Reason string `json:"reason"`
}

// StructuredMemories is retained internally for buildAllMemories but
// no longer serialized into the prompt. Converted to PrimaryContext entries and Decisions entries.
type StructuredMemories struct {
	Files     []FileMemoryHit
	Functions []FuncMemoryHit
	Flows     []FlowMemoryHit
	Decisions []DecisionMemoryHit
}

// Similarity (Sim) on each typed hit is the cosine similarity between the
// query embedding and the stored memory embedding, clamped to [0,1]. It's
// the only honest relevance signal we have at the prompt-build layer; both
// per-entry Confidence and the top-level ContextConfidence/signals are now
// derived from it instead of from a fixed source-quality lookup.
type FileMemoryHit struct {
	Path       string
	Purpose    string
	KeySymbols string
	Source     string
	Stale      bool
	Sim        float64
	Key        string
}

type FuncMemoryHit struct {
	Name    string
	File    string
	Purpose string
	Callers string
	Callees string
	Source  string
	Stale   bool
	Sim     float64
	Key     string
}

type FlowMemoryHit struct {
	Name        string
	Purpose     string
	Files       string
	EntryPoints string
	Source      string
	Stale       bool
	Sim         float64
	Key         string
}

type DecisionMemoryHit struct {
	Name         string
	DecisionType string
	Decision     string
	Rationale    string
	Alternatives string
	Constraint   string
	Scope        string
	Files        string
	Status       string
	Supersedes   string
	SupersededBy string
	Source       string
}

// DecisionContextEntry is a decision returned in the DevPrompt for Claude's attention.
type DecisionContextEntry struct {
	Name         string  `json:"name"`
	DecisionType string  `json:"decision_type"`
	Decision     string  `json:"decision"`
	Rationale    string  `json:"rationale"`
	Alternatives string  `json:"alternatives,omitempty"`
	Constraint   string  `json:"constraint,omitempty"`
	Scope        string  `json:"scope,omitempty"`
	Status       string  `json:"status,omitempty"`
	Supersedes   string  `json:"supersedes,omitempty"`
	SupersededBy string  `json:"superseded_by,omitempty"`
	Confidence   float64 `json:"confidence"`
}

// CallChain shows upstream callers and downstream callees for key symbols.
type CallChain struct {
	Upstream   []CallEdge `json:"upstream,omitempty"`
	Downstream []CallEdge `json:"downstream,omitempty"`
}

type CallEdge struct {
	From     string `json:"from"`
	To       string `json:"to"`
	FilePath string `json:"file,omitempty"`
}

// GraphLinks captures non-CALLS relationships from the knowledge graph.
type GraphLinks struct {
	Importers []GraphEdge `json:"importers,omitempty"`
	Extends   []GraphEdge `json:"extends,omitempty"`
	Methods   []GraphEdge `json:"methods,omitempty"`
	Siblings  []string    `json:"siblings,omitempty"`
}

// GraphEdge is a generic directed edge with optional file location.
type GraphEdge struct {
	From     string `json:"from"`
	To       string `json:"to"`
	FilePath string `json:"file,omitempty"`
}

type Snippet struct {
	File    string `json:"file"`
	Lines   string `json:"lines,omitempty"`
	Content string `json:"content"`
}

// DocEntry is one piece of external documentation / tracker context
// returned by a non-code retrieval tool (cmdocs PageIndex docs, GitLab
// issues/MRs, and later ClickUp tasks). It is deliberately generic so
// every such tool shares one prompt channel (DevPrompt.Documentation),
// grouped by Source. Code-bearing tools use CodeSnippets instead.
type DocEntry struct {
	Source     string `json:"source"`               // "cmdocs" | "gitlab" | ...
	ID         string `json:"id,omitempty"`         // doc_id / issue iid / task id
	Title      string `json:"title,omitempty"`      // doc/issue/task title
	Collection string `json:"collection,omitempty"` // doc collection / project / list
	URL        string `json:"url,omitempty"`        // canonical link when available
	Content    string `json:"content"`              // section/issue/task body (capped)
}

// RetrievalTrace captures detailed observability data from each stage of the retrieval pipeline.
// Used for debugging, measuring quality, and understanding why specific context was selected.
type RetrievalTrace struct {
	QueryID   string `json:"query_id"`
	Query     string `json:"query"`
	Timestamp int64  `json:"timestamp"` // unix milliseconds

	// Intent classification result. Detection itself is keyword-only and
	// effectively free, so it doesn't get its own StageTrace; surfacing it
	// here lets debug consumers see which budget/trim profile the query
	// got mapped to (debug/trace/refactor/explore/general).
	Intent string `json:"intent,omitempty"`

	// HeuristicProfileID is the stable hash ID of the budget/trim profile
	// that drove this retrieval. The same ID joins reward rows and trace
	// hashes across the heuristics:reward:* and feedback:trace:* keys.
	HeuristicProfileID string `json:"heuristic_profile_id,omitempty"`

	// Repo and TopicID identify the per-(intent, repo, topic) heuristic
	// bucket this query was scored against. Filled when topics are
	// enabled (which they are by default — see TopicsEnabled). Used by
	// dev_feedback / repeat-detection to route the reward back to the
	// exact bucket whose profile generated the retrieval.
	//
	// TopicID == "*" (IntentGlobalTopic) means this query landed on
	// the intent-global fallback — either topics are off, the bucket
	// is below the sample floor, or embedding failed. The reward
	// still has to credit the right bucket to drive cold-bucket
	// warm-up, so we always persist both fields when known.
	Repo    string `json:"repo,omitempty"`
	TopicID string `json:"topic_id,omitempty"`

	// TopicLabel is the human-readable kebab-case tag derived from the
	// seed query that created this topic (e.g. "redis-session-cache").
	// Purely cosmetic — nothing keys on it. Empty when the topic is the
	// intent-global sentinel or pre-dates the labelling feature.
	TopicLabel string `json:"topic_label,omitempty"`

	// HeuristicFromTopic records whether the profile was sourced from
	// the bucket-specific profile (true) or from the intent-global
	// surface (false). Lets the dashboard distinguish "hot bucket
	// using its own tuning" from "cold bucket inheriting the global"
	// at a glance.
	HeuristicFromTopic bool `json:"heuristic_from_topic,omitempty"`

	// RepeatedExplorationOf is set when the implicit-repeat detector
	// finds a semantically-similar prior query within the lookback
	// window — value is the prior query_id. The prior query's reward
	// has been retroactively penalised; this trace's only role is to
	// make the chain debuggable from any single retrieve_debug call.
	RepeatedExplorationOf string `json:"repeated_exploration_of,omitempty"`

	PlannerStage *StageTrace `json:"planner_stage,omitempty"`
	SearchStage  *StageTrace `json:"search_stage,omitempty"`
	GraphStage   *StageTrace `json:"graph_stage,omitempty"`
	RerankStage  *StageTrace `json:"rerank_stage,omitempty"`
	PackingStage *StageTrace `json:"packing_stage,omitempty"`

	// ToolStages holds one StageTrace per external retrieval tool that
	// ran in the parallel fan-out (cmdocs, gitlab, …), keyed by the
	// source Name(). Records latency, docs returned, and any non-fatal
	// error so a slow/failing tool is visible without blocking the query.
	ToolStages map[string]*StageTrace `json:"tool_stages,omitempty"`

	// SourceExplore records which external source the per-source breadth
	// bandit sampled on this query (and at what breadth), so dev_feedback
	// and repeat-detection can route the reward to that (intent, repo,
	// topic, source) cell. Nil when no source explored.
	SourceExplore *SourceExploreRec `json:"source_explore,omitempty"`

	FinalTokens    int   `json:"final_tokens"`
	TotalLatencyMs int64 `json:"total_latency_ms"`

	// Signal breakdown for final scoring
	Signals map[string]float64 `json:"signals,omitempty"`

	// Outcome is the per-query joined "span" record. Decision-side fields
	// are populated at end of HandleQuery; feedback-side fields are filled
	// later by dev_feedback or repeat-detection. This is what makes
	// dashboards, replay, regression, and per-knob attribution trivial.
	Outcome *RetrievalOutcome `json:"outcome,omitempty"`
}

// SourceExploreRec is the trace-side mirror of the source-breadth
// bandit's explore record: the source sampled this query, the served
// breadth (Val), and the cell's current best (Base).
type SourceExploreRec struct {
	Source string `json:"source"`
	Val    int    `json:"val"`
	Base   int    `json:"base"`
}

// RetrievalOutcome is the per-query joined "span" record. Filled in two
// phases:
//
//	Decision side — at end of HandleQuery, before the response is written.
//	  Always present. Captures what we returned and how much of the budget
//	  we actually used.
//
//	Feedback side — later, by dev_feedback or repeat-detection. Zero until
//	  signal arrives. Captures whether the agent had to do follow-up work.
type RetrievalOutcome struct {
	// Decision side
	PromptTokens       int     `json:"prompt_tokens"`
	FilesReturned      int     `json:"files_returned"`
	SymbolsReturned    int     `json:"symbols_returned"`
	SnippetsReturned   int     `json:"snippets_returned"`
	DocsReturned       int     `json:"docs_returned,omitempty"`
	TrimmedFiles       int     `json:"trimmed_files"`
	BudgetUsedFraction float64 `json:"budget_used_fraction"`
	LatencyMs          int     `json:"latency_ms"`

	// Feedback side (omitempty until signal arrives)
	AdditionalFiles int     `json:"additional_files,omitempty"`
	Revisits        int     `json:"revisits,omitempty"`
	RepeatQuery     bool    `json:"repeat_query,omitempty"`
	ExplicitSuccess bool    `json:"explicit_success,omitempty"`
	RawReward       float64 `json:"raw_reward,omitempty"`
	AdjustedReward  float64 `json:"adjusted_reward,omitempty"`
	FeedbackSource  string  `json:"feedback_source,omitempty"` // "explicit" | "implicit_repeat"
	FeedbackAt      int64   `json:"feedback_at,omitempty"`     // unix-ms when feedback joined
}

// StageTrace captures metrics and descriptions for a single stage of retrieval.
type StageTrace struct {
	LatencyMs   int64  `json:"latency_ms"`
	Description string `json:"description,omitempty"`

	// Numeric results from this stage
	CandidatesIn  int `json:"candidates_in,omitempty"`  // how many we started with
	CandidatesOut int `json:"candidates_out,omitempty"` // how many passed this stage

	// Stage-specific details
	Details map[string]interface{} `json:"details,omitempty"`

	// Any errors that occurred but didn't block (degradation)
	Warnings []string `json:"warnings,omitempty"`
}
