// Package directory is "pinger2" but the declared package name is "pinger".
// Callers import it with an explicit alias: pinger "…/internal/pinger2".
package pinger

func GetURL() string {
	return "/ping"
}
