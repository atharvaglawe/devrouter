package failover

// Homonym of svc/failover.GetWafFailover in a different package — makes the
// bare name ambiguous across the repo.
func GetWafFailover() string {
	return "other"
}
