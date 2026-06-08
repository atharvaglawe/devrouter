package mcpsource

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// transport abstracts "call one read-only tool and give me back the text
// payload to map". Three implementations cover the tools we integrate:
//
//   - httpJSONTransport: a plain HTTP POST returning JSON (the cmdocs
//     FastAPI sidecar). No MCP framing.
//   - mcpHTTPTransport: MCP JSON-RPC 2.0 over Streamable HTTP (GitLab).
//   - mcpStdioTransport: MCP JSON-RPC 2.0 over a long-lived subprocess's
//     stdin/stdout (newline-delimited), for stdio MCP servers.
//
// call returns the tool's textual result (for MCP, the concatenated
// text content blocks; for http-json, the raw response body), which the
// configured mapper turns into []prompt.DocEntry.
type transport interface {
	call(ctx context.Context, tool string, args map[string]any) (string, error)
	Close() error
}

const (
	protocolVersion = "2025-06-18"
	clientName      = "devrouter"
	clientVersion   = "1"
)

// ---------------------------------------------------------------------------
// httpJSONTransport — plain HTTP POST (cmdocs sidecar)
// ---------------------------------------------------------------------------

type httpJSONTransport struct {
	endpoint string
	headers  map[string]string
	client   *http.Client
}

func newHTTPJSONTransport(endpoint string, headers map[string]string, timeout time.Duration) *httpJSONTransport {
	return &httpJSONTransport{
		endpoint: endpoint,
		headers:  headers,
		client:   &http.Client{Timeout: timeout},
	}
}

func (t *httpJSONTransport) call(ctx context.Context, _ string, args map[string]any) (string, error) {
	body, err := json.Marshal(args)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.endpoint, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range t.headers {
		req.Header.Set(k, v)
	}
	resp, err := t.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("http-json %s returned %d", t.endpoint, resp.StatusCode)
	}
	return string(out), nil
}

func (t *httpJSONTransport) Close() error { return nil }

// ---------------------------------------------------------------------------
// JSON-RPC envelope shared by the MCP transports
// ---------------------------------------------------------------------------

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("rpc error %d: %s", e.Code, e.Message) }

// toolCallResult is the MCP tools/call result shape. We consume the
// text content blocks and, when present, the structured output
// (structuredContent, MCP 2025-06-18) which the generic normalizer
// reads directly.
type toolCallResult struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	StructuredContent json.RawMessage `json:"structuredContent"`
	IsError           bool            `json:"isError"`
}

// extractText returns the most structured payload available from an MCP
// tools/call result: structuredContent (JSON) when present, otherwise
// the concatenated text content blocks, otherwise the raw result.
func extractText(result json.RawMessage) (string, error) {
	var tc toolCallResult
	if err := json.Unmarshal(result, &tc); err != nil {
		// Some servers return a bare value; fall back to the raw JSON.
		return string(result), nil
	}
	if len(tc.StructuredContent) > 0 && !isJSONNull(tc.StructuredContent) {
		return string(tc.StructuredContent), nil
	}
	if len(tc.Content) == 0 {
		return string(result), nil
	}
	var sb strings.Builder
	for _, c := range tc.Content {
		if c.Text != "" {
			if sb.Len() > 0 {
				sb.WriteByte('\n')
			}
			sb.WriteString(c.Text)
		}
	}
	return sb.String(), nil
}

func isJSONNull(raw json.RawMessage) bool {
	return string(bytes.TrimSpace(raw)) == "null"
}

// ---------------------------------------------------------------------------
// Tool discovery (MCP tools/list)
// ---------------------------------------------------------------------------

// toolDesc is one entry from an MCP tools/list response.
type toolDesc struct {
	Name        string
	Description string
	InputSchema json.RawMessage
}

// discoverer is the optional capability a transport implements when it
// can enumerate the tools it exposes. Source.New uses it to auto-pick
// the search tool and infer its query argument, so an MCP tool needs
// only its endpoint/command configured.
type discoverer interface {
	listTools(ctx context.Context) ([]toolDesc, error)
}

type toolsListResult struct {
	Tools []struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		InputSchema json.RawMessage `json:"inputSchema"`
	} `json:"tools"`
}

func parseToolsList(result json.RawMessage) ([]toolDesc, error) {
	var r toolsListResult
	if err := json.Unmarshal(result, &r); err != nil {
		return nil, err
	}
	out := make([]toolDesc, 0, len(r.Tools))
	for _, t := range r.Tools {
		out = append(out, toolDesc{Name: t.Name, Description: t.Description, InputSchema: t.InputSchema})
	}
	return out, nil
}

func toolCallParams(tool string, args map[string]any) map[string]any {
	return map[string]any{"name": tool, "arguments": args}
}

func initializeParams() map[string]any {
	return map[string]any{
		"protocolVersion": protocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": clientName, "version": clientVersion},
	}
}

// ---------------------------------------------------------------------------
// mcpHTTPTransport — MCP JSON-RPC 2.0 over Streamable HTTP (GitLab)
// ---------------------------------------------------------------------------

type mcpHTTPTransport struct {
	endpoint string
	headers  map[string]string
	client   *http.Client

	mu          sync.Mutex
	idSeq       int64
	sessionID   string
	initialized bool
}

func newMCPHTTPTransport(endpoint string, headers map[string]string, timeout time.Duration) *mcpHTTPTransport {
	return &mcpHTTPTransport{
		endpoint: endpoint,
		headers:  headers,
		client:   &http.Client{Timeout: timeout},
	}
}

func (t *mcpHTTPTransport) call(ctx context.Context, tool string, args map[string]any) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if !t.initialized {
		if err := t.handshake(ctx); err != nil {
			return "", err
		}
	}

	resp, err := t.rpc(ctx, "tools/call", toolCallParams(tool, args), true)
	if err != nil {
		return "", err
	}
	if resp.Error != nil {
		return "", resp.Error
	}
	return extractText(resp.Result)
}

func (t *mcpHTTPTransport) listTools(ctx context.Context) ([]toolDesc, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.initialized {
		if err := t.handshake(ctx); err != nil {
			return nil, err
		}
	}
	resp, err := t.rpc(ctx, "tools/list", map[string]any{}, true)
	if err != nil {
		return nil, err
	}
	if resp.Error != nil {
		return nil, resp.Error
	}
	return parseToolsList(resp.Result)
}

func (t *mcpHTTPTransport) handshake(ctx context.Context) error {
	resp, err := t.rpc(ctx, "initialize", initializeParams(), true)
	if err != nil {
		return fmt.Errorf("mcp initialize: %w", err)
	}
	if resp.Error != nil {
		return fmt.Errorf("mcp initialize: %w", resp.Error)
	}
	// notifications/initialized is a notification (no id, no response).
	_, _ = t.rpc(ctx, "notifications/initialized", nil, false)
	t.initialized = true
	return nil
}

// rpc performs one JSON-RPC call. When expectResult is false the message
// is sent as a notification (no id) and the response is ignored.
func (t *mcpHTTPTransport) rpc(ctx context.Context, method string, params any, expectResult bool) (*rpcResponse, error) {
	reqBody := rpcRequest{JSONRPC: "2.0", Method: method, Params: params}
	var wantID int64
	if expectResult {
		wantID = atomic.AddInt64(&t.idSeq, 1)
		reqBody.ID = wantID
	}
	raw, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, t.endpoint, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json, text/event-stream")
	if t.sessionID != "" {
		httpReq.Header.Set("Mcp-Session-Id", t.sessionID)
	}
	for k, v := range t.headers {
		httpReq.Header.Set(k, v)
	}

	resp, err := t.client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		t.sessionID = sid
	}
	if !expectResult {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return nil, nil
	}
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return nil, fmt.Errorf("mcp %s returned %d: %s", method, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return parseRPCResponse(resp.Header.Get("Content-Type"), resp.Body, wantID)
}

func (t *mcpHTTPTransport) Close() error { return nil }

// parseRPCResponse handles both a single application/json body and a
// text/event-stream (SSE) body, returning the JSON-RPC response whose id
// matches wantID (or the first/last response when ids are absent).
func parseRPCResponse(contentType string, body io.Reader, wantID int64) (*rpcResponse, error) {
	limited := io.LimitReader(body, 8<<20)
	if strings.Contains(contentType, "text/event-stream") {
		return parseSSE(limited, wantID)
	}
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	return matchResponse(bytes.Split(data, []byte("\n")), wantID)
}

// parseSSE accumulates `data:` lines per SSE event and returns the
// matching JSON-RPC response.
func parseSSE(body io.Reader, wantID int64) (*rpcResponse, error) {
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64<<10), 8<<20)
	var data [][]byte
	var candidates [][]byte
	flush := func() {
		if len(data) > 0 {
			candidates = append(candidates, bytes.Join(data, []byte("\n")))
			data = nil
		}
	}
	for sc.Scan() {
		line := sc.Bytes()
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) == 0 {
			flush()
			continue
		}
		if bytes.HasPrefix(trimmed, []byte("data:")) {
			data = append(data, bytes.TrimSpace(trimmed[len("data:"):]))
		}
	}
	flush()
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return matchResponse(candidates, wantID)
}

// matchResponse decodes each candidate JSON blob and returns the one
// whose id matches wantID. Falls back to the last decodable response so
// servers that omit/reformat ids still work.
func matchResponse(candidates [][]byte, wantID int64) (*rpcResponse, error) {
	var last *rpcResponse
	for _, c := range candidates {
		c = bytes.TrimSpace(c)
		if len(c) == 0 || c[0] != '{' {
			continue
		}
		var r rpcResponse
		if err := json.Unmarshal(c, &r); err != nil {
			continue
		}
		if r.Result == nil && r.Error == nil {
			continue // a request/notification echoed back, not a response
		}
		last = &r
		if idMatches(r.ID, wantID) {
			return &r, nil
		}
	}
	if last != nil {
		return last, nil
	}
	return nil, fmt.Errorf("no JSON-RPC response found")
}

func idMatches(raw json.RawMessage, want int64) bool {
	if len(raw) == 0 {
		return false
	}
	var n int64
	if err := json.Unmarshal(raw, &n); err == nil {
		return n == want
	}
	return false
}

// ---------------------------------------------------------------------------
// mcpStdioTransport — MCP JSON-RPC 2.0 over a long-lived subprocess
// ---------------------------------------------------------------------------

type mcpStdioTransport struct {
	args []string
	env  []string

	mu          sync.Mutex
	idSeq       int64
	cmd         *exec.Cmd
	stdin       io.WriteCloser
	stdout      *bufio.Reader
	initialized bool
}

func newMCPStdioTransport(args []string, env []string) *mcpStdioTransport {
	return &mcpStdioTransport{args: args, env: env}
}

func (t *mcpStdioTransport) ensureStarted() error {
	if t.cmd != nil {
		return nil
	}
	if len(t.args) == 0 {
		return fmt.Errorf("stdio transport: empty command")
	}
	cmd := exec.Command(t.args[0], t.args[1:]...)
	if len(t.env) > 0 {
		cmd.Env = append(cmd.Environ(), t.env...)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	t.cmd = cmd
	t.stdin = stdin
	t.stdout = bufio.NewReaderSize(stdout, 64<<10)
	return nil
}

func (t *mcpStdioTransport) call(ctx context.Context, tool string, args map[string]any) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if err := t.ensureStarted(); err != nil {
		return "", err
	}
	if !t.initialized {
		if _, err := t.rpc(ctx, "initialize", initializeParams(), true); err != nil {
			return "", fmt.Errorf("mcp initialize: %w", err)
		}
		if err := t.notify("notifications/initialized", nil); err != nil {
			return "", err
		}
		t.initialized = true
	}
	resp, err := t.rpc(ctx, "tools/call", toolCallParams(tool, args), true)
	if err != nil {
		return "", err
	}
	if resp.Error != nil {
		return "", resp.Error
	}
	return extractText(resp.Result)
}

func (t *mcpStdioTransport) listTools(ctx context.Context) ([]toolDesc, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := t.ensureStarted(); err != nil {
		return nil, err
	}
	if !t.initialized {
		if _, err := t.rpc(ctx, "initialize", initializeParams(), true); err != nil {
			return nil, fmt.Errorf("mcp initialize: %w", err)
		}
		if err := t.notify("notifications/initialized", nil); err != nil {
			return nil, err
		}
		t.initialized = true
	}
	resp, err := t.rpc(ctx, "tools/list", map[string]any{}, true)
	if err != nil {
		return nil, err
	}
	if resp.Error != nil {
		return nil, resp.Error
	}
	return parseToolsList(resp.Result)
}

func (t *mcpStdioTransport) writeMessage(v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	_, err = t.stdin.Write(raw)
	return err
}

func (t *mcpStdioTransport) notify(method string, params any) error {
	return t.writeMessage(rpcRequest{JSONRPC: "2.0", Method: method, Params: params})
}

// rpc writes a request and reads newline-delimited responses until it
// sees the matching id (skipping server-initiated notifications). A
// goroutine enforces the context deadline by closing stdin on timeout.
func (t *mcpStdioTransport) rpc(ctx context.Context, method string, params any, _ bool) (*rpcResponse, error) {
	id := atomic.AddInt64(&t.idSeq, 1)
	if err := t.writeMessage(rpcRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params}); err != nil {
		return nil, err
	}

	type result struct {
		resp *rpcResponse
		err  error
	}
	done := make(chan result, 1)
	go func() {
		for {
			line, err := t.stdout.ReadBytes('\n')
			if err != nil {
				done <- result{nil, err}
				return
			}
			line = bytes.TrimSpace(line)
			if len(line) == 0 || line[0] != '{' {
				continue
			}
			var r rpcResponse
			if err := json.Unmarshal(line, &r); err != nil {
				continue
			}
			if r.Result == nil && r.Error == nil {
				continue
			}
			if idMatches(r.ID, id) {
				done <- result{&r, nil}
				return
			}
		}
	}()

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case res := <-done:
		return res.resp, res.err
	}
}

func (t *mcpStdioTransport) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.stdin != nil {
		t.stdin.Close()
	}
	if t.cmd != nil && t.cmd.Process != nil {
		_ = t.cmd.Process.Kill()
		_ = t.cmd.Wait()
		t.cmd = nil
	}
	return nil
}
