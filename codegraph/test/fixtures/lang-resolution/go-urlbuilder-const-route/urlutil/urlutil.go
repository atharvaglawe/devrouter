// Minimal fluent URL builder, mirroring lib/urlutil in the real
// codebase. The path passed to SetPath is dynamic (comes from a
// getter), so the extractor records a pending getter lookup.
package urlutil

type UrlBuilder struct {
	host string
	path string
}

func NewUrlBuilder() *UrlBuilder { return &UrlBuilder{} }

func (b *UrlBuilder) SetHost(host string) { b.host = host }

func (b *UrlBuilder) SetPath(path string) { b.path = path }

func (b *UrlBuilder) String() string { return b.host + b.path }
