// Binary entrypoint. main → initialiseStartupTasks → getStartupTasks,
// which builds the []startup.StartupTaskInterface slice naming the
// concrete cron task. The framework then dispatches the task's
// StartupRun/PeriodicRun via the interface. Without the recognizer,
// the task's lifecycle work (and the /refresh fetch) is orphaned from
// main; with it, main reaches refreshtask.StartupRun → refresh →
// FETCHES /refresh.
package main

import (
	"cronsvc/refreshtask"
	"cronsvc/startup"
)

func main() {
	initialiseStartupTasks()
}

func initialiseStartupTasks() {
	startup.RegisterTasks(getStartupTasks())
}

func getStartupTasks() []startup.StartupTaskInterface {
	return []startup.StartupTaskInterface{
		refreshtask.GetTask(),
	}
}
