package main

import (
	"fmt"
	"os"

	"github.com/Glance-Studios/Lodestone/internal/config"
)

var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "lodestoned: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	fmt.Printf("lodestoned %s starting on %s:%d (data %s)\n",
		version, cfg.Addr, cfg.Port, cfg.DataDir)
	return nil
}
