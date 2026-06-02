package main

import (
	commonfailover "github.com/example/multi/internal/common/failover"
	"github.com/example/multi/internal/svc/failover"
)

func main() {
	// Plain package-qualified call. Receiver `failover` shares its directory
	// basename with the aliased common/failover import. Must resolve to
	// svc/failover.GetWafFailover, never other/failover.GetWafFailover.
	_ = failover.GetWafFailover()
	_ = commonfailover.Cleanup()
}
