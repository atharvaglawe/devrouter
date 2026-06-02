package health

// GetURL collides by name with pinger.GetURL — so resolving pinger.GetURL()
// REQUIRES the import alias to disambiguate (a bare single-candidate fallback
// would see two candidates and bail).
func GetURL() string {
	return "/health"
}
