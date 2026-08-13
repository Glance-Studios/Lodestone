package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/Glance-Studios/Lodestone/internal/api"
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

// handleDeploy accepts an artifact, packages it into an image, pushes it, and
// rolls the target Deployment onto it - the whole pipeline in one request.
//
// Only one deploy runs at a time. Interleaved deploys to one Deployment corrupt
// each other: if A replaces X with B, then C replaces B with D, and A's rollout
// then fails, A rolls back to X - silently destroying C's healthy deploy. The
// rollback is correct in isolation and wrong in company, so the fix is to refuse
// the overlap rather than to reason about it.
func (s *Server) handleDeploy(w http.ResponseWriter, r *http.Request) {
	if s.packager == nil || s.deployer == nil {
		http.Error(w, "deploying is not configured", http.StatusNotImplemented)
		return
	}

	// TryLock rather than Lock: a caller learns immediately that a deploy is in
	// flight instead of hanging for minutes behind one.
	if !s.deployMu.TryLock() {
		w.Header().Set("Retry-After", "30")
		http.Error(w, "a deploy is already in progress", http.StatusLocked)
		return
	}
	defer s.deployMu.Unlock()

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

	// Everything above can still use a real status code, because no byte of the
	// body has been written yet.
	built, err := s.packager.Package(r.Context(), f)
	if err != nil {
		writeResult(w, http.StatusBadGateway, api.Result{
			Digest: art.Digest,
			Error:  "packaging or pushing the image failed: " + err.Error(),
		})
		return
	}

	events := s.deployer(r.Context(), built.Ref)

	if wantsStream(r) {
		streamDeploy(w, art.Digest, built.Ref, events)
		return
	}

	res := rollout.Collect(events)
	out := api.Result{
		Digest:   art.Digest,
		Image:    built.Ref,
		Deployed: res.Succeeded(),
		Events:   toAPIEvents(res.Events),
	}

	status := http.StatusOK
	if !res.Succeeded() {
		out.Error = res.Err.Error()
		// The deploy was rejected and rolled back. That is the agent working
		// correctly, but the caller's artifact is not live - so not a 2xx.
		status = http.StatusConflict
	}
	writeResult(w, status, out)
}

// wantsStream reports whether the client asked for a newline-delimited stream.
func wantsStream(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), api.ContentTypeNDJSON)
}

// streamDeploy writes one JSON object per line as the rollout progresses, then a
// final result line.
//
// It always responds 200. A status code is sent with the first byte of the body
// and cannot be revised afterwards, so a streamed deploy cannot report failure
// that way - the caller reads the final line instead.
func streamDeploy(w http.ResponseWriter, digest, imageRef string, events <-chan rollout.Event) {
	w.Header().Set("Content-Type", api.ContentTypeNDJSON)
	// Ask intermediaries not to buffer; a proxy holding the response defeats the
	// point of streaming it.
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	enc := json.NewEncoder(w) // Encode writes a trailing newline, which is the framing
	flusher, canFlush := w.(http.Flusher)

	flush := func() {
		if canFlush {
			// Without this the bytes sit in Go's response buffer until it fills
			// or the handler returns, so the client sees nothing until the end.
			flusher.Flush()
		}
	}

	res := rollout.CollectFunc(events, func(e rollout.Event) error {
		if err := enc.Encode(toAPIEvent(e)); err != nil {
			return err // client hung up; CollectFunc keeps draining
		}
		flush()
		return nil
	})

	final := api.Result{
		Kind:     api.KindResult,
		Digest:   digest,
		Image:    imageRef,
		Deployed: res.Succeeded(),
	}
	if !res.Succeeded() {
		final.Error = res.Err.Error()
	}

	_ = enc.Encode(final)
	flush()
}

func toAPIEvent(e rollout.Event) api.Event {
	return api.Event{
		Kind:    api.KindEvent,
		Phase:   string(e.Phase),
		Message: e.Message,
		At:      e.At,
		Error:   e.Err,
	}
}

func toAPIEvents(in []rollout.Event) []api.Event {
	out := make([]api.Event, 0, len(in))
	for _, e := range in {
		out = append(out, toAPIEvent(e))
	}
	return out
}

func writeResult(w http.ResponseWriter, status int, body api.Result) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
