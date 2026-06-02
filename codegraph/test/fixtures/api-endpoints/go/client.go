// Client-side fixtures: each block illustrates one of the five
// recognised Go client-side outbound-call forms.
// Identifiers are intentionally generic.
package server

import (
	"context"
	"net/http"
)

// ── Form 1: stdlib verbs ───────────────────────────────────────────

func fetchHealth() {
	http.Get("/v1/health")
	http.Post("/v1/events", "application/json", nil)
	http.Head("/v1/ping")
}

// ── Form 2: Verb-as-method on a client receiver ────────────────────

type apiClient struct{}

func (c *apiClient) Get(url string) (*http.Response, error)  { return nil, nil }
func (c *apiClient) Post(url string) (*http.Response, error) { return nil, nil }

func fetchUser() {
	var client *apiClient
	client.Get("/v1/users/123")
	client.Post("/v1/users")
}

// ── Form 3: Request builder ────────────────────────────────────────

func sendOrder(ctx context.Context) {
	req, _ := http.NewRequest(http.MethodPost, "/v1/orders", nil)
	_ = req
	req2, _ := http.NewRequestWithContext(ctx, "DELETE", "/v1/orders/42", nil)
	_ = req2
}

// ── Form 4: Options-bag literal ────────────────────────────────────
// Generic shape: any composite literal with Url + Method fields.

type httpOptions struct {
	Url               string
	Method            string
	ProviderShortName string
}

func fetchInventory() {
	opts := httpOptions{
		Url:               "/v1/inventory",
		Method:            http.MethodGet,
		ProviderShortName: "INVENTORY_API",
	}
	_ = opts
}

// Same options-bag form but with the URL only recoverable via
// provider tag (URL is built dynamically). The matcher should
// still emit the call with providerTag set.

type lazyOptions struct {
	Endpoint string
	Method   string
	Tag      string
}

func fetchPricing(buildURL func() string) {
	opts := lazyOptions{
		Method: "POST",
		Tag:    "pricing-svc",
	}
	_ = opts
	_ = buildURL
}

// ── Form 4 variant: path-on-config-struct ──────────────────────────
// Some Go HTTP wrappers split URL into {Host, Path} pairs. The
// generic options-bag rule recognises a literal Path field when
// it's accompanied by a Host/HostName/Scheme companion.

type apiConfig struct {
	HostName string
	Path     string
	Protocol string
}

func buildShipmentConfig() apiConfig {
	return apiConfig{
		Protocol: "https",
		HostName: "shipping.internal",
		Path:     "/v1/shipments",
	}
}

// And the negative: a struct literal with a `Path:` field but
// without any host/scheme/method companion is *not* HTTP-shaped
// and must NOT be emitted as a client call.

type fileConfig struct {
	Path string
}

func openFile() fileConfig {
	return fileConfig{Path: "/etc/foo.conf"}
}

// ── Form 5: gRPC stub ──────────────────────────────────────────────

type ordersClient struct{}

func (c *ordersClient) Place(ctx context.Context, req any) (any, error) { return nil, nil }

func NewOrdersServiceClient(_ any) *ordersClient { return &ordersClient{} }

func placeOrder(ctx context.Context, conn any) {
	NewOrdersServiceClient(conn).Place(ctx, nil)
}

// ── Form 6: Provider-tag factory ───────────────────────────────────
// Internal HTTP wrapper that hands back a pre-configured client
// keyed off a logical service tag. URL is loaded from runtime
// config so we can't recover it statically — but the tag is
// enough for the provider-tag resolver to fan the call out to
// every Route living under the matching service directory.

type providerStub struct{}

func (p *providerStub) GetClient(name string) *providerStub { return p }
func (p *providerStub) Do(ctx context.Context, req any) error { return nil }

var httpclient = &providerStub{}

func fetchByTag(ctx context.Context) {
	_ = httpclient.GetClient("kosmos")
}

// ── Form 7: URL builder (SetPath / WithPath) ───────────────────────
// Internal monorepos commonly construct outbound URLs through a
// fluent builder; the path literal sits on the SetPath call and the
// `.String()`/`.Build()` consumes it later before handing the
// result to an HTTP client. Static dataflow can't trace that hand-off
// reliably, but the SetPath literal IS reliable evidence of an
// outbound URL.

type urlBuilder struct{}

func (u *urlBuilder) SetHost(string)              {}
func (u *urlBuilder) SetPath(string)              {}
func (u *urlBuilder) WithPath(string) *urlBuilder { return u }
func (u *urlBuilder) String() string              { return "" }

func newUrlBuilder() *urlBuilder { return &urlBuilder{} }

func buildJsonAdsURL() string {
	b := newUrlBuilder()
	b.SetHost("ads.internal")
	b.SetPath("/jsonAds")
	return b.String()
}

func buildLogsURL() string {
	b := newUrlBuilder()
	return b.WithPath("/log").String()
}

func buildPipelinePath() string {
	// Non-absolute path — should NOT be recognised as an outbound URL.
	b := newUrlBuilder()
	b.SetPath("relative/path")
	return b.String()
}

// ── Form 7 variant: dynamic path via getter ────────────────────────
// The SetPath argument is not a literal — it's a value pulled from a
// getter. The extractor can't recover the path here, but it records a
// pending getter lookup so the in-code-constant resolver (Phase 3.4c)
// can chase the getter chain to a string constant downstream.

type pathProvider struct{}

func (p *pathProvider) GetPath() string { return "" }

type urlService struct {
	pathProvider *pathProvider
}

func (s *urlService) buildDynamicURL() string {
	b := newUrlBuilder()
	path := s.pathProvider.GetPath()
	b.SetPath(path)
	return b.String()
}
