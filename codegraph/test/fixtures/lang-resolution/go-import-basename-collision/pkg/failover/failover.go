package failover

import cmfo "example.com/scs/cmpkg/failover"

// Stands in for smartcacheserving/app/pkg/failover: import path basename is also
// "failover", colliding with cmpkg/failover's basename.
func GetWafFailover() cmfo.Failover { return cmfo.Failover{} }
