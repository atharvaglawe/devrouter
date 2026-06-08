package router

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/atharva-ag/devrouter/internal/prompt"
	"github.com/atharva-ag/devrouter/internal/retrieval"
)

func TestSignalsFromAutoHints(t *testing.T) {
	sig := signalsFromAutoHints([]string{"internal/foo/bar.go", "HandleQuery", "pkg/x.go", "", "DoThing"})
	if len(sig.Paths) != 2 || len(sig.Symbols) != 2 {
		t.Fatalf("split wrong: paths=%v symbols=%v", sig.Paths, sig.Symbols)
	}
}

type stubSource struct {
	name  string
	docs  []prompt.DocEntry
	err   error
	delay time.Duration
}

func (s stubSource) Name() string { return s.name }
func (s stubSource) Search(ctx context.Context, _ retrieval.Request) (retrieval.Result, error) {
	if s.delay > 0 {
		select {
		case <-time.After(s.delay):
		case <-ctx.Done():
			return retrieval.Result{}, ctx.Err()
		}
	}
	if s.err != nil {
		return retrieval.Result{}, s.err
	}
	return retrieval.Result{Docs: s.docs}, nil
}

func TestFetchDocSourcesMergesAndTraces(t *testing.T) {
	r := &Router{
		sourceTimeout: 50 * time.Millisecond,
		Sources: []retrieval.Source{
			stubSource{name: "cmdocs", docs: []prompt.DocEntry{{Source: "cmdocs", Content: "a"}}},
			stubSource{name: "gitlab", err: errors.New("boom")},
			stubSource{name: "slow", docs: []prompt.DocEntry{{Source: "slow", Content: "late"}}, delay: 500 * time.Millisecond},
		},
	}

	start := time.Now()
	docs, stages := r.fetchDocSources(retrieval.Request{Query: "q", Intent: "debug"}, nil)
	elapsed := time.Since(start)

	// The slow source must be bounded by sourceTimeout, not run to completion.
	if elapsed > 300*time.Millisecond {
		t.Fatalf("fan-out not bounded by timeout: took %v", elapsed)
	}
	// Only cmdocs contributed a doc (gitlab errored, slow timed out).
	if len(docs) != 1 || docs[0].Source != "cmdocs" {
		t.Fatalf("unexpected merged docs: %+v", docs)
	}
	if len(stages) != 3 {
		t.Fatalf("want a trace stage per source, got %d", len(stages))
	}
	if len(stages["gitlab"].Warnings) == 0 {
		t.Fatalf("expected error warning on gitlab stage")
	}
	if stages["cmdocs"].CandidatesOut != 1 {
		t.Fatalf("cmdocs stage should report 1 doc, got %d", stages["cmdocs"].CandidatesOut)
	}
}

func TestFetchDocSourcesEmptyRegistry(t *testing.T) {
	r := &Router{}
	docs, stages := r.fetchDocSources(retrieval.Request{Query: "q"}, nil)
	if docs != nil || stages != nil {
		t.Fatalf("empty registry should be a no-op, got docs=%v stages=%v", docs, stages)
	}
}
