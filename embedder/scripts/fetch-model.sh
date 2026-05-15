#!/usr/bin/env bash
# Download nomic-embed-text-v1.5 (ONNX + tokenizer) into the given
# target directory. Idempotent: skips files already present at the
# expected size.
#
# Fetches the ONNX model + tokenizer assets from HuggingFace via plain
# curl. No huggingface-hub / hf_transfer dependency — the embedder
# image has zero Python, so we trade hf_transfer's parallel range
# fetching for portability. The file we actually care about
# (model.onnx, ~440MB) downloads in 30-60s on a normal link.
#
# Usage:
#   scripts/fetch-model.sh /path/to/target-dir
#   scripts/fetch-model.sh                    # default: ./testdata/nomic-embed-text-v1.5

set -euo pipefail

HF_REPO="${HF_REPO:-nomic-ai/nomic-embed-text-v1.5}"
HF_BASE="https://huggingface.co/${HF_REPO}/resolve/main"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DEFAULT_TARGET="$(cd "$SCRIPT_DIR/.." && pwd)/testdata/nomic-embed-text-v1.5"
TARGET="${1:-$DEFAULT_TARGET}"

mkdir -p "$TARGET"

# (remote_path, local_filename, min_bytes_for_sanity_check). min_bytes
# detects partial downloads left over from an aborted run — anything
# smaller is treated as missing.
FILES=(
  "onnx/model.onnx|model.onnx|100000000"
  "tokenizer.json|tokenizer.json|0"
  "tokenizer_config.json|tokenizer_config.json|0"
  "special_tokens_map.json|special_tokens_map.json|0"
  "vocab.txt|vocab.txt|0"
)

ok_size() {
  # POSIX-ish: file exists AND size >= min. macOS stat takes -f, Linux -c.
  local f="$1" min="$2"
  if [[ ! -f "$f" ]]; then return 1; fi
  local size
  if size=$(stat -f%z "$f" 2>/dev/null); then
    :
  else
    size=$(stat -c%s "$f")
  fi
  [[ "$size" -gt "$min" ]]
}

for entry in "${FILES[@]}"; do
  IFS='|' read -r remote local minbytes <<<"$entry"
  dest="$TARGET/$local"
  if ok_size "$dest" "$minbytes"; then
    echo "[skip] $local already present"
    continue
  fi
  url="${HF_BASE}/${remote}"
  echo "[fetch] $url"
  # -L follows redirects (HF returns 302 to S3), -C - resumes a partial
  # download (useful when model.onnx download is interrupted). Retry 3x
  # with 5s backoff so a transient flake doesn't fail the whole run.
  curl -fSL --retry 3 --retry-delay 5 -C - -o "$dest" "$url"
  if ! ok_size "$dest" "$minbytes"; then
    echo "[fatal] downloaded $local is smaller than expected ($minbytes bytes)" >&2
    exit 1
  fi
done

echo ""
echo "[done] model present at $TARGET"
du -h "$TARGET"/*
