package main

// Text -> fixed-dim float32 vector, the canonical recipe for
// nomic-embed-text-v1.5:
//
//   1. tokenize via daulet/tokenizers (CGo binding to HuggingFace's
//      Rust `tokenizers` crate — reads tokenizer.json natively)
//   2. forward pass through ONNX via yalue/onnxruntime_go (CGo binding
//      to Microsoft's ONNX Runtime C library)
//   3. mean-pool the last hidden state with the attention mask
//   4. L2-normalize
//
// Both heavy steps (tokenize + ORT inference) run inside native code;
// this file is the Go orchestration around them. The mean-pool +
// normalize are pure Go (~30 lines, deliberately written longhand
// rather than pulling in a math library — see EncodeBatch below).

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sync"

	"github.com/daulet/tokenizers"
	ort "github.com/yalue/onnxruntime_go"
)

// nomic-embed-text-v1.5's ONNX export advertises three inputs. We
// always feed input_ids and attention_mask; token_type_ids is filled
// with zeros (the WordPiece tokenizer doesn't emit segment ids, and
// nomic only uses a single segment). The names are hardcoded rather
// than discovered at session init because the model is fixed (we
// ship one image, one model) and hardcoding lets the Run() hot path
// skip a name lookup per call.
const (
	inputIDs       = "input_ids"
	attentionMask  = "attention_mask"
	tokenTypeIDs   = "token_type_ids"
	outputLastHS   = "last_hidden_state"
	bertPadTokenID = 0
)

// Embedder is the text -> fixed-dim float32 vector encoder. Safe to
// call from any number of goroutines: EncodeBatch serializes ORT.Run
// + tokenizer.Encode under a single mutex. If throughput ever needs
// more than this, run multiple replicas behind a proxy rather than
// trying to make one replica concurrent — that's a much simpler
// scaling story than juggling multiple ORT sessions in-process.
type Embedder struct {
	cfg       Config
	tokenizer *tokenizers.Tokenizer
	session   *ort.DynamicAdvancedSession
	hasTokenType bool

	mu sync.Mutex
}

// NewEmbedder loads the tokenizer + ONNX model from cfg.ModelDir, runs
// a warmup pass to pay any first-call lazy-init cost up front, and
// returns a ready-to-use Embedder. The caller owns it: call Close() on
// shutdown to release the native handles.
func NewEmbedder(cfg Config) (*Embedder, error) {
	modelPath := filepath.Join(cfg.ModelDir, cfg.ONNXFilename)
	tokPath := filepath.Join(cfg.ModelDir, "tokenizer.json")

	if _, err := os.Stat(modelPath); err != nil {
		return nil, fmt.Errorf("ONNX model not found at %s: %w (did the entrypoint download it?)", modelPath, err)
	}
	if _, err := os.Stat(tokPath); err != nil {
		return nil, fmt.Errorf("tokenizer.json not found at %s: %w (did the entrypoint download it?)", tokPath, err)
	}

	// tokenizer.json -> in-memory bytes -> tokenizer with explicit
	// truncation. The Go binding doesn't expose enable_truncation /
	// enable_padding separately; truncation has to be set at construct
	// time via FromBytesWithTruncation. Padding is handled in
	// padToMaxLen below — daulet/tokenizers has no built-in padding.
	tokBytes, err := os.ReadFile(tokPath)
	if err != nil {
		return nil, fmt.Errorf("read tokenizer.json: %w", err)
	}
	tk, err := tokenizers.FromBytesWithTruncation(tokBytes, uint32(cfg.MaxLength), tokenizers.TruncationDirectionRight)
	if err != nil {
		return nil, fmt.Errorf("load tokenizer: %w", err)
	}

	// Discover whether the ONNX graph wants token_type_ids. nomic's
	// current export does; older ones don't. Cheaper to ask once at
	// startup than to send the wrong feed every Run().
	inputInfos, _, err := ort.GetInputOutputInfo(modelPath)
	if err != nil {
		tk.Close()
		return nil, fmt.Errorf("inspect ONNX inputs: %w", err)
	}
	hasTokenType := false
	for _, in := range inputInfos {
		if in.Name == tokenTypeIDs {
			hasTokenType = true
			break
		}
	}

	// Configure ORT to do all graph optimisations at session init
	// rather than lazily at first Run. Without this, the first /api/embed
	// after startup pays a ~200ms tax that doesn't show up in p50/p95
	// reports but does show up in the integration-test latency.
	opts, err := ort.NewSessionOptions()
	if err != nil {
		tk.Close()
		return nil, fmt.Errorf("ort.NewSessionOptions: %w", err)
	}
	defer opts.Destroy()
	if err := opts.SetGraphOptimizationLevel(ort.GraphOptimizationLevelEnableAll); err != nil {
		tk.Close()
		return nil, fmt.Errorf("set graph opt level: %w", err)
	}
	if cfg.IntraOpThreads > 0 {
		if err := opts.SetIntraOpNumThreads(cfg.IntraOpThreads); err != nil {
			tk.Close()
			return nil, fmt.Errorf("set intra-op threads: %w", err)
		}
	}

	inputNames := []string{inputIDs, attentionMask}
	if hasTokenType {
		inputNames = append(inputNames, tokenTypeIDs)
	}
	session, err := ort.NewDynamicAdvancedSession(modelPath, inputNames, []string{outputLastHS}, opts)
	if err != nil {
		tk.Close()
		return nil, fmt.Errorf("ort.NewDynamicAdvancedSession: %w", err)
	}

	e := &Embedder{
		cfg:          cfg,
		tokenizer:    tk,
		session:      session,
		hasTokenType: hasTokenType,
	}

	// Warm the graph: first Run triggers some lazy kernel selection
	// in ORT. Pay it now so the first /api/embed call after startup
	// isn't slow. Errors here are non-fatal; the real call will
	// surface them with full context.
	_, _ = e.EncodeBatch([]string{"warmup"})
	return e, nil
}

func (e *Embedder) Close() error {
	if e.session != nil {
		if err := e.session.Destroy(); err != nil {
			return err
		}
	}
	if e.tokenizer != nil {
		return e.tokenizer.Close()
	}
	return nil
}

func (e *Embedder) Dim() int      { return e.cfg.ExpectedDim }
func (e *Embedder) ModelID() string {
	return filepath.Base(e.cfg.ModelDir)
}

// EncodeBatch embeds a batch of texts and returns one float32 vector
// per input, each of length cfg.ExpectedDim, L2-normalized so that
// cosine similarity downstream is just a dot product.
//
// Empty batch returns an empty result without touching native code.
// Empty strings inside the batch are passed through to the tokenizer,
// which produces just the CLS+SEP special tokens — same as Ollama.
// The vector you get for "" is not very meaningful but it is valid;
// callers higher up the stack don't pre-validate inputs and we don't
// want to surprise them.
func (e *Embedder) EncodeBatch(texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return [][]float32{}, nil
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	// Encode each text. The Go tokenizer binding doesn't have a
	// batch-encode that pads internally, so we pad to max-in-batch
	// ourselves. The padding token id for nomic's WordPiece is 0
	// ([PAD]); the attention mask is 0 for padded positions so they
	// don't contribute to the mean-pool downstream.
	batchSize := len(texts)
	encs := make([]tokenizers.Encoding, batchSize)
	maxLen := 0
	for i, text := range texts {
		enc, err := e.tokenizer.EncodeWithOptionsErr(text, true, tokenizers.WithReturnAttentionMask())
		if err != nil {
			return nil, fmt.Errorf("tokenize[%d]: %w", i, err)
		}
		encs[i] = enc
		if len(enc.IDs) > maxLen {
			maxLen = len(enc.IDs)
		}
	}
	// Defensive: maxLen=0 can happen if every input is the empty
	// string and the tokenizer suppresses special tokens. Force a
	// length of 1 so we still produce a valid (B,1) tensor.
	if maxLen == 0 {
		maxLen = 1
	}

	idsFlat := make([]int64, batchSize*maxLen)
	maskFlat := make([]int64, batchSize*maxLen)
	for i := range encs {
		ids := encs[i].IDs
		mask := encs[i].AttentionMask
		for j := 0; j < len(ids); j++ {
			idsFlat[i*maxLen+j] = int64(ids[j])
			if j < len(mask) {
				maskFlat[i*maxLen+j] = int64(mask[j])
			} else {
				maskFlat[i*maxLen+j] = 1
			}
		}
		// Pad positions retain idsFlat = bertPadTokenID = 0 and
		// maskFlat = 0, both of which are the defaults of make().
	}

	shape := ort.NewShape(int64(batchSize), int64(maxLen))

	idsT, err := ort.NewTensor(shape, idsFlat)
	if err != nil {
		return nil, fmt.Errorf("create input_ids tensor: %w", err)
	}
	defer idsT.Destroy()

	maskT, err := ort.NewTensor(shape, maskFlat)
	if err != nil {
		return nil, fmt.Errorf("create attention_mask tensor: %w", err)
	}
	defer maskT.Destroy()

	inputs := []ort.Value{idsT, maskT}
	if e.hasTokenType {
		zeros := make([]int64, batchSize*maxLen)
		ttT, err := ort.NewTensor(shape, zeros)
		if err != nil {
			return nil, fmt.Errorf("create token_type_ids tensor: %w", err)
		}
		defer ttT.Destroy()
		inputs = append(inputs, ttT)
	}

	// Pass nil output so ORT auto-allocates a tensor of the right
	// shape and dtype. session.Run fills outputs[0] with the result;
	// we must Destroy it ourselves after extracting the data.
	outputs := []ort.Value{nil}
	if err := e.session.Run(inputs, outputs); err != nil {
		return nil, fmt.Errorf("session.Run: %w", err)
	}
	defer outputs[0].Destroy()

	hsTensor, ok := outputs[0].(*ort.Tensor[float32])
	if !ok {
		return nil, fmt.Errorf("unexpected output type %T (want *Tensor[float32])", outputs[0])
	}
	hsShape := hsTensor.GetShape()
	if len(hsShape) != 3 {
		return nil, fmt.Errorf("unexpected output shape %v (want (B,S,D))", hsShape)
	}
	if int(hsShape[0]) != batchSize || int(hsShape[1]) != maxLen {
		return nil, fmt.Errorf("output shape %v doesn't match inputs (B=%d S=%d)", hsShape, batchSize, maxLen)
	}
	dim := int(hsShape[2])
	if dim != e.cfg.ExpectedDim {
		return nil, fmt.Errorf("model produced %d-dim vectors, config expected %d", dim, e.cfg.ExpectedDim)
	}
	hs := hsTensor.GetData()

	// Mean-pool along the sequence axis with the attention mask:
	//
	//     pooled[b,d] = sum_s ( hs[b,s,d] * mask[b,s] ) / sum_s mask[b,s]
	//
	// Pad positions contribute zero (mask=0) and don't inflate the
	// denominator. Written longhand on purpose — pulling in a math
	// library to save a dozen lines would obscure what's a very
	// simple loop.
	out := make([][]float32, batchSize)
	for b := 0; b < batchSize; b++ {
		pooled := make([]float32, dim)
		var denom float32
		for s := 0; s < maxLen; s++ {
			m := float32(maskFlat[b*maxLen+s])
			if m == 0 {
				continue
			}
			denom += m
			rowOff := (b*maxLen + s) * dim
			for d := 0; d < dim; d++ {
				pooled[d] += hs[rowOff+d] * m
			}
		}
		if denom == 0 {
			denom = 1e-9
		}
		var norm2 float64
		for d := 0; d < dim; d++ {
			pooled[d] /= denom
			norm2 += float64(pooled[d]) * float64(pooled[d])
		}
		norm := float32(math.Sqrt(norm2))
		if norm < 1e-12 {
			norm = 1e-12
		}
		for d := 0; d < dim; d++ {
			pooled[d] /= norm
		}
		out[b] = pooled
	}
	return out, nil
}

// EncodeOne is a thin wrapper around EncodeBatch for the common
// single-text case. The internal pipeline doesn't have a faster
// single-text path — ORT batches of size 1 are how this case is
// handled.
func (e *Embedder) EncodeOne(text string) ([]float32, error) {
	out, err := e.EncodeBatch([]string{text})
	if err != nil {
		return nil, err
	}
	return out[0], nil
}
