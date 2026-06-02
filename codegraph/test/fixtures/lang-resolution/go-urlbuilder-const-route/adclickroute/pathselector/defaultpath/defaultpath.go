package defaultpath

import "urlbuildersvc/adclickroute/constant"

type defaultPath struct{}

func (d *defaultPath) GetPath() string {
	return constant.DefaultPath
}

func New() *defaultPath { return &defaultPath{} }
