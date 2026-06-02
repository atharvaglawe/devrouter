package foo

type Foo struct {
	Name string
}

// New is intentionally overloaded with bar.New to test that the
// package-qualified resolver picks the right one. Defined in the
// package's primary file.
func New(name string) *Foo {
	return &Foo{Name: name}
}
