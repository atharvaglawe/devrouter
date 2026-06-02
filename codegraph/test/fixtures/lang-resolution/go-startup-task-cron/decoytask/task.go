// A decoy task package with the SAME constructor name (GetTask) and a
// StartupRun lifecycle method, but which is NEVER registered in the task
// slice. It exists to prove the recognizer keys on the registered
// package (via the import qualifier) and does not fan out to every
// same-named GetTask / StartupRun in the repo.
package decoytask

import "cronsvc/startup"

type decoyTask struct {
	startup.DefStartupTask
}

func GetTask() *decoyTask {
	return &decoyTask{}
}

func (t *decoyTask) StartupRun() {
	// never reached from main
}
