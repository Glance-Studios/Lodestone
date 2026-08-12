package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/Glance-Studios/Lodestone/internal/image"
	"github.com/Glance-Studios/Lodestone/internal/ledger"
	"github.com/Glance-Studios/Lodestone/internal/rollout"
)

// Packager turns an artifact into a pushed image. Declared here, by the
// consumer, so the server depends on one method rather than on image.Packager.
type Packager interface {
	Package(ctx context.Context, r io.Reader) (image.Built, error)
}

// Deployer runs a health-gated rollout. rollout.Deploy satisfies this as a
// function value, which is why the field is a func rather than an interface.
type Deployer func(ctx context.Context, digest string) <-chan rollout.Event

// DeployResponse is the JSON body returned by POST /deploy.
type DeployResponse struct {
	Digest   string          `json:"digest"`   // the artifact's sha256
	Image    string          `json:"image"`    // the pushed image reference
	Deployed bool            `json:"deployed"` // did the rollout succeed
	Events   []rollout.Event `json:"events"`
	Error    string          `json:"error,omitempty"`
}

// handleDeploy accepts an artifact, packages it into an image, pushes it, and
// rolls the target Deployment onto it - the whole pipeline in one request.
func (s *Server) handleDeploy(w http.ResponseWriter, r *http.Request) {
	if s.packager == nil || s.deployer == nil {
		http.Error(w, "deploying is not configured", http.StatusNotImplemented)
		return
	}

	body := http.MaxBytesReader(w, r.Body, maxUploadBytes)
	defer body.Close()

	// Store first: the artifact is recorded and durable before anything is
	// pushed anywhere, so a failed deploy still leaves an auditable upload.
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

	// Re-open the stored artifact rather than buffering the upload in memory.
	f, err := s.store.Open(art.Digest)
	if err != nil {
		http.Error(w, "reopening artifact failed", http.StatusInternalServerError)
		return
	}
	defer f.Close()

	built, err := s.packager.Package(r.Context(), f)
	if err != nil {
		writeDeployResult(w, http.StatusBadGateway, DeployResponse{
			Digest: art.Digest,
			Error:  "packaging or pushing the image failed: " + err.Error(),
		})
		return
	}

	res := rollout.Collect(s.deployer(r.Context(), built.Ref))

	out := DeployResponse{
		Digest:   art.Digest,
		Image:    built.Ref,
		Deployed: res.Succeeded(),
		Events:   res.Events,
	}
	status := http.StatusOK
	if !res.Succeeded() {
		out.Error = res.Err.Error()
		// The deploy was rejected and rolled back. That is the agent working
		// correctly, but the caller's artifact is not live - so not a 2xx.
		status = http.StatusConflict
	}
	writeDeployResult(w, status, out)
}

func writeDeployResult(w http.ResponseWriter, status int, body DeployResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
