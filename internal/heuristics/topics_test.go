package heuristics

import (
	"strings"
	"testing"
)

// TestMergeCentroidRunningMean exercises the online running-mean update.
// After absorbing N identical samples the centroid should equal the
// sample (post-normalisation). And after absorbing one sample
// orthogonal to the previous mean, the new centroid should be a
// blend roughly halfway through the angular gap (loose check —
// we only assert the cosine moved in the expected direction).
func TestMergeCentroidRunningMean(t *testing.T) {
	a := []float32{1, 0, 0}
	b := []float32{1, 0, 0}
	merged := mergeCentroid(a, b, 5)
	if got := dot(merged, []float32{1, 0, 0}); got < 0.999 {
		t.Errorf("identical samples should keep centroid pointing same way; cos=%v", got)
	}

	c := []float32{0, 1, 0}
	merged = mergeCentroid(a, c, 1)
	// Should land roughly halfway in angular terms — neither pure-x nor pure-y.
	dx := dot(merged, []float32{1, 0, 0})
	dy := dot(merged, []float32{0, 1, 0})
	if dx <= 0 || dy <= 0 {
		t.Errorf("centroid should sit between samples; got dx=%v dy=%v", dx, dy)
	}
	if dx > 0.99 || dy > 0.99 {
		t.Errorf("centroid should not equal either sample; dx=%v dy=%v", dx, dy)
	}
}

func TestNormaliseRepoFallsBackToGlobal(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", globalRepo},
		{"   ", globalRepo},
		{"repoA", "repoA"},
		{"  repoB  ", "repoB"},
	}
	for _, c := range cases {
		if got := normaliseRepo(c.in); got != c.want {
			t.Errorf("normaliseRepo(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestNextTopicIDFindsLowestFreeSlot(t *testing.T) {
	entries := []TopicEntry{
		{ID: "t-0"}, {ID: "t-2"}, {ID: "t-3"},
	}
	if got := nextTopicID(entries); got != "t-1" {
		t.Errorf("expected t-1 (lowest free); got %s", got)
	}
	entries = []TopicEntry{{ID: "t-0"}, {ID: "t-1"}, {ID: "t-2"}}
	if got := nextTopicID(entries); got != "t-3" {
		t.Errorf("expected t-3 (next free after dense); got %s", got)
	}
	// Mixed in a non-conformant id — ignored.
	entries = []TopicEntry{{ID: "t-0"}, {ID: "custom"}, {ID: "t-1"}}
	if got := nextTopicID(entries); got != "t-2" {
		t.Errorf("expected t-2 (custom id ignored); got %s", got)
	}
}

// TestResolveNilStoreReturnsGlobal pins the "Redis not available"
// degradation contract: Resolve must never panic and must always
// return IntentGlobalTopic so the router can keep serving queries
// even with Redis unreachable. Label must be empty in this path —
// nothing was created, nothing to label.
func TestResolveNilStoreReturnsGlobal(t *testing.T) {
	var s *TopicStore // nil deliberately
	embed := make([]float32, topicEmbedFloatsExpected)
	embed[0] = 1.0
	id, label := s.Resolve(nil, "debug", "repoA", embed, "how does redis session cache work")
	if id != IntentGlobalTopic {
		t.Errorf("nil store should yield %q; got %q", IntentGlobalTopic, id)
	}
	if label != "" {
		t.Errorf("nil store should yield empty label; got %q", label)
	}
}

// TestSamplesGlobalBucketIsZero asserts that asking for the
// IntentGlobalTopic always returns 0 — used by picker.PickWithTopic
// to bypass the floor-check fast path.
func TestSamplesGlobalBucketIsZero(t *testing.T) {
	var s *TopicStore
	if got := s.Samples(nil, "debug", "repoA", IntentGlobalTopic); got != 0 {
		t.Errorf("global bucket samples must be 0; got %d", got)
	}
}

// TestExtractTopicLabel pins the per-query keyword extractor: it must
// produce stable, kebab-case, stopword-free labels capped at the token
// limit; and return "" on signal-free inputs so the caller falls back
// to the topic_id cleanly.
func TestExtractTopicLabel(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"basic_question_form", "How does redis session caching work?", "redis-session-caching"},
		{"strips_punctuation", "Explain: the OAuth2 token-refresh flow!", "oauth2-token-refresh"},
		{"dedupe_preserves_order", "redis redis session redis cache", "redis-session-cache"},
		{"caps_at_three_tokens", "kubernetes ingress controller traffic routing rules", "kubernetes-ingress-controller"},
		{"drops_pure_numbers", "fix bug 12345 in the parser module", "bug-parser-module"},
		{"drops_short_tokens", "DB and ORM in our app", "orm-app"},
		{"all_stopwords_yields_empty", "how does it work please show me", ""},
		{"empty_input", "", ""},
		{"only_punctuation", "??? !!! ...", ""},
		{"camel_case_preserved_as_one", "ParseHTTPRequest helper used here", "parsehttprequest-helper"},
		{"length_cap_with_hyphen_boundary", "supercalifragilisticexpialidocious antidisestablishmentarianism floccinaucinihilipilification", "supercalifragilisticexpialidocious"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ExtractTopicLabel(c.in); got != c.want {
				t.Errorf("ExtractTopicLabel(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// TestExtractTopicLabelLengthCap verifies the cap-at-topicLabelMaxLen
// rule, including the "prefer cutting at hyphen boundary" behaviour
// so we never lop a token in half.
func TestExtractTopicLabelLengthCap(t *testing.T) {
	// Three real-ish tokens that together exceed the cap; the cap
	// should land us at a clean boundary, not mid-token.
	in := "authentication authorization session-management"
	got := ExtractTopicLabel(in)
	if len(got) > topicLabelMaxLen {
		t.Errorf("label exceeds cap %d: %q (len=%d)", topicLabelMaxLen, got, len(got))
	}
	if strings.HasSuffix(got, "-") {
		t.Errorf("label should not end with a hyphen: %q", got)
	}
}

// TestIsAllDigits is a tiny utility test — but the all-digits filter
// is the gate that keeps "bug-12345" out of labels, so worth pinning.
func TestIsAllDigits(t *testing.T) {
	cases := map[string]bool{
		"":      false,
		"0":     true,
		"12345": true,
		"12a":   false,
		"a12":   false,
		"x":     false,
	}
	for in, want := range cases {
		if got := isAllDigits(in); got != want {
			t.Errorf("isAllDigits(%q) = %v, want %v", in, got, want)
		}
	}
}
