// Server side: registers the /trf click-transfer route. The
// downstream matcher should connect the click-URL builder
// (clickurl.getUrl, which resolves its path to "/trf" through the
// getter + constant chain) to this Route node via FETCHES.
package router

import "net/http"

func Register(mux *http.ServeMux) {
	mux.HandleFunc("/trf", handleTransfer)
}

func handleTransfer(w http.ResponseWriter, r *http.Request) {}
