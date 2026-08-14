package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Glance-Studios/Lodestone/internal/api"
)

// clientFor spins up a test server with h and returns a Client pointed at it.
func clientFor(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()

	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	return NewClient(Context{API: srv.URL, Token: "tok"})
}

func TestStatus(t *testing.T) {
	c := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer tok" {
			t.Errorf("Authorization = %q, want Bearer tok", got)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"status":"ok","version":"1.2.3","uptime":"5m0s"}`)
	})

	got, err := c.Status(context.Background())
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	if got.Version != "1.2.3" || got.Status != "ok" {
		t.Errorf("Status() = %+v", got)
	}
}

func TestAPIErrorCarriesStatusAndBody(t *testing.T) {
	c := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	})

	_, err := c.Status(context.Background())
	if err == nil {
		t.Fatal("Status() error = nil, want an APIError")
	}

	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error = %v, want an *APIError", err)
	}
	if !apiErr.Unauthorized() {
		t.Errorf("Unauthorized() = false for status %d", apiErr.Status)
	}
	if !strings.Contains(apiErr.Error(), "unauthorized") {
		t.Errorf("message %q should include the server's body", apiErr.Error())
	}
}

func TestBusyIsRecognised(t *testing.T) {
	c := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "a deploy is already in progress", http.StatusLocked)
	})

	_, err := c.Deploy(context.Background(), "dev-lobby", strings.NewReader("jar"), UploadOptions{}, nil)

	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error = %v, want an *APIError", err)
	}
	if !apiErr.Busy() {
		t.Errorf("Busy() = false for status %d", apiErr.Status)
	}
}

func TestPushSendsMetadataAsQuery(t *testing.T) {
	var gotVersion, gotBy, gotBody string

	c := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
		gotVersion = r.URL.Query().Get("version")
		gotBy = r.URL.Query().Get("by")

		buf := make([]byte, 64)
		n, _ := r.Body.Read(buf)
		gotBody = string(buf[:n])

		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"digest":"sha256:abc","size":3}`)
	})

	got, err := c.Push(context.Background(), "dev-lobby", strings.NewReader("jar"),
		UploadOptions{Version: "1.0.0"})
	if err != nil {
		t.Fatalf("Push() error = %v", err)
	}

	if gotVersion != "1.0.0" {
		t.Errorf("query version=%q, want 1.0.0", gotVersion)
	}
	// The client must not send an identity at all. The server derives it from the
	// credential, so a by= parameter is at best ignored and at worst read as
	// something the caller can influence.
	if gotBy != "" {
		t.Errorf("query by=%q, want it not sent", gotBy)
	}
	if gotBody != "jar" {
		t.Errorf("body = %q, want jar", gotBody)
	}
	if got.Digest != "sha256:abc" {
		t.Errorf("Digest = %q", got.Digest)
	}
}

// -- the streaming path -------------------------------------------------------

// streamHandler writes NDJSON lines, flushing each so the client really does
// receive them one at a time.
func streamHandler(lines ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Accept"); !strings.Contains(got, api.ContentTypeNDJSON) {
			http.Error(w, "client did not ask for a stream", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", api.ContentTypeNDJSON)
		w.WriteHeader(http.StatusOK)

		flusher, _ := w.(http.Flusher)
		for _, l := range lines {
			fmt.Fprintln(w, l)
			if flusher != nil {
				flusher.Flush()
			}
		}
	}
}

func TestDeployStreamsEventsInOrder(t *testing.T) {
	c := clientFor(t, streamHandler(
		`{"kind":"event","phase":"starting","message":"deploying","at":"2026-08-13T10:00:00Z"}`,
		`{"kind":"event","phase":"updating","message":"replacing","at":"2026-08-13T10:00:01Z"}`,
		`{"kind":"event","phase":"succeeded","message":"deployed","at":"2026-08-13T10:00:02Z"}`,
		`{"kind":"result","digest":"sha256:abc","image":"r@sha256:def","deployed":true}`,
	))

	var phases []string
	res, err := c.Deploy(context.Background(), "dev-lobby", strings.NewReader("jar"), UploadOptions{},
		func(e api.Event) { phases = append(phases, e.Phase) })
	if err != nil {
		t.Fatalf("Deploy() error = %v", err)
	}

	want := []string{"starting", "updating", "succeeded"}
	if len(phases) != len(want) {
		t.Fatalf("phases = %v, want %v", phases, want)
	}
	for i := range want {
		if phases[i] != want[i] {
			t.Errorf("phase[%d] = %q, want %q", i, phases[i], want[i])
		}
	}

	if !res.Deployed {
		t.Error("Deployed = false, want true")
	}
	if res.Image != "r@sha256:def" {
		t.Errorf("Image = %q", res.Image)
	}
}

// A failed deploy still arrives as HTTP 200; the outcome is in the final line.
func TestDeployFailureComesFromTheResultLine(t *testing.T) {
	c := clientFor(t, streamHandler(
		`{"kind":"event","phase":"rolling_back","message":"health checks failed","at":"2026-08-13T10:00:00Z"}`,
		`{"kind":"result","digest":"sha256:abc","deployed":false,"error":"health checks failed: refused"}`,
	))

	res, err := c.Deploy(context.Background(), "dev-lobby", strings.NewReader("jar"), UploadOptions{}, nil)
	if err != nil {
		t.Fatalf("Deploy() error = %v, want nil - the request itself succeeded", err)
	}
	if res.Deployed {
		t.Error("Deployed = true, want false")
	}
	if !strings.Contains(res.Error, "health checks failed") {
		t.Errorf("Error = %q", res.Error)
	}
}

// A stream that stops before the result line means the connection dropped and
// the deploy may still be running - that must not look like success.
func TestDeployTruncatedStreamIsAnError(t *testing.T) {
	c := clientFor(t, streamHandler(
		`{"kind":"event","phase":"settling","message":"waiting","at":"2026-08-13T10:00:00Z"}`,
	))

	_, err := c.Deploy(context.Background(), "dev-lobby", strings.NewReader("jar"), UploadOptions{}, nil)
	if !errors.Is(err, ErrNoResult) {
		t.Errorf("Deploy() error = %v, want ErrNoResult", err)
	}
}

// An older client must ignore line kinds it does not know rather than fail.
func TestDeployIgnoresUnknownLineKinds(t *testing.T) {
	c := clientFor(t, streamHandler(
		`{"kind":"event","phase":"starting","message":"go","at":"2026-08-13T10:00:00Z"}`,
		`{"kind":"telemetry","cpu":0.5}`,
		`{"kind":"result","digest":"sha256:abc","deployed":true}`,
	))

	var count int
	res, err := c.Deploy(context.Background(), "dev-lobby", strings.NewReader("jar"), UploadOptions{},
		func(api.Event) { count++ })
	if err != nil {
		t.Fatalf("Deploy() error = %v", err)
	}
	if count != 1 {
		t.Errorf("saw %d events, want 1 - the unknown kind should be skipped", count)
	}
	if !res.Deployed {
		t.Error("Deployed = false, want true")
	}
}

// A server that ignores the Accept header sends one object; handle it anyway.
func TestDeployHandlesNonStreamingServer(t *testing.T) {
	c := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"digest":"sha256:abc","image":"r@sha256:def","deployed":true,
			"events":[{"kind":"event","phase":"starting","message":"go","at":"2026-08-13T10:00:00Z"}]}`)
	})

	var count int
	res, err := c.Deploy(context.Background(), "dev-lobby", strings.NewReader("jar"), UploadOptions{},
		func(api.Event) { count++ })
	if err != nil {
		t.Fatalf("Deploy() error = %v", err)
	}
	if !res.Deployed {
		t.Error("Deployed = false, want true")
	}
	if count != 1 {
		t.Errorf("replayed %d events, want 1", count)
	}
}

// Events must reach the caller as they arrive, not in a batch at the end - that
// is the whole point of streaming.
func TestDeployDeliversEventsProgressively(t *testing.T) {
	release := make(chan struct{})

	c := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", api.ContentTypeNDJSON)
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)

		fmt.Fprintln(w, `{"kind":"event","phase":"starting","message":"go","at":"2026-08-13T10:00:00Z"}`)
		if flusher != nil {
			flusher.Flush()
		}

		<-release // hold the response open

		fmt.Fprintln(w, `{"kind":"result","digest":"sha256:abc","deployed":true}`)
		if flusher != nil {
			flusher.Flush()
		}
	})

	firstEvent := make(chan struct{})
	done := make(chan error, 1)

	go func() {
		_, err := c.Deploy(context.Background(), "dev-lobby", strings.NewReader("jar"), UploadOptions{},
			func(api.Event) {
				select {
				case firstEvent <- struct{}{}:
				default:
				}
			})
		done <- err
	}()

	select {
	case <-firstEvent:
		// Good: the event arrived while the server still held the response open.
	case <-time.After(3 * time.Second):
		t.Fatal("no event within 3s; the client is buffering the whole stream")
	}

	close(release)
	if err := <-done; err != nil {
		t.Errorf("Deploy() error = %v", err)
	}
}
