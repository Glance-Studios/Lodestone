package image

import (
	"context"
	"fmt"
	"io"
	"sync"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	v1 "github.com/google/go-containerregistry/pkg/v1"
)

// Packager appends artifacts onto a base image and pushes the result.
type Packager struct {
	// Base is the image to append onto, e.g. "ghcr.io/glance/paper:1.21".
	Base string

	// Repo is where results are pushed, e.g. "ghcr.io/glance/lodestone-builds".
	Repo string

	// DestPath is where the artifact lands inside the image.
	DestPath string

	// Overlay, when set, names an image whose layers are appended between the
	// base and the artifact, e.g. a test server's model files. Like Base it may
	// be a moving tag, and it is resolved on every build.
	Overlay string

	// Keychain resolves registry credentials. Nil uses the ambient config -
	// docker config.json, and cloud provider helpers.
	Keychain authn.Keychain

	// mu guards the cache. Callers currently serialise deploys per target, but
	// this does not rely on that: a cache whose correctness depends on an
	// invariant held in another package is a trap for whoever changes it.
	mu sync.Mutex

	// baseCache holds the pulled base image so repeated builds skip the fetch,
	// and baseDigest is what it was pulled at. Keeping the digest is what makes
	// the cache safe for a moving tag: the tag is re-resolved every build and the
	// cache is only reused when it still points at the same content.
	baseCache  v1.Image
	baseDigest string
}

// Built describes a pushed image.
type Built struct {
	// Ref is the full reference including digest, e.g. "repo@sha256:...".
	Ref string `json:"ref"`

	// Digest is the image digest - what a Deployment should be pinned to.
	Digest string `json:"digest"`

	// BaseRef is the base image this was appended onto, pinned by digest even
	// when Base was configured as a moving tag. It is the answer to "which world
	// was this built on?", and recording it is what makes a moving base tag
	// acceptable rather than a hole in provenance.
	BaseRef string `json:"baseRef"`

	// OverlayRef is the overlay appended, pinned by digest for the same reason.
	// Empty when the target has none.
	OverlayRef string `json:"overlayRef,omitempty"`
}

func (p *Packager) keychain() authn.Keychain {
	if p.Keychain == nil {
		return authn.DefaultKeychain
	}
	return p.Keychain
}

// base returns the base image and the digest-pinned reference it resolved to.
//
// The tag is resolved on every call, because Base may be a moving tag - a dev
// target pointing at "lobby-base:current" gets whatever the latest world is. A
// cache keyed only on "have we pulled once?" would keep building onto the base
// as it was at startup, which is the mutable-tag problem this whole design
// exists to avoid, one level up from the Deployment.
func (p *Packager) base(ctx context.Context) (v1.Image, string, error) {
	pinned, err := p.pin(ctx, "base", p.Base)
	if err != nil {
		return nil, "", err
	}
	digest := pinned.DigestStr()
	opts := p.remoteOptions(ctx)

	p.mu.Lock()
	defer p.mu.Unlock()

	if p.baseCache != nil && p.baseDigest == digest {
		return p.baseCache, pinned.String(), nil
	}

	// Pull by the resolved digest rather than the tag. Between the HEAD above and
	// this fetch the tag could move, and pulling by tag would build onto content
	// we did not resolve and would not be recording.
	img, err := remote.Image(pinned, opts...)
	if err != nil {
		return nil, "", fmt.Errorf("pull base %s: %w", pinned, err)
	}

	p.baseCache = img
	p.baseDigest = digest
	return img, pinned.String(), nil
}

func (p *Packager) remoteOptions(ctx context.Context) []remote.Option {
	return []remote.Option{
		remote.WithContext(ctx),
		remote.WithAuthFromKeychain(p.keychain()),
	}
}

// pin resolves image, which may be a moving tag, to the digest it names now.
func (p *Packager) pin(ctx context.Context, role, image string) (name.Digest, error) {
	ref, err := name.ParseReference(image)
	if err != nil {
		return name.Digest{}, fmt.Errorf("parse %s %q: %w", role, image, err)
	}

	// A HEAD on the manifest, so resolving costs a request rather than a pull.
	desc, err := remote.Head(ref, p.remoteOptions(ctx)...)
	if err != nil {
		return name.Digest{}, fmt.Errorf("resolve %s %s: %w", role, image, err)
	}
	digest := desc.Digest.String()

	// Pin the reference to what we just resolved. Naming the repository from ref
	// keeps any registry host and port intact.
	pinned, err := name.NewDigest(ref.Context().Name() + "@" + digest)
	if err != nil {
		return name.Digest{}, fmt.Errorf("pin %s %s to %s: %w", role, image, digest, err)
	}
	return pinned, nil
}

// overlay pulls the overlay's layers by the digest its tag names now. Not cached:
// it is small, and it is the part expected to change between builds.
func (p *Packager) overlay(ctx context.Context) ([]v1.Layer, string, error) {
	pinned, err := p.pin(ctx, "overlay", p.Overlay)
	if err != nil {
		return nil, "", err
	}
	img, err := remote.Image(pinned, p.remoteOptions(ctx)...)
	if err != nil {
		return nil, "", fmt.Errorf("pull overlay %s: %w", pinned, err)
	}
	layers, err := img.Layers()
	if err != nil {
		return nil, "", fmt.Errorf("read overlay %s: %w", pinned, err)
	}
	return layers, pinned.String(), nil
}

// Package appends the artifact read from r onto the base image and pushes the
// result, returning the pushed digest.
func (p *Packager) Package(ctx context.Context, r io.Reader) (Built, error) {
	base, baseRef, err := p.base(ctx)
	if err != nil {
		return Built{}, err
	}

	var layers []v1.Layer
	overlayRef := ""
	if p.Overlay != "" {
		// Before the artifact, so an overlay can never replace the file deployed.
		layers, overlayRef, err = p.overlay(ctx)
		if err != nil {
			return Built{}, err
		}
	}

	layer, err := LayerFor(r, p.DestPath)
	if err != nil {
		return Built{}, err
	}
	layers = append(layers, layer)

	// mutate.AppendLayers returns a NEW image; the base is untouched, which is
	// what makes caching it safe.
	img, err := mutate.AppendLayers(base, layers...)
	if err != nil {
		return Built{}, fmt.Errorf("append layer: %w", err)
	}

	digest, err := img.Digest()
	if err != nil {
		return Built{}, fmt.Errorf("compute digest: %w", err)
	}

	// Push to the digest, not a tag. Tags are mutable; a digest is the content.
	dst, err := name.NewDigest(p.Repo + "@" + digest.String())
	if err != nil {
		return Built{}, fmt.Errorf("build destination ref: %w", err)
	}

	if err := remote.Write(dst, img,
		remote.WithContext(ctx),
		remote.WithAuthFromKeychain(p.keychain()),
	); err != nil {
		return Built{}, fmt.Errorf("push %s: %w", dst, err)
	}

	return Built{Ref: dst.String(), Digest: digest.String(), BaseRef: baseRef, OverlayRef: overlayRef}, nil
}
