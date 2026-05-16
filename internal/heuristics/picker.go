package heuristics

import (
	"context"
	"log"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// Picker is the per-process handle for selecting a profile per query
// and feeding rewards back into the bandit.
//
// The picker has two surfaces:
//
//   - Legacy: Pick(intent) / Update(intent, ...). Tunes the
//     intent-global profile. Used when topics are disabled or for
//     bench / test paths that don't have a repo or embedding.
//   - Topic-aware: PickWithTopic(intent, repo, topic) /
//     UpdateWithTopic(...). Tunes the per-(intent, repo, topic)
//     bucket profile when the bucket has accumulated at least
//     TopicBanditSampleFloor samples; below that, transparently
//     falls back to the intent-global path.
//
// Topics is the centroid registry used to look up sample counts
// and decide whether a bucket has warmed up enough for per-bucket
// tuning. nil disables the topic-aware path entirely (everything
// behaves like Pick(intent)).
type Picker struct {
	Store  *Store
	Bandit *Bandit
	Topics *TopicStore
	frozen bool

	mu sync.RWMutex
}

// KnownIntents is the set of intents we eagerly seed defaults for at
// startup so the first dev_context call doesn't pay the seed cost.
// Mirrors router.IntentX constants — kept in sync by hand.
var KnownIntents = []string{"debug", "explore", "trace", "refactor", "general"}

// NewPicker constructs a Picker. Self-seeds defaults on first run and
// respects DEVROUTER_HEURISTICS_FROZEN to disable bandit mutations.
func NewPicker(rdb *redis.Client) *Picker {
	s := NewStore(rdb)
	frozen := strings.EqualFold(os.Getenv("DEVROUTER_HEURISTICS_FROZEN"), "true")
	p := &Picker{
		Store:  s,
		Bandit: NewBandit(s),
		Topics: NewTopicStore(rdb),
		frozen: frozen,
	}
	p.warmDefaults(context.Background())

	// Phase 2 ε-perturbation: if DEVROUTER_HEURISTICS_BANDIT="all" enable
	// every knob; if it's a comma-separated list (e.g. "max_trace") enable
	// just those. Default is empty (no perturbation, matching Phase 1).
	if !frozen {
		switch tune := os.Getenv("DEVROUTER_HEURISTICS_BANDIT"); tune {
		case "":
			// no-op
		case "all":
			p.Bandit.EnableAllKnobs()
			log.Printf("[heuristics] bandit enabled for all knobs (DEVROUTER_HEURISTICS_BANDIT=all)")
		default:
			for _, name := range strings.Split(tune, ",") {
				name = strings.TrimSpace(name)
				if name != "" {
					p.Bandit.EnableKnob(name)
				}
			}
			log.Printf("[heuristics] bandit enabled for knobs: %s", tune)
		}
	}

	if frozen {
		log.Printf("[heuristics] frozen mode: bandit updates disabled")
	}
	return p
}

// Frozen reports whether bandit mutations are disabled.
func (p *Picker) Frozen() bool { return p.frozen }

// warmDefaults pre-creates heuristics:current and heuristics:default for
// every known intent. Cheap (12 Redis round trips on first run, no-op
// thereafter).
func (p *Picker) warmDefaults(ctx context.Context) {
	for _, intent := range KnownIntents {
		if _, err := p.Store.CurrentProfile(ctx, intent); err != nil {
			log.Printf("[heuristics] warmDefaults %s: %v", intent, err)
		}
	}
}

// Pick returns the (profileID, profile) to use for an intent — the
// intent-global surface. Kept for callers (bench, tests) that don't
// have a repo or query embedding. Topic-aware callers should use
// PickWithTopic.
func (p *Picker) Pick(intent string) (string, Profile) {
	ctx := context.Background()
	p.mu.RLock()
	defer p.mu.RUnlock()
	base, err := p.Store.CurrentProfile(ctx, intent)
	if err != nil {
		log.Printf("[heuristics] pick %s fallback to default: %v", intent, err)
		def := Default(intent)
		return def.ID(), def
	}
	chosen := p.Bandit.Select(intent, "", IntentGlobalTopic, base, 0)
	return chosen.ID(), chosen
}

// Update feeds reward back into the bandit for the intent-global
// surface. No-op when frozen. Kept for the legacy single-intent
// callers (bench harnesses, anything pre-topic).
func (p *Picker) Update(intent, profileID string, adjustedReward, weight float64) {
	if p.frozen {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.Bandit.Update(intent, "", IntentGlobalTopic, profileID, adjustedReward, weight)
}

// PickWithTopic is the topic-aware analogue of Pick. It returns the
// bucket's profile when the bucket has at least TopicBanditSampleFloor
// samples (so the bandit has signal to lean on); otherwise it returns
// the intent-global profile so cold buckets behave exactly like
// today's pre-topic system. Also returns whether the topic-specific
// surface was used — observability hook for the trace.
func (p *Picker) PickWithTopic(intent, repo, topic string) (profileID string, profile Profile, fromTopic bool) {
	ctx := context.Background()
	p.mu.RLock()
	defer p.mu.RUnlock()

	// Fast path: legacy / cold-bucket. Avoids the centroid-sample
	// Redis round-trip when topics are off or the caller didn't
	// supply enough to address a bucket.
	if p.Topics == nil || isGlobalBucket(repo, topic) {
		base, err := p.Store.CurrentProfile(ctx, intent)
		if err != nil {
			log.Printf("[heuristics] pick %s fallback to default: %v", intent, err)
			def := Default(intent)
			return def.ID(), def, false
		}
		chosen := p.Bandit.Select(intent, "", IntentGlobalTopic, base, 0)
		return chosen.ID(), chosen, false
	}

	samples := p.Topics.Samples(ctx, intent, repo, topic)
	if samples < TopicBanditSampleFloor {
		// Cold bucket: serve the intent-global profile. Note we
		// still bind the (repo, topic) into the bandit's Select
		// call so that any candidate generated here is tracked
		// against this bucket — but the picker will return the
		// base profile because Select's threshold check (below)
		// skips perturbation when samples < floor.
		base, err := p.Store.CurrentProfile(ctx, intent)
		if err != nil {
			log.Printf("[heuristics] pick %s fallback to default: %v", intent, err)
			def := Default(intent)
			return def.ID(), def, false
		}
		chosen := p.Bandit.Select(intent, repo, topic, base, samples)
		return chosen.ID(), chosen, false
	}

	// Hot bucket: serve from the bucket's own profile (auto-seeded
	// from the intent-global on first read).
	base, err := p.Store.CurrentProfileFor(ctx, intent, repo, topic)
	if err != nil {
		log.Printf("[heuristics] pick %s/%s/%s fallback to default: %v",
			intent, repo, topic, err)
		def := Default(intent)
		return def.ID(), def, true
	}
	chosen := p.Bandit.Select(intent, repo, topic, base, samples)
	return chosen.ID(), chosen, true
}

// UpdateWithTopic is the topic-aware analogue of Update. Routes the
// reward to the bucket's bandit slot when (repo, topic) addresses a
// real bucket, otherwise falls through to the legacy intent-global
// path. No-op when frozen.
func (p *Picker) UpdateWithTopic(intent, repo, topic, profileID string, adjustedReward, weight float64) {
	if p.frozen {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if isGlobalBucket(repo, topic) {
		p.Bandit.Update(intent, "", IntentGlobalTopic, profileID, adjustedReward, weight)
		return
	}
	p.Bandit.Update(intent, repo, topic, profileID, adjustedReward, weight)
}

// Reset restores the frozen-default profile for an intent and clears any
// active candidate. Returns the default that was reinstated.
func (p *Picker) Reset(intent string) (Profile, error) {
	if p.frozen {
		// Even in frozen mode an admin reset is allowed because it's a
		// manual recovery operation, not an automated mutation.
		log.Printf("[heuristics] reset %s requested while frozen — proceeding (manual op)", intent)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.Bandit.ResetIntent(intent); err != nil {
		return Default(intent), err
	}
	def, err := p.Store.LoadDefault(context.Background(), intent)
	if err != nil {
		return Default(intent), err
	}
	return def, nil
}

// IntentStats summarizes per-intent reward distribution and bandit state.
// Buckets is the (optional) per-(repo, topic) breakdown — populated
// only when there's at least one hot bucket so the legacy view is
// unchanged for users who never enable topics.
type IntentStats struct {
	Intent             string         `json:"intent"`
	CurrentProfile     Profile        `json:"current_profile"`
	CurrentProfileID   string         `json:"current_profile_id"`
	SamplesToday       int            `json:"samples_today"`
	Samples7d          int            `json:"samples_7d"`
	MeanRawReward7d    float64        `json:"mean_raw_reward_7d,omitempty"`
	P50RawReward7d     float64        `json:"p50_raw_reward_7d,omitempty"`
	P95RawReward7d     float64        `json:"p95_raw_reward_7d,omitempty"`
	ExplicitFraction7d float64        `json:"explicit_fraction_7d,omitempty"`
	ImplicitFraction7d float64        `json:"implicit_fraction_7d,omitempty"`
	RecentHistory      []HistoryEntry `json:"recent_history,omitempty"`
	Buckets            []BucketStats  `json:"buckets,omitempty"`
}

// BucketStats is the per-(repo, topic) cell shown in the dashboard's
// nested heuristics view. Only emitted when the bucket has at least
// one centroid sample.
type BucketStats struct {
	Repo             string         `json:"repo"`
	TopicID          string         `json:"topic_id"`
	TopicLabel       string         `json:"topic_label,omitempty"`
	CentroidSamples  int            `json:"centroid_samples"`
	HotEnough        bool           `json:"hot_enough"` // samples >= TopicBanditSampleFloor
	CurrentProfile   Profile        `json:"current_profile"`
	CurrentProfileID string         `json:"current_profile_id"`
	Samples7d        int            `json:"samples_7d"`
	MeanRawReward7d  float64        `json:"mean_raw_reward_7d,omitempty"`
	RecentHistory    []HistoryEntry `json:"recent_history,omitempty"`
}

// Snapshot is the full output of Picker.Stats().
type Snapshot struct {
	Frozen       bool          `json:"frozen"`
	TunableKnobs []string      `json:"tunable_knobs"`
	SampleFloor  int           `json:"topic_sample_floor"`
	TopicsOn     bool          `json:"topics_enabled"`
	Intents      []IntentStats `json:"intents"`
}

// Stats returns a snapshot of per-intent reward distributions and bandit state.
func (p *Picker) Stats() Snapshot {
	ctx := context.Background()
	p.Bandit.mu.Lock()
	knobs := make([]string, 0, len(p.Bandit.TunableKnobs))
	for k := range p.Bandit.TunableKnobs {
		knobs = append(knobs, k)
	}
	p.Bandit.mu.Unlock()
	sort.Strings(knobs)

	out := Snapshot{
		Frozen:       p.frozen,
		TunableKnobs: knobs,
		SampleFloor:  TopicBanditSampleFloor,
		TopicsOn:     TopicsEnabled,
	}

	for _, intent := range KnownIntents {
		prof, _ := p.Store.CurrentProfile(ctx, intent)
		rows := p.Store.RecentRewards(ctx, intent, 7, 1000)
		is := IntentStats{
			Intent:           intent,
			CurrentProfile:   prof,
			CurrentProfileID: prof.ID(),
			Samples7d:        len(rows),
			RecentHistory:    p.Store.History(ctx, intent, 5),
		}
		is.Buckets = p.bucketStats(ctx, intent)
		if len(rows) == 0 {
			out.Intents = append(out.Intents, is)
			continue
		}
		var raws []float64
		var explicit, implicit int
		todayUTC := time.Now().UTC().Truncate(24 * time.Hour)
		var samplesToday int
		for _, r := range rows {
			raws = append(raws, r.RawReward)
			if r.Source == "explicit" {
				explicit++
			} else if r.Source == "implicit_repeat" {
				implicit++
			}
			if !time.UnixMilli(r.Timestamp).IsZero() &&
				time.UnixMilli(r.Timestamp).UTC().Truncate(24*time.Hour).Equal(todayUTC) {
				samplesToday++
			}
		}
		is.SamplesToday = samplesToday
		is.MeanRawReward7d = avg(raws)
		is.P50RawReward7d = percentile(raws, 0.5)
		is.P95RawReward7d = percentile(raws, 0.95)
		is.ExplicitFraction7d = float64(explicit) / float64(len(rows))
		is.ImplicitFraction7d = float64(implicit) / float64(len(rows))
		out.Intents = append(out.Intents, is)
	}
	return out
}

// bucketStats returns the per-(repo, topic) stat rows for an intent.
// Empty list when topics are off or the intent has never seen a
// real per-bucket query (cold install path).
func (p *Picker) bucketStats(ctx context.Context, intent string) []BucketStats {
	if p.Topics == nil || !TopicsEnabled {
		return nil
	}
	repos := p.Topics.ListRepos(ctx, intent)
	var out []BucketStats
	for _, repo := range repos {
		for _, t := range p.Topics.List(ctx, intent, repo) {
			rows := p.Store.RecentRewardsFor(ctx, intent, repo, t.ID, 7, 1000)
			var raws []float64
			for _, r := range rows {
				raws = append(raws, r.RawReward)
			}
			prof, _ := p.Store.CurrentProfileFor(ctx, intent, repo, t.ID)
			out = append(out, BucketStats{
				Repo:             repo,
				TopicID:          t.ID,
				TopicLabel:       t.Label,
				CentroidSamples:  t.Samples,
				HotEnough:        t.Samples >= TopicBanditSampleFloor,
				CurrentProfile:   prof,
				CurrentProfileID: prof.ID(),
				Samples7d:        len(rows),
				MeanRawReward7d:  avg(raws),
				RecentHistory:    p.Store.HistoryFor(ctx, intent, repo, t.ID, 5),
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Repo != out[j].Repo {
			return out[i].Repo < out[j].Repo
		}
		return out[i].TopicID < out[j].TopicID
	})
	return out
}

func avg(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	sum := 0.0
	for _, x := range xs {
		sum += x
	}
	return sum / float64(len(xs))
}

func percentile(xs []float64, p float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	cp := append([]float64(nil), xs...)
	sort.Float64s(cp)
	idx := int(float64(len(cp)-1) * p)
	if idx < 0 {
		idx = 0
	}
	if idx >= len(cp) {
		idx = len(cp) - 1
	}
	return cp[idx]
}
