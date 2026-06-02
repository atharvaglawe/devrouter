package crossrepo

import (
	"strings"
	"unicode"
)

// LinkTag resolves a provider tag observed in `sourceRepo` (e.g.
// `httpclient.GetClient("cmadserving")`, `config.GetXConfig().Url` →
// "x") to a ranked list of target repos it might refer to. Returns an
// empty slice when no match is found above the floor confidence.
//
// Resolution strategy (highest-confidence rule wins, ties broken by
// name length to prefer specific over generic):
//
//	1.0  exact name match            tag == target.Name
//	0.9  case-insensitive exact      strings.EqualFold(tag, target.Name)
//	0.8  tag is substring of name    contains(target.Name, tag) for tags >= 4 chars
//	0.7  target name is substring    contains(tag, target.Name) for names >= 4 chars
//	0.6  token overlap on name       split-by-camel/snake/kebab share >= 1 long token
//
// The 4-char floor on substring rules keeps "go" / "py" / "js" from
// matching half the registry. The token-overlap rule handles cases
// like tag="adserving" → target="cmadserving" via the shared "adserving"
// token (camelCase split).
//
// Source repo is excluded from candidates — a tag observed in goserving
// pointing at goserving itself is a no-op cross-repo link.
//
// The returned links carry SourceRepo, Tag, TargetRepo, Confidence,
// and a Reason string for provenance ("name:exact", "tokens:adserving",
// etc.). Empty tag returns nil.
func (l *RepoLinker) LinkTag(sourceRepo, tag string) []RepoLink {
	tag = strings.TrimSpace(tag)
	if tag == "" {
		return nil
	}
	reg, err := l.registry.Load()
	if err != nil || reg == nil || len(reg.Entries) == 0 {
		return nil
	}

	tagLower := strings.ToLower(tag)
	tagTokens := splitIdentifierTokens(tag)

	// Dedup target names: the registry can contain stale duplicate
	// rows (see Registry.Names doc). We pick the first occurrence and
	// skip subsequent ones so a duplicated repo isn't reported twice.
	seenTarget := make(map[string]struct{}, len(reg.Entries))
	var out []scoredLink
	for _, e := range reg.Entries {
		if e.Name == "" || strings.EqualFold(e.Name, sourceRepo) {
			continue
		}
		if _, dup := seenTarget[e.Name]; dup {
			continue
		}
		seenTarget[e.Name] = struct{}{}
		confidence, reason := scoreTagAgainstRepo(tagLower, tagTokens, e.Name)
		if confidence == 0 {
			continue
		}
		out = append(out, scoredLink{
			link: RepoLink{
				SourceRepo: sourceRepo,
				Tag:        tag,
				TargetRepo: e.Name,
				Confidence: confidence,
				Reason:     reason,
			},
			nameLen: len(e.Name),
		})
	}

	sortLinksScored(out)

	links := make([]RepoLink, 0, len(out))
	for _, s := range out {
		links = append(links, s.link)
	}
	return links
}

// scoredLink is the internal sort carrier used by LinkTag. nameLen is
// the secondary sort key: prefer longer (more specific) target names
// on confidence ties, since "cmadserving" beats "ad" for tag
// "adserving" when both are substring matches.
type scoredLink struct {
	link    RepoLink
	nameLen int
}

// scoreTagAgainstRepo returns (confidence, reason) for a single
// (tag, repoName) pair following the rules documented on LinkTag.
// Returns (0, "") when no rule matches.
func scoreTagAgainstRepo(tagLower string, tagTokens []string, repoName string) (float64, string) {
	nameLower := strings.ToLower(repoName)

	if tagLower == nameLower {
		// Case differentiates exact vs case-insensitive — distinct
		// enough to be worth separate confidences when both could
		// fire (we picked the higher one).
		if tagLower == repoName {
			return 1.0, "name:exact"
		}
		return 0.9, "name:exact-ci"
	}

	if len(tagLower) >= 4 && strings.Contains(nameLower, tagLower) {
		return 0.8, "name:contains-tag"
	}
	if len(nameLower) >= 4 && strings.Contains(tagLower, nameLower) {
		return 0.7, "tag:contains-name"
	}

	// Token overlap on identifier split. Only count tokens >= 4 chars
	// to avoid "go" / "js" / "api" trivially matching everything.
	nameTokens := splitIdentifierTokens(repoName)
	shared := overlapLongTokens(tagTokens, nameTokens, 4)
	if shared != "" {
		return 0.6, "tokens:" + shared
	}

	return 0, ""
}

// splitIdentifierTokens breaks a mixed-case / snake / kebab / dotted
// identifier into lowercase tokens. Used for the token-overlap rule
// in LinkTag.
//
//	"cmadserving"     → ["cmadserving"]                (no boundaries)
//	"goServing"       → ["go", "serving"]              (camelCase)
//	"go-serving"      → ["go", "serving"]              (kebab)
//	"go_serving"      → ["go", "serving"]              (snake)
//	"go.serving.url"  → ["go", "serving", "url"]       (dotted config key)
//	"CMAdServing"     → ["cm", "ad", "serving"]        (UpperCamel + acronym)
func splitIdentifierTokens(s string) []string {
	if s == "" {
		return nil
	}
	// Pass 1: split on non-letter/digit separators.
	parts := strings.FieldsFunc(s, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	var out []string
	for _, p := range parts {
		// Pass 2: camelCase split inside each separator-bounded chunk.
		var buf []rune
		flush := func() {
			if len(buf) > 0 {
				out = append(out, strings.ToLower(string(buf)))
				buf = buf[:0]
			}
		}
		for i, r := range p {
			if i > 0 && unicode.IsUpper(r) {
				flush()
			}
			buf = append(buf, r)
		}
		flush()
	}
	return out
}

// overlapLongTokens returns the first token of minLen or more that
// appears in both lists. Returns "" when no qualifying overlap exists.
// Deterministic by iterating the first slice in order.
func overlapLongTokens(a, b []string, minLen int) string {
	if len(a) == 0 || len(b) == 0 {
		return ""
	}
	set := make(map[string]struct{}, len(b))
	for _, t := range b {
		if len(t) >= minLen {
			set[t] = struct{}{}
		}
	}
	for _, t := range a {
		if len(t) < minLen {
			continue
		}
		if _, ok := set[t]; ok {
			return t
		}
	}
	return ""
}

// sortLinksScored sorts links in-place by (confidence desc, nameLen
// desc, name asc). Insertion sort — N is small (|registry| candidates
// per call, typically < 30) and we want deterministic ordering on
// ties, which the standard library's sort.Slice doesn't guarantee.
func sortLinksScored(s []scoredLink) {
	for i := 1; i < len(s); i++ {
		j := i
		for j > 0 && lessLinkScored(s[j], s[j-1]) {
			s[j], s[j-1] = s[j-1], s[j]
			j--
		}
	}
}

func lessLinkScored(a, b scoredLink) bool {
	if a.link.Confidence != b.link.Confidence {
		return a.link.Confidence > b.link.Confidence
	}
	if a.nameLen != b.nameLen {
		return a.nameLen > b.nameLen
	}
	return a.link.TargetRepo < b.link.TargetRepo
}
