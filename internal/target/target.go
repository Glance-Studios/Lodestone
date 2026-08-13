// Package target describes the workloads lodestoned can deploy to.
//
// Lodestone addresses many targets and manages none: a target names a Deployment
// that already exists, applied from a manifest by someone else. Creating and
// destroying them is deliberately out of scope.
package target

import (
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

	// Auth. Prefer TokenEnv: it names an environment variable holding the
	// token, so this file can live in a ConfigMap and the secret in a Secret.
	// Token is a literal, for local use.
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
}

// Defaults applied when a field is unset.
const (
	DefaultDestPath      = "/plugins/app.jar"
	DefaultSettleTimeout = 10 * time.Minute
	DefaultMaxReplicas   = 10
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

	if t.Token != "" && t.TokenEnv != "" {
		return Target{}, fmt.Errorf("target %q: set token or tokenEnv, not both", name)
	}

	// Resolve tokenEnv now, so a missing variable fails at startup rather than
	// on the first deploy attempt.
	if t.TokenEnv != "" {
		v := os.Getenv(t.TokenEnv)
		if v == "" {
			return Target{}, fmt.Errorf("target %q: %s is not set in the environment", name, t.TokenEnv)
		}
		t.Token = v
		t.TokenEnv = "" // resolved; do not keep the indirection around
	}
	if t.Token == "" {
		return Target{}, fmt.Errorf("target %q: needs token or tokenEnv - a target with no token can never be deployed to", name)
	}

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

	return t, nil
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
