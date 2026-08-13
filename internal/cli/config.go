// Package cli implements the lodestone command-line client.
//
// The logic lives here rather than in cmd/lodestone so it can be tested without
// running a binary.
package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// Context is one server the CLI can talk to.
//
// Tokens are per target on the server, so a context pairs an address with one
// token and the target that token reaches. Addressing a second target means a
// second context.
type Context struct {
	API    string `json:"api"`              // base URL, e.g. http://127.0.0.1:8080
	Token  string `json:"token"`            // bearer token, scoped to Target
	Target string `json:"target,omitempty"` // default target for this context
}

// Config is the on-disk configuration: named contexts plus which one is default.
// The shape mirrors neo's config, so the mental model carries over.
type Config struct {
	Contexts map[string]Context `json:"contexts"`
	Default  string             `json:"default"`

	// path is where this config was loaded from, so Save writes it back to the
	// same place without the caller having to remember.
	path string
}

// ErrNoContext reports that no context could be resolved.
var ErrNoContext = errors.New("no context configured")

// ConfigPath returns the config file location, honouring LODESTONE_CONFIG.
//
// os.UserConfigDir gives the platform-correct base - ~/.config on Linux,
// %AppData% on Windows - rather than hard-coding a Unix path.
func ConfigPath() (string, error) {
	if p := os.Getenv("LODESTONE_CONFIG"); p != "" {
		return p, nil
	}

	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("locate config dir: %w", err)
	}
	return filepath.Join(dir, "lodestone", "config.json"), nil
}

// LoadConfig reads the config file. A missing file is not an error: it returns an
// empty config, so a first run behaves like an unconfigured one rather than
// failing.
func LoadConfig() (*Config, error) {
	path, err := ConfigPath()
	if err != nil {
		return nil, err
	}

	cfg := &Config{Contexts: map[string]Context{}, path: path}

	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}

	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	if cfg.Contexts == nil {
		cfg.Contexts = map[string]Context{}
	}
	cfg.path = path
	return cfg, nil
}

// Save writes the config back, creating the directory if needed.
//
// Mode 0600 - owner read/write only - because this file holds a bearer token.
// The directory is 0700 for the same reason.
func (c *Config) Save() error {
	if c.path == "" {
		p, err := ConfigPath()
		if err != nil {
			return err
		}
		c.path = p
	}

	if err := os.MkdirAll(filepath.Dir(c.path), 0o700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}

	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	data = append(data, '\n')

	// Write to a temp file then rename, so an interrupted write cannot leave a
	// truncated config - the same durability trick the server uses.
	tmp := c.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	if err := os.Rename(tmp, c.path); err != nil {
		return fmt.Errorf("replace config: %w", err)
	}
	return nil
}

// Path reports where this config lives.
func (c *Config) Path() string { return c.path }

// Set adds or replaces a context, making it the default if there was none.
func (c *Config) Set(name string, ctx Context) {
	if c.Contexts == nil {
		c.Contexts = map[string]Context{}
	}
	c.Contexts[name] = ctx
	if c.Default == "" {
		c.Default = name
	}
}

// Resolve picks the context to use, in precedence order:
//
//  1. LODESTONE_API / LODESTONE_TOKEN from the environment - so CI needs no
//     login step and never touches a config file
//  2. the named context, when --ctx was given
//  3. the default context
//
// The returned name is for display ("env" for case 1).
func (c *Config) Resolve(name string) (Context, string, error) {
	if api := os.Getenv("LODESTONE_API"); api != "" {
		return Context{
			API:    api,
			Token:  os.Getenv("LODESTONE_TOKEN"),
			Target: os.Getenv("LODESTONE_TARGET"),
		}, "env", nil
	}

	if name == "" {
		name = c.Default
	}
	if name == "" {
		return Context{}, "", fmt.Errorf("%w: run `lodestone login` or set LODESTONE_API", ErrNoContext)
	}

	ctx, ok := c.Contexts[name]
	if !ok {
		return Context{}, "", fmt.Errorf("%w %q: run `lodestone contexts` to list them", ErrNoContext, name)
	}
	if ctx.API == "" {
		return Context{}, "", fmt.Errorf("context %q has no api address", name)
	}
	return ctx, name, nil
}
