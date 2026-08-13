// Package target describes the workloads lodestoned can deploy to.
//
// Lodestone addresses many targets and manages none: a target names a Deployment
// that already exists, applied from a manifest by someone else. Creating and
// destroying them is deliberately out of scope.
package target

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Target is one deployable workload.
type Target struct {
	// Kubernetes coordinates. All three are required.
	Namespace  string `json:"namespace"`
	Deployment string `json:"deployment"`
	Container  string `json:"container"`

	// Packaging. BaseImage and Repo are required; DestPath has a default.
	BaseImage string `json:"baseImage"`
	Repo      string `json:"repo"`
	DestPath  string `json:"destPath,omitempty"`

	// Credentials are the named tokens that may deploy to this target. Prefer
	// these over Token: a named credential is an identity, so the ledger can
	// record who deployed rather than who claimed to, and one person can be
	// revoked without rotating the secret for everyone.
	Credentials []Credential `json:"credentials,omitempty"`

	// Token and TokenEnv are the single-shared-token form, kept for
	// compatibility. They resolve to one credential named SharedCredential -
	// which is the honest answer, because a shared secret cannot identify anyone.
	Token    string `json:"token,omitempty"`
	TokenEnv string `json:"tokenEnv,omitempty"`

	// Health gate. Both optional; without either, settled is good enough.
	HealthURL  string `json:"healthURL,omitempty"`
	HealthAddr string `json:"healthAddr,omitempty"`

	// SettleTimeout bounds waiting for a rollout. Generous by default because a
	// workload that saves state on SIGTERM can drain for minutes.
	SettleTimeout Duration `json:"settleTimeout,omitempty"`

	// MaxReplicas caps what a deploy may scale to. Zero means the default.
	MaxReplicas int32 `json:"maxReplicas,omitempty"`

	// Retain is how many ledger entries to keep, and with them their artifacts
	// and image manifests. Zero means the default. Never below 2: one entry is
	// the running revision and the next is the rollback target.
	Retain int `json:"retain,omitempty"`
}

// Credential is one named token permitted to deploy to a target.
//
// The name is not a secret and is not used for authentication - it is the
// identity recorded in the ledger once the token has been accepted.
type Credential struct {
	Name string `json:"name"`

	// Prefer Env: it names an environment variable holding the token, so the
	// targets file can live in a ConfigMap and the secrets in a Secret.
	Token string `json:"token,omitempty"`
	Env   string `json:"env,omitempty"`
}

// SharedCredential is the identity given to the single-token form. It reads as
// what it is in the ledger: a deploy authenticated by a secret several people
// hold, so the actual person is unknown.
const SharedCredential = "shared"

// Defaults applied when a field is unset.
const (
	DefaultDestPath      = "/plugins/app.jar"
	DefaultSettleTimeout = 10 * time.Minute
	DefaultMaxReplicas   = 10
	DefaultRetain        = 10

	// MinRetain is the floor. One entry is the running revision, the next is the
	// rollback target; keeping fewer would break rollback.
	MinRetain = 2
)

// Config is the targets file.
type Config struct {
	Targets map[string]Target `json:"targets"`
}

// Duration is a time.Duration that reads from JSON as a string ("10m"), because
// a bare number in a config file is ambiguous about its unit.
type Duration time.Duration

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("duration must be a string like \"10m\": %w", err)
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("parse duration %q: %w", s, err)
	}
	*d = Duration(parsed)
	return nil
}

func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// nameRE constrains target names. They appear in URL paths and in filenames, so
// anything that could traverse a directory or need escaping is rejected.
var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// Load reads and validates a targets file.
func Load(path string) (map[string]Target, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open targets %s: %w", path, err)
	}
	defer f.Close()

	return Parse(f)
}

// Parse reads and validates targets from r.
func Parse(r io.Reader) (map[string]Target, error) {
	var cfg Config

	dec := json.NewDecoder(r)
	// Reject unknown fields: a typo like "namespce" would otherwise be silently
	// dropped and the target would fail validation for a confusing reason.
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse targets: %w", err)
	}

	if len(cfg.Targets) == 0 {
		return nil, fmt.Errorf("no targets defined")
	}

	// Sorted so that a file with two bad targets always reports the same one
	// first; map iteration order is randomised and would make the error vary.
	out := make(map[string]Target, len(cfg.Targets))
	for _, name := range slices.Sorted(maps.Keys(cfg.Targets)) {
		t, err := normalise(name, cfg.Targets[name])
		if err != nil {
			return nil, err
		}
		out[name] = t
	}
	return out, nil
}

// normalise validates one target and fills in defaults.
func normalise(name string, t Target) (Target, error) {
	if !nameRE.MatchString(name) {
		return Target{}, fmt.Errorf("target %q: name must be lowercase letters, digits and dashes", name)
	}

	required := []struct {
		field, value string
	}{
		{"namespace", t.Namespace},
		{"deployment", t.Deployment},
		{"container", t.Container},
		{"baseImage", t.BaseImage},
		{"repo", t.Repo},
	}
	for _, r := range required {
		if strings.TrimSpace(r.value) == "" {
			return Target{}, fmt.Errorf("target %q: %s is required", name, r.field)
		}
	}

	creds, err := normaliseCredentials(name, t)
	if err != nil {
		return Target{}, err
	}
	t.Credentials = creds
	// Resolved into Credentials; do not keep a second copy of a secret around.
	t.Token, t.TokenEnv = "", ""

	if t.DestPath == "" {
		t.DestPath = DefaultDestPath
	}
	if !strings.HasPrefix(t.DestPath, "/") {
		return Target{}, fmt.Errorf("target %q: destPath %q must be absolute", name, t.DestPath)
	}

	if t.SettleTimeout == 0 {
		t.SettleTimeout = Duration(DefaultSettleTimeout)
	}
	if t.SettleTimeout < 0 {
		return Target{}, fmt.Errorf("target %q: settleTimeout must be positive", name)
	}

	if t.MaxReplicas == 0 {
		t.MaxReplicas = DefaultMaxReplicas
	}
	if t.MaxReplicas < 1 {
		return Target{}, fmt.Errorf("target %q: maxReplicas must be at least 1", name)
	}

	if t.Retain == 0 {
		t.Retain = DefaultRetain
	}
	if t.Retain < MinRetain {
		return Target{}, fmt.Errorf(
			"target %q: retain %d is below %d - one entry is the running revision and the next is the rollback target",
			name, t.Retain, MinRetain)
	}

	return t, nil
}

// normaliseCredentials validates a target's credentials and resolves every token
// from the environment, so a missing variable fails at startup rather than on the
// first deploy attempt.
func normaliseCredentials(name string, t Target) ([]Credential, error) {
	if t.Token != "" && t.TokenEnv != "" {
		return nil, fmt.Errorf("target %q: set token or tokenEnv, not both", name)
	}
	if len(t.Credentials) > 0 && (t.Token != "" || t.TokenEnv != "") {
		return nil, fmt.Errorf("target %q: set credentials or token/tokenEnv, not both", name)
	}

	// The single-token form becomes one credential that says what it is.
	if len(t.Credentials) == 0 {
		if t.Token == "" && t.TokenEnv == "" {
			return nil, fmt.Errorf(
				"target %q: needs credentials, or token/tokenEnv - a target with no credential can never be deployed to", name)
		}
		t.Credentials = []Credential{{Name: SharedCredential, Token: t.Token, Env: t.TokenEnv}}
	}

	seenName := make(map[string]bool, len(t.Credentials))
	// Two credentials sharing a token would make the recorded identity depend on
	// iteration order, so that is rejected rather than resolved arbitrarily.
	seenToken := make(map[string]string, len(t.Credentials))

	out := make([]Credential, 0, len(t.Credentials))
	for i, c := range t.Credentials {
		if !nameRE.MatchString(c.Name) {
			return nil, fmt.Errorf(
				"target %q credential %d: name %q must be lowercase letters, digits and dashes",
				name, i, c.Name)
		}
		if seenName[c.Name] {
			return nil, fmt.Errorf("target %q: duplicate credential name %q", name, c.Name)
		}
		seenName[c.Name] = true

		if c.Token != "" && c.Env != "" {
			return nil, fmt.Errorf("target %q credential %q: set token or env, not both", name, c.Name)
		}
		if c.Env != "" {
			v := os.Getenv(c.Env)
			if v == "" {
				return nil, fmt.Errorf(
					"target %q credential %q: %s is not set in the environment", name, c.Name, c.Env)
			}
			c.Token = v
			c.Env = "" // resolved; do not keep the indirection around
		}
		if c.Token == "" {
			return nil, fmt.Errorf("target %q credential %q: needs token or env", name, c.Name)
		}

		if other, dup := seenToken[c.Token]; dup {
			return nil, fmt.Errorf(
				"target %q: credentials %q and %q share a token, so a deploy could not be attributed to either",
				name, other, c.Name)
		}
		seenToken[c.Token] = c.Name

		out = append(out, c)
	}
	return out, nil
}

// Authenticate returns the name of the credential matching the presented token,
// and whether one did.
//
// Every credential is compared, without an early return, so the time taken does
// not reveal which one matched or how many exist. Tokens are hashed to a fixed
// width first: comparing the raw strings would leak their length, since
// subtle.ConstantTimeCompare returns immediately when lengths differ.
// A Target built in code rather than parsed - a test, or a caller assembling one
// directly - has a Token and no Credentials. Honouring it here means such a
// Target still authenticates, instead of silently rejecting every request
// because a field it never set is empty.
func (t Target) Authenticate(presented string) (string, bool) {
	if presented == "" {
		return "", false
	}
	sum := sha256.Sum256([]byte(presented))

	creds := t.Credentials
	if len(creds) == 0 && t.Token != "" {
		creds = []Credential{{Name: SharedCredential, Token: t.Token}}
	}

	name, found := "", false
	for _, c := range creds {
		want := sha256.Sum256([]byte(c.Token))
		if subtle.ConstantTimeCompare(sum[:], want[:]) == 1 {
			name, found = c.Name, true
		}
	}
	return name, found
}

// Describe names the target for logs.
func (t Target) Describe() string {
	return fmt.Sprintf("%s/%s (container %s)", t.Namespace, t.Deployment, t.Container)
}

// CheckReplicas validates a requested replica count against the target's cap.
func (t Target) CheckReplicas(n int32) error {
	if n < 0 {
		return fmt.Errorf("replicas must not be negative")
	}
	if n > t.MaxReplicas {
		return fmt.Errorf("replicas %d exceeds maxReplicas %d for this target", n, t.MaxReplicas)
	}
	return nil
}

// ValidName reports whether s is usable as a target name. Exported so callers
// can reject a bad path segment before looking it up.
func ValidName(s string) bool { return nameRE.MatchString(s) }
