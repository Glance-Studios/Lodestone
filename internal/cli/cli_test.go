package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Glance-Studios/Lodestone/internal/api"
)

// run executes the CLI with args and captures both streams. This is the whole
// reason Env carries writers instead of using os.Stdout: no subprocess needed.
type result struct {
	code int
	out  string
	err  string
}

func run(t *testing.T, args ...string) result {
	t.Helper()

	var out, errBuf bytes.Buffer
	env := Env{Out: &out, Err: &errBuf, Version: "test"}

	code := Run(context.Background(), env, args)
	return result{code: code, out: out.String(), err: errBuf.String()}
}

// isolate points the config at a temp file and clears the env overrides, so
// tests never read or write the developer's real config.
func isolate(t *testing.T) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("LODESTONE_CONFIG", path)
	t.Setenv("LODESTONE_API", "")
	t.Setenv("LODESTONE_TOKEN", "")
	t.Setenv("LODESTONE_VERSION", "")
	t.Setenv("LODESTONE_BY", "tester")
	return path
}

// -- dispatch -----------------------------------------------------------------

func TestNoArgsPrintsUsage(t *testing.T) {
	isolate(t)

	got := run(t)
	if got.code != ExitUsage {
		t.Errorf("code = %d, want %d", got.code, ExitUsage)
	}
	if !strings.Contains(got.err, "Usage:") {
		t.Error("usage not printed")
	}
	// Every command must be listed, or it is undiscoverable.
	for _, name := range []string{"status", "deploy", "push", "ledger", "login", "contexts", "version"} {
		if !strings.Contains(got.err, name) {
			t.Errorf("command %q missing from the command list", name)
		}
	}
}

func TestUnknownCommand(t *testing.T) {
	isolate(t)

	got := run(t, "frobnicate")
	if got.code != ExitUsage {
		t.Errorf("code = %d, want %d", got.code, ExitUsage)
	}
	if !strings.Contains(got.err, "unknown command") {
		t.Errorf("stderr = %q, want it to name the problem", got.err)
	}
}

func TestVersionCommand(t *testing.T) {
	isolate(t)

	got := run(t, "version")
	if got.code != ExitOK {
		t.Errorf("code = %d, want 0", got.code)
	}
	if !strings.Contains(got.out, "test") {
		t.Errorf("stdout = %q, want the version", got.out)
	}
}

func TestHelpIsNotAnError(t *testing.T) {
	isolate(t)

	got := run(t, "-h")
	if got.code != ExitOK {
		t.Errorf("code = %d, want 0 - asking for help is not a failure", got.code)
	}
}

func TestCommandRejectsExtraArguments(t *testing.T) {
	isolate(t)

	got := run(t, "version", "extra")
	if got.code != ExitUsage {
		t.Errorf("code = %d, want %d", got.code, ExitUsage)
	}
}

func TestDeployWithoutAFile(t *testing.T) {
	isolate(t)

	got := run(t, "deploy")
	if got.code != ExitUsage {
		t.Errorf("code = %d, want %d", got.code, ExitUsage)
	}
	if !strings.Contains(got.err, "needs a file") {
		t.Errorf("stderr = %q", got.err)
	}
}

// A missing context must be a clear, actionable error rather than a nil panic.
func TestNoContextConfigured(t *testing.T) {
	isolate(t)

	got := run(t, "status")
	if got.code != ExitError {
		t.Errorf("code = %d, want %d", got.code, ExitError)
	}
	if !strings.Contains(got.err, "lodestone login") {
		t.Errorf("stderr = %q, want advice on how to fix it", got.err)
	}
}

// Global flags must parse on either side of the command name.
func TestGlobalFlagsOnEitherSide(t *testing.T) {
	isolate(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"status":"ok","version":"1.0.0","uptime":"1s"}`)
	}))
	defer srv.Close()

	for _, args := range [][]string{
		{"--api", srv.URL, "status"}, // before the command
		{"status", "--api", srv.URL}, // after it
	} {
		got := run(t, args...)
		if got.code != ExitOK {
			t.Errorf("%v: code = %d, want 0 (stderr %q)", args, got.code, got.err)
		}
		if !strings.Contains(got.out, "1.0.0") {
			t.Errorf("%v: stdout = %q", args, got.out)
		}
	}
}

// Flags must also work *after* a positional argument. Stdlib flag stops at the
// first non-flag argument, so this needs interleaved parsing - and
// `lodestone deploy plugin.jar --api http://x` is a natural thing to type.
func TestFlagsAfterPositionalArgument(t *testing.T) {
	isolate(t)
	jar := writeJar(t, "jar bytes")

	srv := httptest.NewServer(ndjson(
		`{"kind":"result","digest":"sha256:abc","image":"r@sha256:def","deployed":true}`,
	))
	defer srv.Close()

	// The flag comes after the filename.
	got := run(t, "deploy", jar, "--api", srv.URL, "--target", "dev-lobby")
	if got.code != ExitOK {
		t.Fatalf("code = %d, want 0 (stderr %q)", got.code, got.err)
	}

	// And interleaved with it.
	got = run(t, "deploy", "--json", jar, "--api", srv.URL, "--target", "dev-lobby")
	if got.code != ExitOK {
		t.Errorf("interleaved: code = %d, want 0 (stderr %q)", got.code, got.err)
	}
}

// -- login / contexts round trip ---------------------------------------------

func TestLoginThenContexts(t *testing.T) {
	isolate(t)

	got := run(t, "login", "dev", "--api", "http://127.0.0.1:8080", "--token", "s3cret")
	if got.code != ExitOK {
		t.Fatalf("login code = %d, stderr %q", got.code, got.err)
	}

	got = run(t, "contexts")
	if got.code != ExitOK {
		t.Fatalf("contexts code = %d", got.code)
	}
	if !strings.Contains(got.out, "dev") {
		t.Errorf("stdout = %q, want the saved context", got.out)
	}
	if !strings.Contains(got.out, "* ") {
		t.Error("the default context is not marked")
	}

	// The token must never be printed - this output gets pasted into chats.
	if strings.Contains(got.out, "s3cret") {
		t.Error("contexts printed the token")
	}
	if !strings.Contains(got.out, "token set") {
		t.Errorf("stdout = %q, want it to report that a token exists", got.out)
	}
}

func TestLoginNeedsAPI(t *testing.T) {
	isolate(t)

	got := run(t, "login", "dev")
	if got.code != ExitUsage {
		t.Errorf("code = %d, want %d", got.code, ExitUsage)
	}
}

func TestSavedContextIsUsedByLaterCommands(t *testing.T) {
	isolate(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer saved-token" {
			t.Errorf("Authorization = %q, want the saved token", got)
		}
		fmt.Fprint(w, `{"status":"ok","version":"9.9.9","uptime":"1s"}`)
	}))
	defer srv.Close()

	if got := run(t, "login", "dev", "--api", srv.URL, "--token", "saved-token"); got.code != ExitOK {
		t.Fatalf("login failed: %q", got.err)
	}

	got := run(t, "status") // no flags: must come from the config
	if got.code != ExitOK {
		t.Fatalf("status code = %d, stderr %q", got.code, got.err)
	}
	if !strings.Contains(got.out, "9.9.9") {
		t.Errorf("stdout = %q", got.out)
	}
}

// -- ledger output ------------------------------------------------------------

func TestLedgerEmpty(t *testing.T) {
	isolate(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `[]`)
	}))
	defer srv.Close()

	got := run(t, "ledger", "--api", srv.URL, "--target", "dev-lobby")
	if got.code != ExitOK {
		t.Fatalf("code = %d", got.code)
	}
	if !strings.Contains(got.out, "nothing published") {
		t.Errorf("stdout = %q, want a friendly empty message", got.out)
	}
}

func TestLedgerTable(t *testing.T) {
	isolate(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `[
			{"digest":"sha256:aaaaaaaaaaaabbbbbbb","size":2048,"version":"1.2.3","by":"cammy","at":"2026-08-13T10:00:00Z"},
			{"digest":"sha256:ccccccccccccddddddd","size":100,"at":"2026-08-12T10:00:00Z"}
		]`)
	}))
	defer srv.Close()

	got := run(t, "ledger", "--api", srv.URL, "--target", "dev-lobby")
	if got.code != ExitOK {
		t.Fatalf("code = %d", got.code)
	}
	if !strings.Contains(got.out, "VERSION") {
		t.Error("no table header")
	}
	if !strings.Contains(got.out, "1.2.3") || !strings.Contains(got.out, "cammy") {
		t.Errorf("stdout = %q, want the entry fields", got.out)
	}
	// Digests are truncated for readability.
	if !strings.Contains(got.out, "aaaaaaaaaaaa") {
		t.Errorf("stdout = %q, want a short digest", got.out)
	}
	// Missing optional fields show a dash rather than a blank column.
	if !strings.Contains(got.out, "-") {
		t.Error("missing version/by should render as -")
	}
	// Sizes are human-readable.
	if !strings.Contains(got.out, "KiB") {
		t.Errorf("stdout = %q, want a human size", got.out)
	}
}

// --json output must be a single parseable document, with nothing else on stdout.
func TestJSONOutputIsParseable(t *testing.T) {
	isolate(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"status":"ok","version":"1.0.0","uptime":"2s"}`)
	}))
	defer srv.Close()

	got := run(t, "status", "--api", srv.URL, "--target", "dev-lobby", "--json")
	if got.code != ExitOK {
		t.Fatalf("code = %d", got.code)
	}

	var st api.Status
	if err := json.Unmarshal([]byte(got.out), &st); err != nil {
		t.Fatalf("stdout is not a single JSON document: %v\n%s", err, got.out)
	}
	if st.Version != "1.0.0" {
		t.Errorf("version = %q", st.Version)
	}
}

// -- deploy ------------------------------------------------------------------

func writeJar(t *testing.T, content string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "plugin.jar")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writing jar: %v", err)
	}
	return path
}

// ndjson serves a canned stream.
func ndjson(lines ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", api.ContentTypeNDJSON)
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for _, l := range lines {
			fmt.Fprintln(w, l)
			if flusher != nil {
				flusher.Flush()
			}
		}
	}
}

func TestDeploySuccess(t *testing.T) {
	isolate(t)
	jar := writeJar(t, "jar bytes")

	srv := httptest.NewServer(ndjson(
		`{"kind":"event","phase":"starting","message":"deploying","at":"2026-08-13T10:00:00Z"}`,
		`{"kind":"event","phase":"succeeded","message":"deployed","at":"2026-08-13T10:00:05Z"}`,
		`{"kind":"result","digest":"sha256:abc","image":"r@sha256:def","deployed":true}`,
	))
	defer srv.Close()

	got := run(t, "deploy", jar, "--api", srv.URL, "--target", "dev-lobby")
	if got.code != ExitOK {
		t.Fatalf("code = %d, want 0 (stderr %q)", got.code, got.err)
	}

	// Progress must be narrated, not just the final result.
	for _, want := range []string{"starting", "succeeded", "deployed"} {
		if !strings.Contains(got.out, want) {
			t.Errorf("stdout missing %q:\n%s", want, got.out)
		}
	}
	if !strings.Contains(got.out, "sha256:abc") {
		t.Errorf("stdout missing the artifact digest:\n%s", got.out)
	}
}

// A rolled-back deploy gets its own exit code, so CI can tell it from a crash.
func TestDeployRolledBackExitsTwo(t *testing.T) {
	isolate(t)
	jar := writeJar(t, "jar bytes")

	srv := httptest.NewServer(ndjson(
		`{"kind":"event","phase":"rolling_back","message":"health checks failed","at":"2026-08-13T10:00:00Z"}`,
		`{"kind":"result","digest":"sha256:abc","deployed":false,"error":"health checks failed: refused"}`,
	))
	defer srv.Close()

	got := run(t, "deploy", jar, "--api", srv.URL, "--target", "dev-lobby")
	if got.code != ExitNotHealthy {
		t.Fatalf("code = %d, want %d", got.code, ExitNotHealthy)
	}
	if !strings.Contains(got.out, "NOT deployed") {
		t.Errorf("stdout should say it did not stick:\n%s", got.out)
	}
	if !strings.Contains(got.err, "health checks failed") {
		t.Errorf("stderr should carry the reason: %q", got.err)
	}
}

// In --json mode stdout must stay a single document, with no progress lines.
func TestDeployJSONHasNoProgressLines(t *testing.T) {
	isolate(t)
	jar := writeJar(t, "jar bytes")

	srv := httptest.NewServer(ndjson(
		`{"kind":"event","phase":"starting","message":"deploying","at":"2026-08-13T10:00:00Z"}`,
		`{"kind":"result","digest":"sha256:abc","image":"r@sha256:def","deployed":true}`,
	))
	defer srv.Close()

	got := run(t, "deploy", jar, "--api", srv.URL, "--target", "dev-lobby", "--json")
	if got.code != ExitOK {
		t.Fatalf("code = %d, stderr %q", got.code, got.err)
	}

	var res api.Result
	if err := json.Unmarshal([]byte(got.out), &res); err != nil {
		t.Fatalf("stdout is not a single JSON document: %v\n%s", err, got.out)
	}
	if !res.Deployed {
		t.Error("Deployed = false")
	}
}

func TestDeployMissingFile(t *testing.T) {
	isolate(t)

	got := run(t, "deploy", filepath.Join(t.TempDir(), "nope.jar"), "--api", "http://127.0.0.1:1", "--target", "dev-lobby")
	if got.code != ExitError {
		t.Errorf("code = %d, want %d", got.code, ExitError)
	}
	if !strings.Contains(got.err, "open") {
		t.Errorf("stderr = %q, want it to name the file problem", got.err)
	}
}

// LODESTONE_VERSION labels the ledger entry, so CI can stamp a build.
func TestPushStampsVersion(t *testing.T) {
	isolate(t)
	t.Setenv("LODESTONE_VERSION", "4.5.6")

	jar := writeJar(t, "jar")

	var gotVersion string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotVersion = r.URL.Query().Get("version")
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"digest":"sha256:abc","size":3}`)
	}))
	defer srv.Close()

	if got := run(t, "push", jar, "--api", srv.URL, "--target", "dev-lobby"); got.code != ExitOK {
		t.Fatalf("code = %d, stderr %q", got.code, got.err)
	}
	if gotVersion != "4.5.6" {
		t.Errorf("version=%q, want 4.5.6", gotVersion)
	}
}

// Who deployed is not the client's to say. The server derives it from the
// credential, so the CLI must not offer a way to influence it - a knob that
// looks like it sets attribution but does not is worse than no knob.
func TestClientSendsNoIdentity(t *testing.T) {
	isolate(t)
	t.Setenv("LODESTONE_BY", "someone-else")
	t.Setenv("USER", "someone-else")

	jar := writeJar(t, "jar")

	var gotBy string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBy = r.URL.Query().Get("by")
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"digest":"sha256:abc","size":3}`)
	}))
	defer srv.Close()

	if got := run(t, "push", jar, "--api", srv.URL, "--target", "dev-lobby"); got.code != ExitOK {
		t.Fatalf("code = %d, stderr %q", got.code, got.err)
	}
	if gotBy != "" {
		t.Errorf("by=%q, want no identity sent at all", gotBy)
	}
}

// Busy and unauthorized are the two failures a user can act on, so both must
// produce advice rather than a bare status code.
func TestActionableErrors(t *testing.T) {
	isolate(t)
	jar := writeJar(t, "jar")

	tests := []struct {
		name   string
		status int
		body   string
		want   string
		args   []string
	}{
		{
			name:   "busy",
			status: http.StatusLocked,
			body:   "a deploy is already in progress",
			want:   "another deploy is in progress",
			args:   []string{"deploy", jar},
		},
		{
			name:   "unauthorized",
			status: http.StatusUnauthorized,
			body:   "unauthorized",
			want:   "token was rejected",
			args:   []string{"ledger"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, tt.body, tt.status)
			}))
			defer srv.Close()

			got := run(t, append(tt.args, "--api", srv.URL, "--target", "dev-lobby")...)
			if got.code != ExitError {
				t.Errorf("code = %d, want %d", got.code, ExitError)
			}
			if !strings.Contains(got.err, tt.want) {
				t.Errorf("stderr = %q, want advice containing %q", got.err, tt.want)
			}
		})
	}
}
