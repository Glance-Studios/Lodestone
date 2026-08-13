package config

import "testing"

// clearEnv blanks every variable Load reads, so a test never picks up whatever
// happens to be set in the shell running it.
func clearEnv(t *testing.T) {
	t.Helper()

	for _, name := range []string{
		"LODESTONE_ADDR", "LODESTONE_PORT", "LODESTONE_DATA_DIR",
		"LODESTONE_TARGETS", "LODESTONE_KUBECONFIG",
	} {
		t.Setenv(name, "")
	}
}

func TestLoadDefaults(t *testing.T) {
	clearEnv(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v, want nil", err)
	}

	want := Config{Addr: "0.0.0.0", Port: 8080, DataDir: "/var/lib/lodestone"}
	if cfg != want {
		t.Errorf("Load() = %+v, want %+v", cfg, want)
	}
}

func TestLoadFromEnv(t *testing.T) {
	clearEnv(t)
	t.Setenv("LODESTONE_ADDR", "127.0.0.1")
	t.Setenv("LODESTONE_PORT", "9000")
	t.Setenv("LODESTONE_DATA_DIR", "/tmp/lodestone")
	t.Setenv("LODESTONE_TARGETS", "/etc/lodestone/targets.json")
	t.Setenv("LODESTONE_KUBECONFIG", "/etc/rancher/k3s/k3s.yaml")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v, want nil", err)
	}

	want := Config{
		Addr:        "127.0.0.1",
		Port:        9000,
		DataDir:     "/tmp/lodestone",
		TargetsFile: "/etc/lodestone/targets.json",
		Kubeconfig:  "/etc/rancher/k3s/k3s.yaml",
	}
	if cfg != want {
		t.Errorf("Load() = %+v, want %+v", cfg, want)
	}
}

// No targets file is a valid state: the agent then serves /status only.
func TestLoadWithoutTargetsFile(t *testing.T) {
	clearEnv(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v, want nil", err)
	}
	if cfg.TargetsFile != "" {
		t.Errorf("TargetsFile = %q, want empty", cfg.TargetsFile)
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
			clearEnv(t)
			t.Setenv("LODESTONE_PORT", tt.port)

			if _, err := Load(); err == nil {
				t.Errorf("Load() with LODESTONE_PORT=%q returned no error", tt.port)
			}
		})
	}
}
