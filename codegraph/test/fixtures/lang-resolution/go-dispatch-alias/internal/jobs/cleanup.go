package jobs

// CleanupJob structurally satisfies runner.Task (same method set: Execute()).
type CleanupJob struct{}

func NewCleanup() *CleanupJob {
	return &CleanupJob{}
}

func (c *CleanupJob) Execute() error {
	return nil
}
