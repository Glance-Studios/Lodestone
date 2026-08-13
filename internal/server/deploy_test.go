package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Glance-Studios/Lodestone/internal/api"
	"github.com/Glance-Studios/Lodestone/internal/image"
	"github.com/Glance-Studios/Lodestone/internal/rollout"
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

// deployerFor returns a Deployer emitting a fixed outcome.
func deployerFor(succeed bool) Deployer {
	return func(ctx context.Context, digest string) <-chan rollout.Event {
		ch := make(chan rollout.Event, 4)
		ch <- rollout.Event{Phase: rollout.PhaseStarting, Message: "deploying " + digest, At: time.Now()}
		if succeed {
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
}

func deployServer(t *testing.T, p Packager, d Deployer) *Server {
	t.Helper()
	return New(Options{
		Version:  "test",
		Token:    "tok",
		Store:    newTestStore(t),
		Ledger:   newTestLedger(t),
		Packager: p,
		Deployer: d,
	})
}

func postDeploy(t *testing.T, srv *Server, body string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(http.MethodPost, "/deploy?version=0.1.0&by=cammy", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()

	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func TestDeployHappyPath(t *testing.T) {
	const jar = "plugin jar bytes"
	packager := &fakePackager{built: image.Built{
		Ref:    "ghcr.io/x/builds@sha256:cafe",
		Digest: "sha256:cafe",
	}}

	srv := deployServer(t, packager, deployerFor(true))
	rec := postDeploy(t, srv, jar)

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200 (body %s)", rec.Code, rec.Body)
	}

	var got api.Result
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decoding body: %v", err)
	}

	if !got.Deployed {
		t.Error("Deployed = false, want true")
	}
	if got.Image != "ghcr.io/x/builds@sha256:cafe" {
		t.Errorf("Image = %q, want the pushed reference", got.Image)
	}
	if !strings.HasPrefix(got.Digest, "sha256:") {
		t.Errorf("Digest = %q, want the artifact digest", got.Digest)
	}
	if len(got.Events) == 0 {
		t.Error("Events is empty; the rollout events should be reported")
	}

	// The packager must have received the artifact bytes, read back from the
	// store rather than buffered from the request.
	if packager.gotBytes != jar {
		t.Errorf("packager got %q, want %q", packager.gotBytes, jar)
	}
}

// The artifact must be in the ledger even though it was deployed in one shot.
func TestDeployRecordsInTheLedger(t *testing.T) {
	packager := &fakePackager{built: image.Built{Ref: "r@sha256:1", Digest: "sha256:1"}}
	srv := deployServer(t, packager, deployerFor(true))

	postDeploy(t, srv, "jar")

	entries := srv.ledger.Entries()
	if len(entries) != 1 {
		t.Fatalf("ledger has %d entries, want 1", len(entries))
	}
	if entries[0].Version != "0.1.0" || entries[0].By != "cammy" {
		t.Errorf("entry = %+v, want version 0.1.0 by cammy", entries[0])
	}
}

// A rolled-back deploy is not a 2xx: the agent worked, but the artifact is not
// live, and the caller must be able to tell those apart.
func TestDeployRolledBackReports409(t *testing.T) {
	packager := &fakePackager{built: image.Built{Ref: "r@sha256:2", Digest: "sha256:2"}}
	srv := deployServer(t, packager, deployerFor(false))

	rec := postDeploy(t, srv, "jar")

	if rec.Code != http.StatusConflict {
		t.Fatalf("code = %d, want 409", rec.Code)
	}

	var got api.Result
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decoding body: %v", err)
	}
	if got.Deployed {
		t.Error("Deployed = true, want false")
	}
	if !strings.Contains(got.Error, "health checks failed") {
		t.Errorf("Error = %q, want the rollout failure", got.Error)
	}
}

func TestDeployPackagingFailureReports502(t *testing.T) {
	packager := &fakePackager{err: errors.New("registry unauthorized")}
	srv := deployServer(t, packager, deployerFor(true))

	rec := postDeploy(t, srv, "jar")

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("code = %d, want 502", rec.Code)
	}

	var got api.Result
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decoding body: %v", err)
	}
	if !strings.Contains(got.Error, "registry unauthorized") {
		t.Errorf("Error = %q, want the registry failure", got.Error)
	}
}

// Without a configured target the endpoint must say so, not crash.
func TestDeployNotConfiguredReports501(t *testing.T) {
	srv := New(Options{
		Version: "test",
		Token:   "tok",
		Store:   newTestStore(t),
		Ledger:  newTestLedger(t),
	})

	rec := postDeploy(t, srv, "jar")

	if rec.Code != http.StatusNotImplemented {
		t.Errorf("code = %d, want 501", rec.Code)
	}
}

// A second deploy while one is in flight must be refused, not interleaved.
// Interleaved deploys corrupt each other's rollback.
func TestDeployRefusesConcurrentDeploys(t *testing.T) {
	// Hold the first deploy inside the deployer until the test releases it.
	inFlight := make(chan struct{})
	release := make(chan struct{})

	// Only the first call signals and blocks; later deploys in this test run
	// straight through. sync.Once because the deployer is invoked more than once.
	var first sync.Once
	slowDeployer := func(ctx context.Context, digest string) <-chan rollout.Event {
		ch := make(chan rollout.Event, 2)
		go func() {
			defer close(ch)
			first.Do(func() {
				close(inFlight)
				<-release
			})
			ch <- rollout.Event{Phase: rollout.PhaseSucceeded, Message: "deployed", At: time.Now()}
		}()
		return ch
	}

	packager := &fakePackager{built: image.Built{Ref: "r@sha256:9", Digest: "sha256:9"}}
	srv := deployServer(t, packager, slowDeployer)

	// First deploy, in the background - it will block in the deployer.
	firstDone := make(chan int, 1)
	go func() {
		rec := postDeploy(t, srv, "first jar")
		firstDone <- rec.Code
	}()

	<-inFlight // the first deploy is definitely holding the lock

	// Second deploy while the first is stuck.
	second := postDeploy(t, srv, "second jar")
	if second.Code != http.StatusLocked {
		t.Errorf("concurrent deploy got %d, want %d", second.Code, http.StatusLocked)
	}
	if ra := second.Header().Get("Retry-After"); ra == "" {
		t.Error("no Retry-After header on the refusal")
	}

	// Let the first finish, and confirm it succeeded.
	close(release)
	if code := <-firstDone; code != http.StatusOK {
		t.Errorf("first deploy got %d, want 200", code)
	}

	// The lock must be released, so a later deploy works.
	third := postDeploy(t, srv, "third jar")
	if third.Code != http.StatusOK {
		t.Errorf("deploy after the lock was released got %d, want 200", third.Code)
	}
}

func TestDeployRejectsEmptyBody(t *testing.T) {
	packager := &fakePackager{built: image.Built{Ref: "r@sha256:3", Digest: "sha256:3"}}
	srv := deployServer(t, packager, deployerFor(true))

	rec := postDeploy(t, srv, "")

	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d, want 400", rec.Code)
	}
}

func TestDeployNeedsToken(t *testing.T) {
	packager := &fakePackager{built: image.Built{Ref: "r@sha256:4", Digest: "sha256:4"}}
	srv := deployServer(t, packager, deployerFor(true))

	req := httptest.NewRequest(http.MethodPost, "/deploy", strings.NewReader("jar"))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("code = %d, want 401", rec.Code)
	}
}
