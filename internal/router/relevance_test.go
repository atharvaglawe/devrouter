package router

import (
	"math"
	"testing"

	"github.com/atharva-ag/devrouter/internal/codegraph"
	"github.com/atharva-ag/devrouter/internal/memory"
	"github.com/atharva-ag/devrouter/internal/prompt"
)

func mhit(t string, score float64, fields map[string]string) memory.MemoryHit {
	return memory.MemoryHit{Type: t, Score: score, Fields: fields}
}

// TestDropBelowFloor verifies that the cosine-distance ceiling drops weak
// matches and keeps strong ones. Hits with no score (lexical / future
// graph hits) pass through untouched.
func TestDropBelowFloor(t *testing.T) {
	hits := []memory.MemoryHit{
		mhit("file", 0.10, map[string]string{"path": "strong"}),
		mhit("file", 0.55, map[string]string{"path": "borderline"}),
		mhit("file", 0.80, map[string]string{"path": "weak"}),
		mhit("file", 0.0, map[string]string{"path": "no-score"}),
	}
	out := dropBelowFloor(hits, 0.60)
	if len(out) != 3 {
		t.Fatalf("dropBelowFloor: got %d hits, want 3 (strong + borderline + no-score)", len(out))
	}
	if out[0].Fields["path"] != "strong" || out[2].Fields["path"] != "no-score" {
		t.Errorf("dropBelowFloor: unexpected ordering %+v", out)
	}
}

// TestFilterMemoriesByPlanFieldTargeted is the regression test for the
// seat-provider-mapping false positive: a memory whose ONLY mention of
// "error" is in its free-text purpose must not pass the must-term filter.
func TestFilterMemoriesByPlanFieldTargeted(t *testing.T) {
	// Mirrors the actual seat-provider memory that polluted the
	// "error logging in tag-based clearing" query.
	seatProvider := mhit("flow", 0.45, map[string]string{
		"name":         "seat-provider-mapping-resolution",
		"files":        "cmpkg/integrationseat/autoseatid.go,cmpkg/entity/seatprovidermapping.go",
		"entry_points": "NewSeatProviderMappingConfig",
		"purpose":      "If FM value is incomplete or error, defaults to package defaults...",
	})
	// A genuinely error-handling memory whose name carries the must term.
	errorHandler := mhit("func", 0.30, map[string]string{
		"name":    "ErrorLoggerMiddleware",
		"file":    "lib/middleware/errorlogger.go",
		"purpose": "Wraps the request handler in an error-recovering panic logger.",
	})
	// A memory that mentions "error" only via an unrelated path.
	utilHelper := mhit("file", 0.40, map[string]string{
		"path":    "lib/util/strings.go",
		"purpose": "String utilities; does not deal with error handling.",
	})

	plan := QueryPlan{MustTerms: []string{"error"}}
	out := filterMemoriesByPlan([]memory.MemoryHit{seatProvider, errorHandler, utilHelper}, plan)

	if len(out) != 1 {
		t.Fatalf("expected 1 kept hit, got %d", len(out))
	}
	if out[0].Fields["name"] != "ErrorLoggerMiddleware" {
		t.Errorf("expected ErrorLoggerMiddleware to survive, got %v", out[0].Fields)
	}
}

// TestFilterMemoriesByPlanAutoAnchorBypass covers the case the
// mall-memory benchmark surfaced: when MustTerms came from
// ensureMustAnchor (rarest-query-token, not a caller-supplied must),
// we DON'T hard-gate memories on it. The seed memory's path/name often
// doesn't carry an English filler word like "user" or "order" even
// when the memory is on-topic, so a hard gate would drop 8/8
// cosine-passing hits (real number from the bench). ExcludeTerms still
// applies — auto-anchor only relaxes the must filter, never the
// path-pattern blocklist.
func TestFilterMemoriesByPlanAutoAnchorBypass(t *testing.T) {
	// Hits whose paths/names don't contain "user" but are clearly
	// on-topic for "a user places an order" — the cosine layer would
	// have already credited these. Excludes still take effect for
	// any conventional test path.
	orderService := mhit("file", 0.35, map[string]string{
		"name": "OmsPortalOrderServiceImpl",
		"path": "mall-portal/src/main/java/com/macro/mall/portal/service/impl/OmsPortalOrderServiceImpl.java",
	})
	cancelSender := mhit("file", 0.40, map[string]string{
		"name": "CancelOrderSender",
		"path": "mall-portal/src/main/java/com/macro/mall/portal/component/CancelOrderSender.java",
	})
	testFile := mhit("file", 0.42, map[string]string{
		"name": "OmsPortalOrderServiceImplTest",
		"path": "mall-portal/src/test/java/com/macro/mall/portal/service/impl/OmsPortalOrderServiceImplTest.java",
	})

	// Caller did NOT supply must — ensureMustAnchor promoted "user".
	// Caller DID supply exclude=test (the conventional pattern), and
	// that's not relaxed by the auto-anchor bypass — exclude is a
	// path-pattern blocklist, never widened.
	plan := QueryPlan{
		MustTerms:        []string{"user"},
		MustAutoAnchored: true,
		ExcludeTerms:     []string{"test"},
	}
	out := filterMemoriesByPlan(
		[]memory.MemoryHit{orderService, cancelSender, testFile},
		plan,
	)

	// Both on-topic memories survive even though "user" isn't in their
	// structural text. Test path is dropped by ExcludeTerms.
	if len(out) != 2 {
		t.Fatalf("expected 2 kept hits (must auto-anchored, test excluded), got %d", len(out))
	}
	gotNames := map[string]bool{}
	for _, h := range out {
		gotNames[h.Fields["name"]] = true
	}
	if !gotNames["OmsPortalOrderServiceImpl"] || !gotNames["CancelOrderSender"] {
		t.Errorf("expected OmsPortalOrderServiceImpl + CancelOrderSender, got %v", gotNames)
	}

	// Sanity: same plan with MustAutoAnchored=false would drop all
	// three (no path/name contains "user"). Asserts the bypass is
	// what's doing the work, not some other change.
	planExplicit := QueryPlan{
		MustTerms:    []string{"user"},
		ExcludeTerms: []string{"test"},
	}
	strict := filterMemoriesByPlan(
		[]memory.MemoryHit{orderService, cancelSender, testFile},
		planExplicit,
	)
	if len(strict) != 0 {
		t.Errorf("explicit must=[user] should drop all 3, got %d", len(strict))
	}
}

// TestFilterCallChainByPlanMustExclude exercises the graph-side
// equivalent of filterMemoriesByPlan when applyMust=true (the policy
// used for graph.Importers, the bucket where hubs leak in). With must
// applied, hub edges that don't carry the must term are dropped.
func TestFilterCallChainByPlanMustExclude(t *testing.T) {
	plan := QueryPlan{
		MustTerms:    []string{"ratelimit"},
		ExcludeTerms: []string{"test"},
	}
	edges := []prompt.CallEdge{
		// Keeper: structural text contains "ratelimit".
		{From: "ProcessRequest", To: "RateLimitCheck", FilePath: "proxy.go"},
		// Hub-style noise: structural text has no must-term substring.
		{From: "Init", To: "main", FilePath: "consumer/main.go"},
		// Test convention: exclude rule kicks in on the _test.go marker.
		{From: "TestRateLimit", To: "RateLimitCheck", FilePath: "ratelimit_test.go"},
	}

	out := filterCallChainByPlan(edges, plan, SeedAnchors{}, true /* applyMust */)

	if len(out) != 1 {
		t.Fatalf("expected 1 kept edge, got %d (%+v)", len(out), out)
	}
	if out[0].To != "RateLimitCheck" || out[0].FilePath != "proxy.go" {
		t.Errorf("expected ratelimit-anchored production edge, got %+v", out[0])
	}
}

// TestFilterCallChainByPlanExcludeOnly exercises the policy actually
// used for chain.Upstream / chain.Downstream / graph.Methods /
// graph.Extends: applyMust=false. Edges 1-hop from a search-certified
// seed are inherently relevant — the must filter would over-prune
// them when the auto-anchor token doesn't appear in seed names
// (e.g. must="connection" but the seed symbol is "DBClient").
// Exclude (test/mock/fixture) still applies.
func TestFilterCallChainByPlanExcludeOnly(t *testing.T) {
	plan := QueryPlan{
		MustTerms:    []string{"connection"},
		ExcludeTerms: []string{"test"},
	}
	edges := []prompt.CallEdge{
		// Path/names don't carry the must term, but it's a 1-hop
		// edge to a seed → kept under applyMust=false.
		{From: "DBClient", To: "Pool", FilePath: "lib/db/creator.go"},
		// Test marker: still dropped via exclude.
		{From: "DBClient", To: "Pool", FilePath: "lib/db/creator_test.go"},
	}

	out := filterCallChainByPlan(edges, plan, SeedAnchors{}, false /* applyMust */)

	if len(out) != 1 {
		t.Fatalf("expected 1 kept edge (non-test), got %d (%+v)", len(out), out)
	}
	if out[0].FilePath != "lib/db/creator.go" {
		t.Errorf("expected production creator.go, got %+v", out[0])
	}
}

// TestFilterCallChainByPlanSeedBypass exercises the adjacency-trust
// rule. Regression for the goserving R@10 dip: edges whose FilePath
// is a search-certified seed file should be kept even when their
// structural text doesn't carry the must term, because the search
// layer already certified that file as on-topic.
//
// File-anchored bypass only — symbol-name anchoring was tried and
// over-matched on common Go names (Init, New, Run), reopening the
// hub-file leak. See SeedAnchors doc.
func TestFilterCallChainByPlanSeedBypass(t *testing.T) {
	plan := QueryPlan{MustTerms: []string{"ratelimit"}}
	seeds := newSeedAnchors(
		[]codegraph.SearchResult{
			{FilePath: "lib/clientip/clientip.go"},
		},
	)
	edges := []prompt.CallEdge{
		// Path is a known seed file → keep via file bypass even
		// though neither symbol name carries the must term.
		{From: "ResolveClient", To: "ParseHeaders", FilePath: "lib/clientip/clientip.go"},
		// Same caller name, but NOT a seed file → drop (the hub
		// fix: name-only adjacency to a seed is no longer enough).
		{From: "ResolveClient", To: "Init", FilePath: "consumer/main.go"},
		// Structural text DOES carry the must term — must-filter keeps
		// it independently of seeds.
		{From: "RateLimitCheck", To: "Allow", FilePath: "lib/limiter/ratelimit.go"},
	}

	out := filterCallChainByPlan(edges, plan, seeds, true /* applyMust */)

	if len(out) != 2 {
		t.Fatalf("expected 2 kept edges (seed-file + must-match), got %d (%+v)", len(out), out)
	}
	gotPaths := map[string]bool{out[0].FilePath: true, out[1].FilePath: true}
	if !gotPaths["lib/clientip/clientip.go"] || !gotPaths["lib/limiter/ratelimit.go"] {
		t.Errorf("expected seed-file + must-match edges, got %+v", out)
	}
}

// TestFilterGraphLinksByPlan covers the GraphEdge variant. Same
// semantics as the CallEdge filter; the duplication exists because
// CallEdge / GraphEdge are distinct prompt types.
func TestFilterGraphLinksByPlan(t *testing.T) {
	plan := QueryPlan{ExcludeTerms: []string{"mock"}}
	edges := []prompt.GraphEdge{
		{From: "RateLimiter", To: "Storage", FilePath: "lib/storage/redis.go"},
		{From: "RateLimiter", To: "MockStorage", FilePath: "lib/mocks/storage_mock.go"},
	}

	out := filterGraphLinksByPlan(edges, plan, SeedAnchors{}, true /* applyMust */)

	if len(out) != 1 {
		t.Fatalf("expected 1 kept link, got %d", len(out))
	}
	if out[0].To != "Storage" {
		t.Errorf("expected real Storage edge, got %+v", out[0])
	}
}

// TestFilterSiblingsByPlanExcludeOnly verifies siblings get only the
// exclude filter (not must), per the documented design — siblings are
// flat paths with no symbol context, so must-term substring would
// over-prune legitimate adjacent files.
func TestFilterSiblingsByPlanExcludeOnly(t *testing.T) {
	plan := QueryPlan{
		MustTerms:    []string{"ratelimit"}, // intentionally ignored for siblings
		ExcludeTerms: []string{"test"},
	}
	siblings := []string{
		"proxy.go",                     // does NOT contain must term — must still pass
		"middleware/auth.go",           // does NOT contain must term — must still pass
		"ratelimit_test.go",            // exclude on test marker
		"some/path/tests/helper.go",    // exclude on /tests/ path segment
	}

	out := filterSiblingsByPlan(siblings, plan)

	if len(out) != 2 {
		t.Fatalf("expected 2 kept siblings, got %d (%v)", len(out), out)
	}
	if out[0] != "proxy.go" || out[1] != "middleware/auth.go" {
		t.Errorf("expected non-test siblings, got %v", out)
	}
}

// TestFilterCallChainByPlanEmptyPlan: nil/empty plan must be a no-op.
func TestFilterCallChainByPlanEmptyPlan(t *testing.T) {
	edges := []prompt.CallEdge{
		{From: "A", To: "B", FilePath: "a.go"},
		{From: "C", To: "D", FilePath: "c.go"},
	}
	out := filterCallChainByPlan(edges, QueryPlan{}, SeedAnchors{}, true /* applyMust */)
	if len(out) != 2 {
		t.Errorf("empty plan: expected no-op, got %d edges", len(out))
	}
}

// TestRankByPlanShouldBoost verifies that should-term overlap on
// structural fields outranks a higher-cosine hit with no should overlap.
func TestRankByPlanShouldBoost(t *testing.T) {
	// Higher cosine similarity but zero should-term overlap.
	semanticWinner := mhit("flow", 0.20, map[string]string{ // sim 0.80
		"name":  "auth-flow",
		"files": "auth.go",
	})
	// Lower cosine similarity but every should term lights up structurally.
	planWinner := mhit("flow", 0.40, map[string]string{ // sim 0.60
		"name":         "tagbased-cache-clearing",
		"files":        "terminator/cache.go",
		"entry_points": "ClearByTag",
	})
	plan := QueryPlan{
		ShouldTerms: []string{"tagbased", "cache", "clearing"},
	}
	out := rankByPlan([]memory.MemoryHit{semanticWinner, planWinner}, plan)
	if out[0].Fields["name"] != "tagbased-cache-clearing" {
		t.Fatalf("rankByPlan put %q first; expected tagbased-cache-clearing to win on should-term overlap", out[0].Fields["name"])
	}
}

// TestRankByPlanStable verifies that two equally-scored hits keep their
// input order (FT.SEARCH ordering is preserved as the tiebreaker).
func TestRankByPlanStable(t *testing.T) {
	a := mhit("file", 0.30, map[string]string{"path": "a.go"})
	b := mhit("file", 0.30, map[string]string{"path": "b.go"})
	out := rankByPlan([]memory.MemoryHit{a, b}, QueryPlan{})
	if out[0].Fields["path"] != "a.go" || out[1].Fields["path"] != "b.go" {
		t.Errorf("rankByPlan not stable: got %v", out)
	}
}

// TestConfidenceUsesSimilarity verifies the per-entry confidence is the
// real cosine similarity, optionally damped by stale.
func TestConfidenceUsesSimilarity(t *testing.T) {
	cases := []struct {
		sim   float64
		stale bool
		want  float64
	}{
		{0.90, false, 0.90},
		{0.90, true, 0.54},  // 0.90 * 0.6
		{1.50, false, 1.0},  // clamped
		{-0.10, false, 0.0}, // clamped
	}
	for _, c := range cases {
		got := confidence(c.sim, c.stale)
		if math.Abs(got-c.want) > 1e-9 {
			t.Errorf("confidence(sim=%.2f, stale=%v) = %.4f, want %.4f", c.sim, c.stale, got, c.want)
		}
	}
}

// TestAgentSimilarityStats covers the new honest signals plumbing.
func TestAgentSimilarityStats(t *testing.T) {
	memRes := memoryResults{
		agent: &prompt.StructuredMemories{
			Files: []prompt.FileMemoryHit{{Sim: 0.60}, {Sim: 0.40}},
			Flows: []prompt.FlowMemoryHit{{Sim: 0.80}},
		},
	}
	top, mean := agentSimilarityStats(memRes)
	if math.Abs(top-0.80) > 1e-9 {
		t.Errorf("top: got %.4f, want 0.80", top)
	}
	if math.Abs(mean-0.60) > 1e-9 {
		t.Errorf("mean: got %.4f, want 0.60", mean)
	}

	emptyTop, emptyMean := agentSimilarityStats(memoryResults{})
	if emptyTop != 0 || emptyMean != 0 {
		t.Errorf("empty: got (%.2f, %.2f), want (0, 0)", emptyTop, emptyMean)
	}
}

// TestGraphProximityFromTrace verifies the traced/seed ratio derivation.
func TestGraphProximityFromTrace(t *testing.T) {
	stage := &prompt.StageTrace{
		Details: map[string]interface{}{
			"seed_symbols":   10,
			"traced_symbols": 5,
		},
	}
	if got := graphProximityFromTrace(stage); math.Abs(got-0.5) > 1e-9 {
		t.Errorf("got %.4f, want 0.5", got)
	}

	if got := graphProximityFromTrace(nil); got != -1 {
		t.Errorf("nil stage: got %.4f, want -1", got)
	}
	if got := graphProximityFromTrace(&prompt.StageTrace{}); got != -1 {
		t.Errorf("empty details: got %.4f, want -1", got)
	}
}

// TestHitSimilarityClamp guards against future embedding models that
// could return distance > 1 (non-normalised vectors).
func TestHitSimilarityClamp(t *testing.T) {
	if got := hitSimilarity(0.0); got != 1.0 {
		t.Errorf("hitSimilarity(0.0) = %.4f, want 1.0", got)
	}
	if got := hitSimilarity(1.5); got != 0 {
		t.Errorf("hitSimilarity(1.5) = %.4f, want 0", got)
	}
	if got := hitSimilarity(-0.1); got != 1.0 {
		t.Errorf("hitSimilarity(-0.1) = %.4f, want 1.0", got)
	}
}

// TestMemoryStructuralVsFreetext asserts the field split is exactly what
// the must-filter relies on. If someone moves "purpose" into the
// structural list, the seat-provider regression returns immediately —
// this test will catch it.
func TestMemoryStructuralVsFreetext(t *testing.T) {
	h := mhit("flow", 0.3, map[string]string{
		"name":    "auth-flow",
		"purpose": "handles error recovery",
	})
	if !containsAnyTerm(memoryStructuralText(h), []string{"auth"}) {
		t.Error("structural text missing 'auth'")
	}
	if containsAnyTerm(memoryStructuralText(h), []string{"error"}) {
		t.Error("structural text incorrectly contains 'error' (came from purpose)")
	}
	if !containsAnyTerm(memoryFreetextText(h), []string{"error"}) {
		t.Error("free-text missing 'error'")
	}
}
