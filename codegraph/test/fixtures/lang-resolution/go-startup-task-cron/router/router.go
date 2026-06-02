// Server side: registers the /refresh route the cron task fetches.
package router

import "net/http"

func Register(mux *http.ServeMux) {
	mux.HandleFunc("/refresh", handleRefresh)
}

func handleRefresh(w http.ResponseWriter, r *http.Request) {}
