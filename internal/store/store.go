package store

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Write artifacts into a directory, each file names by its SHA-256 digest
type Store struct {
	dir string
}

// New returns a Store rooted at dir, creating the directory if it's absent
func New(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create store dir %s: %w", dir, err)
	}
	return &Store{dir: dir}, nil
}

// Artifact describes a stored artifact
type Artifact struct {
	Digest string `json:"digest"` // "sha256:" + hex
	Size   int64  `json:"size"`   // bytes written
}

// Put streams the artifact from r into the store
func (s *Store) Put(r io.Reader) (Artifact, error) {
	tmp, err := os.CreateTemp(s.dir, ".upload-*")
	if err != nil {
		return Artifact{}, fmt.Errorf("create temp file: %w", err)
	}

	// Defer so Close happens before Remove
	defer os.Remove(tmp.Name())
	defer tmp.Close()

	hasher := sha256.New()

	size, err := io.Copy(io.MultiWriter(tmp, hasher), r)
	if err != nil {
		return Artifact{}, fmt.Errorf("write artifact: %w", err)
	}

	// Flush to disk before rename publishes it under the final name
	if err := tmp.Close(); err != nil {
		return Artifact{}, fmt.Errorf("flush artifact: %w", err)
	}

	sum := hex.EncodeToString(hasher.Sum(nil))
	final := filepath.Join(s.dir, sum+".jar")
	if err := os.Rename(tmp.Name(), final); err != nil {
		return Artifact{}, fmt.Errorf("finalize artifact: %w", err)
	}

	return Artifact{Digest: "sha256:" + sum, Size: size}, nil
}
