package memory

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	// Legacy index (kept for backward compat reads)
	legacyIndex  = "idx:devmemory"
	legacyPrefix = "devmem:"

	// DefaultKeyspace is the namespace used by the production binary.
	// Keys live under "mem:{repo}:{type}:{identifier}" and the RediSearch
	// indexes scan the "mem:" prefix. Tests override this with
	// NewStoreWithKeyspace so they can't accidentally wipe production
	// data when they FlushDB or scan-and-delete their own keys.
	DefaultKeyspace = "mem"
)

// FileMemory describes what a source file does.
type FileMemory struct {
	Repo       string `json:"repo"`
	Path       string `json:"path"`
	Purpose    string `json:"purpose"`
	KeySymbols string `json:"key_symbols,omitempty"`
	Source     string `json:"source"` // "auto" or "agent"
	Scope      string `json:"scope,omitempty"` // "global" or branch name
	RepoPath   string `json:"-"`      // filesystem path of the repo (for git hash; not stored)
}

// FuncMemory describes what a function does and its call relationships.
type FuncMemory struct {
	Repo          string `json:"repo"`
	Name          string `json:"name"`
	QualifiedName string `json:"qualified_name,omitempty"`
	File          string `json:"file,omitempty"`
	Purpose       string `json:"purpose"`
	Callers       string `json:"callers,omitempty"`
	Callees       string `json:"callees,omitempty"`
	Source        string `json:"source"`
	Scope         string `json:"scope,omitempty"` // "global" or branch name
	RepoPath      string `json:"-"`
}

// FlowMemory describes an end-to-end execution flow or process.
//
// SubgraphJSON is an opaque JSON-encoded codegraph.Subgraph snapshot
// captured at memory_save_flow time from the flow's EntryPoints. It's
// stored as a raw string (not a typed Go struct) so this package
// doesn't have to depend on internal/codegraph — the router does the
// serialisation, the dashboard does the deserialisation. Empty when
// the flow has no entry points, codegraph is unreachable, or the agent
// opted out via capture_callgraph=false.
type FlowMemory struct {
	Repo         string `json:"repo"`
	Name         string `json:"name"`
	Purpose      string `json:"purpose"`
	Files        string `json:"files,omitempty"`
	EntryPoints  string `json:"entry_points,omitempty"`
	Source       string `json:"source"`
	Scope        string `json:"scope,omitempty"` // "global" or branch name
	SubgraphJSON string `json:"subgraph_json,omitempty"`
}

// DecisionMemory describes a developer decision made during a Claude session.
type DecisionMemory struct {
	Repo         string
	Name         string // slug identifier: "use-redis-not-postgres"
	DecisionType string // refactor | optimization | coding_standard | architecture | constraint | tradeoff
	Decision     string // what was decided
	Rationale    string // why
	Alternatives string // what was rejected (optional)
	Constraint   string // forcing constraint (optional)
	Scope        string // branch isolation scope: "global" or branch name
	Files        string // comma-separated affected files (optional)
	Status       string // "active" | "superseded" (default: "active")
	Supersedes   string // name of the decision this one replaces (optional)
	SupersededBy string // name of the decision that replaced this one (optional)
	Source       string // always "agent"
}

// Entry is the legacy flat memory record (kept for backward compat).
type Entry struct {
	Symbol  string `json:"symbol"`
	File    string `json:"file,omitempty"`
	Summary string `json:"summary"`
}

// MemoryHit is a typed search result from any of the three indexes.
//
// Key is the underlying Redis key (e.g. "mem:goserving:flow:seat-provider-…").
// Stable across calls and used as the join key for memory-relevance learning
// (memory:fp:* sorted sets).
//
// Score is the raw cosine distance returned by FT.SEARCH KNN — 0 means
// identical, ~1 means orthogonal, ~2 means opposite. The router converts
// this to a [0,1] similarity = clamp(1 - score) when consuming.
type MemoryHit struct {
	Type   string
	Key    string
	Score  float64
	Fields map[string]string
}

// Store is backed by Redis Stack with vector search.
// Store wraps the Redis client and the namespace under which every
// memory hash and RediSearch index lives. The keyspace is fixed at
// construction time so a single process can't accidentally mix
// production and test data in one *Store — each test that needs Redis
// builds its own *Store via NewStoreWithKeyspace with a unique prefix.
type Store struct {
	rdb *redis.Client

	// keyspace is the namespace prefix without the trailing colon
	// (e.g. "mem", "testmem-Tabc1234"). All hash keys are
	// "<keyspace>:<repo>:<type>:<identifier>" and the four FT indexes
	// scan that prefix.
	keyspace string

	// Per-index names. These are derived from keyspace once at
	// construction so the hot path (SaveX / SearchX) is a plain field
	// read rather than a fmt.Sprintf per call.
	fileIndex     string
	funcIndex     string
	flowIndex     string
	decisionIndex string
}

// keyPrefix returns the keyspace plus its trailing colon —
// "<keyspace>:" — for use when concatenating hash keys.
func (s *Store) keyPrefix() string { return s.keyspace + ":" }

// indexNameFor returns the RediSearch index name for a given memory
// type. Mirrors the per-Store fields so callers that already know the
// type string ("file"/"func"/"flow"/"decision") don't need a switch.
func (s *Store) indexNameFor(memType string) string {
	switch memType {
	case "func":
		return s.funcIndex
	case "flow":
		return s.flowIndex
	case "decision":
		return s.decisionIndex
	default:
		return s.fileIndex
	}
}

// NewStore builds a production Store that reads/writes the default
// "mem:" keyspace. The Cursor-facing devrouter binary uses this.
func NewStore(redisAddr string) (*Store, error) {
	return NewStoreWithKeyspace(redisAddr, DefaultKeyspace)
}

// NewStoreWithKeyspace builds a Store whose hashes and FT indexes are
// scoped to a custom keyspace. Tests pass a per-test unique keyspace
// (e.g. "testmem-Tabc1234") so they can FlushDB-style cleanup their
// own keys without touching the production "mem:" namespace that the
// dev shell's seeded data lives in.
//
// keyspace must be non-empty and must not contain a colon — both would
// produce malformed keys/indexes downstream. Pass DefaultKeyspace
// ("mem") to get production behaviour.
func NewStoreWithKeyspace(redisAddr, keyspace string) (*Store, error) {
	if redisAddr == "" {
		redisAddr = "localhost:6379"
	}
	if keyspace == "" {
		return nil, fmt.Errorf("memory: keyspace must be non-empty")
	}
	if strings.ContainsAny(keyspace, ":*?[]") {
		return nil, fmt.Errorf("memory: keyspace %q contains forbidden chars (no \":*?[]\")", keyspace)
	}

	rdb := redis.NewClient(&redis.Options{
		Addr:        redisAddr,
		DialTimeout: 3 * time.Second,
	})

	ctx := context.Background()
	if err := rdb.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("redis connect: %w", err)
	}

	s := &Store{
		rdb:           rdb,
		keyspace:      keyspace,
		fileIndex:     "idx:" + keyspace + ":file",
		funcIndex:     "idx:" + keyspace + ":func",
		flowIndex:     "idx:" + keyspace + ":flow",
		decisionIndex: "idx:" + keyspace + ":decision",
	}
	if err := s.ensureIndexes(ctx); err != nil {
		return nil, fmt.Errorf("redis indexes: %w", err)
	}
	return s, nil
}

// RDB returns the underlying Redis client. Exposed so sibling packages
// (e.g. internal/heuristics) can reuse the same connection pool rather
// than opening a second one.
func (s *Store) RDB() *redis.Client {
	return s.rdb
}

// Keyspace returns the namespace this Store reads/writes
// (e.g. "mem" in production, "testmem-Tabc1234" in tests).
func (s *Store) Keyspace() string { return s.keyspace }

// WipeKeyspace deletes every hash and drops every RediSearch index
// inside this Store's keyspace. Intended for test cleanup — callers
// must hold a Store built with a non-default keyspace.
//
// Refuses to operate on the production "mem" keyspace, so a mis-wired
// test can't accidentally nuke the dev shell's seeded data the way
// the old FlushDB-based helper did.
func (s *Store) WipeKeyspace(ctx context.Context) error {
	if s.keyspace == DefaultKeyspace {
		return fmt.Errorf("memory: WipeKeyspace refused: cannot wipe production keyspace %q", DefaultKeyspace)
	}
	s.deleteKeysByPattern(ctx, s.keyPrefix()+"*")
	for _, name := range []string{s.fileIndex, s.funcIndex, s.flowIndex, s.decisionIndex} {
		s.rdb.Do(ctx, "FT.DROPINDEX", name)
	}
	return nil
}

// schemaVersion is bumped when the index schema changes, triggering a drop+recreate.
const schemaVersion = "v6" // v6: added status, supersedes, superseded_by for decision supersession tracking

func (s *Store) ensureIndexes(ctx context.Context) error {
	dim := strconv.Itoa(EmbedDim)

	// All indexes share this Store's prefix (e.g. "mem:" in prod,
	// "testmem-XYZ:" in tests) and filter by mem_type TAG within it.
	commonPrefix := s.keyPrefix()

	indexes := []struct {
		name   string
		fields []string
	}{
		{
			name: s.fileIndex,
			fields: []string{
				"mem_type", "TAG",
				"repo", "TAG",
				"path", "TEXT", "SORTABLE",
				"purpose", "TEXT",
				"key_symbols", "TEXT",
				"source", "TAG",
				"scope", "TAG",
				"updated_at", "NUMERIC",
				"embedding", "VECTOR", "HNSW", "6", "TYPE", "FLOAT32", "DIM", dim, "DISTANCE_METRIC", "COSINE",
			},
		},
		{
			name: s.funcIndex,
			fields: []string{
				"mem_type", "TAG",
				"repo", "TAG",
				"name", "TEXT", "SORTABLE",
				"qualified_name", "TEXT",
				"file", "TEXT",
				"purpose", "TEXT",
				"callers", "TEXT",
				"callees", "TEXT",
				"source", "TAG",
				"scope", "TAG",
				"updated_at", "NUMERIC",
				"embedding", "VECTOR", "HNSW", "6", "TYPE", "FLOAT32", "DIM", dim, "DISTANCE_METRIC", "COSINE",
			},
		},
		{
			name: s.flowIndex,
			fields: []string{
				"mem_type", "TAG",
				"repo", "TAG",
				"name", "TEXT", "SORTABLE",
				"purpose", "TEXT",
				"files", "TEXT",
				"entry_points", "TEXT",
				"source", "TAG",
				"scope", "TAG",
				"updated_at", "NUMERIC",
				"embedding", "VECTOR", "HNSW", "6", "TYPE", "FLOAT32", "DIM", dim, "DISTANCE_METRIC", "COSINE",
			},
		},
		{
			name: s.decisionIndex,
			fields: []string{
				"mem_type", "TAG",
				"repo", "TAG",
				"decision_type", "TAG",
				"name", "TEXT", "SORTABLE",
				"decision", "TEXT",
				"rationale", "TEXT",
				"alternatives", "TEXT",
				"constraint", "TEXT",
				"files", "TEXT",
				"source", "TAG",
				"scope", "TAG",
				"status", "TAG",
				"supersedes", "TEXT",
				"superseded_by", "TEXT",
				"updated_at", "NUMERIC",
				"embedding", "VECTOR", "HNSW", "6", "TYPE", "FLOAT32", "DIM", dim, "DISTANCE_METRIC", "COSINE",
			},
		},
	}

	versionKey := "devrouter:schema_version"
	currentVersion, _ := s.rdb.Get(ctx, versionKey).Result()

	needsRecreate := currentVersion != schemaVersion

	if needsRecreate {
		// Clean up old keys from previous schema. The patterns are
		// scoped to *this* Store's keyspace so a test Store with a
		// per-test keyspace can never accidentally wipe production
		// "mem:*" keys when its schema fingerprint mismatches.
		// (Historic note: the previous version of this block scanned
		// "mem:flow:*" — which never even matched the real
		// "mem:{repo}:flow:*" layout, so the migration's "drop and
		// recreate" was effectively a no-op for keys. The new
		// pattern actually matches and respects the keyspace.)
		prefix := s.keyPrefix()
		s.deleteKeysByPattern(ctx, prefix+"*:file:*")
		s.deleteKeysByPattern(ctx, prefix+"*:func:*")
		s.deleteKeysByPattern(ctx, prefix+"*:flow:*")
		s.deleteKeysByPattern(ctx, prefix+"*:decision:*")
	}

	for _, idx := range indexes {
		if needsRecreate {
			s.rdb.Do(ctx, "FT.DROPINDEX", idx.name)
		}

		_, err := s.rdb.Do(ctx, "FT.INFO", idx.name).Result()
		if err == nil {
			continue
		}

		args := []interface{}{
			"FT.CREATE", idx.name,
			"ON", "HASH",
			"PREFIX", "1", commonPrefix,
			"SCHEMA",
		}
		for _, f := range idx.fields {
			args = append(args, f)
		}
		if _, err := s.rdb.Do(ctx, args...).Result(); err != nil {
			return fmt.Errorf("create %s: %w", idx.name, err)
		}
	}

	if needsRecreate {
		s.rdb.Set(ctx, versionKey, schemaVersion, 0)
	}

	return nil
}

func (s *Store) deleteKeysByPattern(ctx context.Context, pattern string) {
	var cursor uint64
	for {
		keys, next, err := s.rdb.Scan(ctx, cursor, pattern, 500).Result()
		if err != nil {
			break
		}
		if len(keys) > 0 {
			s.rdb.Del(ctx, keys...)
		}
		cursor = next
		if cursor == 0 {
			break
		}
	}
}

// ---------------------------------------------------------------------------
// Save methods
// ---------------------------------------------------------------------------

func (s *Store) SaveFile(m FileMemory) error {
	if m.Source == "" {
		m.Source = "agent"
	}
	key := s.keyPrefix() + m.Repo + ":file:" + sanitizeKey(m.Path)

	if m.Source == "auto" {
		existing := s.getField(key, "source")
		if existing == "agent" {
			return nil
		}
	}

	text := m.Path + " " + m.Purpose + " " + m.KeySymbols
	vec, err := Embed(text)
	if err != nil {
		return fmt.Errorf("embed: %w", err)
	}

	fields := map[string]interface{}{
		"mem_type":    "file",
		"repo":        m.Repo,
		"path":        m.Path,
		"purpose":     m.Purpose,
		"key_symbols": m.KeySymbols,
		"source":      m.Source,
		"scope":       m.Scope,
		"updated_at":  time.Now().UnixMilli(),
		"embedding":   Float32ToBytes(vec),
	}
	if m.RepoPath != "" {
		if h := GitFileHash(m.RepoPath, m.Path); h != "" {
			fields["git_hash"] = h
		}
	}
	return s.rdb.HSet(context.Background(), key, fields).Err()
}

func (s *Store) SaveFunc(m FuncMemory) error {
	if m.Source == "" {
		m.Source = "agent"
	}
	qn := m.QualifiedName
	if qn == "" && m.File != "" {
		qn = m.File + ":" + m.Name
	} else if qn == "" {
		qn = m.Name
	}
	key := s.keyPrefix() + m.Repo + ":func:" + sanitizeKey(qn)

	if m.Source == "auto" {
		existing := s.getField(key, "source")
		if existing == "agent" {
			return nil
		}
	}

	text := m.Name + " " + m.Purpose + " " + m.Callers + " " + m.Callees
	vec, err := Embed(text)
	if err != nil {
		return fmt.Errorf("embed: %w", err)
	}

	fields := map[string]interface{}{
		"mem_type":       "func",
		"repo":           m.Repo,
		"name":           m.Name,
		"qualified_name": qn,
		"file":           m.File,
		"purpose":        m.Purpose,
		"callers":        m.Callers,
		"callees":        m.Callees,
		"source":         m.Source,
		"scope":          m.Scope,
		"updated_at":     time.Now().UnixMilli(),
		"embedding":      Float32ToBytes(vec),
	}
	if m.RepoPath != "" && m.File != "" {
		if h := GitFileHash(m.RepoPath, m.File); h != "" {
			fields["git_hash"] = h
		}
	}
	return s.rdb.HSet(context.Background(), key, fields).Err()
}

func (s *Store) SaveFlow(m FlowMemory) error {
	if m.Source == "" {
		m.Source = "agent"
	}
	key := s.keyPrefix() + m.Repo + ":flow:" + sanitizeKey(m.Name)

	if m.Source == "auto" {
		existing := s.getField(key, "source")
		if existing == "agent" {
			return nil
		}
	}

	text := m.Name + " " + m.Purpose + " " + m.Files
	vec, err := Embed(text)
	if err != nil {
		return fmt.Errorf("embed: %w", err)
	}

	fields := map[string]interface{}{
		"mem_type":     "flow",
		"repo":         m.Repo,
		"name":         m.Name,
		"purpose":      m.Purpose,
		"files":        m.Files,
		"entry_points": m.EntryPoints,
		"source":       m.Source,
		"scope":        m.Scope,
		"updated_at":   time.Now().UnixMilli(),
		"embedding":    Float32ToBytes(vec),
	}
	// SubgraphJSON is only written when non-empty so flows that were
	// saved without an entry-point seed (or before this field existed)
	// keep their hash compact and the dashboard transparently falls
	// back to the legacy bipartite SVG.
	if m.SubgraphJSON != "" {
		fields["subgraph_json"] = m.SubgraphJSON
	}
	return s.rdb.HSet(context.Background(), key, fields).Err()
}

// SaveDecision persists a developer decision with conflict detection.
// Returns a list of conflict warnings (save is not blocked by conflicts).
func (s *Store) SaveDecision(m DecisionMemory) ([]string, error) {
	m.Source = "agent"
	if m.Status == "" {
		m.Status = "active"
	}
	key := s.keyPrefix() + m.Repo + ":decision:" + sanitizeKey(m.Name)

	text := m.Name + " " + m.Decision + " " + m.Rationale + " " + m.Constraint
	vec, err := Embed(text)
	if err != nil {
		return nil, fmt.Errorf("embed: %w", err)
	}

	err = s.rdb.HSet(context.Background(), key, map[string]interface{}{
		"mem_type":       "decision",
		"repo":           m.Repo,
		"decision_type":  m.DecisionType,
		"name":           m.Name,
		"decision":       m.Decision,
		"rationale":      m.Rationale,
		"alternatives":   m.Alternatives,
		"constraint":     m.Constraint,
		"scope":          m.Scope,
		"files":          m.Files,
		"status":         m.Status,
		"supersedes":     m.Supersedes,
		"superseded_by":  m.SupersededBy,
		"source":         m.Source,
		"updated_at":     time.Now().UnixMilli(),
		"embedding":      Float32ToBytes(vec),
	}).Err()
	if err != nil {
		return nil, err
	}

	warnings := s.detectDecisionConflicts(m)
	return warnings, nil
}

// SupersedeDecision marks oldName as superseded by newName.
// It updates both decisions to record the relationship and status.
func (s *Store) SupersedeDecision(repo, oldName, newName string) error {
	ctx := context.Background()
	oldKey := s.keyPrefix() + repo + ":decision:" + sanitizeKey(oldName)
	newKey := s.keyPrefix() + repo + ":decision:" + sanitizeKey(newName)

	// Verify both exist
	if n, _ := s.rdb.Exists(ctx, oldKey).Result(); n == 0 {
		return fmt.Errorf("decision %q not found", oldName)
	}
	if n, _ := s.rdb.Exists(ctx, newKey).Result(); n == 0 {
		return fmt.Errorf("decision %q not found", newName)
	}

	// Mark old decision as superseded
	if err := s.rdb.HSet(ctx, oldKey, map[string]interface{}{
		"status":       "superseded",
		"superseded_by": newName,
	}).Err(); err != nil {
		return fmt.Errorf("mark old decision superseded: %w", err)
	}

	// Set supersedes on new decision
	return s.rdb.HSet(ctx, newKey, "supersedes", oldName).Err()
}

// Save persists a legacy flat memory entry (backward compat).
func (s *Store) Save(e Entry) error {
	return s.SaveFunc(FuncMemory{
		Name:    e.Symbol,
		File:    e.File,
		Purpose: e.Summary,
		Source:  "agent",
	})
}

// ---------------------------------------------------------------------------
// Scope detection helpers
// ---------------------------------------------------------------------------

// CurrentBranch returns the current git branch name, or "global" if unable to determine.
func CurrentBranch(repoPath string) string {
	out, err := exec.Command("git", "-C", repoPath, "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		return "global"
	}
	b := strings.TrimSpace(string(out))
	if b == "" || b == "HEAD" {
		return "global"
	}
	return b
}

// fetchRelease ensures origin/release is up-to-date. Errors are silently ignored.
func fetchRelease(repoPath string) {
	exec.Command("git", "-C", repoPath, "fetch", "origin", "release").Run()
}

// ScopeForFile returns "global" if the file is unchanged vs origin/release, else current branch.
func ScopeForFile(repoPath, filePath string) string {
	if repoPath == "" || filePath == "" {
		return "global"
	}
	fetchRelease(repoPath)
	err := exec.Command("git", "-C", repoPath, "diff", "--quiet", "origin/release", "--", filePath).Run()
	if err != nil {
		// exit 1 = file differs from origin/release
		return CurrentBranch(repoPath)
	}
	return "global"
}

// ScopeForFiles returns "global" if all files are unchanged vs origin/release, else current branch.
func ScopeForFiles(repoPath, filesCSV string) string {
	if repoPath == "" {
		return "global"
	}
	fetchRelease(repoPath)
	for _, f := range strings.Split(filesCSV, ",") {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		err := exec.Command("git", "-C", repoPath, "diff", "--quiet", "origin/release", "--", f).Run()
		if err != nil {
			return CurrentBranch(repoPath)
		}
	}
	return "global"
}

// ScopeForDecision returns scope for a decision: if files provided, use ScopeForFiles;
// else return "global" if no commits ahead of origin/release, else current branch.
func ScopeForDecision(repoPath, filesCSV string) string {
	if filesCSV != "" {
		return ScopeForFiles(repoPath, filesCSV)
	}
	// No files → check if branch has any commits ahead of origin/release
	fetchRelease(repoPath)
	out, err := exec.Command("git", "-C", repoPath, "rev-list", "--count", "origin/release..HEAD").Output()
	if err != nil {
		return "global"
	}
	if strings.TrimSpace(string(out)) == "0" {
		return "global"
	}
	return CurrentBranch(repoPath)
}

// ---------------------------------------------------------------------------
// Search methods
// ---------------------------------------------------------------------------

// SearchAll queries all four memory types and returns merged, typed results.
// If repo is non-empty, results are filtered to that repo only.
// If branch is non-empty, results are filtered to scope="global" or scope=<branch>.
func (s *Store) SearchAll(query string, repo string, branch string) []MemoryHit {
	vec, err := Embed(query)
	if err != nil {
		return nil
	}
	return s.SearchAllWithEmbed(vec, repo, branch)
}

// SearchAllWithEmbed is the same as SearchAll but skips the embed
// step — callers that already have the query vector (e.g. the
// router, which also feeds the same vector into topic resolution
// and repeat detection) avoid a redundant nomic-embed round-trip,
// keeping latency under the per-query budget.
func (s *Store) SearchAllWithEmbed(vec []float32, repo string, branch string) []MemoryHit {
	if len(vec) == 0 {
		return nil
	}
	vecBytes := Float32ToBytes(vec)

	var hits []MemoryHit
	for _, mt := range []string{"file", "func", "flow", "decision"} {
		results := s.vectorSearch(vecBytes, 5, repo, mt, branch)
		for _, r := range results {
			r.Type = mt
			hits = append(hits, r)
		}
	}

	return hits
}

// Search performs legacy vector search (backward compat).
func (s *Store) Search(query string) []Entry {
	hits := s.SearchAll(query, "", "")
	var entries []Entry
	for _, h := range hits {
		name := h.Fields["name"]
		if name == "" {
			name = h.Fields["path"]
		}
		if name == "" {
			name = h.Fields["symbol"]
		}
		entries = append(entries, Entry{
			Symbol:  name,
			File:    h.Fields["file"],
			Summary: h.Fields["purpose"],
		})
	}
	return entries
}

// vectorSearch searches a single memory type within optional repo and branch scopes.
// All types share the same index prefix "mem:", differentiated by mem_type TAG.
func (s *Store) vectorSearch(vecBytes []byte, k int, repo string, memType string, branch string) []MemoryHit {
	ctx := context.Background()

	// Build pre-filter: always filter by mem_type, optionally by repo and scope
	filter := fmt.Sprintf("@mem_type:{%s}", memType)
	if repo != "" {
		filter += fmt.Sprintf(" @repo:{%s}", escapeTag(repo))
	}
	if branch != "" {
		scopeFilter := fmt.Sprintf("global|%s", escapeTag(branch))
		filter += fmt.Sprintf(" @scope:{%s}", scopeFilter)
	}
	query := fmt.Sprintf("(%s)=>[KNN %d @embedding $vec AS score]", filter, k)

	// Pick the index that matches this mem_type, scoped to this
	// Store's keyspace (production uses "idx:mem:<type>", tests use
	// "idx:testmem-XYZ:<type>").
	indexName := s.indexNameFor(memType)

	res, err := s.rdb.Do(ctx,
		"FT.SEARCH", indexName,
		query,
		"PARAMS", "2", "vec", vecBytes,
		"SORTBY", "score",
		"LIMIT", "0", strconv.Itoa(k),
		"DIALECT", "2",
	).Result()
	if err != nil {
		return nil
	}

	return parseMemoryHits(res)
}

func escapeTag(s string) string {
	replacer := strings.NewReplacer(
		"-", "\\-",
		".", "\\.",
		"@", "\\@",
		" ", "\\ ",
	)
	return replacer.Replace(s)
}

func parseMemoryHits(raw interface{}) []MemoryHit {
	arr, ok := raw.([]interface{})
	if !ok || len(arr) < 2 {
		return nil
	}

	var hits []MemoryHit
	for i := 1; i+1 < len(arr); i += 2 {
		key, _ := arr[i].(string)
		fields, ok := arr[i+1].([]interface{})
		if !ok {
			continue
		}
		h := MemoryHit{Key: key, Fields: make(map[string]string)}
		for j := 0; j+1 < len(fields); j += 2 {
			name, _ := fields[j].(string)
			val, _ := fields[j+1].(string)
			if name == "score" {
				h.Score, _ = strconv.ParseFloat(val, 64)
			} else if name != "embedding" {
				h.Fields[name] = val
			}
		}
		hits = append(hits, h)
	}
	return hits
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func (s *Store) getField(key, field string) string {
	val, err := s.rdb.HGet(context.Background(), key, field).Result()
	if err != nil {
		return ""
	}
	return val
}

func sanitizeKey(s string) string {
	return strings.Map(func(r rune) rune {
		if r == ':' || r == ' ' || r == '/' || r == '\\' {
			return '_'
		}
		return r
	}, s)
}

// ListDecisions returns all decisions for a repo, optionally filtered by decision_type, scope, or files.
// All filters are additive (AND). Empty filter = match all.
// If includeSuperseded is false, only returns active decisions.
func (s *Store) ListDecisions(repo, decisionType, scope, files string, includeSuperseded bool) []MemoryHit {
	ctx := context.Background()

	// Build filter: mem_type is always pinned; repo is optional so the
	// dashboard can ask for "all repos" with repo="". Same convention
	// as decisionType/scope/files below — empty string means "don't
	// filter on this dimension".
	filter := "@mem_type:{decision}"
	if repo != "" {
		filter += fmt.Sprintf(" @repo:{%s}", escapeTag(repo))
	}
	if decisionType != "" {
		filter += fmt.Sprintf(" @decision_type:{%s}", escapeTag(decisionType))
	}
	if !includeSuperseded {
		filter += " @status:{active}"
	}

	// LIMIT 0 500 — bumped from 100 to comfortably hold the
	// cross-repo "all" view at current corpus sizes (15 × N repos).
	// Hard cap below the FT default of 1000 to keep payloads bounded.
	res, err := s.rdb.Do(ctx,
		"FT.SEARCH", s.decisionIndex,
		filter,
		"LIMIT", "0", "500",
		"DIALECT", "2",
	).Result()
	if err != nil {
		return nil
	}

	hits := parseMemoryHits(res)

	// Post-filter by scope/files (TEXT fields can't be filtered as TAG)
	if scope == "" && files == "" {
		return hits
	}
	var filtered []MemoryHit
	for _, h := range hits {
		if scope != "" {
			hScope := strings.ToLower(h.Fields["scope"])
			if !strings.Contains(hScope, strings.ToLower(scope)) {
				continue
			}
		}
		if files != "" {
			if !hasFileOverlap(h.Fields["files"], files) {
				continue
			}
		}
		filtered = append(filtered, h)
	}
	return filtered
}

// detectDecisionConflicts checks for overlapping scope/files with different decision types.
// Returns a list of warning strings (non-blocking).
func (s *Store) detectDecisionConflicts(m DecisionMemory) []string {
	existing := s.ListDecisions(m.Repo, "", "", "", true) // include superseded to check all decisions
	var warnings []string
	for _, hit := range existing {
		name := hit.Fields["name"]
		// Skip the decision we just saved
		if name == m.Name {
			continue
		}
		existingScope := hit.Fields["scope"]
		existingFiles := hit.Fields["files"]
		existingType := hit.Fields["decision_type"]
		existingDecision := hit.Fields["decision"]

		scopeOverlap := m.Scope != "" && existingScope != "" && (m.Scope == existingScope ||
			strings.Contains(existingScope, m.Scope) || strings.Contains(m.Scope, existingScope))
		filesOverlap := m.Files != "" && existingFiles != "" && hasFileOverlap(m.Files, existingFiles)

		if scopeOverlap || filesOverlap {
			if existingType != m.DecisionType {
				warnings = append(warnings, fmt.Sprintf(
					"conflict: decision %q (type=%s) overlaps scope/files with %q (type=%s): %s",
					m.Name, m.DecisionType, name, existingType, existingDecision,
				))
			}
		}
	}
	return warnings
}

// hasFileOverlap checks if two comma-separated file lists have any overlap.
func hasFileOverlap(filesA, filesB string) bool {
	setA := make(map[string]bool)
	for _, f := range strings.Split(filesA, ",") {
		setA[strings.TrimSpace(f)] = true
	}
	for _, f := range strings.Split(filesB, ",") {
		if setA[strings.TrimSpace(f)] {
			return true
		}
	}
	return false
}
