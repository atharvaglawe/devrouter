// Minimal fluent URL builder, mirroring lib/urlutil.
package urlutil

type UrlBuilder struct {
	host string
	path string
}

func NewUrlBuilder() *UrlBuilder { return &UrlBuilder{} }

func (b *UrlBuilder) SetHost(host string) { b.host = host }

func (b *UrlBuilder) SetPath(path string) { b.path = path }

func (b *UrlBuilder) String() string { return b.host + b.path }
