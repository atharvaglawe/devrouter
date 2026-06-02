package failover

// Stands in for cmpkg/failover: import path basename is "failover".
type FailoverDetails struct {
	ReasonId int
	ActionId int
}

type Failover struct{}

func (f Failover) GetFailoverDetails() (*FailoverDetails, bool) { return nil, false }

func (f Failover) EnableDebugging(a interface{}) {}

func GetForcedFailover(reasonId int) Failover { return Failover{} }
