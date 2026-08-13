package ledger

import (
	"path/filepath"
	"testing"
	"time"
)

// seed builds a ledger with n entries, oldest first, digests sha256:e0..e(n-1).
func seed(t *testing.T, n int) *Ledger {
	t.Helper()

	l, err := Open(filepath.Join(t.TempDir(), "ledger.json"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}

	base := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	for i := range n {
		e := Entry{
			Digest: digestFor(i),
			Size:   int64(i + 1),
			Image:  "reg/repo@sha256:img" + itoa(i),
			At:     base.Add(time.Duration(i) * time.Minute),
		}
		if err := l.Append(e); err != nil {
			t.Fatalf("Append(%d) error = %v", i, err)
		}
	}
	return l
}

func digestFor(i int) string { return "sha256:e" + itoa(i) }

func itoa(i int) string {
	if i < 10 {
		return string(rune('0' + i))
	}
	return string(rune('0'+i/10)) + string(rune('0'+i%10))
}

func digests(entries []Entry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Digest)
	}
	return out
}

func TestPruneKeepsNewest(t *testing.T) {
	l := seed(t, 10)

	dropped, err := l.Prune(3)
	if err != nil {
		t.Fatalf("Prune() error = %v", err)
	}
	if len(dropped) != 7 {
		t.Fatalf("dropped %d, want 7: %v", len(dropped), digests(dropped))
	}

	kept := l.Entries() // newest first
	if len(kept) != 3 {
		t.Fatalf("kept %d, want 3", len(kept))
	}
	// e9, e8, e7 are the newest three.
	for i, want := range []string{"sha256:e9", "sha256:e8", "sha256:e7"} {
		if kept[i].Digest != want {
			t.Errorf("kept[%d] = %q, want %q", i, kept[i].Digest, want)
		}
	}
}

func TestPruneDoesNothingWhenUnderTheLimit(t *testing.T) {
	l := seed(t, 3)

	dropped, err := l.Prune(10)
	if err != nil {
		t.Fatalf("Prune() error = %v", err)
	}
	if dropped != nil {
		t.Errorf("dropped %v, want nothing", digests(dropped))
	}
	if len(l.Entries()) != 3 {
		t.Errorf("kept %d entries, want 3", len(l.Entries()))
	}
}

// The vault's explicit rule: never drop what is deployed, however old.
func TestPruneNeverDropsTheDeployedEntry(t *testing.T) {
	l := seed(t, 10)

	// Mark the OLDEST entry deployed, well outside a window of 3.
	if ok, err := l.MarkDeployed("sha256:e0", "reg/repo@sha256:live"); err != nil || !ok {
		t.Fatalf("MarkDeployed() = %v, %v", ok, err)
	}

	dropped, err := l.Prune(3)
	if err != nil {
		t.Fatalf("Prune() error = %v", err)
	}

	for _, e := range dropped {
		if e.Digest == "sha256:e0" {
			t.Fatal("dropped the deployed entry - rolling back to a pruned record is unrecoverable")
		}
	}

	// It survives, so 4 entries remain: the newest 3 plus the live one.
	kept := l.Entries()
	if len(kept) != 4 {
		t.Errorf("kept %d, want 4 (window of 3 plus the live entry): %v", len(kept), digests(kept))
	}

	var foundLive bool
	for _, e := range kept {
		if e.Digest == "sha256:e0" && e.Deployed {
			foundLive = true
		}
	}
	if !foundLive {
		t.Error("the deployed entry is not in the kept set")
	}
}

// keep below 2 is raised to 2: one entry is live, the next is the rollback target.
func TestPruneFloorsAtTwo(t *testing.T) {
	for _, keep := range []int{0, 1, -5} {
		t.Run("keep "+itoa(max(keep, 0)), func(t *testing.T) {
			l := seed(t, 6)

			if _, err := l.Prune(keep); err != nil {
				t.Fatalf("Prune(%d) error = %v", keep, err)
			}
			if got := len(l.Entries()); got != 2 {
				t.Errorf("Prune(%d) kept %d entries, want 2 - rollback needs the previous revision", keep, got)
			}
		})
	}
}

func TestPrunePersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.json")

	l, err := Open(path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	base := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	for i := range 8 {
		if err := l.Append(Entry{Digest: digestFor(i), At: base.Add(time.Duration(i) * time.Minute)}); err != nil {
			t.Fatalf("Append() error = %v", err)
		}
	}

	if _, err := l.Prune(2); err != nil {
		t.Fatalf("Prune() error = %v", err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopening: %v", err)
	}
	if got := len(reopened.Entries()); got != 2 {
		t.Errorf("after reopen %d entries, want 2 - the prune was not persisted", got)
	}
}

// Dropped entries carry their image reference, so a caller can delete the
// manifest without going back to the ledger it just removed them from.
func TestPruneReturnsImageReferences(t *testing.T) {
	l := seed(t, 5)

	dropped, err := l.Prune(2)
	if err != nil {
		t.Fatalf("Prune() error = %v", err)
	}
	if len(dropped) == 0 {
		t.Fatal("nothing dropped")
	}
	for _, e := range dropped {
		if e.Image == "" {
			t.Errorf("dropped entry %s has no image reference", e.Digest)
		}
	}
}

// -- MarkDeployed -------------------------------------------------------------

// Exactly one entry is deployed at a time.
func TestMarkDeployedIsExclusive(t *testing.T) {
	l := seed(t, 4)

	if ok, err := l.MarkDeployed("sha256:e1", ""); err != nil || !ok {
		t.Fatalf("MarkDeployed(e1) = %v, %v", ok, err)
	}
	if ok, err := l.MarkDeployed("sha256:e3", ""); err != nil || !ok {
		t.Fatalf("MarkDeployed(e3) = %v, %v", ok, err)
	}

	var live []string
	for _, e := range l.Entries() {
		if e.Deployed {
			live = append(live, e.Digest)
		}
	}
	if len(live) != 1 || live[0] != "sha256:e3" {
		t.Errorf("deployed = %v, want only sha256:e3", live)
	}
}

func TestMarkDeployedRecordsTheImage(t *testing.T) {
	l := seed(t, 3)

	if _, err := l.MarkDeployed("sha256:e1", "reg/repo@sha256:newimage"); err != nil {
		t.Fatalf("MarkDeployed() error = %v", err)
	}

	for _, e := range l.Entries() {
		if e.Digest == "sha256:e1" && e.Image != "reg/repo@sha256:newimage" {
			t.Errorf("Image = %q, want the deployed reference", e.Image)
		}
	}
}

func TestMarkDeployedUnknownDigest(t *testing.T) {
	l := seed(t, 3)

	ok, err := l.MarkDeployed("sha256:absent", "")
	if err != nil {
		t.Fatalf("MarkDeployed() error = %v", err)
	}
	if ok {
		t.Error("MarkDeployed() = true for a digest not in the ledger")
	}
}

// -- Digests ------------------------------------------------------------------

func TestDigests(t *testing.T) {
	l := seed(t, 4)

	got := l.Digests()
	if len(got) != 4 {
		t.Fatalf("Digests() returned %d, want 4", len(got))
	}
	for i := range 4 {
		if !contains(got, digestFor(i)) {
			t.Errorf("Digests() missing %s", digestFor(i))
		}
	}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
