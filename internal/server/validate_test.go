package server

import (
	"net/http"
	"strings"
	"testing"
)

// An upload that is not an archive is refused before it reaches the ledger.
func TestUploadRejectsNonArchive(t *testing.T) {
	f := newFixture(t)

	tests := []struct {
		name, body string
	}{
		{"plain text", "this is not a jar at all"},
		{
			// The mistake this check exists to catch: the right four magic bytes
			// and nothing else. A magic-number peek at the front would pass this;
			// reading the central directory does not, because there isn't one.
			name: "zip magic bytes only",
			body: "PK\x03\x04 pretend this is a jar",
		},
		{"truncated archive", zipped(t, "real archive")[:20]},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := f.doRaw(t, http.MethodPost, "/artifacts/dev-lobby", devToken, tt.body)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("code = %d, want 400", rec.Code)
			}
			if !strings.Contains(rec.Body.String(), "not a valid zip archive") {
				t.Errorf("body = %q, want it to say what was wrong", rec.Body)
			}
		})
	}

	// Nothing was recorded, so a rejected upload leaves no trace to explain later.
	if got := len(f.devLedger.Entries()); got != 0 {
		t.Errorf("ledger has %d entries after rejected uploads, want 0", got)
	}
}

// A rejected upload is not left on disk either.
func TestRejectedUploadIsNotStored(t *testing.T) {
	f := newFixture(t)

	rec := f.doRaw(t, http.MethodPost, "/artifacts/dev-lobby", devToken, "PK\x03\x04 not really a zip")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400", rec.Code)
	}

	// The store must be empty: nothing references the file, and keeping an
	// unshippable artifact only wastes disk.
	entries, err := storeEntries(f)
	if err != nil {
		t.Fatalf("reading the store: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("%d files left in the store, want 0: %v", len(entries), entries)
	}
}

// A deploy is refused too, before anything is packaged or rolled out.
func TestDeployRejectsNonArchive(t *testing.T) {
	f := newFixture(t)

	rec := f.doRaw(t, http.MethodPost, "/deploy/dev-lobby", devToken, "definitely not a zip")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400", rec.Code)
	}

	if f.devPackager.gotBytes != "" {
		t.Error("the packager ran on something that is not an archive")
	}
	if len(f.devDeployer.replicas) != 0 {
		t.Error("a rollout was started for something that is not an archive")
	}
}

// The check is only "is this a zip" - deliberately not "is this a jar". A zip
// with no META-INF is a legitimate artifact for anything that is not Java.
func TestArchiveWithoutManifestIsAccepted(t *testing.T) {
	f := newFixture(t)

	rec := f.do(t, http.MethodPost, "/artifacts/dev-lobby", devToken, "no META-INF in here")
	if rec.Code != http.StatusCreated {
		t.Errorf("code = %d, want 201 - a manifest is a Java convention, not our business", rec.Code)
	}
}
