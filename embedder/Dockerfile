# Three-stage build:
#   1. libfetch  — pull prebuilt native libs (libtokenizers.a + libonnxruntime.so)
#   2. builder   — `go build` the CGo binary against those libs
#   3. runtime   — minimal Debian slim image with just the binary + libonnxruntime.so
#
# The ONNX model is NOT baked into the image. It downloads on first
# container start via entrypoint.sh into the /models volume so:
#   - image builds stay fast and resilient (no 440MB HF download in a
#     RUN layer that fails the whole build on a transient network blip),
#   - the model lives on a persistent volume and is reused across restarts,
#   - air-gapped users can pre-populate /models and the entrypoint skips
#     the download.

ARG GO_VERSION=1.24
# Debian variant used by the libfetch + runtime stages. The Go image
# family has no -slim variant, so the builder stage uses GO_DEBIAN.
ARG DEBIAN_VERSION=bookworm-slim
ARG GO_DEBIAN=bookworm
ARG TOKENIZERS_VERSION=v1.27.0
ARG ONNXRUNTIME_VERSION=1.25.0

# ─── Stage 1: libfetch (pull native libs once, isolated) ──────────────────
# Separate stage so a `docker build --no-cache` of just the Go code path
# doesn't re-download 80MB of native libs every time.
FROM debian:${DEBIAN_VERSION} AS libfetch

ARG TOKENIZERS_VERSION
ARG ONNXRUNTIME_VERSION
ARG TARGETARCH

RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl tar \
 && rm -rf /var/lib/apt/lists/*

WORKDIR /libs

# libtokenizers (static, gets linked into the Go binary).
# Resolve arch from $TARGETARCH (BuildKit auto-populates it) and fall
# back to `dpkg --print-architecture` so the legacy `docker build`
# without buildx also produces a non-empty URL. The release archive's
# arch nomenclature matches Debian's: amd64 / arm64.
RUN ARCH="${TARGETARCH:-$(dpkg --print-architecture)}" \
 && curl -fSL \
      "https://github.com/daulet/tokenizers/releases/download/${TOKENIZERS_VERSION}/libtokenizers.linux-${ARCH}.tar.gz" \
    | tar -xz \
 && test -f libtokenizers.a

# onnxruntime (dynamic, loaded at runtime). Microsoft's archive uses
# "x64" / "aarch64" instead of "amd64" / "arm64", so we translate.
RUN ARCH="${TARGETARCH:-$(dpkg --print-architecture)}"; \
    ARCH_ORT="x64"; \
    if [ "${ARCH}" = "arm64" ]; then ARCH_ORT="aarch64"; fi; \
    curl -fSL \
      "https://github.com/microsoft/onnxruntime/releases/download/v${ONNXRUNTIME_VERSION}/onnxruntime-linux-${ARCH_ORT}-${ONNXRUNTIME_VERSION}.tgz" \
      -o /tmp/ort.tgz \
 && tar -xzf /tmp/ort.tgz -C /tmp \
 && cp "/tmp/onnxruntime-linux-${ARCH_ORT}-${ONNXRUNTIME_VERSION}/lib/libonnxruntime.so.${ONNXRUNTIME_VERSION}" libonnxruntime.so \
 && rm -rf /tmp/ort.tgz "/tmp/onnxruntime-linux-${ARCH_ORT}-${ONNXRUNTIME_VERSION}"


# ─── Stage 2: builder (Go + CGo compile) ──────────────────────────────────
FROM golang:${GO_VERSION}-${GO_DEBIAN} AS builder

# build-essential gives us gcc + libc headers needed by CGo. We don't
# install Rust here because daulet/tokenizers ships a prebuilt static
# .a — no compile-from-source needed.
RUN apt-get update \
 && apt-get install -y --no-install-recommends build-essential ca-certificates \
 && rm -rf /var/lib/apt/lists/*

WORKDIR /src

# Cache module downloads in a layer separate from source so editing Go
# code doesn't redo the dependency fetch.
COPY go.mod go.sum ./
RUN go mod download

COPY --from=libfetch /libs/libtokenizers.a /src/lib/libtokenizers.a
COPY --from=libfetch /libs/libonnxruntime.so /src/lib/libonnxruntime.so

COPY *.go ./

# CGO_ENABLED is implied by the daulet/tokenizers cgo directives but
# set it explicitly so a misconfigured base image fails loudly here
# instead of producing a binary that segfaults on first tokenize call.
ENV CGO_ENABLED=1
RUN CGO_LDFLAGS="-L/src/lib" go build -trimpath -ldflags="-s -w" -o /out/embedder .


# ─── Stage 3: slim runtime ────────────────────────────────────────────────
FROM debian:${DEBIAN_VERSION} AS runtime

# Runtime deps:
#   - ca-certificates: HF download in entrypoint.sh uses HTTPS
#   - curl: model download + healthcheck
#   - libgomp1: ONNX Runtime CPU EP links libgomp at runtime
#
# Anything else (libc, libstdc++) is already in the slim base.
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      ca-certificates curl libgomp1 \
 && rm -rf /var/lib/apt/lists/*

# The Go binary statically links libtokenizers.a so we don't need that
# at runtime. libonnxruntime.so does need to be on disk + reachable via
# the env var the binary reads on startup.
COPY --from=libfetch /libs/libonnxruntime.so /usr/local/lib/libonnxruntime.so
COPY --from=builder /out/embedder /usr/local/bin/embedder
COPY entrypoint.sh /entrypoint.sh
COPY scripts/fetch-model.sh /usr/local/bin/fetch-model.sh
RUN chmod +x /entrypoint.sh /usr/local/bin/fetch-model.sh

# Defaults match the canonical HF reference recipe for nomic-embed-text-v1.5.
# ORT_LIBRARY_PATH points at the dynamic ONNX Runtime lib that
# yalue/onnxruntime_go dlopens at startup.
ENV MODEL_DIR=/models/nomic-embed-text-v1.5 \
    ONNX_FILENAME=model.onnx \
    MAX_LENGTH=8192 \
    EXPECTED_DIM=768 \
    INTRA_OP_THREADS=0 \
    ORT_LIBRARY_PATH=/usr/local/lib/libonnxruntime.so \
    LISTEN_ADDR=0.0.0.0:8080

VOLUME ["/models"]
EXPOSE 8080

# Generous start_period so a first-start (model still downloading) isn't
# reported as unhealthy by docker. Subsequent starts come up in seconds
# because the model lives on the persistent volume.
HEALTHCHECK --interval=30s --timeout=5s --start-period=600s --retries=3 \
    CMD curl -f http://localhost:8080/api/health || exit 1

ENTRYPOINT ["/entrypoint.sh"]
