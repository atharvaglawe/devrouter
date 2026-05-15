package heuristics

import (
	"context"
	"log"
	"math/rand"
	"sync"
	"time"
)

// banditStore is the slice of Store the bandit actually depends on.
// Defined as an interface so tests can mock without touching Redis.
type banditStore interface {
	SetCurrent(ctx context.Context, intent string, p Profile) error
	LoadDefault(ctx context.Context, intent string) (Profile, error)
	AppendHistory(ctx context.Context, intent string, entry HistoryEntry) error
}

// Bandit holds the in-memory ε-perturbation state per intent.
//
// Phase 1: Select always returns the base profile (TunableKnobs is empty).
// Phase 2: when one knob is enabled (e.g. "max_trace" for refactor),
//   ε of queries get a perturbed candidate; rewards attributed to
//   candidate vs base; promote on K-sample lift > δ; 3-strike rollback
//   on collapse.
// Phase 3: TunableKnobs covers all knobs.
type Bandit struct {
	Store banditStore

	// Tunable knobs, by canonical name. See allKnobs for the full set.
	TunableKnobs map[string]bool

	mu     sync.Mutex
	active map[string]*candidate
	rng    *rand.Rand
}

type candidate struct {
	intent      string
	profile     Profile
	profileID   string
	base        Profile
	baseID      string
	rewards     []float64 // candidate samples (weighted)
	baseSamples []float64 // base samples in same window (weighted)
	consecBad   int       // consecutive raw rewards < RollbackThreshold
}

// allKnobs is the canonical name list for every knob the bandit can
// perturb. Order matches Profile field order.
var allKnobs = []string{
	"max_trace", "caller_hops",
	"max_upstream", "max_downstream", "max_importers", "max_methods",
	"max_siblings", "max_snippets", "max_impact", "max_symbols",
	"max_primary_ctx", "max_decisions",
}

// AllKnobs returns a copy of the canonical knob name list.
func AllKnobs() []string {
	out := make([]string, len(allKnobs))
	copy(out, allKnobs)
	return out
}

// NewBandit constructs a bandit with no knobs enabled. Use EnableKnob
// or EnableAllKnobs to make knobs eligible for perturbation.
func NewBandit(s banditStore) *Bandit {
	return &Bandit{
		Store:        s,
		TunableKnobs: map[string]bool{},
		active:       map[string]*candidate{},
		rng:          rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

// EnableKnob marks a knob as bandit-tunable. Idempotent.
func (b *Bandit) EnableKnob(name string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.TunableKnobs[name] = true
}

// EnableAllKnobs marks every knob as tunable. Used in Phase 3.
func (b *Bandit) EnableAllKnobs() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, k := range allKnobs {
		b.TunableKnobs[k] = true
	}
}

// Epsilon is the perturbation rate (10% of queries get a candidate).
var Epsilon = 0.10

// PromotionWindow is how many candidate samples we collect before
// deciding to promote/discard.
var PromotionWindow = 20

// PromotionLift is the minimum mean lift over base required to promote.
var PromotionLift = 0.05

// RollbackThreshold is the per-sample raw-reward floor.
var RollbackThreshold = 0.30

// RollbackStrikes is the number of consecutive sub-threshold candidate
// samples that triggers a rollback to the frozen default profile.
var RollbackStrikes = 3

// Select returns the profile to use for this query — either base or a
// perturbed candidate (with probability Epsilon, if any knobs are
// tunable).
func (b *Bandit) Select(intent string, base Profile) Profile {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.TunableKnobs) == 0 {
		return base
	}
	// Reuse the active candidate when one is under test
	if c, ok := b.active[intent]; ok {
		if b.rng.Float64() < Epsilon {
			return c.profile
		}
		return base
	}
	// Generate a new candidate with probability Epsilon
	if b.rng.Float64() < Epsilon {
		cand := b.perturb(base)
		// Don't bother if perturbation produced an identical profile
		// (e.g. all tunable knobs hit a hard bound).
		if cand.ID() == base.ID() {
			return base
		}
		b.active[intent] = &candidate{
			intent:    intent,
			profile:   cand,
			profileID: cand.ID(),
			base:      base,
			baseID:    base.ID(),
		}
		log.Printf("[heuristics] %s new candidate %s (perturbed from %s)",
			intent, cand.ID(), base.ID())
		return cand
	}
	return base
}

// Update records a reward for either the candidate or base profile of an
// intent. Triggers promotion / discard / rollback when thresholds met.
func (b *Bandit) Update(intent, profileID string, adjustedReward, weight float64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	c, ok := b.active[intent]
	if !ok {
		return
	}
	weighted := adjustedReward * weight
	switch profileID {
	case c.profileID:
		c.rewards = append(c.rewards, weighted)
	case c.baseID:
		c.baseSamples = append(c.baseSamples, weighted)
	default:
		// Stale profile (e.g. previously-promoted candidate that's now
		// the base, or a profile that was rolled back). Ignore.
		return
	}

	// 3-strike rollback only on candidate (base is the known-good)
	if profileID == c.profileID {
		if adjustedReward < RollbackThreshold {
			c.consecBad++
			if c.consecBad >= RollbackStrikes {
				b.rollback(intent, c, "3-strike rollback (candidate underperforming)")
				return
			}
		} else {
			c.consecBad = 0
		}
	}

	// Promotion check
	if len(c.rewards) >= PromotionWindow && len(c.baseSamples) >= 5 {
		meanCand := mean(c.rewards)
		meanBase := mean(c.baseSamples)
		if meanCand > meanBase+PromotionLift {
			b.promote(intent, c)
		} else {
			b.discard(intent, c)
		}
	}
}

// ResetIntent clears the active candidate for an intent and restores the
// frozen default profile. Used by dev_heuristics_reset.
func (b *Bandit) ResetIntent(intent string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	ctx := context.Background()
	def, err := b.Store.LoadDefault(ctx, intent)
	if err != nil {
		def = Default(intent)
	}
	if err := b.Store.SetCurrent(ctx, intent, def); err != nil {
		return err
	}
	_ = b.Store.AppendHistory(ctx, intent, HistoryEntry{
		Timestamp: time.Now().UnixMilli(),
		Kind:      "rollback",
		To:        def,
		Reason:    "manual reset via dev_heuristics_reset",
	})
	delete(b.active, intent)
	return nil
}

func (b *Bandit) perturb(p Profile) Profile {
	knob := b.pickTunableKnob()
	if knob == "" {
		return p
	}
	delta := 1
	if b.rng.Float64() < 0.5 {
		delta = -1
	}
	out := p
	switch knob {
	case "max_trace":
		out.MaxTrace += delta
	case "caller_hops":
		out.CallerHops += delta
	case "max_upstream":
		out.MaxUpstream += delta
	case "max_downstream":
		out.MaxDownstream += delta
	case "max_importers":
		out.MaxImporters += delta
	case "max_methods":
		out.MaxMethods += delta
	case "max_siblings":
		out.MaxSiblings += delta
	case "max_snippets":
		out.MaxSnippets += delta
	case "max_impact":
		out.MaxImpact += delta
	case "max_symbols":
		out.MaxSymbols += delta
	case "max_primary_ctx":
		out.MaxPrimaryCtx += delta
	case "max_decisions":
		out.MaxDecisions += delta
	}
	return out.Clip()
}

// pickTunableKnob — caller must hold b.mu.
func (b *Bandit) pickTunableKnob() string {
	if len(b.TunableKnobs) == 0 {
		return ""
	}
	keys := make([]string, 0, len(b.TunableKnobs))
	for k := range b.TunableKnobs {
		keys = append(keys, k)
	}
	return keys[b.rng.Intn(len(keys))]
}

func (b *Bandit) promote(intent string, c *candidate) {
	ctx := context.Background()
	if err := b.Store.SetCurrent(ctx, intent, c.profile); err != nil {
		log.Printf("[heuristics] promote %s failed: %v", intent, err)
		return
	}
	_ = b.Store.AppendHistory(ctx, intent, HistoryEntry{
		Timestamp: time.Now().UnixMilli(),
		Kind:      "promote",
		From:      c.base,
		To:        c.profile,
		Reason:    "candidate beat base by >= PromotionLift over PromotionWindow samples",
	})
	log.Printf("[heuristics] %s promoted candidate (%s -> %s)", intent, c.baseID, c.profileID)
	delete(b.active, intent)
}

func (b *Bandit) discard(intent string, c *candidate) {
	ctx := context.Background()
	_ = b.Store.AppendHistory(ctx, intent, HistoryEntry{
		Timestamp: time.Now().UnixMilli(),
		Kind:      "discard",
		From:      c.profile,
		To:        c.base,
		Reason:    "candidate did not beat base over window",
	})
	log.Printf("[heuristics] %s discarded candidate %s (no lift)", intent, c.profileID)
	delete(b.active, intent)
}

func (b *Bandit) rollback(intent string, c *candidate, reason string) {
	ctx := context.Background()
	def, err := b.Store.LoadDefault(ctx, intent)
	if err != nil {
		def = Default(intent)
	}
	_ = b.Store.SetCurrent(ctx, intent, def)
	_ = b.Store.AppendHistory(ctx, intent, HistoryEntry{
		Timestamp: time.Now().UnixMilli(),
		Kind:      "rollback",
		From:      c.profile,
		To:        def,
		Reason:    reason,
	})
	log.Printf("[heuristics] %s ROLLBACK to default: %s", intent, reason)
	delete(b.active, intent)
}

func mean(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	sum := 0.0
	for _, x := range xs {
		sum += x
	}
	return sum / float64(len(xs))
}
