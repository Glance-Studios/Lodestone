package server

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Glance-Studios/Lodestone/internal/api"
	"github.com/Glance-Studios/Lodestone/internal/image"
	"github.com/Glance-Studios/Lodestone/internal/ledger"
	"github.com/Glance-Studios/Lodestone/internal/rollout"
	"github.com/Glance-Studios/Lodestone/internal/store"
	"github.com/Glance-Studios/Lodestone/internal/target"
)

// Token values used across the server tests. Distinct per target, because
// "a dev credential cannot reach prod" is the property most worth testing.
const (
	devToken  = "dev-token"
	prodToken = "prod-token"
)

// fakePackager records what it was given and returns a canned image reference.
type fakePackager struct {
	gotBytes string
	built    image.Built
	err      error
}

func (f *fakePackager) Package(ctx context.Context, r io.Reader) (image.Built, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return image.Built{}, err
	}
	f.gotBytes = string(b)

	if f.err != nil {
		return image.Built{}, f.err
	}
	return f.built, nil
}

// recordingDeployer notes the replica count it was asked for and emits a fixed
// outcome.
type recordingDeployer struct {
	succeed  bool
	replicas []*int32
}

func (d *recordingDeployer) deploy(ctx context.Context, imageRef string, replicas *int32) <-chan rollout.Event {
	d.replicas = append(d.replicas, replicas)

	ch := make(chan rollout.Event, 4)
	ch <- rollout.Event{Phase: rollout.PhaseStarting, Message: "deploying " + imageRef, At: time.Now()}
	if d.succeed {
		ch <- rollout.Event{Phase: rollout.PhaseSucceeded, Message: "deployed", At: time.Now()}
	} else {
		ch <- rollout.Event{
			Phase:   rollout.PhaseFailed,
			Message: "health checks failed",
			Err:     "connection refused",
			At:      time.Now(),
		}
	}
	close(ch)
	return ch
}

func newStore(t *testing.T) *store.Store {
	t.Helper()

	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New() error = %v", err)
	}
	return st
}

func newLedger(t *testing.T) *ledger.Ledger {
	t.Helper()

	l, err := ledger.Open(filepath.Join(t.TempDir(), "ledger.json"))
	if err != nil {
		t.Fatalf("ledger.Open() error = %v", err)
	}
	return l
}

// fixture is a server with two targets, dev and prod, each with its own token,
// ledger, packager and deployer.
type fixture struct {
	srv *Server

	devPackager *fakePackager
	devDeployer *recordingDeployer
	devLedger   *ledger.Ledger

	prodPackager *fakePackager
	prodDeployer *recordingDeployer
	prodLedger   *ledger.Ledger
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	return newFixtureWith(t, true, true)
}

// newFixtureWith builds the two-target fixture, choosing whether each target's
// deploy succeeds.
func newFixtureWith(t *testing.T, devOK, prodOK bool) *fixture {
	t.Helper()

	f := &fixture{
		devPackager:  &fakePackager{built: image.Built{Ref: "reg/dev/lobby@sha256:dev", Digest: "sha256:dev"}},
		devDeployer:  &recordingDeployer{succeed: devOK},
		devLedger:    newLedger(t),
		prodPackager: &fakePackager{built: image.Built{Ref: "reg/prod/lobby@sha256:prod", Digest: "sha256:prod"}},
		prodDeployer: &recordingDeployer{succeed: prodOK},
		prodLedger:   newLedger(t),
	}

	f.srv = New(Options{
		Version: "test",
		Store:   newStore(t),
		Targets: map[string]TargetSpec{
			"dev-lobby": {
				Config: target.Target{
					Namespace: "hideaway-dev", Deployment: "lobby", Container: "paper",
					Token: devToken, MaxReplicas: 5,
				},
				Packager: f.devPackager,
				Deployer: f.devDeployer.deploy,
				Ledger:   f.devLedger,
			},
			"prod-lobby": {
				Config: target.Target{
					Namespace: "hideaway-prod", Deployment: "lobby", Container: "paper",
					Token: prodToken, MaxReplicas: 20,
				},
				Packager: f.prodPackager,
				Deployer: f.prodDeployer.deploy,
				Ledger:   f.prodLedger,
			},
		},
	})

	return f
}

// do sends a request and returns the recorder.
func (f *fixture) do(t *testing.T, method, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()

	f.srv.Handler().ServeHTTP(rec, req)
	return rec
}

// stream sends a request asking for NDJSON.
func (f *fixture) stream(t *testing.T, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", api.ContentTypeNDJSON)
	rec := httptest.NewRecorder()

	f.srv.Handler().ServeHTTP(rec, req)
	return rec
}

// decodeResult reads a single-object JSON response.
func decodeResult(t *testing.T, rec *httptest.ResponseRecorder) api.Result {
	t.Helper()

	var out api.Result
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("body is not one JSON object: %v\n%s", err, rec.Body)
	}
	return out
}

// decodeStream splits an NDJSON body into its event and result lines.
func decodeStream(t *testing.T, body string) ([]api.Event, api.Result, bool) {
	t.Helper()

	var (
		events []api.Event
		result api.Result
		found  bool
	)

	sc := bufio.NewScanner(strings.NewReader(body))
	for sc.Scan() {
		line := sc.Bytes()
		if strings.TrimSpace(string(line)) == "" {
			continue
		}

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
			t.Fatalf("unknown line kind %q: %s", probe.Kind, line)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scanning body: %v", err)
	}
	return events, result, found
}
