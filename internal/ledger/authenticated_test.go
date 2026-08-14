package ledger

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// Entries written before named credentials existed decode with the field absent.
// That must read as false: By on those was whatever the caller claimed, so
// treating them as proven would let them inherit the new scheme's credibility.
func TestLegacyEntriesAreNotAuthenticated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.json")

	// The pre-cutover shape: a By, and no authenticated field at all.
	old := `[{"seq":1,"digest":"sha256:a","by":"cammy","deployed":true}]`
	if err := os.WriteFile(path, []byte(old), 0o644); err != nil {
		t.Fatalf("writing legacy ledger: %v", err)
	}

	l, err := Open(path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}

	e := l.Entries()[0]
	if e.By != "cammy" {
		t.Errorf("By = %q, want it preserved - the record is not rewritten", e.By)
	}
	if e.Authenticated {
		t.Error("a pre-credential entry reports as authenticated")
	}
}

// A new entry carries the flag, and it survives a restart - an audit field that
// only existed in memory would be worthless.
func TestAuthenticatedPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.json")

	l, err := Open(path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if _, err := l.Append(Entry{Digest: "sha256:b", By: "cammy", Authenticated: true}); err != nil {
		t.Fatalf("Append() error = %v", err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopening: %v", err)
	}
	if !reopened.Entries()[0].Authenticated {
		t.Error("Authenticated did not survive a reopen")
	}
}

// The field is always present in JSON, never omitted. A consumer checking
// attribution must be able to distinguish "false" from "this agent is too old to
// tell you" - and an omitempty tag would collapse those two into one.
func TestAuthenticatedIsAlwaysSerialised(t *testing.T) {
	for _, authenticated := range []bool{true, false} {
		b, err := json.Marshal(Entry{Digest: "sha256:c", Authenticated: authenticated})
		if err != nil {
			t.Fatalf("Marshal() error = %v", err)
		}
		if !jsonHasKey(string(b), "authenticated") {
			t.Errorf("authenticated=%v marshalled without the key: %s", authenticated, b)
		}
	}
}

func jsonHasKey(doc, key string) bool {
	var m map[string]any
	if err := json.Unmarshal([]byte(doc), &m); err != nil {
		return false
	}
	_, ok := m[key]
	return ok
}
