package image

import (
	"archive/tar"
	"context"
	"io"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// -- LayerFor ----------------------------------------------------------------

func TestLayerForContainsTheArtifact(t *testing.T) {
	const content = "pretend plugin jar bytes"

	layer, err := LayerFor(strings.NewReader(content), "/plugins/app.jar")
	if err != nil {
		t.Fatalf("LayerFor() error = %v", err)
	}

	rc, err := layer.Uncompressed()
	if err != nil {
		t.Fatalf("Uncompressed() error = %v", err)
	}
	defer rc.Close()

	tr := tar.NewReader(rc)
	hdr, err := tr.Next()
	if err != nil {
		t.Fatalf("reading tar: %v", err)
	}

	if hdr.Name != "plugins/app.jar" {
		t.Errorf("entry name = %q, want %q", hdr.Name, "plugins/app.jar")
	}
	if hdr.Size != int64(len(content)) {
		t.Errorf("entry size = %d, want %d", hdr.Size, len(content))
	}

	body, err := io.ReadAll(tr)
	if err != nil {
		t.Fatalf("reading entry: %v", err)
	}
	if string(body) != content {
		t.Errorf("entry content = %q, want %q", body, content)
	}

	// Exactly one file in the layer.
	if _, err := tr.Next(); err != io.EOF {
		t.Errorf("expected one entry, got another (err = %v)", err)
	}
}

// Identical content must produce an identical layer digest - that is what makes
// builds reproducible and layer caching possible.
func TestLayerForIsReproducible(t *testing.T) {
	const content = "same bytes every time"

	first, err := LayerFor(strings.NewReader(content), "/plugins/app.jar")
	if err != nil {
		t.Fatalf("first LayerFor() error = %v", err)
	}
	second, err := LayerFor(strings.NewReader(content), "/plugins/app.jar")
	if err != nil {
		t.Fatalf("second LayerFor() error = %v", err)
	}

	d1, err := first.Digest()
	if err != nil {
		t.Fatalf("first Digest() error = %v", err)
	}
	d2, err := second.Digest()
	if err != nil {
		t.Fatalf("second Digest() error = %v", err)
	}

	if d1 != d2 {
		t.Errorf("digests differ:\n %v\n %v\nlayer building must be deterministic", d1, d2)
	}
}

func TestLayerForDifferentContentDiffers(t *testing.T) {
	a, err := LayerFor(strings.NewReader("one"), "/plugins/app.jar")
	if err != nil {
		t.Fatalf("LayerFor() error = %v", err)
	}
	b, err := LayerFor(strings.NewReader("two"), "/plugins/app.jar")
	if err != nil {
		t.Fatalf("LayerFor() error = %v", err)
	}

	da, _ := a.Digest()
	db, _ := b.Digest()
	if da == db {
		t.Error("different content produced the same digest")
	}
}

func TestLayerForRejectsEmptyPath(t *testing.T) {
	if _, err := LayerFor(strings.NewReader("x"), ""); err == nil {
		t.Error("LayerFor() with an empty path returned no error")
	}
}

// -- Packager, against a real in-process registry -----------------------------

// testRegistry starts go-containerregistry's own registry implementation over
// HTTP. Pushes and pulls in these tests are genuine registry traffic.
func testRegistry(t *testing.T) string {
	t.Helper()

	srv := httptest.NewServer(registry.New())
	t.Cleanup(srv.Close)

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parsing registry URL: %v", err)
	}
	return u.Host
}

// seedBase pushes a small random image to act as the base to append onto.
func seedBase(t *testing.T, host string) string {
	t.Helper()

	ref, err := name.ParseReference(host + "/base:latest")
	if err != nil {
		t.Fatalf("parsing base ref: %v", err)
	}

	// A 2-layer image with random content - stands in for a Paper base image.
	img, err := random.Image(256, 2)
	if err != nil {
		t.Fatalf("building random base: %v", err)
	}
	if err := remote.Write(ref, img); err != nil {
		t.Fatalf("seeding base: %v", err)
	}
	return ref.String()
}

func TestPackagePushesAnImage(t *testing.T) {
	host := testRegistry(t)
	base := seedBase(t, host)

	p := &Packager{
		Base:     base,
		Repo:     host + "/builds",
		DestPath: "/plugins/app.jar",
	}

	built, err := p.Package(context.Background(), strings.NewReader("jar bytes"))
	if err != nil {
		t.Fatalf("Package() error = %v", err)
	}

	if !strings.HasPrefix(built.Digest, "sha256:") {
		t.Errorf("Digest = %q, want a sha256: prefix", built.Digest)
	}
	if !strings.Contains(built.Ref, "@sha256:") {
		t.Errorf("Ref = %q, want it pinned by digest", built.Ref)
	}

	// It must actually be retrievable from the registry.
	ref, err := name.NewDigest(built.Ref)
	if err != nil {
		t.Fatalf("parsing pushed ref: %v", err)
	}
	pulled, err := remote.Image(ref)
	if err != nil {
		t.Fatalf("pulling the pushed image: %v", err)
	}

	gotDigest, err := pulled.Digest()
	if err != nil {
		t.Fatalf("Digest() error = %v", err)
	}
	if gotDigest.String() != built.Digest {
		t.Errorf("pulled digest = %q, want %q", gotDigest, built.Digest)
	}
}

// The result must be the base plus exactly one layer.
func TestPackageAppendsOneLayerToTheBase(t *testing.T) {
	host := testRegistry(t)
	base := seedBase(t, host)

	baseRef, err := name.ParseReference(base)
	if err != nil {
		t.Fatalf("parsing base: %v", err)
	}
	baseImg, err := remote.Image(baseRef)
	if err != nil {
		t.Fatalf("pulling base: %v", err)
	}
	baseLayers, err := baseImg.Layers()
	if err != nil {
		t.Fatalf("base Layers() error = %v", err)
	}

	p := &Packager{Base: base, Repo: host + "/builds", DestPath: "/plugins/app.jar"}
	built, err := p.Package(context.Background(), strings.NewReader("jar bytes"))
	if err != nil {
		t.Fatalf("Package() error = %v", err)
	}

	ref, _ := name.NewDigest(built.Ref)
	pulled, err := remote.Image(ref)
	if err != nil {
		t.Fatalf("pulling result: %v", err)
	}
	layers, err := pulled.Layers()
	if err != nil {
		t.Fatalf("Layers() error = %v", err)
	}

	if len(layers) != len(baseLayers)+1 {
		t.Errorf("result has %d layers, want %d (base + 1)", len(layers), len(baseLayers)+1)
	}
}

// Same jar twice must yield the same image digest - so a redeploy of unchanged
// bytes is a no-op all the way down to the Deployment.
func TestPackageIsReproducible(t *testing.T) {
	host := testRegistry(t)
	base := seedBase(t, host)

	p := &Packager{Base: base, Repo: host + "/builds", DestPath: "/plugins/app.jar"}

	first, err := p.Package(context.Background(), strings.NewReader("identical"))
	if err != nil {
		t.Fatalf("first Package() error = %v", err)
	}
	second, err := p.Package(context.Background(), strings.NewReader("identical"))
	if err != nil {
		t.Fatalf("second Package() error = %v", err)
	}

	if first.Digest != second.Digest {
		t.Errorf("digests differ:\n %s\n %s", first.Digest, second.Digest)
	}
}

func TestPackageDifferentJarsDifferentDigests(t *testing.T) {
	host := testRegistry(t)
	base := seedBase(t, host)

	p := &Packager{Base: base, Repo: host + "/builds", DestPath: "/plugins/app.jar"}

	a, err := p.Package(context.Background(), strings.NewReader("version one"))
	if err != nil {
		t.Fatalf("Package() error = %v", err)
	}
	b, err := p.Package(context.Background(), strings.NewReader("version two"))
	if err != nil {
		t.Fatalf("Package() error = %v", err)
	}

	if a.Digest == b.Digest {
		t.Error("different jars produced the same image digest")
	}
}

func TestPackageFailsOnMissingBase(t *testing.T) {
	host := testRegistry(t)

	p := &Packager{
		Base:     host + "/nope:latest",
		Repo:     host + "/builds",
		DestPath: "/plugins/app.jar",
	}

	if _, err := p.Package(context.Background(), strings.NewReader("x")); err == nil {
		t.Error("Package() error = nil, want a pull failure for a missing base")
	}
}

// The base is cached after the first pull, so a second Package does not refetch.
func TestBaseIsCached(t *testing.T) {
	host := testRegistry(t)
	base := seedBase(t, host)

	p := &Packager{Base: base, Repo: host + "/builds", DestPath: "/plugins/app.jar"}

	if _, err := p.base(context.Background()); err != nil {
		t.Fatalf("first base() error = %v", err)
	}
	cached := p.baseCache
	if cached == nil {
		t.Fatal("baseCache is nil after the first pull")
	}

	if _, err := p.base(context.Background()); err != nil {
		t.Fatalf("second base() error = %v", err)
	}
	if p.baseCache != cached {
		t.Error("baseCache was replaced; the second call refetched the base")
	}
}

// empty.Image is a valid base too - appending onto nothing should work.
func TestPackageOntoScratch(t *testing.T) {
	host := testRegistry(t)

	// Seed an empty image as the base.
	ref, err := name.ParseReference(host + "/scratch:latest")
	if err != nil {
		t.Fatalf("parsing ref: %v", err)
	}
	if err := remote.Write(ref, empty.Image); err != nil {
		t.Fatalf("seeding empty base: %v", err)
	}

	p := &Packager{Base: ref.String(), Repo: host + "/builds", DestPath: "/app.jar"}
	built, err := p.Package(context.Background(), strings.NewReader("jar"))
	if err != nil {
		t.Fatalf("Package() error = %v", err)
	}
	if built.Digest == "" {
		t.Error("Digest is empty")
	}
}
