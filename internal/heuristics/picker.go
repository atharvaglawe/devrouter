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
// In Phase 1 the picker just returns the current profile (no perturbation).
// Phase 2+ enable per-knob ε-perturbation through Bandit.EnableKnob /
// EnableAllKnobs.
type Picker struct {
	Store  *Store
	Bandit *Bandit
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

// Pick returns the (profileID, profile) to use for an intent. The bandit
// may return a perturbed candidate (under exploration); in Phase 1 it
// always returns the current live profile.
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
	chosen := p.Bandit.Select(intent, base)
	return chosen.ID(), chosen
}

// Update feeds reward back into the bandit. No-op when frozen.
func (p *Picker) Update(intent, profileID string, adjustedReward, weight float64) {
	if p.frozen {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.Bandit.Update(intent, profileID, adjustedReward, weight)
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
type IntentStats struct {
	Intent             string  `json:"intent"`
	CurrentProfile     Profile `json:"current_profile"`
	CurrentProfileID   string  `json:"current_profile_id"`
	SamplesToday       int     `json:"samples_today"`
	Samples7d          int     `json:"samples_7d"`
	MeanRawReward7d    float64 `json:"mean_raw_reward_7d,omitempty"`
	P50RawReward7d     float64 `json:"p50_raw_reward_7d,omitempty"`
	P95RawReward7d     float64 `json:"p95_raw_reward_7d,omitempty"`
	ExplicitFraction7d float64 `json:"explicit_fraction_7d,omitempty"`
	ImplicitFraction7d float64 `json:"implicit_fraction_7d,omitempty"`
	RecentHistory      []HistoryEntry `json:"recent_history,omitempty"`
}

// Snapshot is the full output of Picker.Stats().
type Snapshot struct {
	Frozen       bool          `json:"frozen"`
	TunableKnobs []string      `json:"tunable_knobs"`
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

	out := Snapshot{Frozen: p.frozen, TunableKnobs: knobs}

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
