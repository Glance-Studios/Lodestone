package config

import (
	"fmt"
	"os"
	"strconv"
)

type Config struct {
	Addr    string // iface address to listen on
	Port    int    // TCP port
	DataDir string // dir for the ledger and cached data
	Token   string // bearer token for protected endpoints; empty = fail closed
}

const (
	defaultAddr    = "0.0.0.0"
	defaultPort    = 8080
	defaultDataDir = "/var/lib/lodestone"
)

// Load builds a Config from the environment
func Load() (Config, error) {
	cfg := Config{
		Addr:    envOr("LODESTONE_ADDR", defaultAddr),
		Port:    defaultPort,
		DataDir: envOr("LODESTONE_DATA_DIR", defaultDataDir),
		Token:   os.Getenv("LODESTONE_TOKEN"),
	}

	if s := os.Getenv("LODESTONE_PORT"); s != "" {
		port, err := strconv.Atoi(s)
		if err != nil {
			return Config{}, fmt.Errorf("LODESTONE_PORT %q: %w", s, err)
		}
		cfg.Port = port
	}

	if cfg.Port < 1 || cfg.Port > 65535 {
		return Config{}, fmt.Errorf("port %d out of range 1-65535", cfg.Port)
	}

	return cfg, nil
}

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}
