package proxy

import (
	"bytes"
	"embed"
	"net/http"
	"strings"
)

//go:embed ui/index.html
var uiFS embed.FS

// serveUI serves the console with the management token embedded. Other
// origins cannot read this page, so the token stays local.
func (s *Server) serveUI(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/ui/" && r.URL.Path != "/ui/index.html" {
		http.NotFound(w, r)
		return
	}
	page, _ := uiFS.ReadFile("ui/index.html")
	page = bytes.Replace(page, []byte("__AGB_TOKEN__"), []byte(s.store.Get().APIToken), 1)
	page = bytes.Replace(page, []byte("__AGB_VERSION__"), []byte(strings.TrimSpace(s.Version)), 1)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-inline'; img-src 'self' data:")
	_, _ = w.Write(page)
}
