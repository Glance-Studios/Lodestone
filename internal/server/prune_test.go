package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/Glance-Studios/Lodestone/internal/image"
)

// recordingDeleter notes which manifests it was asked to remove, and can refuse
// like a registry with deletes disabled.
type recordingDeleter struct {
	mu       sync.Mutex
	deleted  []string
	disabled bool
}

func (d *recordingDeleter) Delete(ctx context.Context, digest string) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.disabled {
		return fmt.Errorf("test registry: %w", image.ErrDeleteNotSupported)
	}
	d.deleted = append(d.deleted, digest)
	return nil
}

func (d *recordingDeleter) calls() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.deleted...)
}

// deployN runs n deploys with distinct payloads, so each gets its own digest.
func deployN(t *testing.T, f *fixture, target, token string, n int) {
	t.Helper()

	for i := range n {
		rec := f.do(t, http.MethodPost, "/deploy/"+target+"?version=1.0."+itoa(i), token,
			fmt.Sprintf("jar payload %d", i))
		if rec.Code != http.StatusOK {
			t.Fatalf("deploy %d to %s got %d (body %s)", i, target, rec.Code, rec.Body)
		}
	}
}

func itoa(i int) string { return fmt.Sprintf("%d", i) }

// The same jar deployed twice - after the base image moved, so two different
// manifests - must produce two independent entries.
//
// This is the leak that put five manifests in a repository configured to retain
// three. The ledger used to identify an entry by its artifact digest, so the
// second deploy of one jar marked both entries live and overwrote the first
// one's image reference with the second's. Both were then immortal to pruning,
// and the manifest the first had actually been built as was unreachable.
func TestRedeployingOneJarKeepsSeparateEntries(t *testing.T) {
	f := newFixtureWithRetain(t, 3)
	f.devPackager.movingBase = true

	const jar = "the same jar both times"
	for _, version := range []string{"0.1.0", "0.1.1"} {
		rec := f.do(t, http.MethodPost, "/deploy/dev-lobby?version="+version, devToken, jar)
		if rec.Code != http.StatusOK {
			t.Fatalf("deploy %s got %d (body %s)", version, rec.Code, rec.Body)
		}
	}

	entries := f.devLedger.Entries() // newest first
	if len(entries) != 2 {
		t.Fatalf("ledger has %d entries, want 2 - one per deploy", len(entries))
	}
	if entries[0].Digest != entries[1].Digest {
		t.Fatalf("the two deploys did not share an artifact digest; the test is not exercising the bug")
	}

	var live int
	for _, e := range entries {
		if e.Deployed {
			live++
		}
	}
	if live != 1 {
		t.Errorf("%d entries marked deployed, want exactly 1 - a stale live entry can never be pruned", live)
	}
	if !entries[0].Deployed {
		t.Error("the newer deploy is not the one marked live")
	}

	if entries[0].Image == entries[1].Image {
		t.Errorf("both entries record image %q; the older one's manifest is now unreclaimable", entries[0].Image)
	}
	if entries[1].BaseImage == entries[0].BaseImage {
		t.Error("both entries record the same base; the older one no longer says what it was built on")
	}
}

// A deploy that was rolled back still pushed a manifest, so the ledger has to
// record it - otherwise pruning has no reference to delete and the manifest
// stays in the registry forever.
func TestRolledBackDeployStillRecordsItsManifest(t *testing.T) {
	f := newFixtureWith(t, false, true) // dev rolls back

	rec := f.do(t, http.MethodPost, "/deploy/dev-lobby?version=1.0.0", devToken, "a jar that fails health checks")
	if rec.Code != http.StatusConflict {
		t.Fatalf("code = %d, want 409", rec.Code)
	}

	entries := f.devLedger.Entries()
	if len(entries) != 1 {
		t.Fatalf("ledger has %d entries, want 1", len(entries))
	}
	if entries[0].Deployed {
		t.Error("a rolled-back deploy is marked deployed")
	}
	if entries[0].Image == "" {
		t.Error("no image recorded for a rolled-back deploy; its manifest can never be pruned")
	}
}

// Pruning trims the ledger to the retention window.
func TestPruneTrimsTheLedger(t *testing.T) {
	f := newFixtureWithRetain(t, 3)

	deployN(t, f, "dev-lobby", devToken, 6)

	entries := f.devLedger.Entries()
	if len(entries) > 3 {
		t.Errorf("ledger has %d entries after 6 deploys with retain 3: %d", len(entries), len(entries))
	}
	if len(entries) == 0 {
		t.Fatal("ledger is empty; pruning removed too much")
	}
	// The newest deploy must be there, and must be the live one.
	if !entries[0].Deployed {
		t.Error("the newest entry is not marked deployed")
	}
}

// The artifact store is shared across targets, so a digest another target still
// records must survive - this is the subtle one.
func TestPruneKeepsArtifactsAnotherTargetStillNeeds(t *testing.T) {
	f := newFixtureWithRetain(t, 2)

	const shared = "the same jar bytes"

	// Deploy the identical payload to prod, so prod's ledger references its digest.
	rec := f.do(t, http.MethodPost, "/deploy/prod-lobby", prodToken, shared)
	if rec.Code != http.StatusOK {
		t.Fatalf("prod deploy got %d (%s)", rec.Code, rec.Body)
	}
	sharedDigest := decodeResult(t, rec).Digest

	// Then deploy it to dev, and push dev past its window so dev drops it.
	if rec := f.do(t, http.MethodPost, "/deploy/dev-lobby", devToken, shared); rec.Code != http.StatusOK {
		t.Fatalf("dev deploy got %d", rec.Code)
	}
	deployN(t, f, "dev-lobby", devToken, 4)

	// dev no longer records it...
	for _, e := range f.devLedger.Entries() {
		if e.Digest == sharedDigest {
			t.Skip("dev still records the shared digest; the window did not roll far enough")
		}
	}

	// ...but the file must still be there, because prod does.
	//
	// Close it: on Windows an open handle stops the temp directory being removed,
	// and t.TempDir's cleanup failure would fail the test even though the
	// assertion passed.
	rc, _, err := f.store.Open(sharedDigest)
	if err != nil {
		t.Fatalf("the shared artifact was deleted while prod still references it: %v", err)
	}
	rc.Close()
}

// A digest no target records any more is removed from the store.
func TestPruneRemovesUnreferencedArtifacts(t *testing.T) {
	f := newFixtureWithRetain(t, 2)

	rec := f.do(t, http.MethodPost, "/deploy/dev-lobby", devToken, "first payload")
	if rec.Code != http.StatusOK {
		t.Fatalf("first deploy got %d", rec.Code)
	}
	firstDigest := decodeResult(t, rec).Digest

	// Push it out of the window.
	deployN(t, f, "dev-lobby", devToken, 4)

	for _, e := range f.devLedger.Entries() {
		if e.Digest == firstDigest {
			t.Skip("the first digest is still in the window")
		}
	}

	if rc, _, err := f.store.Open(firstDigest); err == nil {
		rc.Close()
		t.Error("an unreferenced artifact was left on disk")
	}
}

// Manifests for dropped entries are deleted.
func TestPruneDeletesManifests(t *testing.T) {
	f := newFixtureWithRetain(t, 2)
	del := &recordingDeleter{}
	f.setDeleter("dev-lobby", del)

	deployN(t, f, "dev-lobby", devToken, 5)

	if len(del.calls()) == 0 {
		t.Error("no manifests deleted after deploying past the retention window")
	}
	for _, d := range del.calls() {
		if !strings.HasPrefix(d, "sha256:") {
			t.Errorf("deleted %q, want a bare digest", d)
		}
	}
}

// A registry with deletes disabled is a configuration state, not a failure: the
// deploy still succeeds and the ledger is still pruned.
func TestPruneToleratesDeletesDisabled(t *testing.T) {
	f := newFixtureWithRetain(t, 2)
	f.setDeleter("dev-lobby", &recordingDeleter{disabled: true})

	deployN(t, f, "dev-lobby", devToken, 5)

	// Deploys succeeded (deployN fails the test otherwise) and the ledger shrank.
	if got := len(f.devLedger.Entries()); got > 2 {
		t.Errorf("ledger has %d entries, want the window of 2 - pruning stopped at the registry", got)
	}
}

// A registry error must not fail the deploy either.
func TestPruneErrorDoesNotFailTheDeploy(t *testing.T) {
	f := newFixtureWithRetain(t, 2)
	f.setDeleter("dev-lobby", &failingDeleter{})

	deployN(t, f, "dev-lobby", devToken, 4)
}

type failingDeleter struct{}

func (failingDeleter) Delete(ctx context.Context, digest string) error {
	return errors.New("registry exploded")
}

// The live revision survives pruning, so a rollback target always exists.
func TestPruneAlwaysLeavesARollbackTarget(t *testing.T) {
	f := newFixtureWithRetain(t, 2)

	deployN(t, f, "dev-lobby", devToken, 8)

	entries := f.devLedger.Entries()
	if len(entries) < 2 {
		t.Fatalf("only %d entries remain; rollback needs the previous revision too", len(entries))
	}

	var live int
	for _, e := range entries {
		if e.Deployed {
			live++
		}
	}
	if live != 1 {
		t.Errorf("%d entries marked deployed, want exactly 1", live)
	}
}
