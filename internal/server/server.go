package server

import (
	"encoding/json"
	"net/http"
	"time"
)

// Server holds the API's deps & state
type Server struct {
	version string
	started time.Time
}

// New returns a Server stamped with running version
func New(version string) *Server {
	return &Server{
		version: version,
		started: time.Now(),
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", s.handleStatus)
	return mux
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
