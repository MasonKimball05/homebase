// Package web serves the homebase dashboard and its JSON API.
package web

import (
	"embed"
	"encoding/json"
	"io/fs"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/MasonKimball05/homebase/internal/supervisor"
)

//go:embed static
var staticFiles embed.FS

type Server struct {
	mgr     *supervisor.Manager
	host    string
	started time.Time
}

func New(mgr *supervisor.Manager) *Server {
	host, _ := os.Hostname()
	return &Server{mgr: mgr, host: host, started: time.Now()}
}

func (s *Server) Handler() http.Handler {
	static, _ := fs.Sub(staticFiles, "static")
	mux := http.NewServeMux()
	mux.Handle("GET /", http.FileServerFS(static))
	mux.HandleFunc("GET /api/status", s.status)
	mux.HandleFunc("GET /api/apps/{name}/logs", s.logs)
	mux.HandleFunc("POST /api/apps/{name}/{action}", s.act)
	return securityHeaders(mux)
}

type statusView struct {
	Host          string              `json:"host"`
	UptimeSeconds int64               `json:"uptime_seconds"`
	Apps          []supervisor.Status `json:"apps"`
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, statusView{
		Host:          s.host,
		UptimeSeconds: int64(time.Since(s.started).Seconds()),
		Apps:          s.mgr.Statuses(),
	})
}

// logs returns lines newer than ?after=<seq>, for polling.
func (s *Server) logs(w http.ResponseWriter, r *http.Request) {
	app, ok := s.mgr.App(r.PathValue("name"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	writeJSON(w, http.StatusOK, app.Logs.Since(after))
}

func (s *Server) act(w http.ResponseWriter, r *http.Request) {
	// These buttons start and stop programs, so refuse anything that isn't
	// the dashboard itself. Browsers can't add a custom header to a
	// cross-site request without a CORS preflight, which this server never
	// approves, so requiring one blocks other websites from posting here.
	if r.Header.Get("X-Homebase") != "1" || !sameSite(r) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "request not from the dashboard"})
		return
	}
	app, ok := s.mgr.App(r.PathValue("name"))
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such app"})
		return
	}
	if err := app.Do(r.PathValue("action")); err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusAccepted, app.Status())
}

func sameSite(r *http.Request) bool {
	site := r.Header.Get("Sec-Fetch-Site")
	return site == "" || site == "same-origin" // "" = not a browser (curl, scripts)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; frame-ancestors 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}
