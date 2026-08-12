// Package image packages an artifact into an OCI image by appending it as a
// layer onto a base image. No Docker daemon: go-containerregistry speaks the
// registry API and OCI formats directly.
package image

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io"
	"path"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/v1/tarball"

	v1 "github.com/google/go-containerregistry/pkg/v1"
)

// A fixed timestamp for everything we write into a layer. Real modification
// times would make the layer digest change on every build even when the content
// is identical, which defeats caching and reproducibility.
var epoch = time.Unix(0, 0)

// LayerFor builds a single-file image layer containing the artifact read from r,
// placed at destPath inside the image.
//
// The tar is built in memory: plugin jars are single-digit MB, and holding one
// is cheaper than the temp-file bookkeeping. A layer of arbitrary size would
// want a file instead.
func LayerFor(r io.Reader, destPath string) (v1.Layer, error) {
	if destPath == "" {
		return nil, fmt.Errorf("destination path is empty")
	}

	content, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("read artifact: %w", err)
	}

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)

	header := &tar.Header{
		Typeflag: tar.TypeReg,
		// Tar entries in an OCI layer are relative: "plugins/app.jar", never
		// "/plugins/app.jar". Callers naturally write the absolute in-image
		// path, so normalise it here rather than making them remember.
		Name:    strings.TrimPrefix(path.Clean(destPath), "/"),
		Size:    int64(len(content)),
		Mode:    0o644,
		ModTime: epoch,
		Format:  tar.FormatPAX,
	}
	if err := tw.WriteHeader(header); err != nil {
		return nil, fmt.Errorf("write tar header: %w", err)
	}
	if _, err := tw.Write(content); err != nil {
		return nil, fmt.Errorf("write tar body: %w", err)
	}
	// Close flushes the trailing padding that makes the tar valid - a deferred
	// close would run too late, after the bytes were already read.
	if err := tw.Close(); err != nil {
		return nil, fmt.Errorf("finish tar: %w", err)
	}

	// LayerFromOpener takes a function returning a fresh reader, because the
	// layer may be read more than once (to digest it, then to upload it).
	layer, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(buf.Bytes())), nil
	})
	if err != nil {
		return nil, fmt.Errorf("build layer: %w", err)
	}
	return layer, nil
}
