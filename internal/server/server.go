package server

import (
	"encoding/json"
	"net/http"
	"time"
)

// Server holds the API's deps & state
type Server struct {
	version string
	token   string
	started time.Time
}

// New returns a Server stamped with running version and the token that guards
// protected endpoints.
func New(version, token string) *Server {
	return &Server{
		version: version,
		token:   token,
		started: time.Now(),
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Public: health probes need to reach this without a token.
	mux.HandleFunc("GET /status", s.handleStatus)

	// Protected: wrapped in the auth middleware.
	requireToken := RequireToken(s.token)
	mux.Handle("POST /artifacts", requireToken(http.HandlerFunc(s.handleArtifacts)))

	return mux
}

// handleArtifacts will accept an uploaded artifact. Stubbed until step 4.
func (s *Server) handleArtifacts(w http.ResponseWriter, r *http.Request) {
	http.Error(w, "not implemented", http.StatusNotImplemented)
}

// Status is the JSON body returned by GET /status
type Status struct {
	Status  string `json:"status"`
	Version string `json:"version"`
	Uptime  string `json:"uptime"`
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	body := Status{
		Status:  "ok",
		Version: s.version,
		Uptime:  time.Since(s.started).Round(time.Second).String(),
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(body); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
