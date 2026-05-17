// Server-side fixtures: each block illustrates one of the three
// recognised Go server-side route-registration forms.
// All identifiers are generic — no real repo / framework names.
package server

import (
	"net/http"
)

// ── Form 1: stdlib Handle* ─────────────────────────────────────────
// mux.HandleFunc(path, handler) — single fixed pair, any HTTP method.

func registerStdlibRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/health", healthHandler)
	mux.Handle("/static/", staticHandler)
}

func healthHandler(w http.ResponseWriter, r *http.Request)  {}
func staticHandler() http.Handler                           { return nil }

// ── Form 2: Verb-as-method ─────────────────────────────────────────
// router.GET(path, handler) — gin / echo / chi convention.

type Router struct{}

func (rt *Router) GET(path string, h func())    {}
func (rt *Router) POST(path string, h func())   {}
func (rt *Router) DELETE(path string, h func()) {}

func registerVerbRoutes(rt *Router) {
	rt.GET("/users", listUsers)
	rt.POST("/users", createUser)
	rt.DELETE("/users/:id", deleteUser)
}

func listUsers()   {}
func createUser()  {}
func deleteUser()  {}

// ── Form 3: Tagged register ────────────────────────────────────────
// router.AddRoute(method, []string{path…}, handler).

type TaggedRouter struct{}

func (tr *TaggedRouter) AddRoute(method string, paths []string, h func()) {}
func (tr *TaggedRouter) Group(prefix string) *TaggedRouter             { return tr }
func (tr *TaggedRouter) NewGroup(prefix string) *TaggedRouter          { return tr }

func registerTaggedRoutes(tr *TaggedRouter) {
	tr.AddRoute(http.MethodPost, []string{"/orders"}, createOrder)
	tr.AddRoute(http.MethodGet, []string{"/orders", "/orders/list"}, listOrders)
}

func createOrder() {}
func listOrders()  {}

// ── Group-prefix tracking ──────────────────────────────────────────
// Routes registered on a group variable inherit the group's prefix.
// Generic across gin / echo / chi / custom routers — we recognise
// `<recv>.Group(prefix, …)` / `NewGroup` / `Subrouter` /
// `WithPrefix` / `PathPrefix` as factory methods.

func registerGroupedRoutes(rt *Router, tr *TaggedRouter) {
	v1 := rt.Group("/v1", nil)
	v1.GET("/items", listItems)

	api := tr.NewGroup("/api")
	api.AddRoute(http.MethodPost, []string{"/payments"}, makePayment)
}

func (rt *Router) Group(prefix string, _ any) *Router { return rt }
func listItems()    {}
func makePayment()  {}

// ── Transitive group prefixes ──────────────────────────────────────
// v2 is built off api: full prefix should be /api/v2.

func registerNestedGroupedRoutes(tr *TaggedRouter) {
	api := tr.NewGroup("/api")
	v2 := api.NewGroup("/v2")
	v2.AddRoute(http.MethodGet, []string{"/health"}, healthHandler2)
}

func healthHandler2() {}
