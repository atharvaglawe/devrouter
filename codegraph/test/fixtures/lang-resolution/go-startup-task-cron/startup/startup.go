// Minimal mirror of lib/startup: a background-task framework that
// dispatches StartupRun/PeriodicRun on each registered task through the
// StartupTaskInterface. The dispatch is the interface hop the CALLS
// graph cannot follow — the recognizer bridges registration → lifecycle.
package startup

type StartupTaskInterface interface {
	StartupRun()
	PeriodicRun()
	GetIntervalSec() int
}

// DefStartupTask provides no-op lifecycle defaults that concrete tasks
// embed and selectively override.
type DefStartupTask struct{}

func (DefStartupTask) StartupRun()       {}
func (DefStartupTask) PeriodicRun()      {}
func (DefStartupTask) GetIntervalSec() int { return 0 }

// RegisterTasks invokes each task's lifecycle methods. The invocation is
// dynamic (through the interface), so static call resolution sees only
// DefStartupTask here, never the concrete implementations.
func RegisterTasks(tasks []StartupTaskInterface) {
	for _, t := range tasks {
		t.StartupRun()
		t.PeriodicRun()
	}
}
