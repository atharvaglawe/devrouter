package anchorlearn

import "time"

// Pattern is the learner's notion of an "anchor template" — a path
// suffix the router can probe against any service token. The Suffix
// is what the router appends to a service name (e.g. for service
// "oscar" and Suffix "web/grpc/server.go" the probe is
// "oscar/web/grpc/server.go").
//
// Source distinguishes:
//
//	"static"     — shipped in the cold-start portfolio (the
//	               language-agnostic defaults in DefaultStaticPatterns)
//	"discovered" — learned from observed agent behaviour (Phase 3); a
//	               file the agent kept saving as a memory under <svc>/
//	               that wasn't in the static list
//
// Keywords are query terms that bias this pattern higher when present
// in the query (e.g. "grpc" + "server" boosts web/grpc/server.go).
// Bench-calibrated against goserving but generalises: keyword match is
// just a soft scoring signal, never a hard filter.
type Pattern struct {
	ID       string   // stable identifier; usually equals Suffix
	Suffix   string   // path suffix appended to a service token
	Keywords []string // soft affinity keywords
	Source   string   // "static" or "discovered"
}

// Candidate is a fully-qualified anchor proposal: a Pattern applied to
// a specific service token in a specific repo, scored against the
// current query. The router probes Path via codegraph's /api/file and
// keeps Candidates that resolve to a non-empty file body.
//
// IsExploration is set on Candidates that the bandit picked as an
// ε-greedy exploration slot rather than for raw score — kept on the
// type so the observation log can distinguish "anchor we trusted" from
// "anchor we were probing", which matters for off-policy reward
// attribution.
type Candidate struct {
	Service       string
	Path          string
	Pattern       Pattern
	Score         float64
	IsExploration bool
}

// Observation is what RecordObservation persists per dev_context call:
// the query that triggered anchoring, the candidates that survived
// probing, and enough metadata to credit/blame each pattern when
// downstream feedback arrives. Lifetime matches the heuristics trace
// (30 days) so dev_feedback joins on QueryID still resolve.
type Observation struct {
	QueryID    string
	Repo       string
	Query      string
	Intent     string
	Files      []string  // resolved anchored file paths
	PatternIDs []string  // parallel to Files, same index = same anchor
	Services   []string  // service tokens that drove the probe set
	Timestamp  time.Time
}

// PatternStats is the persisted weight record for a single pattern. We
// store *both* a global view (PatternStats with empty Repo) and a
// per-repo view (Repo set) — the global one is the cross-repo prior
// that bootstraps brand-new repos, the per-repo one drives Decide once
// enough observations accumulate locally.
//
// SuccessCount uses a smoothed Bayesian count rather than raw success
// to avoid a fresh pattern with one lucky hit dominating Decide on its
// second query. See Score in policy.go for the smoothing constant.
type PatternStats struct {
	PatternID    string
	Repo         string // "" for the global record
	FiredCount   int
	SuccessCount int
	LastSeen     time.Time
}

// KeywordAffinity is the (query keyword × pattern) co-occurrence count
// that lets Decide say "queries containing 'grpc' should rank
// web/grpc/server.go higher than web/http/server.go". Updated on every
// rewarded observation; decayed periodically (see policy.go).
type KeywordAffinity struct {
	Keyword   string
	PatternID string
	Score     float64
	LastSeen  time.Time
}

// DefaultStaticPatterns is the language-agnostic cold-start portfolio.
// Calibrated against the goserving wins (main.go, web/web.go,
// web/grpc/server.go, web/routes/route.go, …) plus a portfolio for
// other common conventions: Python (main.py, app.py, urls.py),
// Node (index.js, server.js, routes/index.js), Java (Application.java),
// Rust (src/main.rs).
//
// Repos that don't match any of these patterns will see zero anchors
// fire on day one but accumulate "discovered" patterns through Phase 3
// as the agent builds repo-specific memories. After a few dozen
// dev_context calls the per-repo learned set takes over and the static
// list becomes incidental.
var DefaultStaticPatterns = []Pattern{
	// Go conventions (calibrated on goserving).
	{ID: "main.go", Suffix: "main.go", Source: "static",
		Keywords: []string{"main", "boot", "start", "init", "shutdown", "interrupt", "install"}},
	{ID: "web/web.go", Suffix: "web/web.go", Source: "static",
		Keywords: []string{"web", "server", "listener", "start", "listen"}},
	{ID: "web/http/server.go", Suffix: "web/http/server.go", Source: "static",
		Keywords: []string{"http", "listener", "server", "rest", "listen"}},
	{ID: "web/grpc/server.go", Suffix: "web/grpc/server.go", Source: "static",
		Keywords: []string{"grpc", "server", "rpc"}},
	{ID: "web/routes/route.go", Suffix: "web/routes/route.go", Source: "static",
		Keywords: []string{"route", "routes", "register", "endpoint", "path"}},
	{ID: "web/routes/routes.go", Suffix: "web/routes/routes.go", Source: "static",
		Keywords: []string{"route", "routes", "register", "endpoint", "path"}},
	{ID: "handlers/web/routes/routes.go", Suffix: "handlers/web/routes/routes.go", Source: "static",
		Keywords: []string{"route", "routes", "wire", "register", "handler"}},
	{ID: "server.go", Suffix: "server.go", Source: "static",
		Keywords: []string{"server", "listen", "start", "boot"}},
	{ID: "router.go", Suffix: "router.go", Source: "static",
		Keywords: []string{"route", "router", "register"}},
	{ID: "cmd/server/main.go", Suffix: "cmd/server/main.go", Source: "static",
		Keywords: []string{"main", "server", "start"}},

	// Python conventions.
	{ID: "main.py", Suffix: "main.py", Source: "static",
		Keywords: []string{"main", "start", "boot", "entry"}},
	{ID: "app.py", Suffix: "app.py", Source: "static",
		Keywords: []string{"app", "flask", "fastapi", "server"}},
	{ID: "wsgi.py", Suffix: "wsgi.py", Source: "static",
		Keywords: []string{"wsgi", "django", "deploy"}},
	{ID: "asgi.py", Suffix: "asgi.py", Source: "static",
		Keywords: []string{"asgi", "async", "deploy"}},
	{ID: "manage.py", Suffix: "manage.py", Source: "static",
		Keywords: []string{"django", "manage", "command"}},
	{ID: "urls.py", Suffix: "urls.py", Source: "static",
		Keywords: []string{"route", "url", "register", "django"}},
	{ID: "routes.py", Suffix: "routes.py", Source: "static",
		Keywords: []string{"route", "register", "endpoint"}},

	// Node / TypeScript conventions.
	{ID: "index.js", Suffix: "index.js", Source: "static",
		Keywords: []string{"index", "main", "entry", "express"}},
	{ID: "index.ts", Suffix: "index.ts", Source: "static",
		Keywords: []string{"index", "main", "entry"}},
	{ID: "server.js", Suffix: "server.js", Source: "static",
		Keywords: []string{"server", "listen", "express", "start"}},
	{ID: "server.ts", Suffix: "server.ts", Source: "static",
		Keywords: []string{"server", "listen", "start"}},
	{ID: "app.js", Suffix: "app.js", Source: "static",
		Keywords: []string{"app", "express", "server"}},
	{ID: "app.ts", Suffix: "app.ts", Source: "static",
		Keywords: []string{"app", "server", "nest"}},
	{ID: "main.ts", Suffix: "main.ts", Source: "static",
		Keywords: []string{"main", "boot", "nest"}},

	// Java / Spring conventions.
	//
	// `Application.java` lives at the leaf of the package tree
	// (e.g. src/main/java/com/macro/mall/MallAdminApplication.java) so
	// the simple flat-suffix probe never hits — leaving Spring repos
	// with no cold-start anchors at all and stranding Phase 3 (which
	// requires at least one observation to bootstrap discovery). The
	// three additions below are the universal Maven / Spring-Boot
	// markers: they live at exact, predictable paths under EVERY
	// service module of EVERY Spring repo (mall-admin/pom.xml,
	// mall-portal/src/main/resources/application.yml, …). Probing them
	// gives Decide() a non-empty candidate set, which is all Phase 3
	// needs to start observing the agent's saves and promoting the
	// real entry-point paths into the per-repo discovered set.
	{ID: "src/main/java/Application.java", Suffix: "src/main/java/Application.java", Source: "static",
		Keywords: []string{"application", "spring", "main", "boot"}},
	{ID: "pom.xml", Suffix: "pom.xml", Source: "static",
		Keywords: []string{"maven", "module", "dependency", "spring", "boot"}},
	{ID: "src/main/resources/application.yml", Suffix: "src/main/resources/application.yml", Source: "static",
		Keywords: []string{"config", "configuration", "spring", "yaml", "datasource", "redis", "rabbitmq"}},
	{ID: "src/main/resources/application.properties", Suffix: "src/main/resources/application.properties", Source: "static",
		Keywords: []string{"config", "configuration", "spring", "properties"}},

	// Rust conventions.
	{ID: "src/main.rs", Suffix: "src/main.rs", Source: "static",
		Keywords: []string{"main", "rust", "binary"}},
	{ID: "src/lib.rs", Suffix: "src/lib.rs", Source: "static",
		Keywords: []string{"lib", "rust", "library"}},

	// Ruby / Rails conventions.
	{ID: "config.ru", Suffix: "config.ru", Source: "static",
		Keywords: []string{"rack", "ruby", "deploy"}},
}
