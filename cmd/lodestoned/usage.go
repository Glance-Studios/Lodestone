package main

import (
	"flag"
	"fmt"
	"os"
)

// usage documents the environment variables, not just the flags - lodestoned is
// configured entirely through the environment, so a --help that only listed
// flags would tell the reader almost nothing.
func usage() {
	w := flag.CommandLine.Output()

	fmt.Fprintf(w, `lodestoned %s - artifact-triggered Kubernetes deploy agent

Usage:
  lodestoned [flags]

Flags:
  -version    print the version and exit
  -help       print this message

Configuration is read from the environment.

Server:
  LODESTONE_ADDR         address to listen on            (default 0.0.0.0)
  LODESTONE_PORT         port to listen on               (default 8080)
  LODESTONE_DATA_DIR     artifacts and ledger live here  (default /var/lib/lodestone)
  LODESTONE_TOKEN        bearer token for protected endpoints
                         REQUIRED - unset means every protected request is denied

Deploying (all five required, or POST /deploy answers 501):
  LODESTONE_BASE_IMAGE   image to append artifacts onto, e.g. ghcr.io/you/paper:1.21
  LODESTONE_REPO         registry path to push builds to, e.g. ghcr.io/you/builds
  LODESTONE_NAMESPACE    namespace of the target Deployment
  LODESTONE_DEPLOYMENT   name of the target Deployment
  LODESTONE_CONTAINER    container within the Deployment to update

Deploying (optional):
  LODESTONE_DEST_PATH    where the artifact lands in the image (default /plugins/app.jar)
  LODESTONE_KUBECONFIG   kubeconfig path; unset means in-cluster, then the usual rules

Health gate (optional; unset means "settled is good enough"):
  LODESTONE_HEALTH_URL   HTTP GET must return 2xx
  LODESTONE_HEALTH_ADDR  TCP connect must succeed, as host:port

Endpoints:
  GET  /status           liveness and version              (public)
  POST /artifacts        upload an artifact, record it     (token)
  GET  /artifacts        read the ledger, newest first     (token)
  POST /deploy           upload, package, push, roll out   (token)

Both upload endpoints accept ?version= and ?by= to stamp the ledger entry.

Example:
  LODESTONE_TOKEN=$(openssl rand -hex 32) \
  LODESTONE_BASE_IMAGE=ghcr.io/you/paper:1.21 \
  LODESTONE_REPO=ghcr.io/you/builds \
  LODESTONE_NAMESPACE=game LODESTONE_DEPLOYMENT=lobby LODESTONE_CONTAINER=paper \
  lodestoned
`, version)

	os.Exit(0)
}
