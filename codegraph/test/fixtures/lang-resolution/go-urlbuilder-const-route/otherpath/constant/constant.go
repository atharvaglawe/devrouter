package constant

// A second, unrelated GetPath constant that does NOT name a
// registered route. Exercises the resolver's multi-candidate path:
// `GetPath` resolves (by name) to both "/trf" and this value, and
// Phase 3.4c must prefer the candidate that matches a real route.
const OtherPath = "/notroute"
