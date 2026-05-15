package heuristics

import (
	"container/list"
	"sync"
	"time"
)

// LastCallEntry is one entry in the per-connection last-call LRU.
// The MCP server is a single-process stdio handler per Cursor session,
// so "the most recent dev_context call" on this LRU is unambiguously
// scoped to the calling agent.
type LastCallEntry struct {
	QueryID   string
	Intent    string
	ProfileID string
	Timestamp time.Time
}

// LastCallLRU is a small in-memory LRU used as a best-effort fallback
// when dev_feedback arrives without an explicit query_id (e.g. the
// agent forgot to echo it back). Capacity is intentionally tiny because
// a single Cursor agent rarely has more than a handful of in-flight
// dev_context calls at once.
type LastCallLRU struct {
	mu       sync.Mutex
	capacity int
	ttl      time.Duration
	order    *list.List
	index    map[string]*list.Element
}

// NewLastCallLRU constructs an empty LRU.
func NewLastCallLRU(capacity int, ttl time.Duration) *LastCallLRU {
	if capacity <= 0 {
		capacity = 16
	}
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	return &LastCallLRU{
		capacity: capacity,
		ttl:      ttl,
		order:    list.New(),
		index:    map[string]*list.Element{},
	}
}

// Push records a fresh entry, evicting the oldest if over capacity.
func (l *LastCallLRU) Push(e LastCallEntry) {
	if e.QueryID == "" {
		return
	}
	if e.Timestamp.IsZero() {
		e.Timestamp = time.Now()
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if existing, ok := l.index[e.QueryID]; ok {
		l.order.Remove(existing)
		delete(l.index, e.QueryID)
	}
	elem := l.order.PushFront(e)
	l.index[e.QueryID] = elem
	for l.order.Len() > l.capacity {
		back := l.order.Back()
		if back == nil {
			break
		}
		evicted := back.Value.(LastCallEntry)
		l.order.Remove(back)
		delete(l.index, evicted.QueryID)
	}
}

// MostRecent returns the freshest entry younger than the TTL, or
// (zero, false) when nothing fits.
func (l *LastCallLRU) MostRecent() (LastCallEntry, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	front := l.order.Front()
	if front == nil {
		return LastCallEntry{}, false
	}
	e := front.Value.(LastCallEntry)
	if time.Since(e.Timestamp) > l.ttl {
		return LastCallEntry{}, false
	}
	return e, true
}
