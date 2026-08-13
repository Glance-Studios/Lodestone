package server

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Glance-Studios/Lodestone/internal/api"
	"github.com/Glance-Studios/Lodestone/internal/image"
	"github.com/Glance-Studios/Lodestone/internal/rollout"
)

// postDeployStreaming asks for NDJSON.
func postDeployStreaming(t *testing.T, srv *Server, body string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(http.MethodPost, "/deploy", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("Accept", api.ContentTypeNDJSON)
	rec := httptest.NewRecorder()

	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// decodeLines splits an NDJSON body into its lines, keyed by kind.
func decodeLines(t *testing.T, body string) (events []api.Event, result api.Result, found bool) {
	t.Helper()

	sc := bufio.NewScanner(strings.NewReader(body))
	for sc.Scan() {
		line := sc.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}

		// Peek at kind before committing to a type.
		var probe struct{ Kind string }
		if err := json.Unmarshal(line, &probe); err != nil {
			t.Fatalf("line is not JSON: %s (%v)", line, err)
		}

		switch probe.Kind {
		case api.KindEvent:
			var e api.Event
			if err := json.Unmarshal(line, &e); err != nil {
				t.Fatalf("decoding event: %v", err)
			}
			events = append(events, e)
		case api.KindResult:
			if err := json.Unmarshal(line, &result); err != nil {
				t.Fatalf("decoding result: %v", err)
			}
			found = true
		default:
			t.Fatalf("line has unknown kind %q: %s", probe.Kind, line)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scanning body: %v", err)
	}
	return events, result, found
}

func TestStreamingDeployEmitsEventsThenResult(t *testing.T) {
	packager := &fakePackager{built: image.Built{Ref: "r@sha256:abc", Digest: "sha256:abc"}}
	srv := deployServer(t, packager, deployerFor(true))

	rec := postDeployStreaming(t, srv, "jar bytes")

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != api.ContentTypeNDJSON {
		t.Errorf("Content-Type = %q, want %q", ct, api.ContentTypeNDJSON)
	}

	events, result, found := decodeLines(t, rec.Body.String())

	if len(events) == 0 {
		t.Error("no event lines")
	}
	if !found {
		t.Fatal("no result line; the stream must end with one")
	}
	if !result.Deployed {
		t.Error("result.Deployed = false, want true")
	}
	if result.Image != "r@sha256:abc" {
		t.Errorf("result.Image = %q, want the pushed reference", result.Image)
	}
	if !strings.HasPrefix(result.Digest, "sha256:") {
		t.Errorf("result.Digest = %q, want the artifact digest", result.Digest)
	}
}

// The important subtlety: a status code goes out with the first byte of the body
// and cannot be revised, so a streamed failure is still HTTP 200 and the caller
// learns the outcome from the final line.
func TestStreamingFailureIsStill200(t *testing.T) {
	packager := &fakePackager{built: image.Built{Ref: "r@sha256:bad", Digest: "sha256:bad"}}
	srv := deployServer(t, packager, deployerFor(false))

	rec := postDeployStreaming(t, srv, "jar bytes")

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200 even for a failed deploy", rec.Code)
	}

	_, result, found := decodeLines(t, rec.Body.String())
	if !found {
		t.Fatal("no result line")
	}
	if result.Deployed {
		t.Error("result.Deployed = true, want false")
	}
	if !strings.Contains(result.Error, "health checks failed") {
		t.Errorf("result.Error = %q, want the rollout failure", result.Error)
	}
}

// Every event the rollout emits must appear on the wire, in order.
func TestStreamingPreservesEventOrder(t *testing.T) {
	phases := []rollout.Phase{
		rollout.PhaseStarting,
		rollout.PhaseUpdating,
		rollout.PhaseSettling,
		rollout.PhaseChecking,
		rollout.PhaseSucceeded,
	}

	deployer := func(ctx context.Context, digest string) <-chan rollout.Event {
		ch := make(chan rollout.Event, len(phases))
		for _, p := range phases {
			ch <- rollout.Event{Phase: p, Message: string(p), At: time.Now()}
		}
		close(ch)
		return ch
	}

	srv := deployServer(t, &fakePackager{built: image.Built{Ref: "r@sha256:1"}}, deployer)

	rec := postDeployStreaming(t, srv, "jar")

	events, _, _ := decodeLines(t, rec.Body.String())
	if len(events) != len(phases) {
		t.Fatalf("got %d events, want %d", len(events), len(phases))
	}
	for i, p := range phases {
		if events[i].Phase != string(p) {
			t.Errorf("event[%d].Phase = %q, want %q", i, events[i].Phase, p)
		}
	}
}

// Without the Accept header the old single-object response must still be served,
// so existing clients and a plain curl keep working.
func TestNonStreamingClientGetsSingleObject(t *testing.T) {
	packager := &fakePackager{built: image.Built{Ref: "r@sha256:xyz", Digest: "sha256:xyz"}}
	srv := deployServer(t, packager, deployerFor(true))

	rec := postDeploy(t, srv, "jar") // no Accept header

	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	// One object, with the events embedded - not a stream of lines.
	var got api.Result
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("body is not a single JSON object: %v", err)
	}
	if len(got.Events) == 0 {
		t.Error("Events is empty; a non-streamed reply carries them inline")
	}
	if got.Kind != "" {
		t.Errorf("Kind = %q, want empty on a non-streamed reply", got.Kind)
	}
}
