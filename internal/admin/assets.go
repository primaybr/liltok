package admin

import (
	"io"
	"net/http"

	"github.com/primaybr/liltok/web"
)

// ServeDashboard serves the embedded Single Page Application (SPA) dashboard.
func ServeDashboard(w http.ResponseWriter, r *http.Request) {
	file, err := web.DistFS.Open("index.html")
	if err != nil {
		http.Error(w, "Dashboard asset not found", http.StatusNotFound)
		return
	}
	defer file.Close()

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, file)
}
