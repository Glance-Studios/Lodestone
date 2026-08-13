// Command lodestone is the client for the Lodestone deploy agent.
//
// All the logic lives in internal/cli so it can be tested without running a
// binary; this file only supplies the version and the exit code.
package main

import (
	"os"

	"github.com/Glance-Studios/Lodestone/internal/cli"
)

// version is overwritten at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	os.Exit(cli.Main(version))
}
