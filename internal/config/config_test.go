package config

import "testing"

func TestLoadDefaults(t *testing.T) {
	t.Setenv("LODESTONE_ADDR", "")
	t.Setenv("LODESTONE_PORT", "")
	t.Setenv("LODESTONE_DATA_DIR", "")
	t.Setenv("LODESTONE_TOKEN", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v, want nil", err)
	}

	want := Config{
		Addr:     "0.0.0.0",
		Port:     8080,
		DataDir:  "/var/lib/lodestone",
		DestPath: "/plugins/app.jar",
	}
	if cfg != want {
		t.Errorf("Load() = %+v, want %+v", cfg, want)
	}
}

func TestLoadFromEnv(t *testing.T) {
	t.Setenv("LODESTONE_ADDR", "127.0.0.1")
	t.Setenv("LODESTONE_PORT", "9000")
	t.Setenv("LODESTONE_DATA_DIR", "/tmp/lodestone")
	t.Setenv("LODESTONE_TOKEN", "sekret")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v, want nil", err)
	}

	want := Config{
		Addr:     "127.0.0.1",
		Port:     9000,
		DataDir:  "/tmp/lodestone",
		Token:    "sekret",
		DestPath: "/plugins/app.jar",
	}
	if cfg != want {
		t.Errorf("Load() = %+v, want %+v", cfg, want)
	}
}

func TestDeployEnabled(t *testing.T) {
	full := Config{
		BaseImage:  "ghcr.io/x/paper:1.21",
		Repo:       "ghcr.io/x/builds",
		Namespace:  "game",
		Deployment: "paper-lobby",
		Container:  "paper",
	}

	if !full.DeployEnabled() {
		t.Error("DeployEnabled() = false for a fully configured target")
	}

	// Every field is required: drop any one and deploying must stay off, so a
	// half-configured agent never tries to deploy.
	tests := map[string]func(*Config){
		"no base image": func(c *Config) { c.BaseImage = "" },
		"no repo":       func(c *Config) { c.Repo = "" },
		"no namespace":  func(c *Config) { c.Namespace = "" },
		"no deployment": func(c *Config) { c.Deployment = "" },
		"no container":  func(c *Config) { c.Container = "" },
	}

	for name, drop := range tests {
		t.Run(name, func(t *testing.T) {
			cfg := full // a struct copy, so each case starts clean
			drop(&cfg)

			if cfg.DeployEnabled() {
				t.Errorf("DeployEnabled() = true with %s", name)
			}
		})
	}
}

func TestLoadRejectsBadPort(t *testing.T) {
	tests := []struct {
		name string
		port string
	}{
		{"not a number", "eight-thousand"},
		{"above the range", "70000"},
		{"zero", "0"},
		{"negative", "-1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("LODESTONE_PORT", tt.port)

			if _, err := Load(); err == nil {
				t.Errorf("Load() with LODESTONE_PORT=%q returned no error", tt.port)
			}
		})
	}
}
