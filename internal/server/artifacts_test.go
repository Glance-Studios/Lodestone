package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Glance-Studios/Lodestone/internal/ledger"
	"github.com/Glance-Studios/Lodestone/internal/store"
)

// newTestStore returns a Store rooted in a temp dir that is cleaned up with the
// test. Shared by the tests in this package.
func newTestStore(t *testing.T) *store.Store {
	t.Helper() // failures report the caller's line, not this one

	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New() error = %v", err)
	}
	return st
}

// newTestLedger returns a Ledger backed by a temp file.
func newTestLedger(t *testing.T) *ledger.Ledger {
	t.Helper()

	l, err := ledger.Open(filepath.Join(t.TempDir(), "ledger.json"))
	if err != nil {
		t.Fatalf("ledger.Open() error = %v", err)
	}
	return l
}

func TestUploadArtifact(t *testing.T) {
	const token = "tok"
	const content = "pretend jar bytes"

	srv := New(Options{Version: "test", Token: token, Store: newTestStore(t), Ledger: newTestLedger(t)})

	req := httptest.NewRequest(http.MethodPost, "/artifacts", strings.NewReader(content))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()

	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("code = %d, want %d (body: %s)", rec.Code, http.StatusCreated, rec.Body)
	}

	var got store.Artifact
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decoding body: %v", err)
	}

	sum := sha256.Sum256([]byte(content))
	want := "sha256:" + hex.EncodeToString(sum[:])

	if got.Digest != want {
		t.Errorf("digest = %q, want %q", got.Digest, want)
	}
	if got.Size != int64(len(content)) {
		t.Errorf("size = %d, want %d", got.Size, len(content))
	}
}

func TestUploadRejectsEmptyBody(t *testing.T) {
	const token = "tok"
	srv := New(Options{Version: "test", Token: token, Store: newTestStore(t), Ledger: newTestLedger(t)})

	req := httptest.NewRequest(http.MethodPost, "/artifacts", strings.NewReader(""))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()

	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

// Auth still guards the endpoint now that it does real work.
func TestUploadStillNeedsToken(t *testing.T) {
	srv := New(Options{Version: "test", Token: "tok", Store: newTestStore(t), Ledger: newTestLedger(t)})

	req := httptest.NewRequest(http.MethodPost, "/artifacts", strings.NewReader("bytes"))
	rec := httptest.NewRecorder()

	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("code = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}
