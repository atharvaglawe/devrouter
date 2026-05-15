#!/bin/sh
# Container entrypoint for devrouter-embedder.
#
# Ensures the model is on the mounted /models volume, then exec's the
# Go binary. Model downloads at first run rather than image build:
#   - HF's CDN drops mid-pull; failing at image build wastes the cache
#     layer. Failing at first start is recoverable with `docker compose
#     restart embedder` — the entrypoint resumes (curl -C -).
#   - Air-gapped users can pre-populate /models once and reuse.
#
# `exec` so SIGTERM from `docker stop` reaches the binary directly
# (it has its own signal handler for graceful shutdown).

set -e

MODEL_DIR="${MODEL_DIR:-/models/nomic-embed-text-v1.5}"

echo "[entrypoint] ensuring model present at ${MODEL_DIR}..."
/usr/local/bin/fetch-model.sh "${MODEL_DIR}"

echo "[entrypoint] starting embedder..."
exec /usr/local/bin/embedder
