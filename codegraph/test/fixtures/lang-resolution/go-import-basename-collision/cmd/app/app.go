package app

// App's type lives here; its failover methods live in failovers.go (a separate
// file), so their ownerId is keyed to the wrong file (the phantom-owner case).
type App struct{}

func (a *App) loadData() string { return "x" }
