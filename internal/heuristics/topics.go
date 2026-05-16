package heuristics

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// ---------------------------------------------------------------------------
// Tunables (env-overridable; defaults sit safely on the "small footprint,
// graceful cold start" side so a fresh install never goes into a runaway
// bandit fan-out).
// ---------------------------------------------------------------------------

// MaxTopicsPerBucket caps the number of distinct centroids per
// (intent, repo). Hard ceiling: when a new query doesn't fit any
// existing centroid AND the bucket is at the cap, we LRU-evict the
// least-recently-seen topic and reuse its id slot for the new
// centroid. Default 32 captures real diversity per (intent, repo)
// without exploding bandit fan-out — see docs/configuration.md for
// the rationale.
var MaxTopicsPerBucket = envInt("DEVROUTER_HEURISTICS_MAX_TOPICS", 32, 1, 256)

// NewTopicCosineThreshold is the cosine-similarity floor below which a
// new query is treated as a new topic (rather than absorbed into the
// nearest existing centroid). Higher = more topics; lower = coarser
// buckets. 0.65 is permissive for nomic-embed-text-v1.5: same-domain
// queries cluster at sim >= 0.7 typically, so this lets most genuine
// domain shifts spawn a new bucket while still absorbing paraphrases.
var NewTopicCosineThreshold = envFloat("DEVROUTER_HEURISTICS_NEW_TOPIC_SIM", 0.65, 0.0, 1.0)

// TopicBanditSampleFloor is the per-bucket sample count required before
// the bandit will perturb a topic-specific profile. Below this floor,
// queries inherit the per-(intent, repo) global profile (today's
// behaviour) — so sample-starved buckets never get a flaky tuning
// that nobody can validate.
var TopicBanditSampleFloor = envInt("DEVROUTER_HEURISTICS_TOPIC_SAMPLE_FLOOR", 20, 0, 10_000)

// TopicsEnabled is the master switch. When false, the router skips
// Resolve entirely and every (intent, repo) uses IntentGlobalTopic.
// Default ON because the sample floor above gates the actual bandit
// changes — when off, even observational topic_id assignment is
// disabled.
var TopicsEnabled = envBool("DEVROUTER_HEURISTICS_TOPICS", true)

// IntentGlobalTopic is the sentinel topic_id used for the
// per-(intent, repo) "global" profile (the cold-start fallback and
// rollback target). Kept as "*" so existing Redis state from before
// this change keeps working without migration.
const IntentGlobalTopic = "*"

const (
	keyTopics = "heuristics:topics"

	// topicEmbedFloatsExpected is the embedding dimension we accept on
	// the centroid path. Mismatches are tolerated (we fall back to
	// IntentGlobalTopic) but logged once per process.
	topicEmbedFloatsExpected = 768
)

// globalRepo is the sentinel repo used when the caller hasn't supplied
// a repo name. Centroids still cluster per-intent in that degenerate
// case, just under a single Redis key.
const globalRepo = "*"

// TopicEntry is one centroid in heuristics:topics:{intent}:{repo},
// stored as a JSON value on the bucket hash. The centroid is
// L2-normalised so cosine reduces to a dot product on lookup.
//
// Label is a human-readable tag derived from the seed query at topic
// creation (e.g. "redis-session-cache"). It is *purely cosmetic* —
// nothing in the bandit, trace, or feedback paths keys on it; ID
// remains the stable identity. Label can be backfilled later for
// topics that were created before this field existed by re-resolving
// from a recent matching query (see Resolve's absorb path).
type TopicEntry struct {
	ID        string    `json:"id"`
	Label     string    `json:"label,omitempty"`
	Centroid  []float32 `json:"centroid"`
	Samples   int       `json:"samples"`
	CreatedAt int64     `json:"created_at_ms"`
	LastSeen  int64     `json:"last_seen_ms"`
}

// TopicStore manages the per-(intent, repo) centroid registry. Safe
// for concurrent use; per-bucket locks prevent two queries arriving
// simultaneously from double-creating the same topic.
type TopicStore struct {
	rdb *redis.Client

	mu     sync.Mutex
	locks  map[string]*sync.Mutex // bucketKey -> mutex (lazy)
	logDim sync.Once              // log unexpected embedding dim at most once
}

// NewTopicStore constructs a Redis-backed TopicStore. nil rdb is
// allowed (Resolve will degrade to IntentGlobalTopic) so unit tests
// and bench runs that skip Redis don't blow up.
func NewTopicStore(rdb *redis.Client) *TopicStore {
	return &TopicStore{
		rdb:   rdb,
		locks: map[string]*sync.Mutex{},
	}
}

func (s *TopicStore) bucketLock(bucketKey string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.locks[bucketKey]
	if !ok {
		m = &sync.Mutex{}
		s.locks[bucketKey] = m
	}
	return m
}

// Resolve returns the (topic_id, label) that best matches the given
// query embedding for the given (intent, repo). If a new cluster
// needs to be created, it is — subject to MaxTopicsPerBucket and the
// cosine threshold.
//
// queryText is used purely to derive the human-readable Label on
// topic creation (and to backfill existing topics whose Label is
// empty). An empty queryText is fine — Label will simply remain
// empty and the dashboard falls back to topic_id.
//
// Always returns a non-empty topic_id. On any Redis failure,
// embedding-dim mismatch, or when TopicsEnabled is false, returns
// (IntentGlobalTopic, "") so the caller can transparently keep going.
func (s *TopicStore) Resolve(ctx context.Context, intent, repo string, embed []float32, queryText string) (string, string) {
	if !TopicsEnabled || s == nil || s.rdb == nil || len(embed) == 0 {
		return IntentGlobalTopic, ""
	}
	if len(embed) != topicEmbedFloatsExpected {
		s.logDim.Do(func() {
			log.Printf("[heuristics] topic-resolve: unexpected embed dim %d (expected %d); falling back to %q",
				len(embed), topicEmbedFloatsExpected, IntentGlobalTopic)
		})
		return IntentGlobalTopic, ""
	}
	repo = normaliseRepo(repo)
	bucket := s.topicsKey(intent, repo)

	mu := s.bucketLock(bucket)
	mu.Lock()
	defer mu.Unlock()

	q := normalise(embed)
	entries, err := s.loadBucket(ctx, intent, repo)
	if err != nil {
		log.Printf("[heuristics] topic-resolve load %s failed (non-fatal): %v", bucket, err)
		return IntentGlobalTopic, ""
	}

	bestIdx, bestSim := -1, -1.0
	for i, e := range entries {
		sim := dot(q, e.Centroid)
		if sim > bestSim {
			bestSim, bestIdx = sim, i
		}
	}

	now := time.Now().UnixMilli()

	// Absorb into nearest centroid when the match is strong enough.
	// Backfill Label here if the existing topic predates the field —
	// that way the dashboard catches up the moment a labelable query
	// matches a legacy t-N entry, without needing a separate migration.
	if bestIdx >= 0 && bestSim >= NewTopicCosineThreshold {
		e := entries[bestIdx]
		e.Centroid = mergeCentroid(e.Centroid, q, e.Samples)
		e.Samples++
		e.LastSeen = now
		if e.Label == "" {
			if lbl := ExtractTopicLabel(queryText); lbl != "" {
				e.Label = lbl
			}
		}
		entries[bestIdx] = e
		if err := s.persistEntry(ctx, intent, repo, e); err != nil {
			log.Printf("[heuristics] topic-resolve absorb persist %s failed (non-fatal): %v", bucket, err)
		}
		return e.ID, e.Label
	}

	// LRU-evict and reuse the slot when at cap. We deliberately leave
	// the evicted topic's bandit state (heuristics:current /
	// heuristics:reward) alone — those keys age out via their own TTL
	// or get manually cleared via dev_heuristics_reset.
	if len(entries) >= MaxTopicsPerBucket {
		sort.Slice(entries, func(i, j int) bool {
			return entries[i].LastSeen < entries[j].LastSeen
		})
		victim := entries[0]
		newEntry := TopicEntry{
			ID:        victim.ID,
			Label:     ExtractTopicLabel(queryText),
			Centroid:  q,
			Samples:   1,
			CreatedAt: now,
			LastSeen:  now,
		}
		if err := s.persistEntry(ctx, intent, repo, newEntry); err != nil {
			log.Printf("[heuristics] topic-resolve evict persist %s failed (non-fatal): %v", bucket, err)
		}
		return newEntry.ID, newEntry.Label
	}

	// Free slot: synthesise the next sequential id.
	id := nextTopicID(entries)
	newEntry := TopicEntry{
		ID:        id,
		Label:     ExtractTopicLabel(queryText),
		Centroid:  q,
		Samples:   1,
		CreatedAt: now,
		LastSeen:  now,
	}
	if err := s.persistEntry(ctx, intent, repo, newEntry); err != nil {
		log.Printf("[heuristics] topic-resolve create persist %s failed (non-fatal): %v", bucket, err)
	}
	return id, newEntry.Label
}

// Samples returns the per-bucket sample count for (intent, repo,
// topic). Used by the picker to decide whether to bandit-tune a bucket
// or fall back to the (intent, repo) global profile. Returns 0 when
// topic_id is IntentGlobalTopic (the global bucket has no centroid)
// or when topics are disabled.
func (s *TopicStore) Samples(ctx context.Context, intent, repo, topic string) int {
	if !TopicsEnabled || s == nil || s.rdb == nil || topic == "" || topic == IntentGlobalTopic {
		return 0
	}
	repo = normaliseRepo(repo)
	entries, err := s.loadBucket(ctx, intent, repo)
	if err != nil {
		return 0
	}
	for _, e := range entries {
		if e.ID == topic {
			return e.Samples
		}
	}
	return 0
}

// List returns the full topic catalog for an (intent, repo), sorted
// by sample count desc (for the dashboard).
func (s *TopicStore) List(ctx context.Context, intent, repo string) []TopicEntry {
	if s == nil || s.rdb == nil {
		return nil
	}
	repo = normaliseRepo(repo)
	entries, err := s.loadBucket(ctx, intent, repo)
	if err != nil {
		return nil
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Samples != entries[j].Samples {
			return entries[i].Samples > entries[j].Samples
		}
		return entries[i].LastSeen > entries[j].LastSeen
	})
	return entries
}

// ListRepos returns every repo that has at least one topic centroid
// under the given intent. SCAN-based (the topic-registry hash count
// is small — bounded by MaxTopicsPerBucket * intents — so a SCAN
// here is cheap and stays correct without a separate registry to
// drift). Used by the dashboard to populate its repo filter.
func (s *TopicStore) ListRepos(ctx context.Context, intent string) []string {
	if s == nil || s.rdb == nil {
		return nil
	}
	pattern := fmt.Sprintf("%s:%s:*", keyTopics, intent)
	seen := map[string]struct{}{}
	var cursor uint64
	for {
		keys, next, err := s.rdb.Scan(ctx, cursor, pattern, 256).Result()
		if err != nil {
			break
		}
		for _, k := range keys {
			repo := strings.TrimPrefix(k, fmt.Sprintf("%s:%s:", keyTopics, intent))
			if repo != "" {
				seen[repo] = struct{}{}
			}
		}
		cursor = next
		if cursor == 0 {
			break
		}
	}
	out := make([]string, 0, len(seen))
	for r := range seen {
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------------------
// Redis I/O
// ---------------------------------------------------------------------------

func (s *TopicStore) loadBucket(ctx context.Context, intent, repo string) ([]TopicEntry, error) {
	key := s.topicsKey(intent, repo)
	raw, err := s.rdb.HGetAll(ctx, key).Result()
	if err == redis.Nil || len(raw) == 0 {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	out := make([]TopicEntry, 0, len(raw))
	for id, jsonVal := range raw {
		var e TopicEntry
		if err := json.Unmarshal([]byte(jsonVal), &e); err != nil {
			continue
		}
		if e.ID == "" {
			e.ID = id
		}
		// Defensive: keep the in-memory centroid normalised even if
		// somebody hand-poked Redis with an un-normalised vector.
		e.Centroid = normalise(e.Centroid)
		out = append(out, e)
	}
	return out, nil
}

func (s *TopicStore) persistEntry(ctx context.Context, intent, repo string, e TopicEntry) error {
	data, err := json.Marshal(e)
	if err != nil {
		return err
	}
	return s.rdb.HSet(ctx, s.topicsKey(intent, repo), e.ID, data).Err()
}

func (s *TopicStore) topicsKey(intent, repo string) string {
	return fmt.Sprintf("%s:%s:%s", keyTopics, intent, repo)
}

func normaliseRepo(repo string) string {
	repo = strings.TrimSpace(repo)
	if repo == "" {
		return globalRepo
	}
	return repo
}

// ---------------------------------------------------------------------------
// Vector helpers
// ---------------------------------------------------------------------------

func normalise(v []float32) []float32 {
	if len(v) == 0 {
		return v
	}
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	n := math.Sqrt(sum)
	if n == 0 {
		return v
	}
	out := make([]float32, len(v))
	inv := float32(1.0 / n)
	for i, x := range v {
		out[i] = x * inv
	}
	return out
}

func dot(a, b []float32) float64 {
	if len(a) != len(b) {
		return 0
	}
	var s float64
	for i := range a {
		s += float64(a[i]) * float64(b[i])
	}
	return s
}

// mergeCentroid updates a centroid with a new sample using an online
// running mean, then re-normalises so cosine == dot is preserved on
// the next lookup.
func mergeCentroid(old, sample []float32, oldSamples int) []float32 {
	if len(old) != len(sample) {
		return normalise(sample)
	}
	if oldSamples < 1 {
		oldSamples = 1
	}
	n := float32(oldSamples)
	out := make([]float32, len(old))
	for i := range old {
		out[i] = (old[i]*n + sample[i]) / (n + 1)
	}
	return normalise(out)
}

// nextTopicID returns "t-0", "t-1", … finding the lowest free index.
// Deterministic ordering keeps Redis keys human-readable and stable
// across process restarts.
func nextTopicID(existing []TopicEntry) string {
	used := make(map[int]bool, len(existing))
	for _, e := range existing {
		if n, ok := parseTopicID(e.ID); ok {
			used[n] = true
		}
	}
	for i := 0; ; i++ {
		if !used[i] {
			return fmt.Sprintf("t-%d", i)
		}
	}
}

func parseTopicID(id string) (int, bool) {
	const prefix = "t-"
	if !strings.HasPrefix(id, prefix) {
		return 0, false
	}
	n, err := strconv.Atoi(id[len(prefix):])
	return n, err == nil
}

// ---------------------------------------------------------------------------
// Topic labelling
// ---------------------------------------------------------------------------

// topicLabelMaxTokens is the upper bound on keyword tokens that make
// it into a label. Three is enough to be specific ("redis-session-
// cache", "ast-traversal-cache") without becoming a sentence.
const topicLabelMaxTokens = 3

// topicLabelMaxLen caps the final hyphen-joined label so it fits
// inside the dashboard's topic pill comfortably.
const topicLabelMaxLen = 40

// topicLabelStopwords is intentionally narrow: it covers structural
// English ("the", "is", "of") and the most common imperative verbs
// devs use to phrase questions ("explain", "show", "how"). We
// deliberately *don't* drop words like "function", "class", "file" —
// in the context of a code-search bandit those often *are* the
// topical signal (e.g. "redis client class").
var topicLabelStopwords = map[string]struct{}{
	// articles / determiners
	"a": {}, "an": {}, "the": {}, "this": {}, "that": {}, "these": {}, "those": {},
	"my": {}, "our": {}, "your": {}, "their": {}, "its": {}, "it": {}, "they": {},
	"some": {}, "any": {}, "all": {}, "no": {}, "none": {}, "each": {}, "every": {},
	// linkers / prepositions
	"and": {}, "or": {}, "but": {}, "if": {}, "so": {}, "than": {}, "then": {},
	"of": {}, "to": {}, "in": {}, "on": {}, "at": {}, "by": {}, "for": {}, "with": {},
	"from": {}, "into": {}, "onto": {}, "as": {}, "via": {}, "about": {},
	// be / aux verbs
	"is": {}, "are": {}, "was": {}, "were": {}, "be": {}, "been": {}, "being": {},
	"do": {}, "does": {}, "did": {}, "doing": {}, "done": {},
	"have": {}, "has": {}, "had": {}, "having": {},
	"will": {}, "would": {}, "can": {}, "could": {}, "should": {}, "may": {},
	"might": {}, "must": {}, "shall": {},
	// question words / generic instructional verbs (the noisy ones)
	"how": {}, "what": {}, "why": {}, "when": {}, "where": {}, "which": {},
	"who": {}, "whom": {}, "whose": {},
	"explain": {}, "show": {}, "tell": {}, "list": {}, "find": {}, "give": {},
	"get": {}, "set": {}, "make": {}, "see": {}, "look": {}, "help": {},
	"need": {}, "want": {}, "use": {}, "used": {}, "uses": {}, "using": {},
	"work": {}, "works": {}, "working": {}, "worked": {},
	// generic action verbs that show up in dev queries but rarely
	// carry topical signal ("fix the foo bug" — "fix" is noise, "foo"
	// is the actual topic). Kept separate from above for readability.
	"fix":    {}, "fixes": {}, "fixed": {}, "fixing": {},
	"add":    {}, "adds": {}, "added": {}, "adding": {},
	"remove": {}, "removes": {}, "removed": {}, "removing": {},
	"modify": {}, "modifies": {}, "modified": {}, "modifying": {},
	"update": {}, "updates": {}, "updated": {}, "updating": {},
	"please": {}, "just": {}, "also": {}, "only": {}, "very": {}, "more": {},
	"most": {}, "less": {}, "least": {}, "such": {}, "like": {}, "even": {},
	"here": {}, "there": {}, "now": {}, "yet": {}, "still": {},
	// generic code-noise that virtually every query contains
	"code": {}, "codebase": {}, "repo": {}, "project": {},
}

// ExtractTopicLabel turns a free-form query into a short, kebab-case
// label like "redis-session-cache". Returns "" when the query has no
// usable signal — caller falls back to the topic_id. Production
// callers go through Resolve; this function is exported only because
// the package's test file needs to pin the algorithm directly.
//
// Algorithm:
//  1. Lowercase + tokenize on non-alphanum.
//  2. Drop stopwords, tokens shorter than 3 chars, and pure numbers.
//  3. Deduplicate, preserving first-seen order (the head of a query
//     is overwhelmingly where the topical nouns live).
//  4. Take up to topicLabelMaxTokens tokens, hyphen-join, cap length.
//
// The algorithm is intentionally dependency-free and deterministic
// so a given seed query always produces the same label across
// processes / restarts.
func ExtractTopicLabel(query string) string {
	if query == "" {
		return ""
	}
	tokens := tokenizeForLabel(query)
	if len(tokens) == 0 {
		return ""
	}
	seen := make(map[string]struct{}, len(tokens))
	picked := make([]string, 0, topicLabelMaxTokens)
	for _, t := range tokens {
		if _, dup := seen[t]; dup {
			continue
		}
		seen[t] = struct{}{}
		picked = append(picked, t)
		if len(picked) >= topicLabelMaxTokens {
			break
		}
	}
	if len(picked) == 0 {
		return ""
	}
	label := strings.Join(picked, "-")
	if len(label) > topicLabelMaxLen {
		// Trim from the right at a hyphen boundary if possible so we
		// don't lop a token in half ("redis-session-c…" looks worse
		// than "redis-session").
		if cut := strings.LastIndex(label[:topicLabelMaxLen], "-"); cut > 0 {
			label = label[:cut]
		} else {
			label = label[:topicLabelMaxLen]
		}
	}
	return label
}

// tokenizeForLabel splits on any run of non-alphanumeric characters,
// lowercases, then filters out stopwords / shorts / pure numbers.
func tokenizeForLabel(s string) []string {
	s = strings.ToLower(s)
	out := make([]string, 0, 8)
	var cur strings.Builder
	flush := func() {
		if cur.Len() == 0 {
			return
		}
		tok := cur.String()
		cur.Reset()
		if len(tok) < 3 {
			return
		}
		if _, drop := topicLabelStopwords[tok]; drop {
			return
		}
		if isAllDigits(tok) {
			return
		}
		out = append(out, tok)
	}
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			cur.WriteRune(r)
			continue
		}
		flush()
	}
	flush()
	return out
}

func isAllDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

// ---------------------------------------------------------------------------
// env helpers
// ---------------------------------------------------------------------------

func envInt(name string, def, lo, hi int) int {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	if n < lo {
		return lo
	}
	if n > hi {
		return hi
	}
	return n
}

func envFloat(name string, def, lo, hi float64) float64 {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return def
	}
	if f < lo {
		return lo
	}
	if f > hi {
		return hi
	}
	return f
}

func envBool(name string, def bool) bool {
	v := strings.TrimSpace(strings.ToLower(os.Getenv(name)))
	switch v {
	case "":
		return def
	case "0", "false", "off", "no", "disabled", "none":
		return false
	default:
		return true
	}
}
