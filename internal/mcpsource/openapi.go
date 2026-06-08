package mcpsource

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
)

// openAPITransport calls a REST tool described by an OpenAPI spec. The
// spec is the discovery source: on construction we pick the search
// operation (by operationId or a search-like GET) and the query
// parameter, so an OpenAPI tool needs only its spec URL configured.
//
// Scope is deliberately small — a single GET with the query bound to one
// query parameter, which covers documentation/issue/search APIs. Path
// parameters and request bodies are out of scope.
type openAPITransport struct {
	client     *http.Client
	headers    map[string]string
	method     string
	requestURL string // base server URL + operation path
	queryParam string // resolved query parameter name
}

func newOpenAPITransport(spec string, headers map[string]string, opSelector string, timeout time.Duration) (*openAPITransport, error) {
	loader := openapi3.NewLoader()
	loader.IsExternalRefsAllowed = true

	var (
		doc *openapi3.T
		err error
	)
	if isHTTPURL(spec) {
		u, perr := url.Parse(spec)
		if perr != nil {
			return nil, fmt.Errorf("openapi: bad spec url %q: %w", spec, perr)
		}
		doc, err = loader.LoadFromURI(u)
	} else {
		doc, err = loader.LoadFromFile(spec)
	}
	if err != nil {
		return nil, fmt.Errorf("openapi: load spec %q: %w", spec, err)
	}

	method, path, op := selectOperation(doc, opSelector)
	if op == nil {
		return nil, fmt.Errorf("openapi %q: no usable search operation found", spec)
	}
	qp := selectQueryParam(op)
	if qp == "" {
		return nil, fmt.Errorf("openapi %q: operation %s %s has no query parameter", spec, method, path)
	}
	base := serverURL(doc, spec)
	if base == "" {
		return nil, fmt.Errorf("openapi %q: could not determine server URL", spec)
	}

	return &openAPITransport{
		client:     &http.Client{Timeout: timeout},
		headers:    headers,
		method:     method,
		requestURL: strings.TrimRight(base, "/") + path,
		queryParam: qp,
	}, nil
}

func (t *openAPITransport) call(ctx context.Context, _ string, args map[string]any) (string, error) {
	u, err := url.Parse(t.requestURL)
	if err != nil {
		return "", err
	}
	q := u.Query()
	// The query string lives under the resolved param name, set by
	// Source.Search via Config.QueryArg. Fall back to "query".
	qv := args[t.queryParam]
	if qv == nil {
		qv = args["query"]
	}
	q.Set(t.queryParam, fmt.Sprint(qv))
	// Forward any other static ExtraArgs as query parameters.
	for k, v := range args {
		if k == t.queryParam || k == "query" {
			continue
		}
		q.Set(k, fmt.Sprint(v))
	}
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, t.method, u.String(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")
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
		return "", fmt.Errorf("openapi %s %s returned %d", t.method, u.String(), resp.StatusCode)
	}
	return string(out), nil
}

func (t *openAPITransport) Close() error { return nil }

// selectOperation picks the operation to call. With an explicit selector
// it matches operationId; otherwise it prefers a GET whose
// operationId/path/summary matches a search hint, falling back to the
// first GET (paths sorted for determinism). Operations with required
// path parameters are skipped (out of scope).
func selectOperation(doc *openapi3.T, selector string) (string, string, *openapi3.Operation) {
	if doc.Paths == nil {
		return "", "", nil
	}
	paths := make([]string, 0, len(doc.Paths.Map()))
	for p := range doc.Paths.Map() {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	var (
		fbMethod string
		fbPath   string
		fbOp     *openapi3.Operation
	)
	for _, path := range paths {
		item := doc.Paths.Map()[path]
		for method, op := range item.Operations() {
			if op == nil {
				continue
			}
			if selector != "" {
				if op.OperationID == selector {
					return method, path, op
				}
				continue
			}
			if method != http.MethodGet || hasRequiredPathParam(op) {
				continue
			}
			if fbOp == nil {
				fbMethod, fbPath, fbOp = method, path, op
			}
			hay := strings.ToLower(op.OperationID + " " + path + " " + op.Summary)
			for _, h := range searchToolHints {
				if strings.Contains(hay, h) {
					return method, path, op
				}
			}
		}
	}
	if selector == "" {
		return fbMethod, fbPath, fbOp
	}
	return "", "", nil
}

func hasRequiredPathParam(op *openapi3.Operation) bool {
	for _, ref := range op.Parameters {
		if ref.Value != nil && ref.Value.In == "path" && ref.Value.Required {
			return true
		}
	}
	return false
}

// selectQueryParam picks the parameter that carries the query: a
// conventionally-named query param, else the first required string query
// param, else the first query param.
func selectQueryParam(op *openapi3.Operation) string {
	var params []*openapi3.Parameter
	for _, ref := range op.Parameters {
		if ref.Value != nil && ref.Value.In == "query" {
			params = append(params, ref.Value)
		}
	}
	for _, hint := range queryArgHints {
		for _, p := range params {
			if strings.EqualFold(p.Name, hint) {
				return p.Name
			}
		}
	}
	for _, p := range params {
		if p.Required && isStringSchema(p.Schema) {
			return p.Name
		}
	}
	if len(params) > 0 {
		return params[0].Name
	}
	return ""
}

func isStringSchema(ref *openapi3.SchemaRef) bool {
	return ref != nil && ref.Value != nil && ref.Value.Type != nil && ref.Value.Type.Is("string")
}

// serverURL resolves the base URL: the spec's first server (resolved
// against the spec URL if relative), else the spec URL's origin.
func serverURL(doc *openapi3.T, spec string) string {
	if len(doc.Servers) > 0 && doc.Servers[0].URL != "" {
		su := doc.Servers[0].URL
		if isHTTPURL(su) {
			return su
		}
		if isHTTPURL(spec) {
			if base, err := url.Parse(spec); err == nil {
				if rel, err := url.Parse(su); err == nil {
					return base.ResolveReference(rel).String()
				}
			}
		}
		return su
	}
	if isHTTPURL(spec) {
		if u, err := url.Parse(spec); err == nil {
			return u.Scheme + "://" + u.Host
		}
	}
	return ""
}

func isHTTPURL(s string) bool {
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")
}
