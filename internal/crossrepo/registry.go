package crossrepo

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// RegistryEntry mirrors one record in `~/.codegraph/registry.json`.
// Only the fields devrouter actually consumes are decoded; codegraph's
// schema has a few more (stats, lastCommit, etc.) that we ignore so
// future additions don't break the reader.
type RegistryEntry struct {
	Name        string `json:"name"`
	Path        string `json:"path"`
	StoragePath string `json:"storagePath"`
	IndexedAt   string `json:"indexedAt"`
}

// Registry is an in-memory snapshot of all codegraph-indexed repos.
// Cheap to copy by value; the underlying slice is treated as immutable
// once Load returns so concurrent readers don't need to lock.
type Registry struct {
	Entries []RegistryEntry
	LoadedAt time.Time
	// SourcePath records where this snapshot was loaded from so log
	// messages can tell apart `~/.codegraph/registry.json` vs a
	// CODEGRAPH_HOME override vs a test fixture.
	SourcePath string
}

// RegistryLoader handles registry discovery and caching. Reads
// $CODEGRAPH_HOME/registry.json with $HOME/.codegraph fallback, then
// $GITNEXUS_HOME/registry.json + $HOME/.gitnexus for the legacy path
// (still supported per docs/codegraph.md).
//
// Cache TTL is 60s. registry.json is rewritten only on `analyze`
// completion, so a stale cache during a multi-query CLI session is
// fine; we re-read often enough that a fresh `analyze` shows up
// promptly without hammering the disk on every dev_context.
type RegistryLoader struct {
	ttl time.Duration

	mu     sync.Mutex
	cached *Registry
}

// NewRegistryLoader constructs a loader with the default 60s TTL.
// Tests can override via SetTTL.
func NewRegistryLoader() *RegistryLoader {
	return &RegistryLoader{ttl: 60 * time.Second}
}

// SetTTL adjusts the cache TTL. Use 0 to disable caching entirely
// (every Load re-reads disk).
func (l *RegistryLoader) SetTTL(d time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ttl = d
}

// Load returns the current registry snapshot, re-reading disk when
// the cache is older than TTL or when the file's mtime has changed.
// First call always reads.
//
// Errors propagate from the underlying read — a missing registry
// file is treated as an empty registry (no error) so a fresh devrouter
// install with zero indexed repos doesn't fail the cross-repo path,
// it just returns no candidate repos.
func (l *RegistryLoader) Load() (*Registry, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	path := resolveRegistryPath()
	now := time.Now()

	if l.cached != nil && l.cached.SourcePath == path && now.Sub(l.cached.LoadedAt) < l.ttl {
		return l.cached, nil
	}

	entries, err := readRegistryFile(path)
	if err != nil {
		return nil, fmt.Errorf("crossrepo: read registry %s: %w", path, err)
	}
	snap := &Registry{
		Entries:    entries,
		LoadedAt:   now,
		SourcePath: path,
	}
	l.cached = snap
	return snap, nil
}

// Names returns the list of repo names in the registry, in registry
// order, deduplicated. Registry files are occasionally stale —
// `codegraph index` against an already-registered path appends a
// fresh row rather than upserting in old codegraph versions, so we
// can't assume uniqueness from the file. Dedup here keeps fan-out
// from issuing N copies of the same query.
func (r *Registry) Names() []string {
	if r == nil {
		return nil
	}
	seen := make(map[string]struct{}, len(r.Entries))
	out := make([]string, 0, len(r.Entries))
	for _, e := range r.Entries {
		if _, dup := seen[e.Name]; dup {
			continue
		}
		seen[e.Name] = struct{}{}
		out = append(out, e.Name)
	}
	return out
}

// Lookup returns the entry for name, or nil if not registered.
func (r *Registry) Lookup(name string) *RegistryEntry {
	if r == nil || name == "" {
		return nil
	}
	for i := range r.Entries {
		if r.Entries[i].Name == name {
			return &r.Entries[i]
		}
	}
	return nil
}

// resolveRegistryPath follows the same precedence as codegraph.md and
// the in-tree codegraph CLI: explicit CODEGRAPH_HOME wins, then the
// legacy GITNEXUS_HOME alias, then $HOME/.codegraph, then the legacy
// $HOME/.gitnexus.
//
// We return the first path that exists. If none exist, return the
// canonical default ($HOME/.codegraph/registry.json) so the caller's
// error message points at the path a fresh install would create.
func resolveRegistryPath() string {
	candidates := []string{}
	if v := strings.TrimSpace(os.Getenv("CODEGRAPH_HOME")); v != "" {
		candidates = append(candidates, filepath.Join(v, "registry.json"))
	}
	if v := strings.TrimSpace(os.Getenv("GITNEXUS_HOME")); v != "" {
		candidates = append(candidates, filepath.Join(v, "registry.json"))
	}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates,
			filepath.Join(home, ".codegraph", "registry.json"),
			filepath.Join(home, ".gitnexus", "registry.json"),
		)
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	// Return the canonical default for the error message.
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".codegraph", "registry.json")
	}
	return "registry.json"
}

// readRegistryFile reads the JSON file at path and returns its
// entries. A non-existent file returns (nil, nil) so the caller can
// proceed with an empty registry — codegraph creates the file
// lazily on first `analyze`, and we don't want cross-repo to error
// out on a fresh devrouter install.
func readRegistryFile(path string) ([]RegistryEntry, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var entries []RegistryEntry
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	// Drop entries with missing critical fields. The codegraph CLI
	// should never write these, but a half-written file (e.g.
	// interrupted analyze) shouldn't crash a query.
	cleaned := entries[:0]
	for _, e := range entries {
		if e.Name == "" || e.Path == "" {
			continue
		}
		cleaned = append(cleaned, e)
	}
	return cleaned, nil
}
