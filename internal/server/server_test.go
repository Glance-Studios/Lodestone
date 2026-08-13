package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/Glance-Studios/Lodestone/internal/api"
)

// -- status -------------------------------------------------------------------

func TestStatusIsPublic(t *testing.T) {
	f := newFixture(t)

	rec := f.do(t, http.MethodGet, "/status", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200 - health probes need this without a token", rec.Code)
	}

	var got api.Status
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding body: %v", err)
	}
	if got.Status != "ok" || got.Version != "test" {
		t.Errorf("status = %+v", got)
	}
	// Target names are advertised, so a client can discover what exists.
	if len(got.Targets) != 2 || got.Targets[0] != "dev-lobby" || got.Targets[1] != "prod-lobby" {
		t.Errorf("Targets = %v, want [dev-lobby prod-lobby] sorted", got.Targets)
	}
}

func TestStatusRejectsPost(t *testing.T) {
	f := newFixture(t)

	// The "GET /status" pattern makes the mux answer 405 for other methods.
	rec := f.do(t, http.MethodPost, "/status", "", "")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("code = %d, want 405", rec.Code)
	}
}

// -- target addressing and auth ----------------------------------------------

// The property that matters most: a token for one target cannot reach another.
func TestTokenIsScopedToItsTarget(t *testing.T) {
	f := newFixture(t)

	tests := []struct {
		name, path, token string
		want              int
	}{
		{"dev token on dev", "/deploy/dev-lobby", devToken, http.StatusOK},
		{"prod token on prod", "/deploy/prod-lobby", prodToken, http.StatusOK},
		{"dev token on prod", "/deploy/prod-lobby", devToken, http.StatusUnauthorized},
		{"prod token on dev", "/deploy/dev-lobby", prodToken, http.StatusUnauthorized},
		{"no token", "/deploy/dev-lobby", "", http.StatusUnauthorized},
		{"nonsense token", "/deploy/dev-lobby", "hunter2", http.StatusUnauthorized},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := f.do(t, http.MethodPost, tt.path, tt.token, "jar bytes")
			if rec.Code != tt.want {
				t.Errorf("code = %d, want %d (body %s)", rec.Code, tt.want, rec.Body)
			}
		})
	}
}

// An unknown target is 404, not 401: a caller with a valid token needs to learn
// they typed the name wrong. Target names are not secrets.
func TestUnknownTargetIs404(t *testing.T) {
	f := newFixture(t)

	rec := f.do(t, http.MethodPost, "/deploy/staging-lobby", devToken, "jar")
	if rec.Code != http.StatusNotFound {
		t.Errorf("code = %d, want 404", rec.Code)
	}
}

// A hostile path segment is rejected by the same rule that constrains config.
func TestInvalidTargetNameIs400(t *testing.T) {
	f := newFixture(t)

	for _, name := range []string{"Dev-Lobby", "dev_lobby", "dev%20lobby"} {
		t.Run(name, func(t *testing.T) {
			rec := f.do(t, http.MethodPost, "/deploy/"+name, devToken, "jar")
			if rec.Code != http.StatusBadRequest {
				t.Errorf("code = %d, want 400", rec.Code)
			}
		})
	}
}

// A deploy with no target in the path has nowhere to go.
func TestDeployWithoutTargetIs404(t *testing.T) {
	f := newFixture(t)

	rec := f.do(t, http.MethodPost, "/deploy", devToken, "jar")
	if rec.Code != http.StatusNotFound {
		t.Errorf("code = %d, want 404 - the target is part of the path now", rec.Code)
	}
}

// -- ledgers are per target ---------------------------------------------------

// Two targets must not see each other's history. This is why the ledgers are
// separate files rather than one file filtered on read.
func TestLedgersAreIsolated(t *testing.T) {
	f := newFixture(t)

	f.do(t, http.MethodPost, "/artifacts/dev-lobby?version=1.0.0-dev", devToken, "dev jar")
	f.do(t, http.MethodPost, "/artifacts/prod-lobby?version=1.0.0-prod", prodToken, "prod jar")

	dev := f.do(t, http.MethodGet, "/artifacts/dev-lobby", devToken, "")
	prod := f.do(t, http.MethodGet, "/artifacts/prod-lobby", prodToken, "")

	if !strings.Contains(dev.Body.String(), "1.0.0-dev") {
		t.Errorf("dev ledger missing its own entry: %s", dev.Body)
	}
	if strings.Contains(dev.Body.String(), "1.0.0-prod") {
		t.Error("the dev ledger contains prod's entry")
	}
	if !strings.Contains(prod.Body.String(), "1.0.0-prod") {
		t.Errorf("prod ledger missing its own entry: %s", prod.Body)
	}
	if strings.Contains(prod.Body.String(), "1.0.0-dev") {
		t.Error("the prod ledger contains dev's entry")
	}
}

// An empty ledger must serialise as [] rather than null.
func TestEmptyLedgerIsAnArray(t *testing.T) {
	f := newFixture(t)

	rec := f.do(t, http.MethodGet, "/artifacts/dev-lobby", devToken, "")
	if got := strings.TrimSpace(rec.Body.String()); got != "[]" {
		t.Errorf("body = %q, want []", got)
	}
}

// Entries record which target they belong to, so one copied out still says.
func TestLedgerEntryNamesItsTarget(t *testing.T) {
	f := newFixture(t)

	f.do(t, http.MethodPost, "/artifacts/dev-lobby", devToken, "jar")

	rec := f.do(t, http.MethodGet, "/artifacts/dev-lobby", devToken, "")
	if !strings.Contains(rec.Body.String(), `"target":"dev-lobby"`) {
		t.Errorf("entry does not name its target: %s", rec.Body)
	}
}

// -- upload -------------------------------------------------------------------

func TestUploadReturnsDigest(t *testing.T) {
	f := newFixture(t)

	rec := f.do(t, http.MethodPost, "/artifacts/dev-lobby", devToken, "jar bytes")
	if rec.Code != http.StatusCreated {
		t.Fatalf("code = %d, want 201", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "sha256:") {
		t.Errorf("body = %s, want a digest", rec.Body)
	}
}

func TestUploadRejectsEmptyBody(t *testing.T) {
	f := newFixture(t)

	rec := f.doRaw(t, http.MethodPost, "/artifacts/dev-lobby", devToken, "")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d, want 400", rec.Code)
	}
}
