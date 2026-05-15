#!/usr/bin/env bash
# Download the two native libraries the Go embedder links against.
# Idempotent: skips already-downloaded files.
#
# Produces:
#   embedder/lib/libtokenizers.a           (static, statically linked into the binary)
#   embedder/lib/libonnxruntime.<ext>      (dynamic, loaded at runtime via ORT_LIBRARY_PATH)
#   embedder/lib/onnxruntime_c_api.h       (header — not strictly needed at build time
#                                           since onnxruntime_go bundles its own copy,
#                                           but useful for IDE indexing)
#
# Why a shell script and not Go: this runs *before* the Go build, so it
# can't depend on the Go module. Plain bash + curl + tar is portable
# enough across host (darwin-arm64) and Dockerfile build stage
# (linux/<arch>) without pulling in Python or extra tools.

set -euo pipefail

# Pinned versions. Bumping these requires re-running `make embedder-test`
# before merging — drift in the underlying Rust tokenizer or ONNX
# Runtime can change vectors slightly, and devrouter's downstream
# cosine thresholds are calibrated against the current numbers.
TOKENIZERS_VERSION="${TOKENIZERS_VERSION:-v1.27.0}"
ONNXRUNTIME_VERSION="${ONNXRUNTIME_VERSION:-1.25.0}"

# Resolve OS/arch. Allow override via TARGET_OS / TARGET_ARCH so the
# Dockerfile (always linux) can cross-fetch from a darwin host.
TARGET_OS="${TARGET_OS:-$(uname -s | tr '[:upper:]' '[:lower:]')}"
TARGET_ARCH="${TARGET_ARCH:-$(uname -m)}"

case "$TARGET_OS" in
  darwin) OS_NAME="darwin" ;;
  linux)  OS_NAME="linux"  ;;
  *) echo "unsupported OS: $TARGET_OS" >&2; exit 1 ;;
esac

case "$TARGET_ARCH" in
  arm64|aarch64)   ARCH_TOK="arm64"; ARCH_ORT_LINUX="aarch64"; ARCH_ORT_DARWIN="arm64"  ;;
  x86_64|amd64)    ARCH_TOK="amd64"; ARCH_ORT_LINUX="x64";     ARCH_ORT_DARWIN="x86_64" ;;
  *) echo "unsupported arch: $TARGET_ARCH" >&2; exit 1 ;;
esac

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
LIB_DIR="$(cd "$SCRIPT_DIR/.." && pwd)/lib"
mkdir -p "$LIB_DIR"

# ── libtokenizers (static Rust binding the Go package links into the binary) ──
# Daulet's tokenizers project ships prebuilt archives that contain a
# single libtokenizers.a per (os, arch). The archive name uses a
# slightly different arch nomenclature than ours.

TOK_URL="https://github.com/daulet/tokenizers/releases/download/${TOKENIZERS_VERSION}/libtokenizers.${OS_NAME}-${ARCH_TOK}.tar.gz"
TOK_ARCHIVE="$LIB_DIR/libtokenizers.tar.gz"
TOK_LIB="$LIB_DIR/libtokenizers.a"

if [[ -f "$TOK_LIB" ]]; then
  echo "[skip] libtokenizers.a already present ($(du -h "$TOK_LIB" | cut -f1))"
else
  echo "[fetch] $TOK_URL"
  curl -fSL "$TOK_URL" -o "$TOK_ARCHIVE"
  tar -xzf "$TOK_ARCHIVE" -C "$LIB_DIR"
  rm -f "$TOK_ARCHIVE"
  if [[ ! -f "$TOK_LIB" ]]; then
    echo "[fatal] $TOK_LIB missing after extract — archive layout changed?" >&2
    exit 1
  fi
  echo "[ok] libtokenizers.a installed ($(du -h "$TOK_LIB" | cut -f1))"
fi

# ── onnxruntime (dynamic Microsoft C library) ────────────────────────
# Microsoft's release archive contains lib/ (.so or .dylib) and include/
# (headers). We pluck out exactly what we need and discard the rest to
# keep the lib/ directory uncluttered.

if [[ "$OS_NAME" == "darwin" ]]; then
  ORT_ARCH="$ARCH_ORT_DARWIN"
  ORT_ARCHIVE_NAME="onnxruntime-osx-${ORT_ARCH}-${ONNXRUNTIME_VERSION}"
  ORT_LIB_PATTERN="libonnxruntime.${ONNXRUNTIME_VERSION}.dylib"
  ORT_LIB_DEST="libonnxruntime.dylib"
else
  ORT_ARCH="$ARCH_ORT_LINUX"
  ORT_ARCHIVE_NAME="onnxruntime-linux-${ORT_ARCH}-${ONNXRUNTIME_VERSION}"
  ORT_LIB_PATTERN="libonnxruntime.so.${ONNXRUNTIME_VERSION}"
  ORT_LIB_DEST="libonnxruntime.so"
fi
ORT_URL="https://github.com/microsoft/onnxruntime/releases/download/v${ONNXRUNTIME_VERSION}/${ORT_ARCHIVE_NAME}.tgz"
ORT_LIB_PATH="$LIB_DIR/$ORT_LIB_DEST"

if [[ -f "$ORT_LIB_PATH" ]]; then
  echo "[skip] $ORT_LIB_DEST already present ($(du -h "$ORT_LIB_PATH" | cut -f1))"
else
  ORT_ARCHIVE="$LIB_DIR/onnxruntime.tgz"
  echo "[fetch] $ORT_URL"
  curl -fSL "$ORT_URL" -o "$ORT_ARCHIVE"
  TMP_EXTRACT="$(mktemp -d)"
  tar -xzf "$ORT_ARCHIVE" -C "$TMP_EXTRACT"
  cp "$TMP_EXTRACT/$ORT_ARCHIVE_NAME/lib/$ORT_LIB_PATTERN" "$ORT_LIB_PATH"
  # Header is convenient for IDE indexing but not strictly required —
  # the Go binding ships its own copy. Best-effort; ignore failure.
  if [[ -f "$TMP_EXTRACT/$ORT_ARCHIVE_NAME/include/onnxruntime_c_api.h" ]]; then
    cp "$TMP_EXTRACT/$ORT_ARCHIVE_NAME/include/onnxruntime_c_api.h" "$LIB_DIR/onnxruntime_c_api.h" 2>/dev/null || true
  fi
  rm -rf "$TMP_EXTRACT" "$ORT_ARCHIVE"
  echo "[ok] $ORT_LIB_DEST installed ($(du -h "$ORT_LIB_PATH" | cut -f1))"
fi

echo ""
echo "[done] native libs ready in $LIB_DIR"
ls -la "$LIB_DIR"
