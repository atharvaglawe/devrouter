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

	// Key format: mem:{repo}:{type}:{identifier}
	// This groups by repo in Redis Insight: mem > goserving > file/func/flow
	memPrefix = "mem:"

	fileIndex      = "idx:mem:file"
	funcIndex      = "idx:mem:func"
	flowIndex      = "idx:mem:flow"
	decisionIndex  = "idx:mem:decision"

	// flowOverlayPrefix holds per-flow agent-feedback aggregates. One
	// hash per flow, keyed as `flow:overlay:{repo}:{sanitised_name}`.
	// Fields:
	//   file_useful:{path}   int   — agent read this file from the flow
	//   file_dead:{path}     int   — agent finished without reading it (success && no extras)
	//   missing:{path}       int   — agent had to read this file but it wasn't in the flow
	//   total_feedback       int   — count of feedback events received
	//   last_feedback_at     int64 — unix-ms of most recent update
	//   last_query_id        str   — most recent contributing query (audit)
	// Per-file counters keep file paths in the sub-key (not value) so a
	// single HGETALL on the overlay returns the full per-file breakdown
	// with no second round-trip. We HINCRBY for monotonic counters and
	// HSET for scalar metadata in the same pipeline.
	flowOverlayPrefix = "flow:overlay:"
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
type FlowMemory struct {
	Repo        string `json:"repo"`
	Name        string `json:"name"`
	Purpose     string `json:"purpose"`
	Files       string `json:"files,omitempty"`
	EntryPoints string `json:"entry_points,omitempty"`
	Source      string `json:"source"`
	Scope       string `json:"scope,omitempty"` // "global" or branch name
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
type Store struct {
	rdb *redis.Client
}

func NewStore(redisAddr string) (*Store, error) {
	if redisAddr == "" {
		redisAddr = "localhost:6379"
	}

	rdb := redis.NewClient(&redis.Options{
		Addr:        redisAddr,
		DialTimeout: 3 * time.Second,
	})

	ctx := context.Background()
	if err := rdb.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("redis connect: %w", err)
	}

	s := &Store{rdb: rdb}
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

// schemaVersion is bumped when the index schema changes, triggering a drop+recreate.
const schemaVersion = "v6" // v6: added status, supersedes, superseded_by for decision supersession tracking

func (s *Store) ensureIndexes(ctx context.Context) error {
	dim := strconv.Itoa(EmbedDim)

	// All indexes share prefix "mem:" but filter by mem_type TAG
	commonPrefix := memPrefix

	indexes := []struct {
		name   string
		fields []string
	}{
		{
			name: fileIndex,
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
			name: funcIndex,
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
			name: flowIndex,
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
			name: decisionIndex,
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
		// Clean up old keys from previous schema
		s.deleteKeysByPattern(ctx, "mem:file:*")
		s.deleteKeysByPattern(ctx, "mem:func:*")
		s.deleteKeysByPattern(ctx, "mem:flow:*")
		s.deleteKeysByPattern(ctx, "mem:decision:*")
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
	key := memPrefix + m.Repo + ":file:" + sanitizeKey(m.Path)

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
	key := memPrefix + m.Repo + ":func:" + sanitizeKey(qn)

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
	key := memPrefix + m.Repo + ":flow:" + sanitizeKey(m.Name)

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

	return s.rdb.HSet(context.Background(), key, map[string]interface{}{
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
	}).Err()
}

// FlowOverlayUpdate is one piece of agent feedback applied to a saved flow.
//
// ReadFiles is what the agent actually consumed during the task (sourced
// from dev_feedback.file_paths). MissingFiles is what the agent had to
// fetch outside the flow (slice 2; optional). Success and AdditionalFiles
// come straight from dev_feedback and gate when a file counts as "dead":
// only when the agent succeeded *without* extra reads can absence from
// ReadFiles be confidently scored as dead weight — otherwise the file
// may simply not have been needed *this* time and we abstain.
type FlowOverlayUpdate struct {
	QueryID         string
	ReadFiles       []string
	MissingFiles    []string
	Success         bool
	AdditionalFiles int
}

// FlowOverlay is the deserialised per-flow feedback aggregate as served
// to the dashboard. Counts are monotonic; the dashboard derives ratios
// at render time.
type FlowOverlay struct {
	Repo             string                       `json:"repo"`
	Name             string                       `json:"name"`
	Files            map[string]FlowFileStat      `json:"files,omitempty"`
	Missing          map[string]int               `json:"missing,omitempty"`
	TotalFeedback    int                          `json:"total_feedback"`
	LastFeedbackAt   int64                        `json:"last_feedback_at,omitempty"`
	LastQueryID      string                       `json:"last_query_id,omitempty"`
}

// FlowFileStat is the per-file useful/dead pair. Both counters can be
// zero (file is in the flow but no feedback has touched it yet).
type FlowFileStat struct {
	Useful int `json:"useful"`
	Dead   int `json:"dead"`
}

// UpdateFlowOverlay applies one feedback event to flow:overlay:{repo}:{name}.
//
// Counter rules:
//   - file is in ReadFiles               → file_useful:{path} ++
//   - file is NOT in ReadFiles AND
//     Success AND AdditionalFiles == 0   → file_dead:{path}   ++
//   - file in MissingFiles               → missing:{path}     ++
//
// We only attribute "dead" when the task explicitly succeeded with zero
// additional reads — otherwise a file's absence from ReadFiles is
// ambiguous (the agent may have failed early or branched off the flow).
// This keeps the dead signal high-precision at the cost of recall, which
// matters because the dashboard uses it to grey out nodes.
//
// All-or-nothing within the pipeline: a single round-trip applies every
// counter for the event plus the scalar metadata (total/last_at). On
// Redis error we return it; SubmitFeedback logs and swallows.
func (s *Store) UpdateFlowOverlay(ctx context.Context, repo, name string, upd FlowOverlayUpdate) error {
	if repo == "" || name == "" {
		return fmt.Errorf("repo and name required")
	}
	flowKey := memPrefix + repo + ":flow:" + sanitizeKey(name)
	exists, err := s.rdb.Exists(ctx, flowKey).Result()
	if err != nil {
		return fmt.Errorf("exists check: %w", err)
	}
	if exists == 0 {
		// Don't create overlays for nonexistent flows — keeps the
		// dashboard's overlay map honest with the flow index.
		return fmt.Errorf("flow %s/%s not found", repo, name)
	}

	flowFiles, _ := s.rdb.HGet(ctx, flowKey, "files").Result()
	knownFiles := splitOverlayCSV(flowFiles)

	readSet := normaliseFileSet(upd.ReadFiles)
	overlayKey := flowOverlayPrefix + repo + ":" + sanitizeKey(name)

	pipe := s.rdb.Pipeline()
	canMarkDead := upd.Success && upd.AdditionalFiles == 0

	for _, f := range knownFiles {
		norm := normalisePathLower(f)
		if norm == "" {
			continue
		}
		// Substring match in either direction matches the existing
		// filesOverlap helper used by FP attribution (lines may be
		// quoted with or without leading "/" or trailing ":NN-MM").
		matched := false
		for r := range readSet {
			if r == norm || strings.Contains(r, norm) || strings.Contains(norm, r) {
				matched = true
				break
			}
		}
		if matched {
			pipe.HIncrBy(ctx, overlayKey, "file_useful:"+f, 1)
		} else if canMarkDead {
			pipe.HIncrBy(ctx, overlayKey, "file_dead:"+f, 1)
		}
	}

	// MissingFiles are agent-reported additions; track even when the
	// path isn't in the canonical flow files yet — that's the whole
	// point of the signal.
	for _, f := range upd.MissingFiles {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		pipe.HIncrBy(ctx, overlayKey, "missing:"+f, 1)
	}

	pipe.HIncrBy(ctx, overlayKey, "total_feedback", 1)
	pipe.HSet(ctx, overlayKey, map[string]interface{}{
		"last_feedback_at": time.Now().UnixMilli(),
		"last_query_id":    upd.QueryID,
	})

	_, err = pipe.Exec(ctx)
	return err
}

// LoadFlowOverlay returns the parsed overlay for a single flow. Returns
// a zero-value overlay (not nil) when no feedback has landed yet — the
// dashboard treats this uniformly as "no signal" and renders neutral.
func (s *Store) LoadFlowOverlay(ctx context.Context, repo, name string) FlowOverlay {
	if repo == "" || name == "" {
		return FlowOverlay{}
	}
	key := flowOverlayPrefix + repo + ":" + sanitizeKey(name)
	fields, err := s.rdb.HGetAll(ctx, key).Result()
	if err != nil || len(fields) == 0 {
		return FlowOverlay{Repo: repo, Name: name}
	}
	return parseFlowOverlay(repo, name, fields)
}

// LoadFlowOverlays bulk-loads overlays for every (repo, name) pair in a
// single pipelined batch — used by the dashboard's /api/flows endpoint
// so listing N flows costs one round-trip, not N.
func (s *Store) LoadFlowOverlays(ctx context.Context, repo string, names []string) map[string]FlowOverlay {
	out := make(map[string]FlowOverlay, len(names))
	if repo == "" || len(names) == 0 {
		return out
	}
	pipe := s.rdb.Pipeline()
	cmds := make([]*redis.MapStringStringCmd, len(names))
	for i, n := range names {
		cmds[i] = pipe.HGetAll(ctx, flowOverlayPrefix+repo+":"+sanitizeKey(n))
	}
	if _, err := pipe.Exec(ctx); err != nil && err != redis.Nil {
		return out
	}
	for i, n := range names {
		fields, err := cmds[i].Result()
		if err != nil || len(fields) == 0 {
			out[n] = FlowOverlay{Repo: repo, Name: n}
			continue
		}
		out[n] = parseFlowOverlay(repo, n, fields)
	}
	return out
}

// LoadAllFlowOverlays is the cross-repo variant: SCANs the overlay
// keyspace once. Used by the heuristics aggregator (slice 3) which
// rolls per-bucket dead/useful ratios across every repo.
func (s *Store) LoadAllFlowOverlays(ctx context.Context) []FlowOverlay {
	var cursor uint64
	var out []FlowOverlay
	for {
		keys, next, err := s.rdb.Scan(ctx, cursor, flowOverlayPrefix+"*", 500).Result()
		if err != nil {
			break
		}
		for _, k := range keys {
			// Strip the "flow:overlay:" prefix and split repo:name. Name
			// may itself contain "_" but no further ":" — sanitizeKey
			// rewrites ":" to "_".
			rest := strings.TrimPrefix(k, flowOverlayPrefix)
			repo, name, ok := strings.Cut(rest, ":")
			if !ok {
				continue
			}
			fields, err := s.rdb.HGetAll(ctx, k).Result()
			if err != nil || len(fields) == 0 {
				continue
			}
			out = append(out, parseFlowOverlay(repo, name, fields))
		}
		cursor = next
		if cursor == 0 {
			break
		}
	}
	return out
}

// parseFlowOverlay deserialises a single overlay HGETALL result.
func parseFlowOverlay(repo, name string, fields map[string]string) FlowOverlay {
	ov := FlowOverlay{
		Repo:  repo,
		Name:  name,
		Files: make(map[string]FlowFileStat),
	}
	for k, v := range fields {
		switch {
		case strings.HasPrefix(k, "file_useful:"):
			path := strings.TrimPrefix(k, "file_useful:")
			st := ov.Files[path]
			st.Useful, _ = strconv.Atoi(v)
			ov.Files[path] = st
		case strings.HasPrefix(k, "file_dead:"):
			path := strings.TrimPrefix(k, "file_dead:")
			st := ov.Files[path]
			st.Dead, _ = strconv.Atoi(v)
			ov.Files[path] = st
		case strings.HasPrefix(k, "missing:"):
			path := strings.TrimPrefix(k, "missing:")
			if ov.Missing == nil {
				ov.Missing = make(map[string]int)
			}
			ov.Missing[path], _ = strconv.Atoi(v)
		case k == "total_feedback":
			ov.TotalFeedback, _ = strconv.Atoi(v)
		case k == "last_feedback_at":
			ov.LastFeedbackAt, _ = strconv.ParseInt(v, 10, 64)
		case k == "last_query_id":
			ov.LastQueryID = v
		}
	}
	if len(ov.Files) == 0 {
		ov.Files = nil
	}
	return ov
}

// splitOverlayCSV is the same comma-split used by every CSV field in
// devrouter (file_paths, files, callers, ...). Kept private to memory
// because router/splitCSV is identical and we don't want a cyclic dep.
func splitOverlayCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := parts[:0]
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// normaliseFileSet lowercases + trims line-range suffixes (":NN-MM") and
// strips a leading "/" so substring matching in UpdateFlowOverlay is
// symmetric with the router/feedback.go normalisePath helper.
func normaliseFileSet(in []string) map[string]struct{} {
	out := make(map[string]struct{}, len(in))
	for _, s := range in {
		if n := normalisePathLower(s); n != "" {
			out[n] = struct{}{}
		}
	}
	return out
}

func normalisePathLower(p string) string {
	p = strings.TrimSpace(p)
	p = strings.TrimPrefix(p, "/")
	if i := strings.IndexByte(p, ':'); i > 0 {
		p = p[:i]
	}
	return strings.ToLower(p)
}

// SaveDecision persists a developer decision with conflict detection.
// Returns a list of conflict warnings (save is not blocked by conflicts).
func (s *Store) SaveDecision(m DecisionMemory) ([]string, error) {
	m.Source = "agent"
	if m.Status == "" {
		m.Status = "active"
	}
	key := memPrefix + m.Repo + ":decision:" + sanitizeKey(m.Name)

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
	oldKey := memPrefix + repo + ":decision:" + sanitizeKey(oldName)
	newKey := memPrefix + repo + ":decision:" + sanitizeKey(newName)

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

	// Pick the index that matches this mem_type
	indexName := fileIndex
	switch memType {
	case "func":
		indexName = funcIndex
	case "flow":
		indexName = flowIndex
	case "decision":
		indexName = decisionIndex
	}

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

	// Build filter: always filter by mem_type + repo
	filter := fmt.Sprintf("@mem_type:{decision} @repo:{%s}", escapeTag(repo))
	if decisionType != "" {
		filter += fmt.Sprintf(" @decision_type:{%s}", escapeTag(decisionType))
	}
	if !includeSuperseded {
		filter += " @status:{active}"
	}

	res, err := s.rdb.Do(ctx,
		"FT.SEARCH", decisionIndex,
		filter,
		"LIMIT", "0", "100",
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
