package cli

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// withConfigIn points LODESTONE_CONFIG at a temp file for the duration of a test.
func withConfigIn(t *testing.T) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("LODESTONE_CONFIG", path)
	// Clear the env override so tests exercise the file unless they opt in.
	t.Setenv("LODESTONE_API", "")
	t.Setenv("LODESTONE_TOKEN", "")
	return path
}

func TestLoadConfigMissingFileIsEmpty(t *testing.T) {
	withConfigIn(t)

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig() error = %v, want nil for a first run", err)
	}
	if len(cfg.Contexts) != 0 {
		t.Errorf("Contexts = %v, want empty", cfg.Contexts)
	}
	if cfg.Default != "" {
		t.Errorf("Default = %q, want empty", cfg.Default)
	}
}

func TestSaveAndReload(t *testing.T) {
	withConfigIn(t)

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}

	cfg.Set("dev", Context{API: "http://127.0.0.1:8080", Token: "tok"})
	if err := cfg.Save(); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	reloaded, err := LoadConfig()
	if err != nil {
		t.Fatalf("reloading: %v", err)
	}

	got, ok := reloaded.Contexts["dev"]
	if !ok {
		t.Fatalf("context dev missing after reload; got %v", reloaded.Contexts)
	}
	if got.API != "http://127.0.0.1:8080" || got.Token != "tok" {
		t.Errorf("context = %+v, want the saved values", got)
	}
	// The first context saved becomes the default.
	if reloaded.Default != "dev" {
		t.Errorf("Default = %q, want dev", reloaded.Default)
	}
}

// The config holds a bearer token, so it must not be world-readable.
func TestSaveUsesRestrictivePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix file modes do not apply on windows")
	}

	path := withConfigIn(t)

	cfg, _ := LoadConfig()
	cfg.Set("dev", Context{API: "http://x", Token: "secret"})
	if err := cfg.Save(); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("config mode = %o, want 600 - it holds a token", mode)
	}
}

func TestSaveLeavesNoTempFile(t *testing.T) {
	path := withConfigIn(t)

	cfg, _ := LoadConfig()
	cfg.Set("dev", Context{API: "http://x"})
	if err := cfg.Save(); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	if _, err := os.Stat(path + ".tmp"); err == nil {
		t.Error("a .tmp file survived Save()")
	}
}

func TestLoadConfigRejectsMalformed(t *testing.T) {
	path := withConfigIn(t)
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("writing malformed config: %v", err)
	}

	if _, err := LoadConfig(); err == nil {
		t.Error("LoadConfig() error = nil for a malformed file")
	}
}

// -- Resolve, which is where the precedence rules live ------------------------

func TestResolvePrefersEnvironment(t *testing.T) {
	withConfigIn(t)

	cfg, _ := LoadConfig()
	cfg.Set("dev", Context{API: "http://from-file", Token: "file-token"})

	t.Setenv("LODESTONE_API", "http://from-env")
	t.Setenv("LODESTONE_TOKEN", "env-token")

	// Even naming a context explicitly must not beat the environment - CI sets
	// these and should never be redirected by a stray config file.
	got, name, err := cfg.Resolve("dev")
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if got.API != "http://from-env" || got.Token != "env-token" {
		t.Errorf("resolved %+v, want the environment values", got)
	}
	if name != "env" {
		t.Errorf("name = %q, want env", name)
	}
}

func TestResolveNamedContext(t *testing.T) {
	withConfigIn(t)

	cfg, _ := LoadConfig()
	cfg.Set("dev", Context{API: "http://dev"})
	cfg.Set("prod", Context{API: "http://prod"})

	got, name, err := cfg.Resolve("prod")
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if got.API != "http://prod" {
		t.Errorf("API = %q, want http://prod", got.API)
	}
	if name != "prod" {
		t.Errorf("name = %q, want prod", name)
	}
}

func TestResolveFallsBackToDefault(t *testing.T) {
	withConfigIn(t)

	cfg, _ := LoadConfig()
	cfg.Set("dev", Context{API: "http://dev"})
	cfg.Set("prod", Context{API: "http://prod"})
	cfg.Default = "prod"

	got, name, err := cfg.Resolve("") // no --ctx given
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if got.API != "http://prod" || name != "prod" {
		t.Errorf("resolved %q/%+v, want the default context prod", name, got)
	}
}

func TestResolveErrors(t *testing.T) {
	withConfigIn(t)

	t.Run("nothing configured at all", func(t *testing.T) {
		cfg, _ := LoadConfig()
		if _, _, err := cfg.Resolve(""); !errors.Is(err, ErrNoContext) {
			t.Errorf("Resolve() error = %v, want ErrNoContext", err)
		}
	})

	t.Run("unknown context name", func(t *testing.T) {
		cfg, _ := LoadConfig()
		cfg.Set("dev", Context{API: "http://dev"})

		if _, _, err := cfg.Resolve("staging"); !errors.Is(err, ErrNoContext) {
			t.Errorf("Resolve() error = %v, want ErrNoContext", err)
		}
	})

	t.Run("context with no api", func(t *testing.T) {
		cfg, _ := LoadConfig()
		cfg.Set("broken", Context{Token: "tok"})

		if _, _, err := cfg.Resolve("broken"); err == nil {
			t.Error("Resolve() error = nil for a context with no api")
		}
	})
}
