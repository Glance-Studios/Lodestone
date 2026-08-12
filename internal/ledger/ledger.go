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
	Digest   string    `json:"digest"`
	Size     int64     `json:"size"`
	Version  string    `json:"version,omitempty"`
	By       string    `json:"by,omitempty"`
	At       time.Time `json:"at"`
	Deployed bool      `json:"deployed"`
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
