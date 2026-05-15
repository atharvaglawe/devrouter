#!/usr/bin/env bash
# Vendor and build agentmemory's source so the bench adapter can drive its
# SearchIndex / VectorIndex / HybridSearch primitives directly.
#
# Why we clone instead of npm-installing
# --------------------------------------
# The published `@agentmemory/agentmemory` npm package ships pre-bundled
# build outputs (`dist/standalone.mjs` etc.) but does *not* re-export the
# internal `src/state/search-index.ts` and friends. Their own LongMemEval
# benchmark reaches into `../src/state/...` from the repo, so we vendor
# the same source tree the same way.
#
# This script is idempotent: re-running it on an existing checkout just
# re-pulls and re-installs without clobbering local edits.

set -euo pipefail

REPO_URL="${AGENTMEMORY_REPO_URL:-https://github.com/rohitg00/agentmemory.git}"
HERE="$(cd "$(dirname "$0")/.." && pwd)"
TARGET="$HERE/adapters/agentmemory_src"

if ! command -v node >/dev/null 2>&1; then
  echo "node not found on PATH. Install Node.js >= 20 first." >&2
  exit 1
fi

if [ ! -d "$TARGET/.git" ]; then
  echo "[setup_agentmemory] cloning $REPO_URL -> $TARGET"
  git clone --depth 1 "$REPO_URL" "$TARGET"
else
  echo "[setup_agentmemory] existing clone at $TARGET — pulling latest"
  (cd "$TARGET" && git pull --ff-only || true)
fi

echo "[setup_agentmemory] installing deps (ignore-scripts to skip Xenova binary build)"
(cd "$TARGET" && npm install --silent --no-audit --no-fund --ignore-scripts)

# @xenova/transformers is an optional peer of agentmemory's LocalEmbeddingProvider.
# We need it for hybrid-mode benchmarks. Sharp (its image dep) requires a native
# build that --ignore-scripts skipped above; npm rebuild gets us the .node binary
# without re-running every other postinstall.
echo "[setup_agentmemory] installing @xenova/transformers + rebuilding sharp"
(cd "$TARGET" \
  && npm install --silent --no-audit --no-fund @xenova/transformers \
  && npm rebuild sharp)

echo "[setup_agentmemory] verifying primitives import"
(cd "$TARGET" && cat > /tmp/_am_smoke.mjs <<'EOF'
import { SearchIndex } from "./src/state/search-index.ts";
const idx = new SearchIndex();
idx.add({ id: "x", sessionId: "s", timestamp: new Date().toISOString(),
          type: "code", title: "t", facts: [], narrative: "hello world",
          concepts: [], files: ["f"], importance: 5 });
const r = idx.search("hello", 5);
if (!r.length) { console.error("smoke: empty result"); process.exit(2); }
console.log("smoke ok:", r[0].score.toFixed(3));
EOF
npx tsx /tmp/_am_smoke.mjs)

echo "[setup_agentmemory] done. The bench can now use the agentmemory-bm25 and"
echo "                    agentmemory-hybrid adapters."
