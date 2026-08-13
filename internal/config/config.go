// Package config loads lodestoned's server-level configuration from the
// environment.
//
// Only scalars live here. Deploy targets are a map, which the environment cannot
// express, so they come from a JSON file named by LODESTONE_TARGETS - see
// internal/target.
package config

import (
	"fmt"
	"os"
	"strconv"
)

type Config struct {
	Addr    string // iface address to listen on
	Port    int    // TCP port
	DataDir string // dir for artifacts and the per-target ledgers

	TargetsFile string // targets JSON; empty means no target is configured
	Kubeconfig  string // explicit kubeconfig path; empty = in-cluster or default rules
}

const (
	defaultAddr    = "0.0.0.0"
	defaultPort    = 8080
	defaultDataDir = "/var/lib/lodestone"
)

// Load builds a Config from the environment, applying defaults where a
// variable is unset.
func Load() (Config, error) {
	cfg := Config{
		Addr:        envOr("LODESTONE_ADDR", defaultAddr),
		Port:        defaultPort,
		DataDir:     envOr("LODESTONE_DATA_DIR", defaultDataDir),
		TargetsFile: os.Getenv("LODESTONE_TARGETS"),
		Kubeconfig:  os.Getenv("LODESTONE_KUBECONFIG"),
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
