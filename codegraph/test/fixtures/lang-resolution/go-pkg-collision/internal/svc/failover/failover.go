package failover

// GetWafFailover is also defined in internal/other/failover (same name,
// different package) — so the call is globally ambiguous and resolution must
// use the import to pick THIS one.
func GetWafFailover() string {
	return "svc"
}
