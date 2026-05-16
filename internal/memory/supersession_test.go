package memory

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestSupersessionPreservesLineage verifies that SupersedeDecision creates bidirectional links
// and marks the old decision as superseded without deleting it.
func TestSupersessionPreservesLineage(t *testing.T) {
	ctx := context.Background()
	store, repo, _ := setupTestRedis(t)

	// Save two decisions
	d1 := DecisionMemory{
		Repo:         repo,
		Name:         "use-redis",
		DecisionType: "architecture",
		Decision:     "Use Redis for caching",
		Rationale:    "Fast reads",
		Scope:        "global",
	}
	if _, err := store.SaveDecision(d1); err != nil {
		t.Fatalf("SaveDecision d1: %v", err)
	}

	d2 := DecisionMemory{
		Repo:         repo,
		Name:         "use-postgres",
		DecisionType: "architecture",
		Decision:     "Use PostgreSQL for caching",
		Rationale:    "Better consistency",
		Scope:        "global",
	}
	if _, err := store.SaveDecision(d2); err != nil {
		t.Fatalf("SaveDecision d2: %v", err)
	}

	// Supersede d1 with d2
	if err := store.SupersedeDecision(repo, "use-redis", "use-postgres"); err != nil {
		t.Fatalf("SupersedeDecision: %v", err)
	}

	// Verify d1 is marked as superseded
	d1Key := memPrefix + repo + ":decision:" + sanitizeKey("use-redis")
	status, err := store.rdb.HGet(ctx, d1Key, "status").Result()
	if err != nil {
		t.Fatalf("Get d1 status: %v", err)
	}
	if status != "superseded" {
		t.Errorf("d1 status = %q, want %q", status, "superseded")
	}

	// Verify d1 has superseded_by pointing to d2
	supersededBy, err := store.rdb.HGet(ctx, d1Key, "superseded_by").Result()
	if err != nil {
		t.Fatalf("Get d1 superseded_by: %v", err)
	}
	if supersededBy != "use-postgres" {
		t.Errorf("d1 superseded_by = %q, want %q", supersededBy, "use-postgres")
	}

	// Verify d2 has supersedes pointing to d1
	d2Key := memPrefix + repo + ":decision:" + sanitizeKey("use-postgres")
	supersedes, err := store.rdb.HGet(ctx, d2Key, "supersedes").Result()
	if err != nil {
		t.Fatalf("Get d2 supersedes: %v", err)
	}
	if supersedes != "use-redis" {
		t.Errorf("d2 supersedes = %q, want %q", supersedes, "use-redis")
	}

	// Verify d2 still has status="active"
	d2Status, err := store.rdb.HGet(ctx, d2Key, "status").Result()
	if err != nil {
		t.Fatalf("Get d2 status: %v", err)
	}
	if d2Status != "active" {
		t.Errorf("d2 status = %q, want %q", d2Status, "active")
	}
}

// TestSupersessionRejectsNonExistent verifies that SupersedeDecision rejects non-existent decisions.
func TestSupersessionRejectsNonExistent(t *testing.T) {
	store, repo, _ := setupTestRedis(t)

	// Try to supersede a non-existent decision
	err := store.SupersedeDecision(repo, "nonexistent", "also-nonexistent")
	if err == nil {
		t.Fatal("SupersedeDecision should have failed for non-existent decisions")
	}

	// Save d1 only
	d1 := DecisionMemory{
		Repo:         repo,
		Name:         "use-redis",
		DecisionType: "architecture",
		Decision:     "Use Redis",
		Rationale:    "Fast",
		Scope:        "global",
	}
	if _, err := store.SaveDecision(d1); err != nil {
		t.Fatalf("SaveDecision: %v", err)
	}

	// Try to supersede with non-existent new decision
	err = store.SupersedeDecision(repo, "use-redis", "use-postgres")
	if err == nil {
		t.Fatal("SupersedeDecision should have failed when new decision doesn't exist")
	}
}

// TestListDecisionsFiltersSuperseded verifies that ListDecisions without includeSuperseded=true
// only returns active decisions.
func TestListDecisionsFiltersSuperseded(t *testing.T) {
	store, repo, _ := setupTestRedis(t)

	// Save and supersede a chain: d1 -> d2 -> d3
	decisions := []struct {
		name string
		desc string
	}{
		{"use-redis", "Use Redis for caching"},
		{"use-postgres", "Use PostgreSQL for caching"},
		{"use-memcached", "Use Memcached for caching"},
	}

	for _, d := range decisions {
		dm := DecisionMemory{
			Repo:         repo,
			Name:         d.name,
			DecisionType: "architecture",
			Decision:     d.desc,
			Rationale:    "Performance consideration",
			Scope:        "global",
		}
		if _, err := store.SaveDecision(dm); err != nil {
			t.Fatalf("SaveDecision %s: %v", d.name, err)
		}
	}

	// Create chain: use-redis -> use-postgres -> use-memcached
	if err := store.SupersedeDecision(repo, "use-redis", "use-postgres"); err != nil {
		t.Fatalf("Supersede use-redis with use-postgres: %v", err)
	}
	if err := store.SupersedeDecision(repo, "use-postgres", "use-memcached"); err != nil {
		t.Fatalf("Supersede use-postgres with use-memcached: %v", err)
	}

	// List with includeSuperseded=false should only return use-memcached
	hits := store.ListDecisions(repo, "", "", "", false)

	if len(hits) != 1 {
		t.Errorf("Expected 1 active decision, got %d", len(hits))
	}
	if len(hits) > 0 && hits[0].Fields["name"] != "use-memcached" {
		t.Errorf("Expected use-memcached, got %s", hits[0].Fields["name"])
	}

	// List with includeSuperseded=true should return all 3
	hits = store.ListDecisions(repo, "", "", "", true)

	if len(hits) != 3 {
		t.Errorf("Expected 3 decisions with includeSuperseded=true, got %d", len(hits))
	}

	// Verify status fields
	statusMap := make(map[string]string)
	for _, h := range hits {
		name := h.Fields["name"]
		status := h.Fields["status"]
		if status == "" {
			status = "active"
		}
		statusMap[name] = status
	}

	expectedStatus := map[string]string{
		"use-redis":     "superseded",
		"use-postgres":  "superseded",
		"use-memcached": "active",
	}

	for name, expectedStatus := range expectedStatus {
		if actualStatus, found := statusMap[name]; !found {
			t.Errorf("Decision %s not found in list", name)
		} else if actualStatus != expectedStatus {
			t.Errorf("Decision %s status = %q, want %q", name, actualStatus, expectedStatus)
		}
	}
}

// TestSupersessionBidirectionalLinks verifies both supersedes and superseded_by are set correctly.
func TestSupersessionBidirectionalLinks(t *testing.T) {
	ctx := context.Background()
	store, repo, _ := setupTestRedis(t)

	// Save two decisions
	d1 := DecisionMemory{
		Repo:         repo,
		Name:         "old-approach",
		DecisionType: "refactor",
		Decision:     "Use approach A",
		Rationale:    "Initial design",
		Scope:        "global",
	}
	d2 := DecisionMemory{
		Repo:         repo,
		Name:         "new-approach",
		DecisionType: "refactor",
		Decision:     "Use approach B",
		Rationale:    "Better performance",
		Scope:        "global",
	}

	if _, err := store.SaveDecision(d1); err != nil {
		t.Fatalf("SaveDecision d1: %v", err)
	}
	if _, err := store.SaveDecision(d2); err != nil {
		t.Fatalf("SaveDecision d2: %v", err)
	}

	// Create supersession
	if err := store.SupersedeDecision(repo, "old-approach", "new-approach"); err != nil {
		t.Fatalf("SupersedeDecision: %v", err)
	}

	// Verify both links exist
	oldKey := memPrefix + repo + ":decision:" + sanitizeKey("old-approach")
	newKey := memPrefix + repo + ":decision:" + sanitizeKey("new-approach")

	oldSupersededBy, _ := store.rdb.HGet(ctx, oldKey, "superseded_by").Result()
	newSupersedes, _ := store.rdb.HGet(ctx, newKey, "supersedes").Result()

	if oldSupersededBy != "new-approach" {
		t.Errorf("old decision superseded_by = %q, want %q", oldSupersededBy, "new-approach")
	}
	if newSupersedes != "old-approach" {
		t.Errorf("new decision supersedes = %q, want %q", newSupersedes, "old-approach")
	}

	// Verify both decisions are still retrievable
	oldContent, _ := store.rdb.HGet(ctx, oldKey, "decision").Result()
	newContent, _ := store.rdb.HGet(ctx, newKey, "decision").Result()

	if oldContent != "Use approach A" {
		t.Errorf("old decision lost its content")
	}
	if newContent != "Use approach B" {
		t.Errorf("new decision lost its content")
	}
}

// setupTestRedis returns a Store, a process-unique test repo name, and
// a cleanup function. The repo name is unique per test invocation so
// concurrent tests don't collide, and cleanup SCANs+DELs only keys
// under that repo's prefixes — never FlushDB.
//
// Why not FlushDB:
//
//	The previous implementation called `store.rdb.FlushDB(ctx)` which
//	wipes the entire production keyspace on DB 0. devrouter shares DB 0
//	with everything else in this process, so running `go test
//	./internal/memory/...` against a developer machine destroyed every
//	saved memory, decision, trace, flow overlay, and heuristics profile
//	for every repo. That was a real foot-gun; this isolation pattern
//	closes it for good.
//
// The cleanup function is also registered with t.Cleanup so callers
// can use either `defer cleanup()` (back-compat) or just ignore the
// return value and rely on Go's test cleanup ordering.
func setupTestRedis(t *testing.T) (store *Store, repo string, cleanup func()) {
	t.Helper()

	s, err := NewStore("localhost:6379")
	if err != nil {
		t.Skipf("Redis not available or NewStore failed: %v", err)
	}

	// Unique per test invocation. Time-based + test name so a flaky
	// rerun doesn't reuse the same repo and observe stale keys.
	safeName := strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			return r
		}
		return '-'
	}, t.Name())
	repo = fmt.Sprintf("test-%s-%d", safeName, time.Now().UnixNano())

	clean := func() {
		ctx := context.Background()
		patterns := []string{
			// Memory hashes for this test's repo across every type.
			memPrefix + repo + ":*",
			// Flow overlay hashes use a parallel keyspace.
			flowOverlayPrefix + repo + ":*",
			// Memory false-positive sorted sets are scoped per memory
			// key, so deleting the memory keys orphans them; this
			// catches the orphan ZSETs explicitly.
			"memory:fp:" + memPrefix + repo + ":*",
		}
		for _, pat := range patterns {
			var cursor uint64
			for {
				keys, next, err := s.rdb.Scan(ctx, cursor, pat, 500).Result()
				if err != nil {
					break
				}
				if len(keys) > 0 {
					s.rdb.Del(ctx, keys...)
				}
				cursor = next
				if cursor == 0 {
					break
				}
			}
		}
	}
	t.Cleanup(clean)
	return s, repo, clean
}
