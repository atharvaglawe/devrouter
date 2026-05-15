// This sentinel go.mod exists only so devrouter's Go tooling (`go build ./...`,
// `go vet ./...`, `go test ./...`) does NOT descend into this directory. The
// codegraph subtree is TypeScript/Node — it ships its own package.json and is
// built via `npm run build` (or `make codegraph-build`).
//
// The test/fixtures/ tree contains intentionally malformed Go source code
// used as parser input by codegraph's tree-sitter ingestion. Including it in
// devrouter's Go build graph trips errors like "package X is not in std".
//
// A trailing `_devrouter_skip` suffix on the module path documents the
// intent (this is not a real importable Go module).
module codegraph_devrouter_skip

go 1.21
