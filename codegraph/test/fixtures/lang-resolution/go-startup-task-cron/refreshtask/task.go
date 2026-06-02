// A concrete cron task: GetTask constructs it, and StartupRun/PeriodicRun
// (the overridden lifecycle methods) fetch the /refresh route. Both the
// constructor and the lifecycle methods live in this same package — the
// co-location the recognizer relies on.
package refreshtask

import (
	"cronsvc/startup"
	"cronsvc/urlutil"
)

type refreshTask struct {
	startup.DefStartupTask
}

func GetTask() *refreshTask {
	return &refreshTask{}
}

func (t *refreshTask) StartupRun() {
	t.refresh()
}

func (t *refreshTask) PeriodicRun() {
	t.refresh()
}

func (t *refreshTask) refresh() {
	b := urlutil.NewUrlBuilder()
	b.SetPath("/refresh")
	_ = b.String()
}
