package server

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/Glance-Studios/Lodestone/internal/image"
)

// Deleter removes an image manifest from a registry. Declared here, by the
// consumer, so pruning depends on one method rather than on image.Packager.
type Deleter interface {
	Delete(ctx context.Context, digest string) error
}

// prune trims one target's ledger to its retention window and removes what is no
// longer referenced.
//
// It never returns an error. Housekeeping must not turn a successful deploy into
// a failed one, so every problem is reported through warn and pruning continues
// with whatever it can still do.
func (s *Server) prune(ctx context.Context, name string, t *targetState, warn func(string)) {
	dropped, err := t.Ledger.Prune(t.Config.Retain)
	if err != nil {
		warn(fmt.Sprintf("prune ledger for %s: %v", name, err))
		return
	}
	if len(dropped) == 0 {
		return
	}

	// The artifact store is shared across targets so an identical jar deployed to
	// dev and prod is stored once. A digest this target has finished with may
	// still be referenced by another target's ledger, and deleting the file would
	// take it out from under them.
	stillWanted := s.referencedDigests()

	for _, e := range dropped {
		if slices.Contains(stillWanted, e.Digest) {
			continue
		}
		if err := s.store.Remove(e.Digest); err != nil {
			warn(fmt.Sprintf("remove artifact %s: %v", e.Digest, err))
		}
	}

	// Registry manifests live under this target's own repository path, so no
	// cross-target check applies. An entry pruned before it was ever packaged has
	// no image recorded, and there is nothing to delete.
	if t.Deleter == nil {
		return
	}
	for _, e := range dropped {
		if e.Image == "" {
			continue
		}
		if err := t.Deleter.Delete(ctx, imageDigest(e.Image)); err != nil {
			if errors.Is(err, image.ErrDeleteNotSupported) {
				// A configuration state, not a fault: say it once and stop trying
				// for this run rather than repeating it per manifest.
				warn(fmt.Sprintf("registry pruning unavailable for %s: set REGISTRY_STORAGE_DELETE_ENABLED=true", name))
				return
			}
			warn(fmt.Sprintf("delete manifest %s: %v", e.Image, err))
		}
	}
}

// referencedDigests collects the artifact digests every target still records.
func (s *Server) referencedDigests() []string {
	var out []string
	for _, t := range s.targets {
		out = append(out, t.Ledger.Digests()...)
	}
	return out
}

// imageDigest extracts the digest from a full reference like "repo@sha256:...".
func imageDigest(ref string) string {
	if i := strings.LastIndex(ref, "@"); i >= 0 {
		return ref[i+1:]
	}
	return ref
}
