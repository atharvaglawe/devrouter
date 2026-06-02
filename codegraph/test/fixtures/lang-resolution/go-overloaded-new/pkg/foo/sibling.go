package foo

// Helper lives in a sibling file inside package foo. Exists to validate
// that the Go module-alias builder maps `foo` to the SET of every .go
// file under the foo/ directory — not just one of them. Nothing here
// should be matched by a `foo.New(...)` call from the caller, but the
// resolver must still see this file as part of `foo`'s alias set so
// later sibling-defined symbols (e.g., `foo.Helper(...)`) resolve too.
func Helper(s string) string {
	return s
}
