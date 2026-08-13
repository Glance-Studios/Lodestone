package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/Glance-Studios/Lodestone/internal/ledger"
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
	ledger  *ledger.Ledger
	started time.Time

	// Both nil unless deploying is configured; POST /deploy 501s without them.
	packager Packager
	deployer Deployer

	// deployMu serialises deploys. Held for a whole deploy, so it is taken with
	// TryLock and a second caller is refused rather than queued.
	deployMu sync.Mutex
}

// Options are Server's dependencies. A struct rather than more positional
// arguments, because five of them was already one too many.
type Options struct {
	Version  string
	Token    string
	Store    *store.Store
	Ledger   *ledger.Ledger
	Packager Packager // optional
	Deployer Deployer // optional
}

// New returns a Server. Deploying is enabled only when both Packager and
// Deployer are supplied.
func New(opts Options) *Server {
	return &Server{
		version:  opts.Version,
		token:    opts.Token,
		store:    opts.Store,
		ledger:   opts.Ledger,
		packager: opts.Packager,
		deployer: opts.Deployer,
		started:  time.Now(),
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Public: health probes need to reach this without a token.
	mux.HandleFunc("GET /status", s.handleStatus)

	// Protected: wrapped in the auth middleware.
	requireToken := RequireToken(s.token)
	mux.Handle("POST /artifacts", requireToken(http.HandlerFunc(s.handleArtifacts)))
	mux.Handle("GET /artifacts", requireToken(http.HandlerFunc(s.handleListArtifacts)))
	mux.Handle("POST /deploy", requireToken(http.HandlerFunc(s.handleDeploy)))

	return mux
}

// handleListArtifacts returns the ledger, newest first.
func (s *Server) handleListArtifacts(w http.ResponseWriter, r *http.Request) {
	entries := s.ledger.Entries()
	if entries == nil {
		// Marshal nil as [] rather than null - the nil-vs-empty-slice trap.
		entries = []ledger.Entry{}
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(entries); err != nil {
		return
	}
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

	// Record it. The artifact bytes are already safe on disk, so a ledger
	// failure means we have an artifact we cannot account for - report it
	// rather than pretending the upload succeeded.
	entry := ledger.Entry{
		Digest:  art.Digest,
		Size:    art.Size,
		Version: r.URL.Query().Get("version"),
		By:      r.URL.Query().Get("by"),
	}
	if err := s.ledger.Append(entry); err != nil {
		http.Error(w, "recording artifact failed", http.StatusInternalServerError)
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
