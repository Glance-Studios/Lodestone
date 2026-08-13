package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Glance-Studios/Lodestone/internal/api"
	"github.com/Glance-Studios/Lodestone/internal/image"
	"github.com/Glance-Studios/Lodestone/internal/ledger"
	"github.com/Glance-Studios/Lodestone/internal/rollout"
	"github.com/Glance-Studios/Lodestone/internal/store"
)

// Packager turns an artifact into a pushed image. Declared here, by the
// consumer, so the server depends on one method rather than on image.Packager.
type Packager interface {
	Package(ctx context.Context, r io.Reader) (image.Built, error)
}

// Deployer runs a health-gated rollout. Replicas is nil to leave the count alone.
type Deployer func(ctx context.Context, imageRef string, replicas *int32) <-chan rollout.Event

// handleUpload stores and records an artifact without deploying it.
func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request, t *targetState, by string) {
	art, _, ok := s.receive(w, r, t, nil, by)
	if !ok {
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(api.Artifact{Digest: art.Digest, Size: art.Size})
}

// handleDeploy accepts an artifact, packages it into an image, pushes it, and
// rolls the target onto it - the whole pipeline in one request.
func (s *Server) handleDeploy(w http.ResponseWriter, r *http.Request, t *targetState, by string) {
	if t.Packager == nil || t.Deployer == nil {
		http.Error(w, "this target cannot deploy", http.StatusNotImplemented)
		return
	}

	replicas, err := requestedReplicas(r, t)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Serialise per target, not globally. TryLock rather than Lock so a caller
	// learns immediately instead of hanging behind a ten-minute rollout.
	if !t.mu.TryLock() {
		w.Header().Set("Retry-After", "30")
		http.Error(w, "a deploy to this target is already in progress", http.StatusLocked)
		return
	}
	defer t.mu.Unlock()

	art, seq, ok := s.receive(w, r, t, replicas, by)
	if !ok {
		return
	}

	// Re-open the stored artifact rather than buffering the upload in memory.
	f, _, err := s.store.Open(art.Digest)
	if err != nil {
		http.Error(w, "reopening artifact failed", http.StatusInternalServerError)
		return
	}
	defer f.Close()

	// Everything up to here can still use a real status code, because no byte of
	// the body has been written yet.
	built, err := t.Packager.Package(r.Context(), f)
	if err != nil {
		writeResult(w, http.StatusBadGateway, api.Result{
			Target: r.PathValue("target"),
			Digest: art.Digest,
			Error:  "packaging or pushing the image failed: " + err.Error(),
		})
		return
	}

	// Record the manifest now, not after the rollout. The push has already
	// happened, so the manifest exists whether or not the deploy sticks - and one
	// the ledger never learned about can never be pruned.
	if err := t.Ledger.SetImage(seq, built.Ref, built.BaseRef); err != nil {
		fmt.Fprintf(os.Stderr, "lodestoned: record image for seq %d: %v\n", seq, err)
	}

	events := t.Deployer(r.Context(), built.Ref, replicas)
	name := r.PathValue("target")

	if wantsStream(r) {
		deployed := streamDeploy(w, name, art.Digest, built, replicas, events)
		s.afterDeploy(r.Context(), name, t, seq, deployed)
		return
	}

	res := rollout.Collect(events)
	out := api.Result{
		Target:    name,
		Digest:    art.Digest,
		Image:     built.Ref,
		BaseImage: built.BaseRef,
		Replicas:  replicas,
		Deployed:  res.Succeeded(),
		Events:    toAPIEvents(res.Events),
	}

	status := http.StatusOK
	if !res.Succeeded() {
		out.Error = res.Err.Error()
		// The deploy was rejected and rolled back. That is the agent working
		// correctly, but the caller's artifact is not live - so not a 2xx.
		status = http.StatusConflict
	}
	writeResult(w, status, out)

	s.afterDeploy(r.Context(), name, t, seq, res.Succeeded())
}

// afterDeploy records what is now live and trims the retention window.
//
// Runs after the response is written, so housekeeping never delays the caller
// and cannot turn a successful deploy into a failed one. It uses a context
// detached from the request for the same reason: the client hanging up must not
// abandon the ledger half-updated.
func (s *Server) afterDeploy(ctx context.Context, name string, t *targetState, seq uint64, deployed bool) {
	warn := func(msg string) { fmt.Fprintf(os.Stderr, "lodestoned: %s\n", msg) }

	if deployed {
		if err := t.Ledger.MarkDeployed(seq); err != nil {
			warn(fmt.Sprintf("mark seq %d deployed: %v", seq, err))
		}
	}

	pruneCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), pruneTimeout)
	defer cancel()

	s.prune(pruneCtx, name, t, warn)
}

// pruneTimeout bounds housekeeping. Generous enough for a slow registry, short
// enough that a hung one does not hold the deploy lock indefinitely.
const pruneTimeout = 2 * time.Minute

// receive stores the request body and records it in the target's ledger. It
// writes its own error response and reports whether to continue.
func (s *Server) receive(w http.ResponseWriter, r *http.Request, t *targetState, replicas *int32, by string) (store.Artifact, uint64, bool) {
	body := http.MaxBytesReader(w, r.Body, maxUploadBytes)
	defer body.Close()

	// Store first: the artifact is durable before anything is pushed anywhere, so
	// a failed deploy still leaves an auditable upload.
	art, err := s.store.Put(body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			http.Error(w, "artifact too large", http.StatusRequestEntityTooLarge)
			return store.Artifact{}, 0, false
		}
		http.Error(w, "storing artifact failed", http.StatusInternalServerError)
		return store.Artifact{}, 0, false
	}
	if art.Size == 0 {
		http.Error(w, "empty artifact", http.StatusBadRequest)
		return store.Artifact{}, 0, false
	}

	// Reject anything that is not an archive, before it reaches the ledger. A
	// truncated upload or the wrong file otherwise costs a full rollout to
	// discover, and leaves a ledger entry for something that was never shippable.
	//
	// The check needs the stored file rather than the request body, because a
	// zip's central directory is at the end and the body has already been consumed.
	if err := s.validateArchive(art.Digest); err != nil {
		// Remove it: an unshippable artifact should not linger in the store, and
		// nothing references it yet.
		if rmErr := s.store.Remove(art.Digest); rmErr != nil {
			fmt.Fprintf(os.Stderr, "lodestoned: remove rejected artifact %s: %v\n", art.Digest, rmErr)
		}
		http.Error(w, err.Error(), http.StatusBadRequest)
		return store.Artifact{}, 0, false
	}

	entry := ledger.Entry{
		Digest:   art.Digest,
		Size:     art.Size,
		Target:   r.PathValue("target"),
		Version:  r.URL.Query().Get("version"),
		By:       by,
		Replicas: replicas,
	}
	seq, err := t.Ledger.Append(entry)
	if err != nil {
		http.Error(w, "recording artifact failed", http.StatusInternalServerError)
		return store.Artifact{}, 0, false
	}

	return art, seq, true
}

// requestedReplicas reads ?replicas=N and checks it against the target's cap.
func requestedReplicas(r *http.Request, t *targetState) (*int32, error) {
	raw := r.URL.Query().Get("replicas")
	if raw == "" {
		return nil, nil // leave the count alone
	}

	n, err := strconv.ParseInt(raw, 10, 32)
	if err != nil {
		return nil, fmt.Errorf("replicas %q: not a number", raw)
	}

	count := int32(n)
	if err := t.Config.CheckReplicas(count); err != nil {
		return nil, err
	}
	return &count, nil
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
// It returns whether the deploy succeeded, so the caller can record it.
func streamDeploy(w http.ResponseWriter, name, digest string, built image.Built, replicas *int32, events <-chan rollout.Event) bool {
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
		Kind:      api.KindResult,
		Target:    name,
		Digest:    digest,
		Image:     built.Ref,
		BaseImage: built.BaseRef,
		Replicas:  replicas,
		Deployed:  res.Succeeded(),
	}
	if !res.Succeeded() {
		final.Error = res.Err.Error()
	}

	_ = enc.Encode(final)
	flush()

	return res.Succeeded()
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
