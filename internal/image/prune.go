package image

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
)

// ErrDeleteNotSupported reports that the registry refuses deletes.
//
// The Docker registry rejects them unless REGISTRY_STORAGE_DELETE_ENABLED is
// true, so this is a configuration state rather than a fault - callers treat it
// as "pruning unavailable" and carry on.
var ErrDeleteNotSupported = errors.New("registry does not allow deletes")

// Delete removes an image manifest from the registry.
//
// It unlinks the manifest and stops there. The registry does not free the
// underlying blobs until `registry garbage-collect` runs, which wants the
// registry read-only or stopped - that is registry maintenance, and a deploy
// agent has no business stopping it. Documented as an operator task instead.
func (p *Packager) Delete(ctx context.Context, digest string) error {
	ref, err := name.NewDigest(p.Repo + "@" + digest)
	if err != nil {
		return fmt.Errorf("build reference for %s: %w", digest, err)
	}

	err = remote.Delete(ref,
		remote.WithContext(ctx),
		remote.WithAuthFromKeychain(p.keychain()),
	)
	if err == nil {
		return nil
	}

	// A registry with deletes disabled answers 405; one that has already dropped
	// the manifest answers 404, which for pruning is success.
	var te *transport.Error
	if errors.As(err, &te) {
		switch te.StatusCode {
		case http.StatusMethodNotAllowed, http.StatusUnsupportedMediaType:
			return fmt.Errorf("%s: %w", p.Repo, ErrDeleteNotSupported)
		case http.StatusNotFound:
			return nil
		}
	}
	return fmt.Errorf("delete %s: %w", ref, err)
}
