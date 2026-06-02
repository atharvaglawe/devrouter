package failover

import cmfo "example.com/scs/cmpkg/failover"

// A THIRD package also named `failover` (mirrors the mega-repo, where oscar,
// ola, cmpkg and scs each ship a `package failover`). Makes the bare package
// name `failover` resolve to multiple packages globally.
func GetWafFailover() cmfo.Failover { return cmfo.Failover{} }

func Unused() {}
