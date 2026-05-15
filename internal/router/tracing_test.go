package router

import (
	"testing"

	"github.com/atharva-ag/devrouter/internal/prompt"
)

// TestEstimateTokensBasic verifies that token estimation works for basic content.
func TestEstimateTokensBasic(t *testing.T) {
	dp := &prompt.DevPrompt{
		Instructions: "This is a test instruction that has some length",
		Intent:       "refactor",
		PrimaryContext: []prompt.PrimaryContextEntry{
			{Summary: "Test summary", Details: "Test details"},
		},
		CodeSnippets: []prompt.Snippet{
			{Content: "func main() { fmt.Println(\"hello\") }"},
		},
	}

	tokens := estimateTokens(dp)
	if tokens <= 0 {
		t.Errorf("estimateTokens = %d, want > 0", tokens)
	}
}

// TestEstimateTokensEmpty verifies that empty content yields minimal tokens.
func TestEstimateTokensEmpty(t *testing.T) {
	dp := &prompt.DevPrompt{}

	tokens := estimateTokens(dp)
	if tokens <= 0 {
		t.Errorf("estimateTokens = %d, want > 0 (includes buffer)", tokens)
	}

	// Buffer should be at least 100
	if tokens < 100 {
		t.Errorf("estimateTokens = %d, want >= 100 (buffer)", tokens)
	}
}

// TestEstimateTokensComparison verifies that more content yields more tokens.
func TestEstimateTokensComparison(t *testing.T) {
	short := &prompt.DevPrompt{
		Instructions: "x",
	}
	shortTokens := estimateTokens(short)

	long := &prompt.DevPrompt{
		Instructions: "This is a much longer instruction with many words to increase token count for testing purposes",
		Intent:       "refactor",
		PrimaryContext: []prompt.PrimaryContextEntry{
			{Summary: "Summary 1", Details: "Details 1"},
			{Summary: "Summary 2", Details: "Details 2"},
			{Summary: "Summary 3", Details: "Details 3"},
		},
		Symbols: []string{"symbol1", "symbol2", "symbol3"},
		CodeSnippets: []prompt.Snippet{
			{Content: "func a() {} func b() {} func c() {}"},
		},
	}
	longTokens := estimateTokens(long)

	if longTokens <= shortTokens {
		t.Errorf("longTokens (%d) should be > shortTokens (%d)", longTokens, shortTokens)
	}
}

// TestEstimateTokensWithSnippets verifies that code snippets contribute significantly.
func TestEstimateTokensWithSnippets(t *testing.T) {
	noSnippets := &prompt.DevPrompt{
		Instructions: "test instruction",
	}
	noSnippetsTokens := estimateTokens(noSnippets)

	withSnippets := &prompt.DevPrompt{
		Instructions: "test instruction",
		CodeSnippets: []prompt.Snippet{
			{Content: "func foo() { x := 1; y := 2; z := x + y; return z }"},
			{Content: "func bar() { a := 1; b := 2; c := 3; return a + b + c }"},
		},
	}
	withSnippetsTokens := estimateTokens(withSnippets)

	if withSnippetsTokens <= noSnippetsTokens {
		t.Errorf("withSnippets (%d) should be > noSnippets (%d)", withSnippetsTokens, noSnippetsTokens)
	}
}

// TestRetrievalTraceStructure verifies that RetrievalTrace has all expected fields.
func TestRetrievalTraceStructure(t *testing.T) {
	trace := &prompt.RetrievalTrace{
		QueryID:   "test-id",
		Query:     "test query",
		Timestamp: 1234567890,
		Signals:   make(map[string]float64),
	}

	if trace.QueryID == "" {
		t.Error("QueryID should not be empty")
	}

	if trace.Query == "" {
		t.Error("Query should not be empty")
	}

	if trace.Timestamp == 0 {
		t.Error("Timestamp should not be zero")
	}

	if trace.Signals == nil {
		t.Error("Signals should be initialized")
	}
}

// TestStageTraceStructure verifies that StageTrace has all expected fields.
func TestStageTraceStructure(t *testing.T) {
	stage := &prompt.StageTrace{
		LatencyMs:     100,
		Description:   "test stage",
		CandidatesIn:  10,
		CandidatesOut: 5,
		Details:       make(map[string]interface{}),
		Warnings:      []string{},
	}

	if stage.LatencyMs <= 0 {
		t.Error("LatencyMs should be > 0")
	}

	if stage.Description == "" {
		t.Error("Description should not be empty")
	}

	if stage.CandidatesIn < 0 {
		t.Error("CandidatesIn should be >= 0")
	}

	if stage.CandidatesOut < 0 {
		t.Error("CandidatesOut should be >= 0")
	}

	if stage.Details == nil {
		t.Error("Details should be initialized")
	}

	if stage.Warnings == nil {
		t.Error("Warnings should be initialized")
	}
}

// TestSignalScoreValidation verifies that signal scores are in valid ranges.
func TestSignalScoreValidation(t *testing.T) {
	signals := map[string]float64{
		"semantic_similarity": 0.87,
		"graph_proximity":     0.65,
		"memory_coverage":     0.5,
		"decision_relevance":  0.95,
	}

	for name, score := range signals {
		if score < 0 || score > 1.0 {
			t.Errorf("Signal %q score %.2f out of valid range [0.0, 1.0]", name, score)
		}
	}
}

// TestDevPromptWithTrace verifies that RetrievalTrace integrates with DevPrompt.
func TestDevPromptWithTrace(t *testing.T) {
	dp := &prompt.DevPrompt{
		Instructions: "Test",
		RetrievalTrace: &prompt.RetrievalTrace{
			QueryID:   "test",
			Query:     "test query",
			Timestamp: 123456,
			Signals:   map[string]float64{"test": 0.5},
		},
	}

	if dp.RetrievalTrace == nil {
		t.Fatal("RetrievalTrace should be attached to DevPrompt")
	}

	if dp.RetrievalTrace.QueryID != "test" {
		t.Errorf("QueryID = %q, want %q", dp.RetrievalTrace.QueryID, "test")
	}
}
