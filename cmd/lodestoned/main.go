package main

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"

	"github.com/Glance-Studios/Lodestone/internal/config"
	"github.com/Glance-Studios/Lodestone/internal/server"
	"github.com/Glance-Studios/Lodestone/internal/store"
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

	if cfg.Token == "" {
		fmt.Fprintln(os.Stderr, "warning: LODESTONE_TOKEN not set; protected endpoints will reject every request")
	}

	st, err := store.New(filepath.Join(cfg.DataDir, "artifacts"))
	if err != nil {
		return fmt.Errorf("open artifact store: %w", err)
	}

	srv := server.New(version, cfg.Token, st)
	addr := fmt.Sprintf("%s:%d", cfg.Addr, cfg.Port)

	fmt.Printf("lodestoned %s listening on %s (data %s)\n", version, addr, cfg.DataDir)

	if err := http.ListenAndServe(addr, srv.Handler()); err != nil {
		return fmt.Errorf("http server on %s: %w", addr, err)
	}
	return nil
}
