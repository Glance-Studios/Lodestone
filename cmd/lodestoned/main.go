package main

import (
	"fmt"
	"net/http"
	"os"

	"github.com/Glance-Studios/Lodestone/internal/config"
	"github.com/Glance-Studios/Lodestone/internal/server"
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

	srv := server.New(version)
	addr := fmt.Sprintf("%s:%d", cfg.Addr, cfg.Port)

	fmt.Printf("lodestoned %s listening on %s (data %s)\n", version, addr, cfg.DataDir)

	if err := http.ListenAndServe(addr, srv.Handler()); err != nil {
		return fmt.Errorf("http server on %s: %w", addr, err)
	}
	return nil
}
