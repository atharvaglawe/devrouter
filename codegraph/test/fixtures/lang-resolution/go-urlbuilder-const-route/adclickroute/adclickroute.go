// Mirrors oscar/app/pkg/adclickroute: GetPath delegates to an
// interface field (pathSelector), whose concrete implementation
// returns a package constant.
package adclickroute

import (
	"urlbuildersvc/adclickroute/contract"
	"urlbuildersvc/adclickroute/pathselector"
)

type AdClickRoute struct {
	pathSelector contract.IPathSelector
}

func (a *AdClickRoute) GetHost() string {
	return "ads.example.internal"
}

func (a *AdClickRoute) GetPath() string {
	return a.pathSelector.GetPath()
}

func (a *AdClickRoute) init() {
	a.pathSelector = a.getPathSelector()
}

func (a *AdClickRoute) getPathSelector() contract.IPathSelector {
	return pathselector.New().DefaultPath()
}

func New() *AdClickRoute {
	a := &AdClickRoute{}
	a.init()
	return a
}
