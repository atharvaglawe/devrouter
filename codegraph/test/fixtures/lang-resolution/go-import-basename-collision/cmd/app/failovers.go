package app

import (
	cmpkgfailover "example.com/scs/cmpkg/failover"
	"example.com/scs/pkg/failover"
)

// Faithful mirror of smartcacheserving/.../cmd/smartcache/failovers.go: imports
// BOTH an aliased failover (cmpkgfailover) and an unaliased failover whose
// import-path basename also collides. All calls below — package-qualified
// (failover.GetWafFailover) and intra-package receiver calls (a.method) — must
// still resolve despite the colliding basename.
func (a *App) getFailoverDetailsAndLog(o cmpkgfailover.Failover) (d *cmpkgfailover.FailoverDetails, ok bool) {
	a.loadData()
	return o.GetFailoverDetails()
}

func (a *App) GetWafBasedFailoverDetails() (d *cmpkgfailover.FailoverDetails, applicable bool) {
	obj := failover.GetWafFailover()
	obj.EnableDebugging(a)
	return a.getFailoverDetailsAndLog(obj)
}
