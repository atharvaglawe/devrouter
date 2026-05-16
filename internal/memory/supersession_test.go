package memory

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"strings"
	"testing"
)

// TestSupersessionPreservesLineage verifies that SupersedeDecision creates bidirectional links
// and marks the old decision as superseded without deleting it.
func TestSupersessionPreservesLineage(t *testing.T) {
	ctx := context.Background()
	store := setupTestRedis(t)

	// Save two decisions
	d1 := DecisionMemory{
		Repo:         "test-repo",
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
		Repo:         "test-repo",
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
	if err := store.SupersedeDecision("test-repo", "use-redis", "use-postgres"); err != nil {
		t.Fatalf("SupersedeDecision: %v", err)
	}

	// Verify d1 is marked as superseded
	d1Key := store.keyPrefix() + "test-repo" + ":decision:" + sanitizeKey("use-redis")
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
	d2Key := store.keyPrefix() + "test-repo" + ":decision:" + sanitizeKey("use-postgres")
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
	store := setupTestRedis(t)

	// Try to supersede a non-existent decision
	err := store.SupersedeDecision("test-repo", "nonexistent", "also-nonexistent")
	if err == nil {
		t.Fatal("SupersedeDecision should have failed for non-existent decisions")
	}

	// Save d1 only
	d1 := DecisionMemory{
		Repo:         "test-repo",
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
	err = store.SupersedeDecision("test-repo", "use-redis", "use-postgres")
	if err == nil {
		t.Fatal("SupersedeDecision should have failed when new decision doesn't exist")
	}
}

// TestListDecisionsFiltersSuperseded verifies that ListDecisions without includeSuperseded=true
// only returns active decisions.
func TestListDecisionsFiltersSuperseded(t *testing.T) {
	store := setupTestRedis(t)

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
			Repo:         "test-repo",
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
	if err := store.SupersedeDecision("test-repo", "use-redis", "use-postgres"); err != nil {
		t.Fatalf("Supersede use-redis with use-postgres: %v", err)
	}
	if err := store.SupersedeDecision("test-repo", "use-postgres", "use-memcached"); err != nil {
		t.Fatalf("Supersede use-postgres with use-memcached: %v", err)
	}

	// List with includeSuperseded=false should only return use-memcached
	hits := store.ListDecisions("test-repo", "", "", "", false)

	if len(hits) != 1 {
		t.Errorf("Expected 1 active decision, got %d", len(hits))
	}
	if len(hits) > 0 && hits[0].Fields["name"] != "use-memcached" {
		t.Errorf("Expected use-memcached, got %s", hits[0].Fields["name"])
	}

	// List with includeSuperseded=true should return all 3
	hits = store.ListDecisions("test-repo", "", "", "", true)

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
	store := setupTestRedis(t)

	// Save two decisions
	d1 := DecisionMemory{
		Repo:         "test-repo",
		Name:         "old-approach",
		DecisionType: "refactor",
		Decision:     "Use approach A",
		Rationale:    "Initial design",
		Scope:        "global",
	}
	d2 := DecisionMemory{
		Repo:         "test-repo",
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
	if err := store.SupersedeDecision("test-repo", "old-approach", "new-approach"); err != nil {
		t.Fatalf("SupersedeDecision: %v", err)
	}

	// Verify both links exist
	oldKey := store.keyPrefix() + "test-repo" + ":decision:" + sanitizeKey("old-approach")
	newKey := store.keyPrefix() + "test-repo" + ":decision:" + sanitizeKey("new-approach")

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

// setupTestRedis creates a test Store pinned to a per-test keyspace
// (e.g. "testmem-Tabc1234") so the tests can never touch the
// production "mem:*" namespace the devrouter binary and bench seeders
// write to. RediSearch limits FT indexes to db=0, so logical-DB
// isolation isn't viable — keyspace prefixing is the next best thing.
//
// Historic note: the original helper called NewStore("localhost:6379")
// (db=0, "mem:" namespace) and then FlushDB'd it on every test entry.
// Running `go test ./internal/memory/...` would silently wipe every
// seeded flow / decision / topic / heuristic bucket / retrieval trace
// the dev shell had built up. That was found the hard way after a
// benchmark seed cycle disappeared mid-test. The new helper:
//
//   - Uses a unique keyspace per test via NewStoreWithKeyspace, so
//     parallel tests don't collide either.
//   - Calls Store.WipeKeyspace on cleanup, which refuses to touch
//     the production "mem" keyspace as a belt-and-braces safety net.
//   - Never calls FlushDB.
//
// We still don't use miniredis because the tests exercise RediSearch
// (FT.CREATE / FT.SEARCH / FT.DROPINDEX), which miniredis doesn't
// implement.
func setupTestRedis(t *testing.T) *Store {
	t.Helper()

	keyspace := uniqueTestKeyspace(t)

	store, err := NewStoreWithKeyspace("localhost:6379", keyspace)
	if err != nil {
		t.Skipf("Redis not available at localhost:6379 (keyspace %q): %v", keyspace, err)
	}

	// Per-test isolation + leave no artefacts for whoever runs next.
	// Cleanup runs even when the test itself fails.
	t.Cleanup(func() {
		_ = store.WipeKeyspace(context.Background())
		_ = store.rdb.Close()
	})

	return store
}

// uniqueTestKeyspace returns a per-test keyspace that's safe to FT-index
// alongside the production "mem" keyspace on the same Redis DB. Format:
// "testmem-<sanitised-test-name>-<8 hex>". The hex suffix guards
// against the (rare) case where two tests with identical names run
// against the same Redis (e.g. test re-runs that race the cleanup).
func uniqueTestKeyspace(t *testing.T) string {
	t.Helper()
	var rnd [4]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	clean := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z':
			return r
		case r >= 'A' && r <= 'Z':
			return r
		case r >= '0' && r <= '9':
			return r
		}
		return '-'
	}, t.Name())
	if len(clean) > 32 {
		clean = clean[:32]
	}
	return "testmem-" + clean + "-" + hex.EncodeToString(rnd[:])
}
