package main

import (
	"github.com/example/overloaded/pkg/bar"
	"github.com/example/overloaded/pkg/foo"
)

func main() {
	// Two package-qualified calls to overloaded `New`. With the Go
	// module-alias builder enabled, `foo.New` must resolve to
	// pkg/foo/foo.go:New and `bar.New` must resolve to pkg/bar/bar.go:New.
	// Without it, the resolver would null-route both (calledName 'New' has
	// 2 tiered candidates and the global tier returns ambiguity) or pick
	// arbitrarily — the precise failure mode that broke
	// scrrmodulemanager.New in goserving.
	_ = foo.New("alice")
	_ = bar.New(42)
	_ = foo.Helper("kept-alive") // make sure sibling-file alias mapping works
}
