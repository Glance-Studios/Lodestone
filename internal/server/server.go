package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/Glance-Studios/Lodestone/internal/store"
)

// maxUploadBytes caps an artifact upload. Paper plugin jars are single-digit MB;
// 256 MiB is generous and still bounds what one request can cost us.
const maxUploadBytes = 256 << 20

// Server holds the API's deps & state
type Server struct {
	version string
	token   string
	store   *store.Store
	started time.Time
}

// New returns a Server stamped with running version and the token that guards
// protected endpoints.
func New(version, token string, st *store.Store) *Server {
	return &Server{
		version: version,
		token:   token,
		store:   st,
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

// handleArtifacts accepts an uploaded artifact, stores it by content digest and
// reports what was stored.
func (s *Server) handleArtifacts(w http.ResponseWriter, r *http.Request) {
	// Bound the request before reading a byte of it, so an oversized upload
	// costs us the limit rather than the whole body.
	body := http.MaxBytesReader(w, r.Body, maxUploadBytes)
	defer body.Close()

	art, err := s.store.Put(body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			http.Error(w, "artifact too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "storing artifact failed", http.StatusInternalServerError)
		return
	}

	if art.Size == 0 {
		http.Error(w, "empty artifact", http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	if err := json.NewEncoder(w).Encode(art); err != nil {
		return // response already begun; nothing useful left to say
	}
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
