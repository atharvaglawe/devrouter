package failover

// Same directory basename ("failover") as svc/failover, imported into the same
// caller file under an explicit alias. This collides on the directory-basename
// alias key.
func Cleanup() string {
	return "common"
}
