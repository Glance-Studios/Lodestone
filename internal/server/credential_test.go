package server

import (
	"net/http"
	"strings"
	"testing"

	"github.com/Glance-Studios/Lodestone/internal/target"
)

// Named credentials for the attribution tests. Distinct per person, which is the
// whole point: a shared secret cannot say who deployed.
const (
	cammyToken = "cammy-token"
	ciToken    = "ci-token"
)

// withNamedCredentials replaces a target's single token with two named ones.
func withNamedCredentials(f *fixture, name string) {
	t := f.srv.targets[name]
	t.Config.Token = ""
	t.Config.Credentials = []target.Credential{
		{Name: "cammy", Token: cammyToken},
		{Name: "ci", Token: ciToken},
	}
}

// The ledger records the credential that authenticated the request.
func TestLedgerRecordsTheAuthenticatedCredential(t *testing.T) {
	f := newFixture(t)
	withNamedCredentials(f, "dev-lobby")

	rec := f.do(t, http.MethodPost, "/artifacts/dev-lobby", ciToken, "jar")
	if rec.Code != http.StatusCreated {
		t.Fatalf("code = %d, want 201 (body %s)", rec.Code, rec.Body)
	}

	entries := f.devLedger.Entries()
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	if entries[0].By != "ci" {
		t.Errorf("By = %q, want ci - the credential that authenticated", entries[0].By)
	}
}

// The crux of the change: ?by= used to be taken at face value, so anyone holding
// the token could write any name into the audit trail. It must now be ignored in
// favour of the authenticated identity.
func TestByQueryParameterCannotForgeIdentity(t *testing.T) {
	f := newFixture(t)
	withNamedCredentials(f, "dev-lobby")

	// Authenticate as ci, claim to be cammy.
	rec := f.do(t, http.MethodPost, "/artifacts/dev-lobby?by=cammy", ciToken, "jar")
	if rec.Code != http.StatusCreated {
		t.Fatalf("code = %d, want 201 (body %s)", rec.Code, rec.Body)
	}

	got := f.devLedger.Entries()[0].By
	if got == "cammy" {
		t.Fatal("?by= overrode the authenticated identity - the audit trail is forgeable")
	}
	if got != "ci" {
		t.Errorf("By = %q, want ci", got)
	}
}

// Each credential is recorded as itself, so the ledger distinguishes two people
// deploying to one target.
func TestCredentialsAreDistinguishedInTheLedger(t *testing.T) {
	f := newFixture(t)
	withNamedCredentials(f, "dev-lobby")

	f.do(t, http.MethodPost, "/artifacts/dev-lobby", cammyToken, "first jar")
	f.do(t, http.MethodPost, "/artifacts/dev-lobby", ciToken, "second jar")

	entries := f.devLedger.Entries() // newest first
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(entries))
	}
	if entries[0].By != "ci" || entries[1].By != "cammy" {
		t.Errorf("By values = %q, %q; want ci, cammy", entries[0].By, entries[1].By)
	}
}

// A revoked credential is simply removed. The others keep working, which is the
// reason for having them: no rotation for everyone when one person leaves.
func TestRemovingOneCredentialLeavesTheOthers(t *testing.T) {
	f := newFixture(t)
	withNamedCredentials(f, "dev-lobby")

	// Revoke ci.
	creds := f.srv.targets["dev-lobby"].Config.Credentials
	f.srv.targets["dev-lobby"].Config.Credentials = creds[:1]

	if rec := f.do(t, http.MethodPost, "/artifacts/dev-lobby", ciToken, "jar"); rec.Code != http.StatusUnauthorized {
		t.Errorf("revoked credential got %d, want 401", rec.Code)
	}
	if rec := f.do(t, http.MethodPost, "/artifacts/dev-lobby", cammyToken, "jar"); rec.Code != http.StatusCreated {
		t.Errorf("remaining credential got %d, want 201", rec.Code)
	}
}

// A credential is scoped to its target, exactly as the single token was.
func TestCredentialDoesNotCrossTargets(t *testing.T) {
	f := newFixture(t)
	withNamedCredentials(f, "dev-lobby")

	if rec := f.do(t, http.MethodPost, "/artifacts/prod-lobby", cammyToken, "jar"); rec.Code != http.StatusUnauthorized {
		t.Errorf("dev credential reached prod: got %d, want 401", rec.Code)
	}
}

// The compatibility path: a target still configured with one shared token records
// "shared" rather than a name it cannot know.
func TestSingleTokenRecordsShared(t *testing.T) {
	f := newFixture(t)

	f.do(t, http.MethodPost, "/artifacts/dev-lobby?by=cammy", devToken, "jar")

	if got := f.devLedger.Entries()[0].By; got != target.SharedCredential {
		t.Errorf("By = %q, want %q", got, target.SharedCredential)
	}
}

// An entry written by the current agent is marked as proved, so an auditor can
// tell it from a pre-credential row where By was merely claimed.
func TestLedgerMarksEntriesAuthenticated(t *testing.T) {
	f := newFixture(t)
	withNamedCredentials(f, "dev-lobby")

	f.do(t, http.MethodPost, "/artifacts/dev-lobby", cammyToken, "jar")

	if !f.devLedger.Entries()[0].Authenticated {
		t.Error("entry written by an authenticated request is not marked as such")
	}
}

// The flag reaches the wire, so tooling can check attribution without scraping a
// rendered table.
func TestArtifactsListExposesAuthenticated(t *testing.T) {
	f := newFixture(t)
	withNamedCredentials(f, "dev-lobby")

	f.do(t, http.MethodPost, "/artifacts/dev-lobby", cammyToken, "jar")

	rec := f.do(t, http.MethodGet, "/artifacts/dev-lobby", cammyToken, "")
	if !strings.Contains(rec.Body.String(), `"authenticated":true`) {
		t.Errorf("listing omits the attribution flag: %s", rec.Body)
	}
}
