package server

import (
	"encoding/json"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/Glance-Studios/Lodestone/internal/api"
	"github.com/Glance-Studios/Lodestone/internal/ledger"
	"github.com/Glance-Studios/Lodestone/internal/store"
	"github.com/Glance-Studios/Lodestone/internal/target"
)

// maxUploadBytes caps an artifact upload. Paper plugin jars are single-digit MB;
// 256 MiB is generous and still bounds what one request can cost us.
const maxUploadBytes = 256 << 20

// TargetSpec is everything the server needs to serve one target. Built by main,
// which owns the Kubernetes client and the registry credentials.
type TargetSpec struct {
	Config   target.Target
	Packager Packager
	Deployer Deployer
	Ledger   *ledger.Ledger

	// Deleter prunes image manifests. Optional: without it the ledger and
	// artifact store are still pruned, and manifests are left alone.
	Deleter Deleter
}

// targetState adds the server's own per-target state to a spec.
type targetState struct {
	TargetSpec

	// mu serialises deploys to this target and only this target. Interleaved
	// deploys to one Deployment corrupt each other's rollback; deploys to
	// different Deployments have no reason to wait for one another, and a global
	// lock would make one developer queue behind another's ten-minute rollout.
	mu sync.Mutex
}

// Server holds the API's deps & state.
type Server struct {
	version string
	store   *store.Store
	started time.Time

	// targets is fixed at construction: Lodestone addresses targets, it does not
	// create or destroy them. Nothing mutates this map, so it needs no lock.
	targets map[string]*targetState
}

// Options are Server's dependencies.
type Options struct {
	Version string
	Store   *store.Store

	// Targets keyed by name. Empty is allowed - the agent then serves /status
	// and nothing else, which is a useful state for a fresh install.
	Targets map[string]TargetSpec
}

// New returns a Server.
func New(opts Options) *Server {
	targets := make(map[string]*targetState, len(opts.Targets))
	for name, spec := range opts.Targets {
		targets[name] = &targetState{TargetSpec: spec}
	}

	return &Server{
		version: opts.Version,
		store:   opts.Store,
		targets: targets,
		started: time.Now(),
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Public: health probes need to reach this without a token.
	mux.HandleFunc("GET /status", s.handleStatus)

	// Everything else is addressed by target and guarded by that target's token.
	// {target} is a Go 1.22 path wildcard, read back with r.PathValue.
	mux.Handle("POST /deploy/{target}", s.requireTargetToken(s.handleDeploy))
	mux.Handle("POST /artifacts/{target}", s.requireTargetToken(s.handleUpload))
	mux.Handle("GET /artifacts/{target}", s.requireTargetToken(s.handleListArtifacts))

	return mux
}

// TargetNames returns the configured target names, sorted. For logging.
func (s *Server) TargetNames() []string {
	out := make([]string, 0, len(s.targets))
	for name := range s.targets {
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}

// handleListArtifacts returns one target's ledger, newest first.
func (s *Server) handleListArtifacts(w http.ResponseWriter, r *http.Request, t *targetState, _ string) {
	entries := t.Ledger.Entries()

	out := make([]api.LedgerEntry, 0, len(entries))
	for _, e := range entries {
		out = append(out, api.LedgerEntry{
			Seq:           e.Seq,
			Digest:        e.Digest,
			Size:          e.Size,
			Target:        e.Target,
			Version:       e.Version,
			By:            e.By,
			Authenticated: e.Authenticated,
			At:            e.At,
			Replicas:      e.Replicas,
			Deployed:      e.Deployed,
			Image:         e.Image,
			BaseImage:     e.BaseImage,
		})
	}

	// A nil slice marshals to null; an empty one to [], which is what a client
	// iterating the response needs.
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(out); err != nil {
		return
	}
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	// api.Status, not a local copy: one definition means the CLI cannot drift
	// from what the server actually sends.
	body := api.Status{
		Status:  "ok",
		Version: s.version,
		Uptime:  time.Since(s.started).Round(time.Second).String(),
		Targets: s.TargetNames(),
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(body); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
