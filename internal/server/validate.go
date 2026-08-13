package server

import (
	"archive/zip"
	"errors"
	"fmt"
	"io"
)

// ErrNotAnArchive reports an upload that is not a valid zip archive.
var ErrNotAnArchive = errors.New("not a valid zip archive")

// validArchive reports whether the artifact is a readable zip.
//
// Deliberately the only check, and deliberately not "is this a jar". Looking for
// META-INF/MANIFEST.MF would make Lodestone start knowing what kind of payload it
// carries, and a zip artifact for something that is not Java is a legitimate
// thing to deploy. "Is this a valid archive" is generic and still catches every
// case worth catching: a truncated upload, the wrong file, an empty body.
//
// It needs the whole file rather than a magic-number peek at the front, because a
// zip's index is its central directory and that sits at the *end*. A file
// starting with the right four bytes and containing nothing else passes a magic
// check and fails here - which is exactly the mistake this exists to catch.
func validArchive(r io.ReaderAt, size int64) error {
	if _, err := zip.NewReader(r, size); err != nil {
		return fmt.Errorf("%w: %v", ErrNotAnArchive, err)
	}
	return nil
}

// validateArchive checks a stored artifact.
func (s *Server) validateArchive(digest string) error {
	f, size, err := s.store.Open(digest)
	if err != nil {
		return fmt.Errorf("reopening artifact to validate it: %w", err)
	}
	defer f.Close()

	return validArchive(f, size)
}
