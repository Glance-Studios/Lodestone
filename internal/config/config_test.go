package config

import "testing"

func TestLoadDefaults(t *testing.T) {
	t.Setenv("LODESTONE_ADDR", "")
	t.Setenv("LODESTONE_PORT", "")
	t.Setenv("LODESTONE_DATA_DIR", "")

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
	t.Setenv("LODESTONE_ADDR", "127.0.0.1")
	t.Setenv("LODESTONE_PORT", "9000")
	t.Setenv("LODESTONE_DATA_DIR", "/tmp/lodestone")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v, want nil", err)
	}

	want := Config{Addr: "127.0.0.1", Port: 9000, DataDir: "/tmp/lodestone"}
	if cfg != want {
		t.Errorf("Load() = %+v, want %+v", cfg, want)
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
