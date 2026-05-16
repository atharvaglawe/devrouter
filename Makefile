BINARY        := devrouter
REDIS_ADDR    ?= localhost:6379
CODEGRAPH_URL ?= http://localhost:4747
CODEGRAPH_DIR := codegraph
CODEGRAPH_PORT?= 4747

# Lightweight read-only observability UI bundled into the binary.
# On by default (127.0.0.1:8088) — `make run` and `make dashboard` both
# start it. Override to bind elsewhere, or set to "off" to disable.
DASHBOARD_ADDR ?= 127.0.0.1:8088

# Bundled Dockerized Go-native ONNX embedder. See embedder/README.md.
EMBEDDER_DIR    := embedder
EMBEDDER_IMAGE  := devrouter-embedder:latest
EMBEDDER_PORT   ?= 11435
EMBEDDER_URL    := http://localhost:$(EMBEDDER_PORT)

# ── Node resolver (vendored codegraph needs Node >= 20) ──────────────
# Resolution order:
#   1. $NODE / $DEVROUTER_NODE   — explicit override
#   2. `node` on PATH if it reports version >= 20
#   3. newest ~/.nvm/versions/node/v{20,22,24,...}/bin/node available
# Errors out with a useful message if none qualify.
NODE ?= $(or $(DEVROUTER_NODE),$(shell \
  resolve_node() { \
    if command -v node >/dev/null 2>&1; then \
      v=$$(node -e 'process.stdout.write(String(process.versions.node.split(".")[0]))' 2>/dev/null); \
      if [ -n "$$v" ] && [ "$$v" -ge 20 ] 2>/dev/null; then echo "$$(command -v node)"; return; fi; \
    fi; \
    for n in $$(ls -1 $$HOME/.nvm/versions/node 2>/dev/null | sort -V -r); do \
      maj=$$(echo "$$n" | sed -e 's/^v//' -e 's/\..*//'); \
      if [ "$$maj" -ge 20 ] 2>/dev/null && [ -x "$$HOME/.nvm/versions/node/$$n/bin/node" ]; then \
        echo "$$HOME/.nvm/versions/node/$$n/bin/node"; return; \
      fi; \
    done; \
  }; resolve_node))
NPM := $(if $(NODE),$(dir $(NODE))npm,npm)

require-node:
	@if [ -z "$(NODE)" ]; then \
		echo "Node >= 20 required for the vendored codegraph subtree."; \
		echo "Install Node 20+ (e.g. \`nvm install 22\`), or set DEVROUTER_NODE=/abs/path/to/node."; \
		exit 1; \
	fi

.PHONY: all build deps up down status redis codegraph clean require-node \
        codegraph-install codegraph-build codegraph-serve codegraph-analyze codegraph-migrate \
        embedder-deps embedder-fetch-model embedder-build-local embedder-test \
        embedder-build embedder-up embedder-down embedder-status embedder-logs \
        run dashboard

all: build codegraph-build

# ── Build ───────────────────────────────────────────────────
build:
	go build -o $(BINARY) ./cmd/router/

clean:
	rm -f $(BINARY)
	rm -rf $(CODEGRAPH_DIR)/dist

# ── Codegraph (vendored Node subtree) ────────────────────────

codegraph-install: require-node
	cd $(CODEGRAPH_DIR) && $(NPM) install --no-audit --no-fund --legacy-peer-deps

codegraph-build: require-node
	@if [ ! -d $(CODEGRAPH_DIR)/node_modules ]; then \
		$(MAKE) codegraph-install; \
	fi
	cd $(CODEGRAPH_DIR) && $(NPM) run build

codegraph-serve: codegraph-build
	cd $(CODEGRAPH_DIR) && $(NODE) dist/cli/index.js serve --port $(CODEGRAPH_PORT)

codegraph-analyze: codegraph-build
	@if [ -z "$(REPO)" ]; then echo "Usage: make codegraph-analyze REPO=/path/to/repo"; exit 1; fi
	cd $(CODEGRAPH_DIR) && $(NODE) dist/cli/index.js analyze $(REPO)

# One-shot migration of legacy gitnexus on-disk paths -> codegraph names.
# Safe to run repeatedly; skips already-migrated entries.
codegraph-migrate: require-node
	@if [ ! -f $(CODEGRAPH_DIR)/scripts/migrate-from-gitnexus.js ]; then \
		echo "Migration script not found at $(CODEGRAPH_DIR)/scripts/migrate-from-gitnexus.js"; \
		exit 1; \
	fi
	$(NODE) $(CODEGRAPH_DIR)/scripts/migrate-from-gitnexus.js

# ── Start everything ────────────────────────────────────────
up: deps
	@echo ""
	@echo "✓ All services running. Use 'make status' to verify."
	@echo "  Redis:     $(REDIS_ADDR)"
	@echo "  Embedder:  $(EMBEDDER_URL)/api/embed"
	@echo "  Codegraph: $(CODEGRAPH_URL)"
	@echo ""
	@echo "To start devrouter MCP:  make run"

deps: redis embedder-up codegraph build

# ── Stop everything ─────────────────────────────────────────
down:
	@echo "Stopping Redis Stack..."
	-@redis-cli shutdown nosave 2>/dev/null || true
	@echo "Stopping embedder container..."
	-@cd $(EMBEDDER_DIR) && docker compose down 2>/dev/null || true
	@echo "Stopping Codegraph..."
	-@pkill -f "codegraph.*serve" 2>/dev/null || true
	@echo "Done."

# ── Service health ──────────────────────────────────────────
status:
	@printf "Redis:     " && (redis-cli ping 2>/dev/null || echo "DOWN")
	@printf "Embedder:  " && (curl -sf $(EMBEDDER_URL)/api/health >/dev/null && echo "PONG" || echo "DOWN")
	@printf "Codegraph: " && (curl -sf $(CODEGRAPH_URL)/api/repos >/dev/null && echo "PONG" || echo "DOWN")
	@printf "Memories:  " && echo "total=$$(redis-cli KEYS 'mem:*' 2>/dev/null | grep -v '^$$' | wc -l | tr -d ' ')"
	@echo "  Repos:" && redis-cli KEYS 'mem:*' 2>/dev/null | grep -v '^$$' | sed 's/^mem:\([^:]*\):.*/\1/' | sort -u | while read repo; do \
		files=$$(redis-cli KEYS "mem:$$repo:file:*" 2>/dev/null | wc -l | tr -d ' '); \
		funcs=$$(redis-cli KEYS "mem:$$repo:func:*" 2>/dev/null | wc -l | tr -d ' '); \
		flows=$$(redis-cli KEYS "mem:$$repo:flow:*" 2>/dev/null | wc -l | tr -d ' '); \
		echo "    $$repo: files=$$files funcs=$$funcs flows=$$flows"; \
	done

# ── Individual services ─────────────────────────────────────
redis:
	@if redis-cli ping 2>/dev/null | grep -q PONG; then \
		echo "Redis already running"; \
	else \
		echo "Starting Redis Stack..."; \
		redis-stack-server --daemonize yes 2>/dev/null || redis-server --daemonize yes 2>/dev/null; \
		sleep 1; \
		redis-cli ping; \
	fi

codegraph: require-node
	@if curl -sf $(CODEGRAPH_URL)/api/repos >/dev/null 2>&1; then \
		echo "Codegraph already running on $(CODEGRAPH_URL)"; \
	else \
		if [ ! -f $(CODEGRAPH_DIR)/dist/cli/index.js ]; then \
			echo "Building in-tree codegraph..."; \
			$(MAKE) codegraph-build; \
		fi; \
		echo "Starting in-tree codegraph on $(CODEGRAPH_URL) (node: $(NODE))..."; \
		cd $(CODEGRAPH_DIR) && nohup $(NODE) dist/cli/index.js serve --port $(CODEGRAPH_PORT) >/tmp/devrouter-codegraph.log 2>&1 & \
		for i in 1 2 3 4 5 6 7 8 9 10; do \
			curl -sf $(CODEGRAPH_URL)/api/repos >/dev/null 2>&1 && break; \
			sleep 1; \
		done; \
		if curl -sf $(CODEGRAPH_URL)/api/repos >/dev/null 2>&1; then \
			echo "Codegraph ready (logs: /tmp/devrouter-codegraph.log)"; \
		else \
			echo "Codegraph failed to start — last 20 log lines:"; \
			tail -20 /tmp/devrouter-codegraph.log | sed 's/^/  /'; \
			exit 1; \
		fi; \
	fi

# ── Run devrouter ───────────────────────────────────────────
# Both targets start the same daemon (MCP server on stdio + HTTP
# dashboard on DASHBOARD_ADDR). `make dashboard` is a discoverable alias.
# Disable the dashboard by overriding: make run DASHBOARD_ADDR=off
run dashboard: build
	@echo "Dashboard: http://$(DASHBOARD_ADDR)"
	DEVROUTER_REDIS=$(REDIS_ADDR) \
	CODEGRAPH_URL=$(CODEGRAPH_URL) \
	DEVROUTER_EMBEDDING_URL=$(EMBEDDER_URL)/api/embed \
	DEVROUTER_DASHBOARD_ADDR=$(DASHBOARD_ADDR) \
	./$(BINARY)

# ── Embedder ────────────────────────────────────────────────
# Two paths: host-side (deps/build/test outside docker, for iteration)
# and Dockerized (the deployment path that `make up` chains into).

embedder-deps:
	$(EMBEDDER_DIR)/scripts/fetch-libs.sh

embedder-fetch-model:
	$(EMBEDDER_DIR)/scripts/fetch-model.sh

embedder-build-local: embedder-deps
	cd $(EMBEDDER_DIR) && CGO_LDFLAGS="-L./lib" go build -o embedder .

embedder-test: embedder-deps embedder-fetch-model
	cd $(EMBEDDER_DIR) && CGO_LDFLAGS="-L./lib" go test -v -count=1 ./...

embedder-build:
	cd $(EMBEDDER_DIR) && docker compose build

embedder-up:
	@if curl -sf $(EMBEDDER_URL)/api/health >/dev/null 2>&1; then \
		echo "Embedder already healthy at $(EMBEDDER_URL)"; \
		exit 0; \
	fi
	@if ! docker image inspect $(EMBEDDER_IMAGE) >/dev/null 2>&1; then \
		echo "Image $(EMBEDDER_IMAGE) not built yet — building (one-time, 1-2 min)..."; \
		$(MAKE) embedder-build; \
	fi
	cd $(EMBEDDER_DIR) && docker compose up -d
	@echo "Waiting for embedder to be ready on $(EMBEDDER_URL)..."
	@echo "(first start downloads ~440MB of model — can take several minutes;"
	@echo " subsequent starts come up in seconds because of the volume cache)"
	@start=$$(date +%s); \
	while true; do \
		if curl -sf $(EMBEDDER_URL)/api/health >/dev/null 2>&1; then break; fi; \
		now=$$(date +%s); elapsed=$$((now - start)); \
		if [ $$elapsed -gt 600 ]; then \
			echo "Embedder failed health check after 10min — check 'make embedder-logs'"; \
			exit 1; \
		fi; \
		if [ $$((elapsed % 30)) -lt 2 ] && [ $$elapsed -gt 0 ]; then \
			echo "  ($$elapsed s elapsed — model downloading? 'make embedder-logs' to watch)"; \
		fi; \
		sleep 2; \
	done
	@echo "Embedder ready at $(EMBEDDER_URL)/api/embed"

embedder-down:
	cd $(EMBEDDER_DIR) && docker compose down

embedder-status:
	@printf "Embedder: " && (curl -sf $(EMBEDDER_URL)/api/health 2>/dev/null || echo "DOWN")

embedder-logs:
	cd $(EMBEDDER_DIR) && docker compose logs -f --tail=200

# ── Utilities ───────────────────────────────────────────────
flush-memories:
	@echo "Dropping memory indexes and data..."
	-@redis-cli FT.DROPINDEX idx:mem:file DD 2>/dev/null || true
	-@redis-cli FT.DROPINDEX idx:mem:func DD 2>/dev/null || true
	-@redis-cli FT.DROPINDEX idx:mem:flow DD 2>/dev/null || true
	-@redis-cli DEL devrouter:schema_version 2>/dev/null || true
	@echo "Cleaning up old-format keys..."
	-@redis-cli KEYS 'mem:*' 2>/dev/null | xargs -r redis-cli DEL 2>/dev/null || true
	@echo "Done. Indexes will be recreated on next run."

list-memories:
	@redis-cli KEYS 'mem:*' 2>/dev/null | grep -v '^$$' | sed 's/^mem:\([^:]*\):.*/\1/' | sort -u | while read repo; do \
		echo "=== $$repo ==="; \
		echo "  Files ($$(redis-cli KEYS "mem:$$repo:file:*" 2>/dev/null | wc -l | tr -d ' ')):"; \
		redis-cli KEYS "mem:$$repo:file:*" 2>/dev/null | head -10 | sed 's/^/    /'; \
		echo "  Funcs ($$(redis-cli KEYS "mem:$$repo:func:*" 2>/dev/null | wc -l | tr -d ' ')):"; \
		redis-cli KEYS "mem:$$repo:func:*" 2>/dev/null | head -10 | sed 's/^/    /'; \
		echo "  Flows ($$(redis-cli KEYS "mem:$$repo:flow:*" 2>/dev/null | wc -l | tr -d ' ')):"; \
		redis-cli KEYS "mem:$$repo:flow:*" 2>/dev/null | head -10 | sed 's/^/    /'; \
		echo ""; \
	done

list-memories-repo:
	@if [ -z "$(REPO)" ]; then echo "Usage: make list-memories-repo REPO=goserving"; exit 1; fi
	@echo "=== $(REPO) ==="
	@echo "Files ($$(redis-cli KEYS 'mem:$(REPO):file:*' 2>/dev/null | wc -l | tr -d ' ')):"
	@redis-cli KEYS "mem:$(REPO):file:*" 2>/dev/null | head -10 | sed 's/^/  /'
	@echo "Funcs ($$(redis-cli KEYS 'mem:$(REPO):func:*' 2>/dev/null | wc -l | tr -d ' ')):"
	@redis-cli KEYS "mem:$(REPO):func:*" 2>/dev/null | head -10 | sed 's/^/  /'
	@echo "Flows ($$(redis-cli KEYS 'mem:$(REPO):flow:*' 2>/dev/null | wc -l | tr -d ' ')):"
	@redis-cli KEYS "mem:$(REPO):flow:*" 2>/dev/null | head -10 | sed 's/^/  /'

help:
	@echo "devrouter Makefile"
	@echo ""
	@echo "Common:"
	@echo "  make up              Start Redis + embedder + codegraph, build binary"
	@echo "  make down            Stop all services"
	@echo "  make status          Health check all services"
	@echo "  make run             Build and run the MCP server (stdio)"
	@echo "  make build           Build the binary only"
	@echo "  make clean           Remove binaries"
	@echo ""
	@echo "Individual services:"
	@echo "  make redis           Start Redis Stack only"
	@echo "  make codegraph       Start the in-tree codegraph serve if not running"
	@echo "  make embedder-up     Start embedder container on port $(EMBEDDER_PORT)"
	@echo "  make embedder-down   Stop embedder container"
	@echo "  make embedder-logs   Follow embedder container logs"
	@echo ""
	@echo "Codegraph maintenance:"
	@echo "  make codegraph-build                  Compile vendored codegraph (TS -> dist/)"
	@echo "  make codegraph-analyze REPO=/path     Index a repo into the local LadybugDB"
	@echo "  make codegraph-migrate                One-shot rename of legacy .gitnexus -> .codegraph"
	@echo ""
	@echo "Embedder dev (host-side, no docker):"
	@echo "  make embedder-build-local         Build the embedder Go binary on the host"
	@echo "  make embedder-test                Run embedder unit tests (downloads model on first run)"
	@echo ""
	@echo "Memory utilities:"
	@echo "  make flush-memories                Drop all stored memories from Redis"
	@echo "  make list-memories                 List all memory keys (all repos)"
	@echo "  make list-memories-repo REPO=name  List memories for a specific repo"
	@echo ""
	@echo "Environment:"
	@echo "  REDIS_ADDR         (default: localhost:6379)"
	@echo "  CODEGRAPH_URL      (default: http://localhost:4747)"
	@echo "  CODEGRAPH_PORT     (default: 4747)"
	@echo "  EMBEDDER_PORT      (default: 11435)"
