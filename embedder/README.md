# devrouter-embedder

Dockerized ONNX server for `nomic-embed-text-v1.5`. Single static-ish
Go binary on top of the canonical native libraries (HuggingFace's Rust
`tokenizers`, Microsoft's ONNX Runtime C library), no Python, no
framework. Serves the same `/api/embed` wire shape as Ollama so
`DEVROUTER_EMBEDDING_URL` swaps between this and any other compatible
endpoint without code changes.

## TL;DR

```bash
make embedder-build       # ~1-2 min, fetches native libs + compiles
make embedder-up          # downloads ~440MB model on first run
make embedder-status

# devrouter's default DEVROUTER_EMBEDDING_URL already points here;
# nothing to flip in your MCP config.
```

```bash
# Host-side dev path (runs go test outside docker)
make embedder-deps         # fetch native libs into ./lib
make embedder-fetch-model  # fetch model into ./testdata
make embedder-test         # runs the test suite
```

### About the first start

The model (~440MB ONNX + tokenizer) is **not baked into the image** —
it downloads on first container start into a docker volume named
`embedder-models` and persists across rebuilds. This is deliberate:

- HuggingFace's CDN occasionally drops mid-download; failing inside a
  Docker `RUN` layer wastes the entire image cache. Failing inside the
  entrypoint is recoverable with `docker compose restart embedder` —
  the entrypoint resumes from where it stopped (curl `-C -`).
- Air-gapped users can pre-populate the `embedder-models` volume once
  and reuse it across hosts.

First start: 1-3 minutes depending on link speed. Subsequent starts: <5s.

## Why this exists

devrouter needs canonical HuggingFace `nomic-embed-text-v1.5` vectors:

- **canonical**: matches the reference PyTorch implementation exactly.
  Ollama's `nomic-embed-text` ships a llama.cpp/GGUF runtime that
  drifts on long text — fine for short queries but not for memories
  with longer flow descriptions, where the drift can shift devrouter's
  cosine-floor (default 0.60) and FP-threshold (default 0.70) decisions
  on borderline hits.
- **self-contained**: no external Ollama / hosted-API dependency, no
  Python interpreter inside the container, single binary.
- **predictable**: fixed model, fixed wire shape, single mutex around
  inference. Throughput-bound? Run multiple replicas, don't try to
  thread one harder.

## Wire shape

```
POST /api/embed
{"model": "<ignored>", "input": "<text>"}
{"model": "<ignored>", "input": ["t1", "t2", ...]}

→ 200
{"embeddings": [[<768 floats>], ...], "model": "nomic-embed-text-v1.5"}
```

The `model` field is accepted for Ollama compatibility but ignored
— each container instance serves exactly one model. Run multiple
containers behind a proxy if you need to route between models.

Other endpoints:

- `GET /api/health` — `{"status": "ok", "model", "dim"}`
- `GET /api/version` — `{"model", "dim", "max_length", "onnx_filename"}`

## Build / run / stop

From the repo root:

```bash
make embedder-build       # docker compose build (one-time)
make embedder-up          # docker compose up -d, waits for /api/health
make embedder-down        # docker compose down
make embedder-status      # curl /api/health
make embedder-logs        # docker compose logs -f
```

Or directly with `docker compose` from this directory:

```bash
cd embedder
docker compose up -d
docker compose logs -f
docker compose down
```

## How it's built

Two native libraries do all the heavy lifting:

| Lib                       | Source                                                | Link kind | Runtime?                  |
|---------------------------|-------------------------------------------------------|-----------|---------------------------|
| `libtokenizers.a`         | [`daulet/tokenizers`](https://github.com/daulet/tokenizers) (Rust) | static    | embedded in binary        |
| `libonnxruntime.so/dylib` | [`microsoft/onnxruntime`](https://github.com/microsoft/onnxruntime) | dynamic   | loaded at startup via env |

The Go code does:

1. Tokenize each text via the static Rust binding (WordPiece, truncated to 8192).
2. Pad each input to the batch-max length with `[PAD]` (id 0) and mask 0.
3. Run ONNX inference via the dynamic library (input_ids + attention_mask + token_type_ids).
4. Mean-pool the last hidden state over the sequence axis, masked.
5. L2-normalize so cosine = dot product downstream.

Step 4 is hand-rolled in Go (~20 lines, see `embedder.go`) — simpler
to read than pulling in a math library to save a dozen lines.

## Image layout

```
embedder/
├── go.mod / go.sum         # github.com/daulet/tokenizers + github.com/yalue/onnxruntime_go
├── config.go               # env-var loading
├── embedder.go             # tokenize + ORT + mean-pool + L2 normalize
├── server.go               # net/http handlers (/api/embed, /api/health, /api/version)
├── main.go                 # process lifecycle, signal handling, graceful shutdown
├── embedder_test.go        # invariants of the pipeline (dim, L2, batch=single, etc.)
├── Dockerfile              # 3-stage: libfetch → builder → slim runtime
├── docker-compose.yml      # port 11435, model on persistent volume
├── entrypoint.sh           # downloads model on first start, exec's binary
├── scripts/
│   ├── fetch-libs.sh       # native libs into ./lib for host dev
│   └── fetch-model.sh      # model into ./testdata for host dev
├── lib/                    # populated by fetch-libs.sh (gitignored)
└── testdata/               # populated by fetch-model.sh (gitignored)
```

## Configuration

Read at startup:

| Variable           | Default                                | Notes                                                                                |
|--------------------|----------------------------------------|--------------------------------------------------------------------------------------|
| `MODEL_DIR`        | `/models/nomic-embed-text-v1.5`        | Where the model + tokenizer live. Mounted from a docker volume in production.        |
| `ONNX_FILENAME`    | `model.onnx`                           | Swap to `model_quantized.onnx` for an int8 variant.                                  |
| `MAX_LENGTH`       | `8192`                                 | Tokenizer truncation. Matches the canonical HF reference for nomic-embed-text-v1.5.  |
| `EXPECTED_DIM`     | `768`                                  | Sanity-check; refuses to start if the model produces a different size.               |
| `INTRA_OP_THREADS` | `0`                                    | ONNX intra-op threads. 0 = let ORT pick (= physical cores).                          |
| `ORT_LIBRARY_PATH` | `/usr/local/lib/libonnxruntime.so`     | Path to `libonnxruntime.so` / `.dylib`. Set explicitly because Go bindings dlopen it. |
| `LISTEN_ADDR`      | `0.0.0.0:8080`                         | HTTP listen address.                                                                 |

## Operational notes

- **Image size**: ~120MB (debian slim + libonnxruntime.so + 26MB binary).
- **Cold start**: ~1.0s for ORT session init + warmup pass.
- **Per-call latency** (M-series Mac, CPU EP): ~20ms p50 for short text,
  ~50ms p95 for long flow-description style inputs. Varies with
  `INTRA_OP_THREADS`.
- **Concurrency**: a coarse process-wide mutex around tokenize +
  ORT.Run. Throughput-bound? Run multiple replicas behind a load
  balancer rather than trying to make one replica concurrent.
- **No GPU**: this image ships CPU EP. For GPU, replace the base image
  with `nvidia/cuda` and use `libonnxruntime_providers_cuda.so`.

## Tests

```bash
make embedder-test
```

Runs against the local model on the host (downloads on first run if
not already present). Covers:

- `TestDimMatchesExpected` — model returns 768-dim
- `TestL2Normalized` — vectors are unit-norm
- `TestReproducibleSameInput` — deterministic given identical input
- `TestLongInputTruncatedNotErrored` — 100K-char input doesn't crash
- `TestBatchMatchesSingleton` — batched and singleton calls agree
- `TestSemanticallySimilarTextsAreClose` — paraphrases beat unrelated topics in cosine
- `TestEmptyBatchReturnsEmpty` — `EncodeBatch(nil)` is a no-op, not an error
