// Mirrors oscar/app/pkg/clickurl: builds a click URL whose path is
// pulled dynamically from adClickRouteService.GetPath(). The path
// literal is never written here — it lives behind a getter +
// interface-field + constant chain that the resolver recovers.
package clickurl

import (
	"urlbuildersvc/adclickroute"
	"urlbuildersvc/urlutil"
)

type ClickUrl struct {
	adClickRouteService *adclickroute.AdClickRoute
}

func (c *ClickUrl) getUrl() string {
	host := c.adClickRouteService.GetHost()
	path := c.adClickRouteService.GetPath()

	urlBuilder := urlutil.NewUrlBuilder()
	urlBuilder.SetHost(host)
	urlBuilder.SetPath(path)
	return urlBuilder.String()
}

func New(svc *adclickroute.AdClickRoute) *ClickUrl {
	return &ClickUrl{adClickRouteService: svc}
}
