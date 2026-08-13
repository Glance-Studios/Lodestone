package ledger

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// seed builds a ledger with n entries, oldest first, digests sha256:e0..e(n-1).
// It returns the assigned sequence numbers alongside, indexed the same way, so a
// test can name an entry without knowing how Append numbers them.
func seed(t *testing.T, n int) (*Ledger, []uint64) {
	t.Helper()

	l, err := Open(filepath.Join(t.TempDir(), "ledger.json"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}

	base := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	seqs := make([]uint64, 0, n)
	for i := range n {
		e := Entry{
			Digest: digestFor(i),
			Size:   int64(i + 1),
			Image:  "reg/repo@sha256:img" + itoa(i),
			At:     base.Add(time.Duration(i) * time.Minute),
		}
		seq, err := l.Append(e)
		if err != nil {
			t.Fatalf("Append(%d) error = %v", i, err)
		}
		seqs = append(seqs, seq)
	}
	return l, seqs
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
	l, _ := seed(t, 10)

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
	l, _ := seed(t, 3)

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
	l, seqs := seed(t, 10)

	// Mark the OLDEST entry deployed, well outside a window of 3.
	if err := l.MarkDeployed(seqs[0]); err != nil {
		t.Fatalf("MarkDeployed() error = %v", err)
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
			l, _ := seed(t, 6)

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
		if _, err := l.Append(Entry{Digest: digestFor(i), At: base.Add(time.Duration(i) * time.Minute)}); err != nil {
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
	l, _ := seed(t, 5)

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

// -- Seq ----------------------------------------------------------------------

// openTemp returns an empty ledger in its own directory.
func openTemp(t *testing.T) *Ledger {
	t.Helper()

	l, err := Open(filepath.Join(t.TempDir(), "ledger.json"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	return l
}

// Every entry gets its own sequence number, including entries that share a digest.
func TestAppendAssignsUniqueSeqs(t *testing.T) {
	l := openTemp(t)

	seen := map[uint64]bool{}
	for range 5 {
		// The same digest every time: republishing one artifact is legitimate.
		seq, err := l.Append(Entry{Digest: "sha256:same"})
		if err != nil {
			t.Fatalf("Append() error = %v", err)
		}
		if seq == 0 {
			t.Fatal("Append() returned seq 0; zero is reserved for unset")
		}
		if seen[seq] {
			t.Fatalf("seq %d assigned twice", seq)
		}
		seen[seq] = true
	}
}

// Sequence numbers continue across a restart, so a reopened ledger cannot hand
// out an identity an existing entry already holds.
func TestSeqContinuesAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.json")

	first, err := Open(path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	var last uint64
	for range 3 {
		if last, err = first.Append(Entry{Digest: "sha256:x"}); err != nil {
			t.Fatalf("Append() error = %v", err)
		}
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopening: %v", err)
	}
	next, err := reopened.Append(Entry{Digest: "sha256:y"})
	if err != nil {
		t.Fatalf("Append() after reopen error = %v", err)
	}
	if next <= last {
		t.Errorf("seq after reopen = %d, want greater than %d - the sequence restarted", next, last)
	}
}

// Ledgers written before Seq existed decode with Seq 0. Open backfills them,
// because two entries at 0 are indistinguishable to MarkDeployed.
func TestOpenBackfillsMissingSeqs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.json")

	// Hand-written, in the pre-Seq shape.
	old := `[{"digest":"sha256:a","deployed":false},{"digest":"sha256:b","deployed":true}]`
	if err := os.WriteFile(path, []byte(old), 0o644); err != nil {
		t.Fatalf("writing legacy ledger: %v", err)
	}

	l, err := Open(path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}

	seen := map[uint64]bool{}
	for _, e := range l.Entries() {
		if e.Seq == 0 {
			t.Errorf("entry %s still has seq 0 after Open", e.Digest)
		}
		if seen[e.Seq] {
			t.Errorf("seq %d shared by two entries after backfill", e.Seq)
		}
		seen[e.Seq] = true
	}

	// And the backfill is persisted, so it happens once rather than every restart.
	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopening: %v", err)
	}
	for _, e := range reopened.Entries() {
		if e.Seq == 0 {
			t.Errorf("entry %s has seq 0 after reopen - the backfill was not saved", e.Digest)
		}
	}
}

// -- SetImage -----------------------------------------------------------------

func TestSetImageTargetsOneEntry(t *testing.T) {
	l, seqs := seed(t, 3)

	if err := l.SetImage(seqs[1], "reg/repo@sha256:packaged", "reg/base@sha256:base"); err != nil {
		t.Fatalf("SetImage() error = %v", err)
	}

	for _, e := range l.Entries() {
		switch e.Seq {
		case seqs[1]:
			if e.Image != "reg/repo@sha256:packaged" {
				t.Errorf("Image = %q, want the packaged reference", e.Image)
			}
			if e.BaseImage != "reg/base@sha256:base" {
				t.Errorf("BaseImage = %q, want the pinned base", e.BaseImage)
			}
		default:
			if e.BaseImage != "" {
				t.Errorf("entry %s picked up a base image it was not given", e.Digest)
			}
		}
	}
}

func TestSetImageUnknownSeq(t *testing.T) {
	l, _ := seed(t, 2)

	if err := l.SetImage(9999, "reg/repo@sha256:x", ""); err == nil {
		t.Error("SetImage() on an unknown seq returned no error")
	}
}

// -- MarkDeployed -------------------------------------------------------------

// Exactly one entry is deployed at a time.
func TestMarkDeployedIsExclusive(t *testing.T) {
	l, seqs := seed(t, 4)

	if err := l.MarkDeployed(seqs[1]); err != nil {
		t.Fatalf("MarkDeployed(e1) error = %v", err)
	}
	if err := l.MarkDeployed(seqs[3]); err != nil {
		t.Fatalf("MarkDeployed(e3) error = %v", err)
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

// The bug this identity exists to prevent: the same jar deployed twice, onto two
// different base images. Keying on the digest marked both entries live and
// overwrote the first one's image with the second's, orphaning a manifest that
// nothing could then reclaim.
func TestMarkDeployedDoesNotAffectAnEntrySharingItsDigest(t *testing.T) {
	l := openTemp(t)

	const jar = "sha256:samejar"
	base := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)

	firstSeq, err := l.Append(Entry{Digest: jar, Version: "0.1.0", At: base})
	if err != nil {
		t.Fatalf("Append() error = %v", err)
	}
	if err := l.SetImage(firstSeq, "reg/repo@sha256:onbase4", "reg/base@sha256:four"); err != nil {
		t.Fatalf("SetImage() error = %v", err)
	}
	if err := l.MarkDeployed(firstSeq); err != nil {
		t.Fatalf("MarkDeployed() error = %v", err)
	}

	// Same jar again, packaged onto a newer base, so a different manifest.
	secondSeq, err := l.Append(Entry{Digest: jar, Version: "0.1.1", At: base.Add(time.Hour)})
	if err != nil {
		t.Fatalf("Append() error = %v", err)
	}
	if err := l.SetImage(secondSeq, "reg/repo@sha256:onbase5", "reg/base@sha256:five"); err != nil {
		t.Fatalf("SetImage() error = %v", err)
	}
	if err := l.MarkDeployed(secondSeq); err != nil {
		t.Fatalf("MarkDeployed() error = %v", err)
	}

	var live int
	for _, e := range l.Entries() {
		if e.Deployed {
			live++
		}
		switch e.Seq {
		case firstSeq:
			if e.Deployed {
				t.Error("the superseded entry is still marked deployed, so it can never be pruned")
			}
			if e.Image != "reg/repo@sha256:onbase4" {
				t.Errorf("Image = %q, want the manifest it was actually built as - overwriting it orphans that manifest", e.Image)
			}
		case secondSeq:
			if !e.Deployed {
				t.Error("the entry just deployed is not marked live")
			}
		}
	}
	if live != 1 {
		t.Errorf("%d entries marked deployed, want exactly 1", live)
	}
}

func TestMarkDeployedUnknownSeq(t *testing.T) {
	l, _ := seed(t, 3)

	if err := l.MarkDeployed(9999); err == nil {
		t.Error("MarkDeployed() on a seq not in the ledger returned no error")
	}
}

// -- Digests ------------------------------------------------------------------

func TestDigests(t *testing.T) {
	l, _ := seed(t, 4)

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
