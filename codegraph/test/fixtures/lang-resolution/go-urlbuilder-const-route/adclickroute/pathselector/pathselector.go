package pathselector

import (
	"urlbuildersvc/adclickroute/contract"
	"urlbuildersvc/adclickroute/pathselector/defaultpath"
)

type PathSelector struct{}

func (p *PathSelector) DefaultPath() contract.IPathSelector {
	return defaultpath.New()
}

func New() *PathSelector { return &PathSelector{} }
