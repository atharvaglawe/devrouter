package bar

type Bar struct {
	ID int
}

// New shares its name with foo.New. The package-qualified call site
// `bar.New(...)` must land here, never on foo.New.
func New(id int) *Bar {
	return &Bar{ID: id}
}
