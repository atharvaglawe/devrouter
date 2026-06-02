package runner

// Task is an interface with no concrete implementor declared via any
// `implements` keyword (Go has none) — implementors are matched structurally.
type Task interface {
	Execute() error
}

type Runner struct {
	tasks []Task
}

func New() *Runner {
	return &Runner{}
}

func (r *Runner) Register(t Task) {
	r.tasks = append(r.tasks, t)
}

// runOne calls an interface-typed receiver. The primary CALLS edge lands on
// Task.Execute; interface-dispatch should add a CALLS edge to every concrete
// implementor's Execute (e.g. CleanupJob.Execute).
func (r *Runner) runOne(t Task) {
	_ = t.Execute()
}
