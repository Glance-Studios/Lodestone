package image

import (
	"context"
	"fmt"
	"io"

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

	// Keychain resolves registry credentials. Nil uses the ambient config -
	// docker config.json, and cloud provider helpers.
	Keychain authn.Keychain

	// baseCache holds the pulled base image so repeated builds skip the fetch.
	baseCache v1.Image
}

// Built describes a pushed image.
type Built struct {
	// Ref is the full reference including digest, e.g. "repo@sha256:...".
	Ref string `json:"ref"`

	// Digest is the image digest - what a Deployment should be pinned to.
	Digest string `json:"digest"`
}

func (p *Packager) keychain() authn.Keychain {
	if p.Keychain == nil {
		return authn.DefaultKeychain
	}
	return p.Keychain
}

// Base returns the base image, pulling it on first use and caching it.
func (p *Packager) base(ctx context.Context) (v1.Image, error) {
	if p.baseCache != nil {
		return p.baseCache, nil
	}

	ref, err := name.ParseReference(p.Base)
	if err != nil {
		return nil, fmt.Errorf("parse base %q: %w", p.Base, err)
	}

	img, err := remote.Image(ref,
		remote.WithContext(ctx),
		remote.WithAuthFromKeychain(p.keychain()),
	)
	if err != nil {
		return nil, fmt.Errorf("pull base %s: %w", p.Base, err)
	}

	p.baseCache = img
	return img, nil
}

// Package appends the artifact read from r onto the base image and pushes the
// result, returning the pushed digest.
func (p *Packager) Package(ctx context.Context, r io.Reader) (Built, error) {
	base, err := p.base(ctx)
	if err != nil {
		return Built{}, err
	}

	layer, err := LayerFor(r, p.DestPath)
	if err != nil {
		return Built{}, err
	}

	// mutate.AppendLayers returns a NEW image; the base is untouched, which is
	// what makes caching it safe.
	img, err := mutate.AppendLayers(base, layer)
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

	return Built{Ref: dst.String(), Digest: digest.String()}, nil
}
