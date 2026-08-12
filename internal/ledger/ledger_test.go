package ledger

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func newTestLedger(t *testing.T) (*Ledger, string) {
	t.Helper()

	path := filepath.Join(t.TempDir(), "ledger.json")
	l, err := Open(path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	return l, path
}

func TestOpenMissingFileIsEmpty(t *testing.T) {
	l, _ := newTestLedger(t)

	if got := l.Entries(); len(got) != 0 {
		t.Errorf("Entries() = %v, want empty on first run", got)
	}
}

func TestAppendAndRead(t *testing.T) {
	l, _ := newTestLedger(t)

	if err := l.Append(Entry{Digest: "sha256:aaa", Size: 1, Version: "0.1.0", By: "cammy"}); err != nil {
		t.Fatalf("Append() error = %v", err)
	}

	entries := l.Entries()
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	if entries[0].Digest != "sha256:aaa" {
		t.Errorf("Digest = %q, want sha256:aaa", entries[0].Digest)
	}
	if entries[0].By != "cammy" {
		t.Errorf("By = %q, want cammy", entries[0].By)
	}
	// Append stamps At when it is unset.
	if entries[0].At.IsZero() {
		t.Error("At is zero; Append should stamp it")
	}
}

// The ledger must survive a restart - that is the whole point of persisting it.
func TestPersistsAcrossReopen(t *testing.T) {
	l, path := newTestLedger(t)

	if err := l.Append(Entry{Digest: "sha256:bbb", Size: 2}); err != nil {
		t.Fatalf("Append() error = %v", err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopening: %v", err)
	}

	entries := reopened.Entries()
	if len(entries) != 1 || entries[0].Digest != "sha256:bbb" {
		t.Errorf("after reopen got %+v, want the one appended entry", entries)
	}
}

func TestEntriesNewestFirst(t *testing.T) {
	l, _ := newTestLedger(t)

	base := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	// Appended oldest-first on purpose, so sorting has work to do.
	for i, d := range []string{"sha256:old", "sha256:mid", "sha256:new"} {
		e := Entry{Digest: d, At: base.Add(time.Duration(i) * time.Hour)}
		if err := l.Append(e); err != nil {
			t.Fatalf("Append() error = %v", err)
		}
	}

	entries := l.Entries()
	want := []string{"sha256:new", "sha256:mid", "sha256:old"}
	for i, w := range want {
		if entries[i].Digest != w {
			t.Errorf("entries[%d] = %q, want %q", i, entries[i].Digest, w)
		}
	}
}

// Entries returns a copy: mutating it must not corrupt the ledger.
func TestEntriesReturnsACopy(t *testing.T) {
	l, _ := newTestLedger(t)

	if err := l.Append(Entry{Digest: "sha256:ccc"}); err != nil {
		t.Fatalf("Append() error = %v", err)
	}

	got := l.Entries()
	got[0].Digest = "clobbered"

	if l.Entries()[0].Digest != "sha256:ccc" {
		t.Error("mutating the returned slice changed the ledger; return a copy")
	}
}

// Concurrent appends from many goroutines - run with -race to prove the lock
// actually protects the slice and the file.
func TestConcurrentAppends(t *testing.T) {
	l, _ := newTestLedger(t)

	const n = 50
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = l.Append(Entry{Digest: "sha256:", Size: int64(i)})
		}()
	}
	wg.Wait()

	if got := len(l.Entries()); got != n {
		t.Errorf("got %d entries after %d concurrent appends, want %d", got, n, n)
	}
}

func TestOpenRejectsCorruptLedger(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.json")
	if err := os.WriteFile(path, []byte(`{not json`), 0o644); err != nil {
		t.Fatalf("writing corrupt file: %v", err)
	}

	if _, err := Open(path); err == nil {
		t.Error("Open() on a corrupt ledger returned no error")
	}
}
