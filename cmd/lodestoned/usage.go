package main

import (
	"flag"
	"fmt"
	"os"
)

// usage documents the environment variables and the targets file, not just the
// flags - lodestoned is configured almost entirely outside its command line.
func usage() {
	w := flag.CommandLine.Output()

	fmt.Fprintf(w, `lodestoned %s - artifact-triggered Kubernetes deploy agent

Usage:
  lodestoned [flags]

Flags:
  -version    print the version and exit
  -help       print this message

Server settings come from the environment:
  LODESTONE_ADDR         address to listen on            (default 0.0.0.0)
  LODESTONE_PORT         port to listen on               (default 8080)
  LODESTONE_DATA_DIR     artifacts and ledgers live here (default /var/lib/lodestone)
  LODESTONE_TARGETS      path to the targets JSON file
  LODESTONE_KUBECONFIG   kubeconfig path; unset means in-cluster, then the usual rules

Deploy targets come from the file named by LODESTONE_TARGETS, because the
environment cannot express a map. With no targets file the agent serves /status
and nothing else.

  {
    "targets": {
      "dev-lobby": {
        "namespace":     "hideaway-dev",
        "deployment":    "lobby",
        "container":     "paper",
        "baseImage":     "localhost:5000/hideaway/paper:26.2-87",
        "repo":          "localhost:5000/dev/lobby",
        "destPath":      "/plugins/app.jar",
        "tokenEnv":      "LODESTONE_TOKEN_DEV_LOBBY",
        "healthAddr":    "127.0.0.1:25565",
        "settleTimeout": "12m",
        "maxReplicas":   5
      }
    }
  }

  namespace, deployment, container, baseImage and repo are required.
  Use tokenEnv (naming an environment variable) rather than a literal token, so
  the file can live in a ConfigMap and the secret in a Secret.

  settleTimeout should exceed the Deployment's progressDeadlineSeconds, so that
  Kubernetes gets to report why a rollout failed before we stop watching.
  maxReplicas caps what one deploy may scale to.

Endpoints:
  GET  /status                      liveness, version, target names   (public)
  POST /artifacts/{target}          upload and record                 (target token)
  GET  /artifacts/{target}          that target's ledger              (target token)
  POST /deploy/{target}             upload, package, push, roll out   (target token)

Each target's token reaches only that target. Upload endpoints accept ?version=
and ?by= to stamp the ledger; /deploy also accepts ?replicas= to scale.

Send "Accept: application/x-ndjson" to /deploy for streamed progress. A streamed
deploy always returns 200 - read the final result line for the outcome.

Example:
  export LODESTONE_TARGETS=/etc/lodestone/targets.json
  export LODESTONE_TOKEN_DEV_LOBBY=$(openssl rand -hex 32)
  lodestoned
`, version)

	os.Exit(0)
}
