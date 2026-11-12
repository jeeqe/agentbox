// Package server exposes the local AgentBox dashboard over net/http.
package server

import (
	"io/fs"
	"net/http"

	"github.com/jeeqe/agentbox/internal/app"
	"github.com/jeeqe/agentbox/internal/ui"
)

// server keeps the HTTP-facing concerns around one application instance.
// App owns all mutation and process lifecycle behaviour; handlers only turn
// HTTP requests into calls to that service layer.
type server struct {
	app    *app.App
	static http.Handler
}

// New returns the dashboard handler for a single AgentBox application.
func New(a *app.App) http.Handler {
	staticFiles, err := fs.Sub(ui.StaticFS, "static")
	var static http.Handler = http.NotFoundHandler()
	if err == nil {
		static = http.StripPrefix("/static/", http.FileServer(http.FS(staticFiles)))
	}

	s := &server{app: a, static: static}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.dashboard)
	mux.HandleFunc("GET /runs/{id}", s.run)
	mux.HandleFunc("GET /tasks/{id}/stream", s.stream)
	mux.HandleFunc("GET /tasks/{id}/diff", s.diff)
	mux.HandleFunc("POST /tasks/{id}/merge", s.merge)
	mux.HandleFunc("POST /tasks/{id}/kill", s.kill)
	mux.HandleFunc("POST /tasks/{id}/restart", s.restart)
	mux.HandleFunc("POST /tasks/{id}/discard", s.discard)
	mux.HandleFunc("POST /tasks/{id}/rebase", s.rebase)
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.Handle("GET /static/{path...}", static)
	return mux
}
