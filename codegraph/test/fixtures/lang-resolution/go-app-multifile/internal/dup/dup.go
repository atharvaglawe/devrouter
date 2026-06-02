package dup

// Homonym of cmd/app.App in a different package/directory. This makes the bare
// type name `App` ambiguous across the repo (mirrors the mega-repo where ~24
// packages each declare their own `App`). Receiver typing + owner resolution
// must still resolve cmd/app.App methods correctly despite the homonym.
type App struct{}

func (a *App) loadData() string { return "dup" }

func (a *App) getDetailsAndLog(flag bool) (string, bool) { return "dup", flag }

func (a *App) GetCurlTimeoutData(n int) string {
	a.getDetailsAndLog(true)
	return a.loadData()
}
