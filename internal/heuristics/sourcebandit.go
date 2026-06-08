package heuristics

import (
	"context"
	"log"
	"math/rand"
	"sync"
	"time"

	"github.com/atharva-ag/devrouter/internal/telemetry"
)

// SourceBandit tunes one integer per (intent, repo, topic, source)
// cell: the number of docs that external source returns. It reuses the
// codegraph bandit's ε-perturb / K-sample-promote / 3-strike-rollback
// shape (sharing Epsilon/PromotionWindow/PromotionLift/RollbackThreshold/
// RollbackStrikes) but on a scalar, and keys by an extra `source`
// dimension because external sources are dynamic config rather than
// fixed Profile fields.
//
// Credit assignment: the query reward is a single scalar. To keep
// attribution clean (mirroring the codegraph bandit perturbing one knob
// per query) at most ONE source explores a perturbed breadth per query;
// the rest run at their learned current value. The query's reward routes
// to that one explored cell via the trace's explore record.
// sourceStore is the slice of Store the SourceBandit depends on,
// defined as an interface so tests can mock it without Redis (mirrors
// banditStore).
type sourceStore interface {
	CurrentSourceBreadth(ctx context.Context, intent, repo, topic, source string, seedDefault int) int
	SetSourceBreadth(ctx context.Context, intent, repo, topic, source string, val int) error
	LoadSourceDefault(ctx context.Context, intent, repo, topic, source string, fallback int) int
	AppendSourceHistory(ctx context.Context, intent, repo, topic, source string, entry SourceHistoryEntry) error
}

type SourceBandit struct {
	store   sourceStore
	enabled bool

	mu     sync.Mutex
	active map[string]*sourceCandidate // key: banditKey(intent,repo,topic)
	rng    *rand.Rand
}

type sourceCandidate struct {
	intent, repo, topic, source string
	val                         int // perturbed breadth under test
	base                        int // base breadth at candidate creation
	rewards                     []float64
	baseSamples                 []float64
	consecBad                   int
}

// SourceSeed names a configured source and its default breadth (the
// tool's Config.MaxDocs), used to seed a cold cell.
type SourceSeed struct {
	Name    string
	Default int
}

// ExploreRec records which source was sampled on a query and at what
// value, so the reward path can route credit to the right cell. Val is
// the served breadth; Base is the cell's current best.
type ExploreRec struct {
	Source string
	Val    int
	Base   int
}

// SourceDocsBounds is the hard guardrail for a source's doc breadth.
var SourceDocsBounds = [2]int{2, 15}

func clipSourceDocs(v int) int { return clipInt(v, SourceDocsBounds[0], SourceDocsBounds[1]) }

// SourceDocsKnob is the canonical knob name used to enable this bandit
// via DEVROUTER_HEURISTICS_BANDIT (alongside "all").
const SourceDocsKnob = "source_docs"

func NewSourceBandit(s sourceStore) *SourceBandit {
	return &SourceBandit{
		store:  s,
		active: map[string]*sourceCandidate{},
		rng:    rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

// Enable turns on perturbation. Without it SelectAll always returns the
// learned current breadths (no exploration).
func (b *SourceBandit) Enable() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.enabled = true
}

func (b *SourceBandit) Enabled() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.enabled
}

// SelectAll returns the doc breadth to use for each source this query,
// plus the explore record (nil when no source is being sampled). At most
// one source explores per query. bucketSamples gates exploration on a
// real (non-global) bucket the same way the codegraph bandit does.
func (b *SourceBandit) SelectAll(intent, repo, topic string, seeds []SourceSeed, bucketSamples int) (map[string]int, *ExploreRec) {
	b.mu.Lock()
	defer b.mu.Unlock()

	ctx := context.Background()
	breadths := make(map[string]int, len(seeds))
	for _, sd := range seeds {
		def := sd.Default
		if def <= 0 {
			def = 5
		}
		breadths[sd.Name] = b.store.CurrentSourceBreadth(ctx, intent, repo, topic, sd.Name, def)
	}

	if !b.enabled || len(seeds) == 0 {
		return breadths, nil
	}
	if !isGlobalBucket(repo, topic) && bucketSamples < TopicBanditSampleFloor {
		return breadths, nil
	}

	key := banditKey(intent, repo, topic)

	// Continue sampling an active candidate cell: serve candidate w.p.
	// Epsilon, else base — so we accumulate both candidate and base
	// samples for a clean comparison.
	if c, ok := b.active[key]; ok {
		if _, present := breadths[c.source]; !present {
			delete(b.active, key) // source de-configured; abandon
			return breadths, nil
		}
		if b.rng.Float64() < Epsilon {
			breadths[c.source] = c.val
			return breadths, &ExploreRec{Source: c.source, Val: c.val, Base: c.base}
		}
		breadths[c.source] = c.base
		return breadths, &ExploreRec{Source: c.source, Val: c.base, Base: c.base}
	}

	// Start a new candidate on one randomly chosen source w.p. Epsilon.
	if b.rng.Float64() >= Epsilon {
		return breadths, nil
	}
	sd := seeds[b.rng.Intn(len(seeds))]
	baseVal := breadths[sd.Name]
	cand := b.perturb(baseVal)
	if cand == baseVal {
		return breadths, nil
	}
	b.active[key] = &sourceCandidate{
		intent: intent, repo: repo, topic: topic, source: sd.Name,
		val: cand, base: baseVal,
	}
	breadths[sd.Name] = cand
	log.Printf("[heuristics] source bandit %s/%s new candidate docs=%d (base=%d)", key, sd.Name, cand, baseVal)
	return breadths, &ExploreRec{Source: sd.Name, Val: cand, Base: baseVal}
}

// Update records a reward for the explored source cell. servedVal is the
// breadth that actually served the query (from the explore record).
func (b *SourceBandit) Update(intent, repo, topic, source string, servedVal int, adjustedReward, weight float64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	key := banditKey(intent, repo, topic)
	c, ok := b.active[key]
	if !ok || c.source != source {
		return
	}
	weighted := adjustedReward * weight
	switch servedVal {
	case c.val:
		c.rewards = append(c.rewards, weighted)
		if adjustedReward < RollbackThreshold {
			c.consecBad++
			if c.consecBad >= RollbackStrikes {
				b.rollback(key, c, "3-strike rollback (source candidate underperforming)")
				return
			}
		} else {
			c.consecBad = 0
		}
	case c.base:
		c.baseSamples = append(c.baseSamples, weighted)
	default:
		return // stale served value
	}

	if len(c.rewards) >= PromotionWindow && len(c.baseSamples) >= 5 {
		if mean(c.rewards) > mean(c.baseSamples)+PromotionLift {
			b.promote(key, c)
		} else {
			b.discard(key, c)
		}
	}
}

func (b *SourceBandit) perturb(base int) int {
	delta := 1
	if b.rng.Float64() < 0.5 {
		delta = -1
	}
	return clipSourceDocs(base + delta)
}

func (b *SourceBandit) promote(key string, c *sourceCandidate) {
	ctx := context.Background()
	if err := b.store.SetSourceBreadth(ctx, c.intent, c.repo, c.topic, c.source, c.val); err != nil {
		log.Printf("[heuristics] source promote %s/%s failed: %v", key, c.source, err)
		return
	}
	_ = b.store.AppendSourceHistory(ctx, c.intent, c.repo, c.topic, c.source, SourceHistoryEntry{
		Kind:   "promote",
		From:   c.base,
		To:     c.val,
		Reason: "candidate beat base by >= PromotionLift over PromotionWindow samples",
	})
	telemetry.SourceDocsPromotions.WithLabelValues(c.source).Inc()
	log.Printf("[heuristics] source bandit %s/%s promoted docs %d -> %d", key, c.source, c.base, c.val)
	delete(b.active, key)
}

func (b *SourceBandit) discard(key string, c *sourceCandidate) {
	telemetry.SourceDocsDiscards.WithLabelValues(c.source).Inc()
	log.Printf("[heuristics] source bandit %s/%s discarded candidate docs=%d (no lift)", key, c.source, c.val)
	delete(b.active, key)
}

func (b *SourceBandit) rollback(key string, c *sourceCandidate, reason string) {
	ctx := context.Background()
	def := b.store.LoadSourceDefault(ctx, c.intent, c.repo, c.topic, c.source, c.base)
	_ = b.store.SetSourceBreadth(ctx, c.intent, c.repo, c.topic, c.source, def)
	_ = b.store.AppendSourceHistory(ctx, c.intent, c.repo, c.topic, c.source, SourceHistoryEntry{
		Kind:   "rollback",
		From:   c.val,
		To:     def,
		Reason: reason,
	})
	telemetry.SourceDocsRollbacks.WithLabelValues(c.source).Inc()
	log.Printf("[heuristics] source bandit %s/%s ROLLBACK docs -> %d: %s", key, c.source, def, reason)
	delete(b.active, key)
}
