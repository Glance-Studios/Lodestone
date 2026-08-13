package target

import (
	"strings"
	"testing"
	"time"
)

// minimal is a valid single-target config, for tests to vary one field of.
const minimal = `{
  "targets": {
    "dev-lobby": {
      "namespace": "hideaway-dev",
      "deployment": "lobby",
      "container": "paper",
      "baseImage": "localhost:30500/base/paper:1.21",
      "repo": "localhost:30500/dev/lobby",
      "token": "tok"
    }
  }
}`

func parse(t *testing.T, doc string) (map[string]Target, error) {
	t.Helper()
	return Parse(strings.NewReader(doc))
}

func mustParse(t *testing.T, doc string) map[string]Target {
	t.Helper()

	got, err := parse(t, doc)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	return got
}

func TestParseMinimal(t *testing.T) {
	targets := mustParse(t, minimal)

	tgt, ok := targets["dev-lobby"]
	if !ok {
		t.Fatalf("target dev-lobby missing; got %v", targets)
	}
	if tgt.Namespace != "hideaway-dev" || tgt.Deployment != "lobby" || tgt.Container != "paper" {
		t.Errorf("coordinates = %+v", tgt)
	}
}

func TestParseAppliesDefaults(t *testing.T) {
	tgt := mustParse(t, minimal)["dev-lobby"]

	if tgt.DestPath != DefaultDestPath {
		t.Errorf("DestPath = %q, want %q", tgt.DestPath, DefaultDestPath)
	}
	if time.Duration(tgt.SettleTimeout) != DefaultSettleTimeout {
		t.Errorf("SettleTimeout = %v, want %v", time.Duration(tgt.SettleTimeout), DefaultSettleTimeout)
	}
	if tgt.MaxReplicas != DefaultMaxReplicas {
		t.Errorf("MaxReplicas = %d, want %d", tgt.MaxReplicas, DefaultMaxReplicas)
	}
}

// Durations read as strings, because a bare number is ambiguous about its unit.
func TestParseDuration(t *testing.T) {
	doc := strings.Replace(minimal, `"token": "tok"`, `"token": "tok", "settleTimeout": "90s"`, 1)

	tgt := mustParse(t, doc)["dev-lobby"]
	if got := time.Duration(tgt.SettleTimeout); got != 90*time.Second {
		t.Errorf("SettleTimeout = %v, want 90s", got)
	}
}

func TestParseRejectsNumericDuration(t *testing.T) {
	doc := strings.Replace(minimal, `"token": "tok"`, `"token": "tok", "settleTimeout": 600`, 1)

	if _, err := parse(t, doc); err == nil {
		t.Error("Parse() error = nil for a numeric duration; the unit would be a guess")
	}
}

// A typo in a field name must fail loudly, not be silently dropped.
func TestParseRejectsUnknownField(t *testing.T) {
	doc := strings.Replace(minimal, `"namespace"`, `"namespce"`, 1)

	err := func() error { _, e := parse(t, doc); return e }()
	if err == nil {
		t.Fatal("Parse() error = nil for a misspelled field")
	}
	if !strings.Contains(err.Error(), "namespce") {
		t.Errorf("error = %v, want it to name the unknown field", err)
	}
}

func TestParseRequiresTargets(t *testing.T) {
	if _, err := parse(t, `{"targets":{}}`); err == nil {
		t.Error("Parse() error = nil for an empty targets map")
	}
}

func TestParseRequiredFields(t *testing.T) {
	for _, field := range []string{"namespace", "deployment", "container", "baseImage", "repo"} {
		t.Run("missing "+field, func(t *testing.T) {
			// Blank the field rather than removing it, so the JSON stays valid.
			doc := strings.Replace(minimal, `"`+field+`": "`, `"`+field+`": "" , "_x": "`, 1)
			doc = strings.Replace(doc, `"_x": "`, `"`+field+`_unused": "`, 1)

			err := func() error { _, e := Parse(strings.NewReader(doc)); return e }()
			if err == nil {
				t.Errorf("Parse() error = nil with %s blank", field)
			}
		})
	}
}

// -- tokens -------------------------------------------------------------------

func TestTokenFromEnvironment(t *testing.T) {
	t.Setenv("LODESTONE_TOKEN_DEV", "from-env")

	doc := strings.Replace(minimal, `"token": "tok"`, `"tokenEnv": "LODESTONE_TOKEN_DEV"`, 1)
	tgt := mustParse(t, doc)["dev-lobby"]

	if got, ok := tgt.Authenticate("from-env"); !ok || got != SharedCredential {
		t.Errorf("Authenticate() = %q, %v; want %q, true", got, ok, SharedCredential)
	}
	// The indirection is resolved and discarded, so nothing downstream re-reads it,
	// and the secret is not left in two places.
	if tgt.TokenEnv != "" || tgt.Token != "" {
		t.Errorf("Token = %q, TokenEnv = %q; want both cleared once resolved", tgt.Token, tgt.TokenEnv)
	}
}

// The single-token form is compatibility, not an identity - so it records as
// "shared" rather than pretending to name someone.
func TestSingleTokenIsNamedShared(t *testing.T) {
	tgt := mustParse(t, minimal)["dev-lobby"]

	if len(tgt.Credentials) != 1 {
		t.Fatalf("got %d credentials, want 1", len(tgt.Credentials))
	}
	if tgt.Credentials[0].Name != SharedCredential {
		t.Errorf("name = %q, want %q", tgt.Credentials[0].Name, SharedCredential)
	}
}

// A named variable that is not set must fail at startup, not on first deploy.
func TestTokenEnvMissingFailsAtLoad(t *testing.T) {
	t.Setenv("LODESTONE_TOKEN_ABSENT", "")

	doc := strings.Replace(minimal, `"token": "tok"`, `"tokenEnv": "LODESTONE_TOKEN_ABSENT"`, 1)

	err := func() error { _, e := Parse(strings.NewReader(doc)); return e }()
	if err == nil {
		t.Fatal("Parse() error = nil for an unset tokenEnv")
	}
	if !strings.Contains(err.Error(), "LODESTONE_TOKEN_ABSENT") {
		t.Errorf("error = %v, want it to name the variable", err)
	}
}

func TestTokenAndTokenEnvAreExclusive(t *testing.T) {
	t.Setenv("LODESTONE_TOKEN_DEV", "x")

	doc := strings.Replace(minimal, `"token": "tok"`, `"token": "tok", "tokenEnv": "LODESTONE_TOKEN_DEV"`, 1)

	if _, err := parse(t, doc); err == nil {
		t.Error("Parse() error = nil with both token and tokenEnv")
	}
}

// A target with no token could never be deployed to, so it is a config error.
func TestTargetWithoutTokenIsRejected(t *testing.T) {
	doc := strings.Replace(minimal, `"token": "tok"`, `"destPath": "/plugins/app.jar"`, 1)

	err := func() error { _, e := Parse(strings.NewReader(doc)); return e }()
	if err == nil {
		t.Fatal("Parse() error = nil for a target with no token")
	}
	if !strings.Contains(err.Error(), "token") {
		t.Errorf("error = %v", err)
	}
}

// -- credentials --------------------------------------------------------------

// withCredentials swaps the minimal config's single token for a credentials list.
func withCredentials(creds string) string {
	return strings.Replace(minimal, `"token": "tok"`, `"credentials": `+creds, 1)
}

func TestCredentialsAuthenticateByName(t *testing.T) {
	tgt := mustParse(t, withCredentials(`[
      {"name": "cammy", "token": "cammy-tok"},
      {"name": "ci",    "token": "ci-tok"}
    ]`))["dev-lobby"]

	tests := []struct {
		token, want string
		ok          bool
	}{
		{"cammy-tok", "cammy", true},
		{"ci-tok", "ci", true},
		{"nope", "", false},
		{"", "", false},
	}
	for _, tt := range tests {
		got, ok := tgt.Authenticate(tt.token)
		if got != tt.want || ok != tt.ok {
			t.Errorf("Authenticate(%q) = %q, %v; want %q, %v", tt.token, got, ok, tt.want, tt.ok)
		}
	}
}

func TestCredentialTokenFromEnvironment(t *testing.T) {
	t.Setenv("LODESTONE_TOKEN_CAMMY", "secret-from-env")

	tgt := mustParse(t, withCredentials(`[
      {"name": "cammy", "env": "LODESTONE_TOKEN_CAMMY"}
    ]`))["dev-lobby"]

	if got, ok := tgt.Authenticate("secret-from-env"); !ok || got != "cammy" {
		t.Errorf("Authenticate() = %q, %v; want cammy, true", got, ok)
	}
	if tgt.Credentials[0].Env != "" {
		t.Errorf("Env = %q, want it cleared once resolved", tgt.Credentials[0].Env)
	}
}

func TestCredentialEnvMissingFailsAtLoad(t *testing.T) {
	t.Setenv("LODESTONE_TOKEN_GONE", "")

	_, err := parse(t, withCredentials(`[{"name": "cammy", "env": "LODESTONE_TOKEN_GONE"}]`))
	if err == nil {
		t.Fatal("Parse() error = nil for an unset credential env var")
	}
	// The message must name both, or an operator with several cannot tell which.
	for _, want := range []string{"cammy", "LODESTONE_TOKEN_GONE"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want it to mention %q", err, want)
		}
	}
}

func TestCredentialsRejectBadConfigurations(t *testing.T) {
	tests := []struct {
		name, creds, wantIn string
	}{
		{
			// Two people, one name: revoking one would revoke the wrong entry.
			name:   "duplicate names",
			creds:  `[{"name": "ci", "token": "a"}, {"name": "ci", "token": "b"}]`,
			wantIn: "duplicate credential name",
		},
		{
			// A shared token cannot be attributed, which defeats the point.
			name:   "shared token",
			creds:  `[{"name": "cammy", "token": "same"}, {"name": "ci", "token": "same"}]`,
			wantIn: "share a token",
		},
		{
			name:   "empty name",
			creds:  `[{"name": "", "token": "a"}]`,
			wantIn: "name",
		},
		{
			// Names are recorded in the ledger and shown back; keep them boring.
			name:   "name needs escaping",
			creds:  `[{"name": "../etc", "token": "a"}]`,
			wantIn: "name",
		},
		{
			name:   "both token and env",
			creds:  `[{"name": "ci", "token": "a", "env": "SOMETHING"}]`,
			wantIn: "not both",
		},
		{
			name:   "no token at all",
			creds:  `[{"name": "ci"}]`,
			wantIn: "needs token or env",
		},
		{
			name:   "empty list",
			creds:  `[]`,
			wantIn: "never be deployed to",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parse(t, withCredentials(tt.creds))
			if err == nil {
				t.Fatalf("Parse() error = nil for %s", tt.name)
			}
			if !strings.Contains(err.Error(), tt.wantIn) {
				t.Errorf("error = %v, want it to mention %q", err, tt.wantIn)
			}
		})
	}
}

// Mixing the two forms is ambiguous about which credential a deploy used.
func TestCredentialsAndTokenAreExclusive(t *testing.T) {
	doc := strings.Replace(minimal, `"token": "tok"`,
		`"token": "tok", "credentials": [{"name": "ci", "token": "a"}]`, 1)

	_, err := parse(t, doc)
	if err == nil {
		t.Fatal("Parse() error = nil with both credentials and token")
	}
	if !strings.Contains(err.Error(), "not both") {
		t.Errorf("error = %v", err)
	}
}

// A Target assembled in code, not parsed, has Token and no Credentials. It must
// still authenticate rather than silently rejecting everything.
func TestHandBuiltTargetHonoursToken(t *testing.T) {
	tgt := Target{Token: "literal"}

	if got, ok := tgt.Authenticate("literal"); !ok || got != SharedCredential {
		t.Errorf("Authenticate() = %q, %v; want %q, true", got, ok, SharedCredential)
	}
	if _, ok := tgt.Authenticate("wrong"); ok {
		t.Error("Authenticate() accepted the wrong token")
	}
}

// A target with neither is not deployable, so it must reject rather than accept
// an empty bearer token.
func TestTargetWithNoCredentialAcceptsNothing(t *testing.T) {
	var tgt Target

	for _, token := range []string{"", "anything"} {
		if _, ok := tgt.Authenticate(token); ok {
			t.Errorf("Authenticate(%q) = true on a target with no credential", token)
		}
	}
}

// -- names --------------------------------------------------------------------

// Names appear in URL paths and in filenames, so traversal and escaping hazards
// must be rejected outright.
func TestParseRejectsBadNames(t *testing.T) {
	bad := []string{
		"../escape",
		"dev/lobby",
		"dev lobby",
		"Dev-Lobby",
		"-leading",
		"",
		strings.Repeat("a", 64),
	}

	for _, name := range bad {
		t.Run("name "+name, func(t *testing.T) {
			doc := strings.Replace(minimal, `"dev-lobby"`, `"`+name+`"`, 1)

			if _, err := Parse(strings.NewReader(doc)); err == nil {
				t.Errorf("Parse() accepted the name %q", name)
			}
		})
	}
}

func TestValidName(t *testing.T) {
	for _, ok := range []string{"dev", "dev-lobby", "a", "prod-survival-2"} {
		if !ValidName(ok) {
			t.Errorf("ValidName(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{"", "../x", "A", "a_b", "a/b"} {
		if ValidName(bad) {
			t.Errorf("ValidName(%q) = true, want false", bad)
		}
	}
}

// -- destPath -----------------------------------------------------------------

func TestDestPathMustBeAbsolute(t *testing.T) {
	doc := strings.Replace(minimal, `"token": "tok"`, `"token": "tok", "destPath": "plugins/app.jar"`, 1)

	if _, err := parse(t, doc); err == nil {
		t.Error("Parse() error = nil for a relative destPath")
	}
}

// -- replicas -----------------------------------------------------------------

func TestCheckReplicas(t *testing.T) {
	tgt := Target{MaxReplicas: 5}

	for _, n := range []int32{0, 1, 3, 5} {
		if err := tgt.CheckReplicas(n); err != nil {
			t.Errorf("CheckReplicas(%d) = %v, want nil", n, err)
		}
	}
	for _, n := range []int32{6, 500, -1} {
		if err := tgt.CheckReplicas(n); err == nil {
			t.Errorf("CheckReplicas(%d) = nil, want an error", n)
		}
	}
}

func TestMaxReplicasFromConfig(t *testing.T) {
	doc := strings.Replace(minimal, `"token": "tok"`, `"token": "tok", "maxReplicas": 3`, 1)

	tgt := mustParse(t, doc)["dev-lobby"]
	if tgt.MaxReplicas != 3 {
		t.Fatalf("MaxReplicas = %d, want 3", tgt.MaxReplicas)
	}
	if err := tgt.CheckReplicas(4); err == nil {
		t.Error("CheckReplicas(4) = nil, want it capped at 3")
	}
}

// -- multiple targets ---------------------------------------------------------

func TestMultipleTargetsWithSeparateTokens(t *testing.T) {
	doc := `{
      "targets": {
        "dev-lobby": {
          "namespace": "hideaway-dev", "deployment": "lobby", "container": "paper",
          "baseImage": "localhost:30500/base/paper:1.21",
          "repo": "localhost:30500/dev/lobby",
          "token": "dev-token"
        },
        "prod-lobby": {
          "namespace": "hideaway-prod", "deployment": "lobby", "container": "paper",
          "baseImage": "localhost:30500/base/paper:1.21",
          "repo": "localhost:30500/prod/lobby",
          "token": "prod-token",
          "maxReplicas": 20,
          "settleTimeout": "20m"
        }
      }
    }`

	targets := mustParse(t, doc)
	if len(targets) != 2 {
		t.Fatalf("got %d targets, want 2", len(targets))
	}

	// Separate credentials per scope is the whole enforcement mechanism: a dev
	// token must not authenticate against prod.
	if _, ok := targets["prod-lobby"].Authenticate("dev-token"); ok {
		t.Error("the dev token authenticates against prod")
	}
	if _, ok := targets["dev-lobby"].Authenticate("prod-token"); ok {
		t.Error("the prod token authenticates against dev")
	}
	if targets["prod-lobby"].MaxReplicas != 20 {
		t.Errorf("prod maxReplicas = %d, want 20", targets["prod-lobby"].MaxReplicas)
	}
	if got := time.Duration(targets["prod-lobby"].SettleTimeout); got != 20*time.Minute {
		t.Errorf("prod settleTimeout = %v, want 20m", got)
	}
	// Defaults still apply to the target that did not set them.
	if targets["dev-lobby"].MaxReplicas != DefaultMaxReplicas {
		t.Errorf("dev maxReplicas = %d, want the default", targets["dev-lobby"].MaxReplicas)
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load("does-not-exist.json"); err == nil {
		t.Error("Load() error = nil for a missing file")
	}
}
