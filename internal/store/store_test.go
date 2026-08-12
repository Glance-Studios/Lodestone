package store

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Put takes an io.Reader, so a test needs no file and no HTTP - a string reader
// satisfies the same contract as a request body.
func TestPutStoresByDigest(t *testing.T) {
	// t.TempDir is cleaned up automatically when the test finishes.
	st, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	const content = "pretend this is a plugin jar"
	art, err := st.Put(strings.NewReader(content))
	if err != nil {
		t.Fatalf("Put() error = %v", err)
	}

	sum := sha256.Sum256([]byte(content))
	wantDigest := "sha256:" + hex.EncodeToString(sum[:])

	if art.Digest != wantDigest {
		t.Errorf("Digest = %q, want %q", art.Digest, wantDigest)
	}
	if art.Size != int64(len(content)) {
		t.Errorf("Size = %d, want %d", art.Size, len(content))
	}
}

func TestPutWritesTheBytesToDisk(t *testing.T) {
	dir := t.TempDir()
	st, err := New(dir)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	const content = "artifact bytes"
	art, err := st.Put(strings.NewReader(content))
	if err != nil {
		t.Fatalf("Put() error = %v", err)
	}

	// The file is named by the hex digest, without the "sha256:" prefix.
	name := strings.TrimPrefix(art.Digest, "sha256:") + ".jar"
	got, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("reading stored artifact: %v", err)
	}
	if string(got) != content {
		t.Errorf("stored content = %q, want %q", got, content)
	}
}

// Content addressing means the same bytes twice is one file, not two.
func TestPutIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	st, err := New(dir)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	first, err := st.Put(strings.NewReader("same bytes"))
	if err != nil {
		t.Fatalf("first Put() error = %v", err)
	}
	second, err := st.Put(strings.NewReader("same bytes"))
	if err != nil {
		t.Fatalf("second Put() error = %v", err)
	}

	if first.Digest != second.Digest {
		t.Errorf("digests differ: %q then %q", first.Digest, second.Digest)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading dir: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("%d files in the store, want 1 - identical uploads should collapse", len(entries))
	}
}

// No temp files should survive a successful Put.
func TestPutLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	st, err := New(dir)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	if _, err := st.Put(strings.NewReader("x")); err != nil {
		t.Fatalf("Put() error = %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading dir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".upload-") {
			t.Errorf("temp file %q left behind", e.Name())
		}
	}
}
