package app

// App is declared here; its methods are spread across sibling files in the
// same package (app.go, flows.go, failovers.go) — mirroring the real
// smartcacheserving/app/cmd/smartcache layout.
type App struct {
	name string
}

func New() *App {
	return &App{}
}

func (a *App) loadData() string {
	return a.name
}
