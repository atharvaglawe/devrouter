package mcp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"strings"

	"github.com/atharva-ag/devrouter/internal/router"
)

type jsonRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type jsonRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type Server struct {
	router *router.Router
}

func NewServer(r *router.Router) *Server {
	return &Server{router: r}
}

func (s *Server) Run() error {
	reader := bufio.NewReader(os.Stdin)
	encoder := json.NewEncoder(os.Stdout)

	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return fmt.Errorf("read stdin: %w", err)
		}

		var req jsonRPCRequest
		if err := json.Unmarshal(line, &req); err != nil {
			log.Printf("[mcp] invalid json: %v", err)
			continue
		}

		resp := s.handle(req)
		if resp != nil {
			if err := encoder.Encode(resp); err != nil {
				log.Printf("[mcp] write error: %v", err)
			}
		}
	}
}

func (s *Server) handle(req jsonRPCRequest) *jsonRPCResponse {
	switch req.Method {

	case "initialize":
		return s.success(req.ID, map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities": map[string]any{
				"tools": map[string]any{},
			},
			"serverInfo": map[string]any{
				"name":    "devrouter",
				"version": "0.2.0",
			},
		})

	case "notifications/initialized":
		return nil

	case "tools/list":
		return s.success(req.ID, map[string]any{
			"tools": s.toolDefinitions(),
		})

	case "tools/call":
		return s.handleToolCall(req)

	default:
		return s.errResp(req.ID, -32601, "method not found: "+req.Method)
	}
}

func (s *Server) toolDefinitions() []map[string]any {
	return []map[string]any{
		{
			"name": "dev_context",
			"description": "Retrieve structured repository context and dev memories for a developer question. " +
				"Returns symbols, call chains, graph relationships, code snippets, and any saved memories relevant to the query. " +
				"Call this FIRST when starting work on any task to see what's already known. " +
				"Always supply a structured `plan` alongside the raw query — you have conversation context that devrouter doesn't, " +
				"and a well-formed plan dramatically improves retrieval. See the `plan` schema below for shape, semantics, and caps. " +
				"The response contains a top-level `query_id`. After your task is complete (or when you've finished " +
				"acting on the returned context), call `dev_feedback` with that exact `query_id` and the count of " +
				"additional files you had to read beyond what was returned. This closes the feedback loop that lets " +
				"devrouter tune its retrieval heuristics over time.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{"type": "string", "description": "The developer question or topic, in natural language"},
					"repo":  map[string]any{"type": "string", "description": "Repository name (optional, uses default if omitted)"},
					"plan":  devContextPlanSchema(),
				},
				"required": []string{"query"},
			},
		},
		{
			"name": "retrieve_debug",
			"description": "Debug retrieval pipeline: returns the same context as dev_context plus detailed latency, ranking signals, and stage-by-stage breakdown. " +
				"Use this to understand why specific memories were selected, what the retrieval pipeline did, and where time was spent. " +
				"Shows the active plan (and its source — `agent` if you supplied one, `auto` if devrouter only auto-anchored), " +
				"vector search scores, graph expansion depth, and final token count.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{"type": "string", "description": "The developer question or topic"},
					"repo":  map[string]any{"type": "string", "description": "Repository name (optional, uses default if omitted)"},
					"plan":  devContextPlanSchema(),
				},
				"required": []string{"query"},
			},
		},
		{
			"name": "memory_save_file",
			"description": "Save what you learned about a source file. Call this AFTER you read, explore, or debug a file and now understand its role. " +
				"Write the purpose as if explaining to a new developer: what does this file do, why does it exist, what patterns does it use, any gotchas? " +
				"Good: 'Entry point for the consumer service. Bootstraps Kafka consumers, registers /health endpoint, handles SIGTERM graceful shutdown. " +
				"Config comes from env vars via config.go. Gotcha: consumer.Start() blocks, so it runs in a goroutine.' " +
				"Bad: 'Defines 17 functions: main, startServer...' (that's just a symbol list, not understanding).",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"repo":        map[string]any{"type": "string", "description": "Repository name"},
					"path":        map[string]any{"type": "string", "description": "File path relative to repo root"},
					"purpose":     map[string]any{"type": "string", "description": "What this file does, why it exists, its role in the system, patterns it uses, any gotchas"},
					"key_symbols": map[string]any{"type": "string", "description": "Key exported functions/types (optional, comma-separated)"},
					"scope":       map[string]any{"type": "string", "description": "Scope override. Use \"global\" to share across all branches. Omit to auto-detect: if the file differs from the release branch, scope is set to the current branch; otherwise \"global\"."},
				},
				"required": []string{"repo", "path", "purpose"},
			},
		},
		{
			"name": "memory_save_func",
			"description": "Save what you learned about a function or method. Call this AFTER you trace through a function's logic and understand what it really does. " +
				"Write the purpose as if explaining to a colleague: what does it do, what are the important params, what are the edge cases, what does it return? " +
				"Good: 'Resolves a provider by short name. Looks up the registry map, falls back to default provider if not found. " +
				"Returns ErrProviderNotFound if name is empty. Called during request handling to dispatch to the right provider.' " +
				"Bad: 'Function in providers/factory.go' (that's just location, not understanding).",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"repo":    map[string]any{"type": "string", "description": "Repository name"},
					"name":    map[string]any{"type": "string", "description": "Function or method name"},
					"file":    map[string]any{"type": "string", "description": "Source file path"},
					"purpose": map[string]any{"type": "string", "description": "What this function does, its params/returns, edge cases, why it's called, any gotchas"},
					"callers": map[string]any{"type": "string", "description": "Key callers you discovered (comma-separated, optional)"},
					"callees": map[string]any{"type": "string", "description": "Key functions it calls (comma-separated, optional)"},
					"scope":   map[string]any{"type": "string", "description": "Scope override. Use \"global\" to share across all branches. Omit to auto-detect: if the file differs from the release branch, scope is set to the current branch; otherwise \"global\"."},
				},
				"required": []string{"repo", "name", "file", "purpose"},
			},
		},
		{
			"name": "memory_save_flow",
			"description": "Save what you learned about an end-to-end flow or integration pattern. Call this AFTER you trace a full flow across multiple files. " +
				"Describe the sequence: what triggers it, what steps happen, which files are involved, where does data flow? " +
				"Good: 'Adding a new content provider: 1) Create provider struct implementing IProvider in cmpkg/providers/, " +
				"2) Register short name in shortnames/constants.go, 3) Add case in providerfactory.go switch, " +
				"4) Wire config in providerconfig/, 5) Add to enabled list in contentservice config. " +
				"Entry: HTTP request → ContentService.Serve() → ProviderFactory.Get() → YourProvider.Fetch()' " +
				"Bad: 'Flow involving 5 files' (that's a count, not understanding).",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"repo":         map[string]any{"type": "string", "description": "Repository name"},
					"name":         map[string]any{"type": "string", "description": "Descriptive flow name (e.g. 'add-content-provider', 'kafka-message-processing')"},
					"purpose":      map[string]any{"type": "string", "description": "Step-by-step description of the flow: what triggers it, what happens, which files, where data flows"},
					"files":        map[string]any{"type": "string", "description": "Key file paths involved (comma-separated)"},
					"entry_points": map[string]any{"type": "string", "description": "Entry point functions that kick off this flow (comma-separated)"},
					"query_id":     map[string]any{"type": "string", "description": "Optional query_id from dev_context. When supplied, devrouter reuses the stored query plan to filter noisy graph edges before snapshotting the flow graph."},
					"scope":        map[string]any{"type": "string", "description": "Scope override. Use \"global\" to share across all branches. Omit to auto-detect: if any file differs from the release branch, scope is set to the current branch; otherwise \"global\"."},
				},
				"required": []string{"repo", "name", "purpose"},
			},
		},
		{
			"name": "memory_populate",
			"description": "Bootstrap structural skeleton from codegraph index (file paths, function names, callers/callees). " +
				"This creates bare-bones entries with source=auto. Use this once when onboarding a new repo, then enrich memories " +
				"organically using memory_save_file/func/flow as you explore. Auto entries are never overwritten by agent-written ones.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"repo":      map[string]any{"type": "string", "description": "Repository name to populate structural skeleton for"},
					"max_files": map[string]any{"type": "integer", "description": "Max files to index (default: 10000)"},
					"max_funcs": map[string]any{"type": "integer", "description": "Max functions to index (default: 50000)"},
					"max_flows": map[string]any{"type": "integer", "description": "Max flows to index (default: 500)"},
				},
				"required": []string{"repo"},
			},
		},
		{
			"name": "decision_save",
			"description": "Save a developer decision made during a Claude session. " +
				"Use this when you or the developer make a deliberate architectural, refactoring, optimization, " +
				"coding standard, constraint, or tradeoff decision. These decisions will be surfaced in future " +
				"dev_context calls so they aren't forgotten or contradicted.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"repo":           map[string]any{"type": "string", "description": "Repository name"},
					"name":           map[string]any{"type": "string", "description": "Slug identifier for this decision (e.g. 'use-redis-not-postgres')"},
					"decision_type":  map[string]any{"type": "string", "description": "One of: refactor, optimization, coding_standard, architecture, constraint, tradeoff"},
					"decision":       map[string]any{"type": "string", "description": "What was decided"},
					"rationale":      map[string]any{"type": "string", "description": "Why this decision was made"},
					"alternatives":   map[string]any{"type": "string", "description": "What alternatives were rejected (optional)"},
					"constraint":     map[string]any{"type": "string", "description": "What constraint forced or shaped this decision (optional)"},
					"decision_scope": map[string]any{"type": "string", "description": "Where this decision applies: service layer, all constructors, file path, etc. (optional)"},
					"files":          map[string]any{"type": "string", "description": "Comma-separated affected file paths (optional)"},
					"scope":          map[string]any{"type": "string", "description": "Scope override. Use \"global\" to share across all branches. Omit to auto-detect: if any file differs from the release branch, scope is set to the current branch; otherwise \"global\"."},
				},
				"required": []string{"repo", "name", "decision_type", "decision", "rationale"},
			},
		},
		{
			"name": "decision_list",
			"description": "List saved developer decisions for a repository. " +
				"Optionally filter by decision_type (e.g. 'architecture'), scope (substring match), or files (overlap match). " +
				"Use this to review what decisions are already recorded before proposing changes.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"repo":          map[string]any{"type": "string", "description": "Repository name"},
					"decision_type": map[string]any{"type": "string", "description": "Filter by type: refactor, optimization, coding_standard, architecture, constraint, tradeoff (optional)"},
					"scope":         map[string]any{"type": "string", "description": "Filter by scope substring (optional)"},
					"files":         map[string]any{"type": "string", "description": "Filter by file overlap (comma-separated, optional)"},
				},
				"required": []string{"repo"},
			},
		},
		{
			"name": "decision_supersede",
			"description": "Mark an existing decision as superseded by a newer decision. " +
				"Call this AFTER saving the new decision with decision_save. " +
				"The old decision is preserved for lineage — never deleted. " +
				"It will no longer appear as an active decision in dev_context, " +
				"but is shown as lineage when the new decision is retrieved.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"repo":     map[string]any{"type": "string", "description": "Repository name"},
					"old_name": map[string]any{"type": "string", "description": "Slug of the decision being superseded"},
					"new_name": map[string]any{"type": "string", "description": "Slug of the new decision that supersedes it (must already be saved)"},
				},
				"required": []string{"repo", "old_name", "new_name"},
			},
		},
		{
			"name": "dev_feedback",
			"description": "Report retrieval quality after acting on a dev_context response. Call this when your task " +
				"is complete (or when you've finished acting on what dev_context returned). " +
				"`query_id` should be the value from the top of the dev_context response that drove this work — " +
				"if you forget it, devrouter falls back to the most recent dev_context call on this connection " +
				"(best-effort). `additional_files` is the count of files you had to read beyond what dev_context " +
				"returned (zero is best). " +
				"If a saved flow drove your work, pass `flow_id` as \"{repo}/{flow_name}\" so the dashboard can " +
				"score each file in the flow as useful or dead weight. Use `missing_files` to report files you " +
				"needed but the flow didn't include — they appear in the dashboard as 'augmented' nodes. " +
				"This is the primary signal that lets devrouter tune its budget/trim heuristics over time.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query_id":         map[string]any{"type": "string", "description": "query_id from the dev_context response that drove this task (recommended; falls back to last call if omitted)"},
					"additional_files": map[string]any{"type": "integer", "description": "Count of files you had to read beyond what dev_context returned"},
					"revisited_files":  map[string]any{"type": "integer", "description": "Count of files you read more than once during the task (optional)"},
					"file_paths":       map[string]any{"type": "string", "description": "File paths you ended up reading (comma-separated, optional; used to detect over-aggressive trimming)"},
					"success":          map[string]any{"type": "boolean", "description": "Whether the task was completed successfully (optional)"},
					"flow_id":          map[string]any{"type": "string", "description": "Saved flow that drove this task, formatted \"{repo}/{flow_name}\" (optional; sourced from a flow-typed entry in primary_context)"},
					"missing_files":    map[string]any{"type": "string", "description": "Files you needed but the matched flow didn't include (comma-separated, optional; surfaces as augmented nodes on the dashboard)"},
				},
				"required": []string{"additional_files"},
			},
		},
		{
			"name": "dev_feedback_stats",
			"description": "Inspect the per-intent reward distribution and current heuristic profiles. " +
				"Useful for verifying the bandit is converging, checking feedback coverage, and " +
				"reviewing recent profile promotions / rollbacks.",
			"inputSchema": map[string]any{
				"type":       "object",
				"properties": map[string]any{},
				"required":   []string{},
			},
		},
		{
			"name": "dev_heuristics_reset",
			"description": "Roll one (or all) heuristic profiles back to the frozen default snapshot. " +
				"Use this for incident recovery when the bandit has settled on a regression. " +
				"Restoration is logged to heuristics:history for audit.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"intent": map[string]any{"type": "string", "description": "Specific intent to reset: debug, explore, trace, refactor, general. Omit or pass \"all\" to reset every intent."},
				},
				"required": []string{},
			},
		},
	}
}

func (s *Server) handleToolCall(req jsonRPCRequest) *jsonRPCResponse {
	var params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return s.errResp(req.ID, -32602, "invalid params: "+err.Error())
	}

	switch params.Name {

	case "dev_context":
		var args struct {
			Query string                  `json:"query"`
			Repo  string                  `json:"repo"`
			Plan  *devContextPlanArgument `json:"plan,omitempty"`
		}
		if err := json.Unmarshal(params.Arguments, &args); err != nil {
			return s.errResp(req.ID, -32602, "invalid arguments: "+err.Error())
		}
		result, err := s.router.HandleQueryWithPlan(args.Query, args.Repo, args.Plan.toRouterPlan())
		if err != nil {
			return s.errResp(req.ID, -32000, err.Error())
		}
		text, _ := json.MarshalIndent(result, "", "  ")
		return s.success(req.ID, map[string]any{
			"content": []map[string]any{
				{"type": "text", "text": string(text)},
			},
		})

	case "retrieve_debug":
		var args struct {
			Query string                  `json:"query"`
			Repo  string                  `json:"repo"`
			Plan  *devContextPlanArgument `json:"plan,omitempty"`
		}
		if err := json.Unmarshal(params.Arguments, &args); err != nil {
			return s.errResp(req.ID, -32602, "invalid arguments: "+err.Error())
		}
		result, err := s.router.HandleQueryWithPlan(args.Query, args.Repo, args.Plan.toRouterPlan())
		if err != nil {
			return s.errResp(req.ID, -32000, err.Error())
		}
		// Format with detailed trace information
		var sb strings.Builder
		sb.WriteString("=== RETRIEVAL TRACE ===\n\n")
		sb.WriteString(fmt.Sprintf("Query ID: %s\n", result.RetrievalTrace.QueryID))
		sb.WriteString(fmt.Sprintf("Query: %s\n", result.RetrievalTrace.Query))
		sb.WriteString(fmt.Sprintf("Total Latency: %dms\n", result.RetrievalTrace.TotalLatencyMs))
		sb.WriteString(fmt.Sprintf("Final Tokens: %d\n\n", result.RetrievalTrace.FinalTokens))

		// Stage breakdown
		sb.WriteString("--- STAGE BREAKDOWN ---\n")
		if result.RetrievalTrace.PlannerStage != nil {
			sb.WriteString(fmt.Sprintf("Planner: %dms | %s\n",
				result.RetrievalTrace.PlannerStage.LatencyMs,
				result.RetrievalTrace.PlannerStage.Description))
			// Show warnings if planner had issues
			if len(result.RetrievalTrace.PlannerStage.Warnings) > 0 {
				sb.WriteString("  ⚠ Warnings:\n")
				for _, w := range result.RetrievalTrace.PlannerStage.Warnings {
					sb.WriteString(fmt.Sprintf("    - %s\n", w))
				}
			}
		}
		if result.RetrievalTrace.SearchStage != nil {
			sb.WriteString(fmt.Sprintf("Memory Search: %dms | %d hits\n",
				result.RetrievalTrace.SearchStage.LatencyMs,
				result.RetrievalTrace.SearchStage.CandidatesOut))
		}
		if result.RetrievalTrace.GraphStage != nil {
			sb.WriteString(fmt.Sprintf("Graph Expansion: %dms | %d edges found\n",
				result.RetrievalTrace.GraphStage.LatencyMs,
				result.RetrievalTrace.GraphStage.Details["edges_found"]))
		}
		if result.RetrievalTrace.PackingStage != nil {
			sb.WriteString(fmt.Sprintf("Context Packing: %d primary + %d decisions + %d snippets\n",
				len(result.PrimaryContext),
				len(result.Decisions),
				len(result.CodeSnippets)))
		}

		// Stage details (why each stage filtered/produced candidates)
		sb.WriteString("\n--- STAGE DETAILS ---\n")
		if result.RetrievalTrace.PlannerStage != nil && result.RetrievalTrace.PlannerStage.Details != nil {
			if query, ok := result.RetrievalTrace.PlannerStage.Details["query"].(string); ok {
				sb.WriteString(fmt.Sprintf("Query: %q\n", query))
			}
		}
		if result.RetrievalTrace.SearchStage != nil && result.RetrievalTrace.SearchStage.Details != nil {
			if branch, ok := result.RetrievalTrace.SearchStage.Details["branch"].(string); ok {
				sb.WriteString(fmt.Sprintf("Current Branch: %s\n", branch))
			}
		}
		if result.RetrievalTrace.GraphStage != nil && result.RetrievalTrace.GraphStage.Details != nil {
			sb.WriteString(fmt.Sprintf("Graph: %d seed symbols → %d traced\n",
				result.RetrievalTrace.GraphStage.CandidatesIn,
				result.RetrievalTrace.GraphStage.CandidatesOut))
		}

		// Signal scores
		sb.WriteString("\n--- RANKING SIGNALS ---\n")
		for signal, score := range result.RetrievalTrace.Signals {
			sb.WriteString(fmt.Sprintf("%s: %.2f\n", signal, score))
		}

		// Full context as JSON for detailed inspection
		sb.WriteString("\n--- FULL CONTEXT (JSON) ---\n")
		fullJSON, _ := json.MarshalIndent(result, "", "  ")
		sb.WriteString(string(fullJSON))

		return s.success(req.ID, map[string]any{
			"content": []map[string]any{
				{"type": "text", "text": sb.String()},
			},
		})

	case "memory_save_file":
		var args struct {
			Repo    string `json:"repo"`
			Path    string `json:"path"`
			Purpose string `json:"purpose"`
			Scope   string `json:"scope"`
		}
		if err := json.Unmarshal(params.Arguments, &args); err != nil {
			return s.errResp(req.ID, -32602, "invalid arguments: "+err.Error())
		}
		if err := s.router.SaveFileMemory(args.Repo, args.Path, args.Purpose, args.Scope); err != nil {
			return s.errResp(req.ID, -32000, err.Error())
		}
		return s.success(req.ID, map[string]any{
			"content": []map[string]any{
				{"type": "text", "text": fmt.Sprintf("Saved file memory for %s", args.Path)},
			},
		})

	case "memory_save_func":
		var args struct {
			Repo    string `json:"repo"`
			Name    string `json:"name"`
			File    string `json:"file"`
			Purpose string `json:"purpose"`
			Callers string `json:"callers"`
			Callees string `json:"callees"`
			Scope   string `json:"scope"`
		}
		if err := json.Unmarshal(params.Arguments, &args); err != nil {
			return s.errResp(req.ID, -32602, "invalid arguments: "+err.Error())
		}
		if err := s.router.SaveFuncMemory(args.Repo, args.Name, args.File, args.Purpose, args.Callers, args.Callees, args.Scope); err != nil {
			return s.errResp(req.ID, -32000, err.Error())
		}
		return s.success(req.ID, map[string]any{
			"content": []map[string]any{
				{"type": "text", "text": fmt.Sprintf("Saved func memory for %s", args.Name)},
			},
		})

	case "memory_save_flow":
		var args struct {
			Repo        string `json:"repo"`
			Name        string `json:"name"`
			Purpose     string `json:"purpose"`
			Files       string `json:"files"`
			EntryPoints string `json:"entry_points"`
			QueryID     string `json:"query_id"`
			Scope       string `json:"scope"`
		}
		if err := json.Unmarshal(params.Arguments, &args); err != nil {
			return s.errResp(req.ID, -32602, "invalid arguments: "+err.Error())
		}
		if err := s.router.SaveFlowMemory(args.Repo, args.Name, args.Purpose, args.Files, args.EntryPoints, args.Scope, args.QueryID); err != nil {
			return s.errResp(req.ID, -32000, err.Error())
		}
		return s.success(req.ID, map[string]any{
			"content": []map[string]any{
				{"type": "text", "text": fmt.Sprintf("Saved flow memory for %s", args.Name)},
			},
		})

	case "memory_populate":
		var args struct {
			Repo     string `json:"repo"`
			MaxFiles int    `json:"max_files"`
			MaxFuncs int    `json:"max_funcs"`
			MaxFlows int    `json:"max_flows"`
		}
		if err := json.Unmarshal(params.Arguments, &args); err != nil {
			return s.errResp(req.ID, -32602, "invalid arguments: "+err.Error())
		}
		if err := s.router.PopulateMemories(args.Repo, args.MaxFiles, args.MaxFuncs, args.MaxFlows); err != nil {
			return s.errResp(req.ID, -32000, err.Error())
		}
		return s.success(req.ID, map[string]any{
			"content": []map[string]any{
				{"type": "text", "text": fmt.Sprintf("Auto-populated memories for repo %s", args.Repo)},
			},
		})

	case "decision_save":
		var args struct {
			Repo          string `json:"repo"`
			Name          string `json:"name"`
			DecisionType  string `json:"decision_type"`
			Decision      string `json:"decision"`
			Rationale     string `json:"rationale"`
			Alternatives  string `json:"alternatives"`
			Constraint    string `json:"constraint"`
			DecisionScope string `json:"decision_scope"`
			Files         string `json:"files"`
			Scope         string `json:"scope"`
		}
		if err := json.Unmarshal(params.Arguments, &args); err != nil {
			return s.errResp(req.ID, -32602, "invalid arguments: "+err.Error())
		}
		warnings, err := s.router.SaveDecisionMemory(
			args.Repo, args.Name, args.DecisionType, args.Decision,
			args.Rationale, args.Alternatives, args.Constraint, args.DecisionScope, args.Files, args.Scope,
		)
		if err != nil {
			return s.errResp(req.ID, -32000, err.Error())
		}
		text := fmt.Sprintf("Saved decision %q (type=%s)", args.Name, args.DecisionType)
		if len(warnings) > 0 {
			text += "\n\nWARNINGS:\n" + strings.Join(warnings, "\n")
		}
		return s.success(req.ID, map[string]any{
			"content": []map[string]any{
				{"type": "text", "text": text},
			},
		})

	case "decision_list":
		var args struct {
			Repo         string `json:"repo"`
			DecisionType string `json:"decision_type"`
			Scope        string `json:"scope"`
			Files        string `json:"files"`
		}
		if err := json.Unmarshal(params.Arguments, &args); err != nil {
			return s.errResp(req.ID, -32602, "invalid arguments: "+err.Error())
		}
		if args.Repo == "" {
			return s.errResp(req.ID, -32602, "repo is required")
		}
		hits := s.router.ListDecisionMemory(args.Repo, args.DecisionType, args.Scope, args.Files)
		if len(hits) == 0 {
			return s.success(req.ID, map[string]any{
				"content": []map[string]any{
					{"type": "text", "text": "No decisions found."},
				},
			})
		}
		// Format each hit as a concise text block
		var sb strings.Builder
		for _, h := range hits {
			status := h.Fields["status"]
			if status == "" {
				status = "active"
			}
			label := "[ACTIVE]"
			if status == "superseded" {
				label = "[SUPERSEDED]"
			}
			sb.WriteString(fmt.Sprintf("%s %s (%s)\n  Type: %s\n  Decision: %s\n  Rationale: %s\n",
				label, h.Fields["name"],
				h.Fields["scope"],
				h.Fields["decision_type"],
				h.Fields["decision"],
				h.Fields["rationale"],
			))
			if h.Fields["alternatives"] != "" {
				sb.WriteString("  Alternatives rejected: " + h.Fields["alternatives"] + "\n")
			}
			if h.Fields["constraint"] != "" {
				sb.WriteString("  Constraint: " + h.Fields["constraint"] + "\n")
			}
			if h.Fields["superseded_by"] != "" {
				sb.WriteString("  Superseded by: " + h.Fields["superseded_by"] + "\n")
			}
			if h.Fields["supersedes"] != "" {
				sb.WriteString("  Supersedes: " + h.Fields["supersedes"] + "\n")
			}
			sb.WriteByte('\n')
		}
		return s.success(req.ID, map[string]any{
			"content": []map[string]any{
				{"type": "text", "text": sb.String()},
			},
		})

	case "decision_supersede":
		var args struct {
			Repo    string `json:"repo"`
			OldName string `json:"old_name"`
			NewName string `json:"new_name"`
		}
		if err := json.Unmarshal(params.Arguments, &args); err != nil {
			return s.errResp(req.ID, -32602, "invalid arguments: "+err.Error())
		}
		if err := s.router.SupersedeDecision(args.Repo, args.OldName, args.NewName); err != nil {
			return s.errResp(req.ID, -32000, err.Error())
		}
		text := fmt.Sprintf("Decision %q is now superseded by %q. Lineage preserved.", args.OldName, args.NewName)
		return s.success(req.ID, map[string]any{
			"content": []map[string]any{
				{"type": "text", "text": text},
			},
		})

	case "dev_feedback":
		var args router.FeedbackInput
		if err := json.Unmarshal(params.Arguments, &args); err != nil {
			return s.errResp(req.ID, -32602, "invalid arguments: "+err.Error())
		}
		result := s.router.SubmitFeedback(args)
		text, _ := json.MarshalIndent(result, "", "  ")
		return s.success(req.ID, map[string]any{
			"content": []map[string]any{
				{"type": "text", "text": string(text)},
			},
		})

	case "dev_feedback_stats":
		stats := s.router.FeedbackStats()
		text, _ := json.MarshalIndent(stats, "", "  ")
		return s.success(req.ID, map[string]any{
			"content": []map[string]any{
				{"type": "text", "text": string(text)},
			},
		})

	case "dev_heuristics_reset":
		var args struct {
			Intent string `json:"intent"`
		}
		if err := json.Unmarshal(params.Arguments, &args); err != nil {
			return s.errResp(req.ID, -32602, "invalid arguments: "+err.Error())
		}
		reset, err := s.router.ResetHeuristics(args.Intent)
		if err != nil {
			return s.errResp(req.ID, -32000, err.Error())
		}
		text := fmt.Sprintf("Reset heuristic profiles to default for: %s", strings.Join(reset, ", "))
		return s.success(req.ID, map[string]any{
			"content": []map[string]any{
				{"type": "text", "text": text},
			},
		})

	default:
		return s.errResp(req.ID, -32602, "unknown tool: "+params.Name)
	}
}

func (s *Server) success(id json.RawMessage, result any) *jsonRPCResponse {
	return &jsonRPCResponse{JSONRPC: "2.0", ID: id, Result: result}
}

func (s *Server) errResp(id json.RawMessage, code int, msg string) *jsonRPCResponse {
	return &jsonRPCResponse{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: msg}}
}

// devContextPlanSchema returns the JSON-Schema fragment advertised to
// MCP clients for the `plan` field on dev_context and retrieve_debug.
// The numeric caps documented here match the server-side enforcement in
// router.SanitizePlan; treat that as the single source of truth — these
// strings exist purely so the MCP client surface is self-describing.
func devContextPlanSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"description": "Structured retrieval plan derived from the conversation context. " +
			"You (the agent) own this — devrouter has no in-process LLM. " +
			"Strongly recommended: producing a good plan is the single highest-leverage thing you can " +
			"do to improve retrieval quality. An omitted plan still works (devrouter auto-anchors the " +
			"rarest query token), but the agent has conversation context that bare-query extraction never had.",
		"properties": map[string]any{
			"must_terms": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"description": "1-2 hard-anchor tokens (lowercase, no stop words) that MUST scope retrieval. Keep tight: too many tokens collapses recall. Prefer short domain abbreviations (\"fms\", \"kbb\") over generic verbs (\"error\", \"debug\"). Server caps at 2.",
			},
			"should_terms": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"description": "0-6 synonyms / expansions / canonical identifier spellings (lowercase). Do NOT duplicate morphological variants — the indexer already stems. Server caps at 6.",
			},
			"exclude_terms": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"description": "0-3 tokens that mark conventional noise (\"test\", \"mock\", \"fixture\"). Targeted, not substring contains — \"test\" will not nuke \"requestSettings\". Server caps at 3.",
			},
			"phrases": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"description": "0-3 multi-word strings worth matching verbatim in code/comments. Server caps at 3.",
			},
			"context_hints": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"description": "0-3 likely package or file-path fragments (e.g. \"gobackend/fms\"). Soft bias only — a wrong hint can't blackhole the query. Server caps at 3.",
			},
		},
		"additionalProperties": false,
	}
}

// devContextPlanArgument is the wire shape of the `plan` field on
// dev_context / retrieve_debug. The caller (typically the agent) fills
// this in with the structured retrieval plan they want devrouter to
// honour; SanitizePlan in the router package enforces caps and normalisation
// so callers cannot bypass downstream invariants by over-stuffing fields.
//
// Kept separate from router.QueryPlan so JSON unmarshal errors point at
// MCP wire fields ("must_terms") rather than Go field names ("MustTerms").
type devContextPlanArgument struct {
	MustTerms    []string `json:"must_terms,omitempty"`
	ShouldTerms  []string `json:"should_terms,omitempty"`
	ExcludeTerms []string `json:"exclude_terms,omitempty"`
	Phrases      []string `json:"phrases,omitempty"`
	ContextHints []string `json:"context_hints,omitempty"`
}

// toRouterPlan converts the MCP wire representation into the router's
// internal QueryPlan. Returns nil when the caller did not supply a
// `plan` field at all, so the router falls back to auto-anchoring.
func (a *devContextPlanArgument) toRouterPlan() *router.QueryPlan {
	if a == nil {
		return nil
	}
	return &router.QueryPlan{
		MustTerms:    a.MustTerms,
		ShouldTerms:  a.ShouldTerms,
		ExcludeTerms: a.ExcludeTerms,
		Phrases:      a.Phrases,
		ContextHints: a.ContextHints,
	}
}
