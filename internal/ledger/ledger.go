// Package ledger records what was published, by whom, and when.
package ledger

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"
)

// Entry is one line of the ledger: an artifact that was published.
type Entry struct {
	Digest string `json:"digest"`
	Size   int64  `json:"size"`

	// Target names the workload this was published for. Ledgers are per-target,
	// so this is redundant on disk - it is here so an entry copied out of one
	// still says what it belongs to.
	Target string `json:"target,omitempty"`

	Version string    `json:"version,omitempty"`
	By      string    `json:"by,omitempty"`
	At      time.Time `json:"at"`

	// Replicas records the count a deploy asked for, when it asked for one.
	Replicas *int32 `json:"replicas,omitempty"`

	Deployed bool   `json:"deployed"`
	Image    string `json:"image,omitempty"` // the pushed image, once packaged

	// BaseImage is the base this was appended onto, pinned by digest even when
	// the target names a moving tag. Without it, "which world was this built on?"
	// has no answer once the tag has moved.
	BaseImage string `json:"baseImage,omitempty"`
}

// Ledger is an append-only record persisted as a single JSON file. It is safe
// for concurrent use: every HTTP request is its own goroutine.
type Ledger struct {
	path string

	mu      sync.Mutex
	entries []Entry
}

// Open loads the ledger at path, creating an empty one if the file is absent.
func Open(path string) (*Ledger, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create ledger dir: %w", err)
	}

	l := &Ledger{path: path}

	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return l, nil // first run: an empty ledger is not an error
	}
	if err != nil {
		return nil, fmt.Errorf("read ledger %s: %w", path, err)
	}

	if err := json.Unmarshal(data, &l.entries); err != nil {
		return nil, fmt.Errorf("decode ledger %s: %w", path, err)
	}
	return l, nil
}

// Append records an entry and persists the ledger. It stamps At if unset.
func (l *Ledger) Append(e Entry) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if e.At.IsZero() {
		e.At = time.Now().UTC()
	}
	l.entries = append(l.entries, e)

	if err := l.save(); err != nil {
		// Roll the in-memory state back so it keeps matching the file on disk.
		l.entries = l.entries[:len(l.entries)-1]
		return err
	}
	return nil
}

// Entries returns a copy of the ledger, newest first. A copy, because handing
// out the internal slice would let a caller mutate it without the lock.
func (l *Ledger) Entries() []Entry {
	l.mu.Lock()
	defer l.mu.Unlock()

	out := slices.Clone(l.entries)
	slices.SortFunc(out, func(a, b Entry) int {
		return b.At.Compare(a.At) // b before a == descending
	})
	return out
}

// MarkDeployed records that a digest is the one now live, clearing the flag from
// whatever held it before. Exactly one entry is deployed at a time.
//
// It reports whether the digest was found, so a caller can tell "recorded" from
// "that digest is not in this ledger".
func (l *Ledger) MarkDeployed(digest, imageRef, baseRef string) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	found := false
	for i := range l.entries {
		if l.entries[i].Digest == digest {
			l.entries[i].Deployed = true
			if imageRef != "" {
				l.entries[i].Image = imageRef
			}
			if baseRef != "" {
				l.entries[i].BaseImage = baseRef
			}
			found = true
		} else {
			l.entries[i].Deployed = false
		}
	}
	if !found {
		return false, nil
	}
	return true, l.save()
}

// Prune keeps the newest keep entries and drops the rest, returning the dropped
// entries so a caller can remove what they referenced. Whole entries rather than
// digests, because each one records the image it was packaged into.
//
// A deployed entry is never dropped, however old. Rolling back to a revision
// whose record was pruned is not a recoverable position, so the entry survives
// even if it falls outside the window - which means Prune can return fewer than
// it was asked to drop, on purpose.
//
// keep below 2 is raised to 2: one entry is the running revision and the next is
// the rollback target, so a tighter window would break rollback.
func (l *Ledger) Prune(keep int) ([]Entry, error) {
	if keep < 2 {
		keep = 2
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	if len(l.entries) <= keep {
		return nil, nil
	}

	// Newest first, so the window is the front of the slice.
	byNewest := slices.Clone(l.entries)
	slices.SortFunc(byNewest, func(a, b Entry) int { return b.At.Compare(a.At) })

	kept := make([]Entry, 0, keep+1)
	var dropped []Entry

	for i, e := range byNewest {
		switch {
		case i < keep:
			kept = append(kept, e)
		case e.Deployed:
			// Outside the window but live: keep it anyway.
			kept = append(kept, e)
		default:
			dropped = append(dropped, e)
		}
	}

	if len(dropped) == 0 {
		return nil, nil
	}

	previous := l.entries
	l.entries = kept

	if err := l.save(); err != nil {
		l.entries = previous // keep memory consistent with disk
		return nil, err
	}
	return dropped, nil
}

// Digests returns every digest the ledger still records. Used to decide whether a
// stored artifact is referenced by any target before deleting it.
func (l *Ledger) Digests() []string {
	l.mu.Lock()
	defer l.mu.Unlock()

	out := make([]string, 0, len(l.entries))
	for _, e := range l.entries {
		out = append(out, e.Digest)
	}
	return out
}

// save writes the ledger to disk. The caller must hold l.mu.
func (l *Ledger) save() error {
	data, err := json.MarshalIndent(l.entries, "", "  ")
	if err != nil {
		return fmt.Errorf("encode ledger: %w", err)
	}

	// Write beside the target then rename, so a crash mid-write cannot leave a
	// truncated ledger - the same durability trick the artifact store uses.
	tmp := l.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("write ledger: %w", err)
	}
	if err := os.Rename(tmp, l.path); err != nil {
		return fmt.Errorf("replace ledger: %w", err)
	}
	return nil
}
