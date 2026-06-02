// Mirrors the goserving cmorigin package: a Request struct with a
// Path field that the New constructor copies into a per-call
// HTTP client. The path itself comes from the caller's config —
// no string literals here.
package cmorigin

type Request struct {
	Protocol string
	Host     string
	Port     string
	Path     string
}

func NewScrr(r *Request) *Request {
	return r
}
