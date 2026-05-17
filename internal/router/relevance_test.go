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

// TestFilterSubgraphByPlanRelevantSeedAdjacency is the regression test for
// the "lots of noise from bare-name fallback seeds" Flow UI bug.
//
// Reproduces a real captured snapshot for the
// `{must=[rps multipage], context_hints=[rps]}` plan where:
//   - "MultiPageRpsSERP.Init" is the agent's qualified entry point.
//   - "Init" is a synthetic bare-name fallback seed (added inside
//     codegraph.Subgraph because the qualified form didn't resolve).
//
// Assertions:
//   - 1-hop callees of the relevant seed survive even when their own
//     name doesn't carry the must term (immediate flow context).
//   - The synthetic "Init" seed is itself dropped, taking its unrelated
//     callers/callees and incoming IMPORTS with it.
//   - 2-hop nodes that directly match a must-term are kept.
func TestFilterSubgraphByPlanRelevantSeedAdjacency(t *testing.T) {
	sg := &codegraph.Subgraph{
		Seeds: []string{"MultiPageRpsSERP.Init", "Init"},
		Nodes: []codegraph.SubgraphNode{
			{Name: "MultiPageRpsSERP.Init", Role: "seed", Depth: 0,
				FilePath: "oscar/app/pkg/multipagerpsserp/multipagerpsserp.go"},
			{Name: "Init", Role: "seed", Depth: 0},
			{Name: "SetMultiPageRpsData", Role: "callee", Depth: 1,
				FilePath: "cmpkg/cme/setters.go"},
			{Name: "AddSingleParams", Role: "callee", Depth: 1,
				FilePath: "cmpkg/cme/setters.go"},
			{Name: "chnm3", Role: "callee", Depth: 1,
				FilePath: "cmpkg/channelname/chnm3.go"},
			{Name: "EnableLogging", Role: "callee", Depth: 1,
				FilePath: "lib/stdlog/debugapplog.go"},
			{Name: "GetRpsProviderId", Role: "callee", Depth: 2,
				FilePath: "cmpkg/rpsencryptedparams/getters.go"},
			{Name: "Tag", Role: "callee", Depth: 2,
				FilePath: "lib/debug/taggableVardump.go"},
			{Name: "factory.go", Role: "importer", Depth: 1,
				FilePath: "cmpkg/relink/buyplatforms/factory.go"},
		},
		Edges: []codegraph.SubgraphEdge{
			{From: "MultiPageRpsSERP.Init", To: "SetMultiPageRpsData", Type: "CALLS"},
			{From: "MultiPageRpsSERP.Init", To: "AddSingleParams", Type: "CALLS"},
			{From: "SetMultiPageRpsData", To: "GetRpsProviderId", Type: "CALLS"},
			{From: "AddSingleParams", To: "Tag", Type: "CALLS"},
			{From: "Init", To: "chnm3", Type: "CALLS"},
			{From: "Init", To: "EnableLogging", Type: "CALLS"},
			{From: "factory.go", To: "Init", Type: "IMPORTS"},
		},
	}

	plan := QueryPlan{
		MustTerms:    []string{"rps", "multipage"},
		ContextHints: []string{"rps"},
	}
	// Only the qualified form was supplied by the agent. "Init" is
	// a synthetic bare-name fallback that should NOT be protected.
	agentSeeds := []string{"MultiPageRpsSERP.Init"}

	out := filterSubgraphByPlan(sg, plan, agentSeeds)
	if out == nil {
		t.Fatal("filterSubgraphByPlan returned nil")
	}

	gotNames := make(map[string]bool, len(out.Nodes))
	for _, n := range out.Nodes {
		gotNames[n.Name] = true
	}
	mustKeep := []string{
		"MultiPageRpsSERP.Init", // protected agent seed
		"SetMultiPageRpsData",   // hop-1 to relevant seed
		"AddSingleParams",       // hop-1 to relevant seed
		"GetRpsProviderId",      // direct must-term match (rps)
	}
	for _, name := range mustKeep {
		if !gotNames[name] {
			t.Errorf("expected %q to survive filter, got nodes=%v", name, gotNames)
		}
	}
	mustDrop := []string{
		"Init",          // synthetic bare-name seed, not protected
		"chnm3",         // hop-1 to synthetic seed
		"EnableLogging", // hop-1 to synthetic seed
		"Tag",           // hop-2, no must match
		"factory.go",    // IMPORTS hub, importer doesn't match must
	}
	for _, name := range mustDrop {
		if gotNames[name] {
			t.Errorf("expected %q to be dropped, but it survived. nodes=%v", name, gotNames)
		}
	}

	survivingSeeds := make(map[string]bool, len(out.Seeds))
	for _, s := range out.Seeds {
		survivingSeeds[s] = true
	}
	if survivingSeeds["Init"] {
		t.Error("synthetic seed 'Init' should be removed from out.Seeds")
	}
	if !survivingSeeds["MultiPageRpsSERP.Init"] {
		t.Error("agent seed 'MultiPageRpsSERP.Init' should remain in out.Seeds")
	}

	for _, e := range out.Edges {
		if !gotNames[e.From] || !gotNames[e.To] {
			t.Errorf("dangling edge after filter: %s -> %s (%s)", e.From, e.To, e.Type)
		}
	}
}

// TestFilterSubgraphByPlanProtectsAgentSeedsLiteral covers the
// edge case where the agent literally supplies a generic name like
// "Init" as an entry_point — we must respect that and keep it even
// when it doesn't match must-terms.
func TestFilterSubgraphByPlanProtectsAgentSeedsLiteral(t *testing.T) {
	sg := &codegraph.Subgraph{
		Seeds: []string{"Init"},
		Nodes: []codegraph.SubgraphNode{
			{Name: "Init", Role: "seed", Depth: 0},
			{Name: "noise", Role: "callee", Depth: 1, FilePath: "lib/x.go"},
		},
		Edges: []codegraph.SubgraphEdge{
			{From: "Init", To: "noise", Type: "CALLS"},
		},
	}
	out := filterSubgraphByPlan(sg, QueryPlan{MustTerms: []string{"rps"}}, []string{"Init"})
	got := make(map[string]bool, len(out.Nodes))
	for _, n := range out.Nodes {
		got[n.Name] = true
	}
	if !got["Init"] {
		t.Error("agent-supplied 'Init' should be protected even without must-term match")
	}
	if got["noise"] {
		t.Error("non-relevant callee of literal-but-irrelevant seed should still drop")
	}
}

// TestFilterSubgraphByPlanEmptyPlanIsPassthrough guards against the
// filter accidentally dropping nodes when no plan is supplied (e.g.
// snapshots saved without query_id).
func TestFilterSubgraphByPlanEmptyPlanIsPassthrough(t *testing.T) {
	sg := &codegraph.Subgraph{
		Seeds: []string{"S"},
		Nodes: []codegraph.SubgraphNode{
			{Name: "S", Role: "seed", Depth: 0},
			{Name: "X", Role: "callee", Depth: 1, FilePath: "lib/x.go"},
		},
		Edges: []codegraph.SubgraphEdge{
			{From: "S", To: "X", Type: "CALLS"},
		},
	}
	out := filterSubgraphByPlan(sg, QueryPlan{}, []string{"S"})
	if len(out.Nodes) != 2 || len(out.Edges) != 1 {
		t.Errorf("empty plan should be a no-op: got nodes=%d edges=%d, want 2/1",
			len(out.Nodes), len(out.Edges))
	}
}

// TestCollapseSubgraphToFileLevel asserts the function-level subgraph
// is correctly aggregated into one node per file:
//   - Multiple functions in the same file collapse to a single node.
//   - Self-loops (intra-file calls) are dropped.
//   - The agent-supplied seed's containing file is the seed file.
//   - Edge endpoints are full file paths so the node Name is unique.
//   - Duplicate function-edges between the same pair of files dedupe
//     to one file edge.
func TestCollapseSubgraphToFileLevel(t *testing.T) {
	sg := &codegraph.Subgraph{
		Seeds: []string{"pkg.Seed"},
		Nodes: []codegraph.SubgraphNode{
			{Name: "pkg.Seed", Role: "seed", Depth: 0, FilePath: "a/seed.go"},
			{Name: "pkg.HelperA", Role: "callee", Depth: 1, FilePath: "a/seed.go"},
			{Name: "pkg.HelperB", Role: "callee", Depth: 1, FilePath: "a/helpers.go"},
			{Name: "pkg.HelperC", Role: "callee", Depth: 2, FilePath: "a/helpers.go"},
			{Name: "pkg.Logger", Role: "callee", Depth: 1, FilePath: "lib/log.go"},
		},
		Edges: []codegraph.SubgraphEdge{
			// Intra-file: dropped after collapse.
			{From: "pkg.Seed", To: "pkg.HelperA", Type: "CALLS"},
			// a/seed.go → a/helpers.go (two function edges, dedupe to one).
			{From: "pkg.Seed", To: "pkg.HelperB", Type: "CALLS"},
			{From: "pkg.HelperA", To: "pkg.HelperC", Type: "CALLS"},
			// a/seed.go → lib/log.go.
			{From: "pkg.Seed", To: "pkg.Logger", Type: "CALLS"},
		},
	}
	out := collapseSubgraphToFileLevel(sg, []string{"pkg.Seed"})
	if out == nil {
		t.Fatal("collapseSubgraphToFileLevel returned nil")
	}

	gotFiles := make(map[string]codegraph.SubgraphNode, len(out.Nodes))
	for _, n := range out.Nodes {
		gotFiles[n.Name] = n
	}
	wantPaths := []string{"a/seed.go", "a/helpers.go", "lib/log.go"}
	for _, p := range wantPaths {
		if _, ok := gotFiles[p]; !ok {
			t.Errorf("expected file node %q, got %v", p, gotFiles)
		}
	}
	if len(out.Nodes) != 3 {
		t.Errorf("expected 3 file nodes, got %d (%v)", len(out.Nodes), out.Nodes)
	}
	if got := gotFiles["a/seed.go"].Role; got != "seed" {
		t.Errorf("a/seed.go role: got %q, want seed", got)
	}
	if got := gotFiles["a/seed.go"].FilePath; got != "a/seed.go" {
		t.Errorf("a/seed.go FilePath: got %q, want a/seed.go", got)
	}

	// Two file edges: a/seed.go -> a/helpers.go (dedup of 2 fn edges)
	// and a/seed.go -> lib/log.go.
	if len(out.Edges) != 2 {
		t.Fatalf("expected 2 deduped file edges, got %d (%+v)", len(out.Edges), out.Edges)
	}
	type pair struct{ from, to string }
	seen := make(map[pair]bool)
	for _, e := range out.Edges {
		if e.From == e.To {
			t.Errorf("self-loop survived collapse: %+v", e)
		}
		seen[pair{e.From, e.To}] = true
	}
	for _, want := range []pair{{"a/seed.go", "a/helpers.go"}, {"a/seed.go", "lib/log.go"}} {
		if !seen[want] {
			t.Errorf("missing file edge %s -> %s", want.from, want.to)
		}
	}

	if len(out.Seeds) != 1 || out.Seeds[0] != "a/seed.go" {
		t.Errorf("Seeds: got %v, want [a/seed.go]", out.Seeds)
	}
}

// TestCollapseSubgraphToFileLevelMultipleSeedsInSameFile verifies that
// when several agent seeds resolve to the same file, the file shows up
// as a seed node exactly once.
func TestCollapseSubgraphToFileLevelMultipleSeedsInSameFile(t *testing.T) {
	sg := &codegraph.Subgraph{
		Seeds: []string{"pkg.SeedA", "pkg.SeedB"},
		Nodes: []codegraph.SubgraphNode{
			{Name: "pkg.SeedA", Role: "seed", Depth: 0, FilePath: "a/file.go"},
			{Name: "pkg.SeedB", Role: "seed", Depth: 0, FilePath: "a/file.go"},
		},
	}
	out := collapseSubgraphToFileLevel(sg, []string{"pkg.SeedA", "pkg.SeedB"})
	if len(out.Nodes) != 1 || out.Nodes[0].Name != "a/file.go" {
		t.Errorf("expected one seed file node, got %+v", out.Nodes)
	}
	if len(out.Seeds) != 1 || out.Seeds[0] != "a/file.go" {
		t.Errorf("seeds dedup: got %v", out.Seeds)
	}
}

// TestFilterSubgraphByPlanExcludeOnly verifies exclude terms apply even
// when no must-terms are supplied.
func TestFilterSubgraphByPlanExcludeOnly(t *testing.T) {
	sg := &codegraph.Subgraph{
		Seeds: []string{"S"},
		Nodes: []codegraph.SubgraphNode{
			{Name: "S", Role: "seed", Depth: 0},
			{Name: "AdClickHandler", Role: "callee", Depth: 1, FilePath: "ad/handler.go"},
			{Name: "ConfigLoader_test", Role: "callee", Depth: 1, FilePath: "config/loader_test.go"},
		},
		Edges: []codegraph.SubgraphEdge{
			{From: "S", To: "AdClickHandler", Type: "CALLS"},
			{From: "S", To: "ConfigLoader_test", Type: "CALLS"},
		},
	}
	out := filterSubgraphByPlan(sg, QueryPlan{ExcludeTerms: []string{"test"}}, []string{"S"})
	got := make(map[string]bool, len(out.Nodes))
	for _, n := range out.Nodes {
		got[n.Name] = true
	}
	if !got["AdClickHandler"] {
		t.Error("AdClickHandler should be kept")
	}
	if got["ConfigLoader_test"] {
		t.Error("ConfigLoader_test should be excluded by 'test'")
	}
}
