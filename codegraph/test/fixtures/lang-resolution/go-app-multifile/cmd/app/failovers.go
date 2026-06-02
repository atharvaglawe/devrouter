package app

import (
	"example.com/scs/internal/wrapper"
	"strconv"
)

// Mirrors failovers.go: named multiple returns, params referencing other
// packages, intra-package method calls, and a package-qualified sibling call.
func (a *App) getDetailsAndLog(flag bool) (out string, applicable bool) {
	out = a.loadData()
	return out, flag
}

func (a *App) GetCurlTimeoutData(n int) *wrapper.DataStore {
	label := strconv.Itoa(n)
	a.getDetailsAndLog(true)
	return wrapper.New(label)
}
