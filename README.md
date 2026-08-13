# 🧭 Lodestone

A deploy agent for Kubernetes: artifact in, health-gated rollout, automatic rollback, and a ledger of
what shipped where.

**The trigger is publishing an artifact, not pushing a commit.** 
Deploying to Kubernetes usually means
adopting a full GitOps stack, where git is the source of truth and deploys follow commits - or writing
a bash script. Neither suits a small team that wants automated, observable deploys dozens of times a
day without a commit being the trigger.

```
POST /deploy  ──▶  store the artifact by content digest
                   record it in the ledger (who, when, version, sha256)
                   append it as a layer onto a cached base image
                   push to the registry
                   update the Deployment to the new digest
                   watch the rollout, gate on health checks
                   unhealthy? roll back and report
```

## Status

**Working prototype, verified against a real cluster.** The pipeline above runs end to end - a real
deploy to k3s, a real automatic rollback with zero downtime, and 132 tests passing under `-race`.

Not yet built, and designed but absent: environment scopes (dev/staging/prod), promotion between them,
ledger retention/pruning, artifact signature verification, prebuilt release binaries, and a container
image. Treat it as something that works rather than something finished.

## Install

No releases yet, so build from source. Go 1.26 or newer:

```bash
go install github.com/Glance-Studios/Lodestone/cmd/lodestone@latest   # the CLI
go install github.com/Glance-Studios/Lodestone/cmd/lodestoned@latest  # the agent
```

Or clone and build, which is what you want for the agent anyway since it runs on a server:

```bash
git clone https://github.com/Glance-Studios/Lodestone.git
cd Lodestone
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o lodestoned ./cmd/lodestoned
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o lodestone  ./cmd/lodestone
```

Both are static binaries with no runtime dependency - the agent is ~27 MB including the whole
Kubernetes client, the CLI ~6 MB. Cross-compiling needs no toolchain:

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o lodestoned-arm64 ./cmd/lodestoned
```

## Quick start

### 1. Run the agent

The agent needs a token, and a deploy target if you want it to deploy. Without a target it is still a
useful upload-and-ledger service, and `POST /deploy` answers `501`.

```bash
export LODESTONE_TOKEN=$(openssl rand -hex 32)
export LODESTONE_DATA_DIR=/var/lib/lodestone

# Where artifacts get packaged to
export LODESTONE_BASE_IMAGE=ghcr.io/you/paper:1.21
export LODESTONE_REPO=ghcr.io/you/builds
export LODESTONE_DEST_PATH=/plugins/app.jar

# What to deploy to
export LODESTONE_NAMESPACE=game
export LODESTONE_DEPLOYMENT=lobby
export LODESTONE_CONTAINER=paper

# Optional health gate — a rollout that settles but does not serve is a failure
export LODESTONE_HEALTH_URL=http://lobby.game.svc:8080/health

lodestoned
```

Run `lodestoned -help` for the full list.

### 2. Point the CLI at it

```bash
lodestone login prod --api https://lodestone.example.com --token "$LODESTONE_TOKEN"
lodestone status
```

Config lives in `~/.config/lodestone/config.json` (mode `0600`, it holds a token). For CI, set
`LODESTONE_API` and `LODESTONE_TOKEN` instead and skip `login` entirely - the environment overrides
the config file completely, so a stray config on a build agent cannot redirect a deploy.

### 3. Deploy

```bash
$ lodestone deploy build/libs/myplugin.jar
deploying build/libs/myplugin.jar
  starting      deploying ghcr.io/you/builds@sha256:acac48… to deployment game/lobby (container paper)
  updating      replacing ghcr.io/you/builds@sha256:fa13ee…
  settling      waiting for the rollout to settle
  checking      gating on 1 health check(s)
  succeeded     deployed ghcr.io/you/builds@sha256:acac48…

artifact  sha256:d43ec8b762ba10b7487998a163c36589057b82114c5c714d726c806827edc2d1
image     ghcr.io/you/builds@sha256:acac48a1457fd604aaee022f1cd583eb1abb25e225bffbc08092a5bffc9b8164
result    deployed
```

Progress streams as it happens. If the rollout stalls or the health gate rejects it, Lodestone puts
the previous digest back and tells you why:

```
  rolling_back  rollout did not settle
                progress deadline exceeded: ReplicaSet "lobby-76bff5bdf4" has timed out progressing
  failed        rollout did not settle
  result        NOT deployed
```

## CLI

| Command | |
|---|---|
| `lodestone deploy <file>` | upload, package, push, roll out, streaming progress |
| `lodestone push <file>` | upload and record, without deploying |
| `lodestone ledger` | what has been published, newest first |
| `lodestone status` | is the agent up, what version |
| `lodestone login [ctx]` | save an address and token as a named context |
| `lodestone contexts` | list configured contexts (never prints tokens) |
| `lodestone version` | client version |

Global flags: `--ctx <name>` to pick a context, `--api` / `--token` to override it, `--json` for
machine-readable output.

**Exit codes** are part of the interface:

| |                                                                                      |
|---|--------------------------------------------------------------------------------------|
| `0` | deployed                                                                             |
| `1` | something went wrong                                                                 |
| `2` | the deploy ran and was **rolled back** - the agent worked, your artifact is not live |
| `64` | bad invocation                                                                       |

`2` exists so a CI script can tell a rejected deploy from a crash.

## Configuration

### Agent (`lodestoned`)

| Variable | Default              | |
|---|----------------------|---|
| `LODESTONE_ADDR` | `0.0.0.0`            | listen address |
| `LODESTONE_PORT` | `8080`               | listen port |
| `LODESTONE_DATA_DIR` | `/var/lib/lodestone` | artifacts and ledger |
| `LODESTONE_TOKEN` | -                    | bearer token. **Unset means every protected request is denied** |
| `LODESTONE_BASE_IMAGE` | -                    | image to append artifacts onto |
| `LODESTONE_REPO` | -                    | registry path to push builds to |
| `LODESTONE_DEST_PATH` | `/plugins/app.jar`   | where the artifact lands inside the image |
| `LODESTONE_NAMESPACE` | -                    | namespace of the target Deployment |
| `LODESTONE_DEPLOYMENT` | -                    | name of the target Deployment |
| `LODESTONE_CONTAINER` | -                    | container to update |
| `LODESTONE_KUBECONFIG` | -                    | unset means in-cluster, then the usual rules |
| `LODESTONE_HEALTH_URL` | -                    | HTTP GET must return 2xx |
| `LODESTONE_HEALTH_ADDR` | -                    | TCP connect must succeed (`host:port`) |

The five unset-by-default deploy variables are all required together; with any missing, deploying is
disabled rather than half-configured.

### CLI (`lodestone`)

| Variable | |
|---|---|
| `LODESTONE_API` | server address. Overrides the config file entirely, including `--ctx` |
| `LODESTONE_TOKEN` | bearer token |
| `LODESTONE_CONFIG` | config file path |
| `LODESTONE_VERSION` | version to stamp on the ledger entry |
| `LODESTONE_BY` | who to record as the publisher |

## HTTP API

| | |                                                        |
|---|---|--------------------------------------------------------|
| `GET` | `/status` | public - health probes need it without a token         |
| `POST` | `/artifacts` | upload and record. `?version=` `&by=` stamp the ledger |
| `GET` | `/artifacts` | read the ledger, newest first                          |
| `POST` | `/deploy` | the whole pipeline                                     |

All but `/status` need `Authorization: Bearer <token>`.

`POST /deploy` returns one JSON object by default. Send `Accept: application/x-ndjson` for a stream of
one object per line, discriminated by `kind`:

```
{"kind":"event","phase":"settling","message":"waiting for the rollout to settle","at":"…"}
{"kind":"result","digest":"sha256:…","image":"…@sha256:…","deployed":true}
```

**A streamed deploy always returns HTTP 200**, even when it fails. A status code is sent with the first
byte of the body and cannot be revised, so the outcome lives in the final `result` line. Read that,
not the status.

## How it works

**Deploy by digest, never by tag.** The Deployment is pinned to `image@sha256:…`. Tags are mutable;
digests are not. This removes an entire class of "the thing running is not the thing you approved" -
and it means an unchanged artifact produces an unchanged digest, so redeploying it is a no-op.

**Layers are appended without a Docker daemon.** `go-containerregistry` pulls a cached base image,
appends the artifact as a single-file layer, and pushes - a few hundred milliseconds, no build
toolchain on the box. Tar entries use a fixed timestamp so identical bytes yield an identical digest,
and the registry skips base layers it already has.

**Rollback restores the digest it replaced**, rather than using `kubectl rollout undo`. Undo walks
ReplicaSet history to find a previous pod template, which is indirect and depends on revision history
that may have been pruned - during testing Kubernetes renumbered a revision out of existence entirely.
Lodestone already knows the exact digest it replaced, so it puts that back.

**Health checks are one interface with three implementations** - HTTP, TCP connect, and exec. They run
concurrently and report *all* failures, because "these three things are broken" is actionable and
"something is broken" is not.

**One deploy at a time.** Interleaved deploys corrupt each other's rollback: if A replaces X with B,
then C replaces B with D, and A's rollout fails, A would roll back to X and silently destroy C's
healthy deploy. A second concurrent deploy gets `423 Locked` and a `Retry-After` instead.

**The agent never executes the artifact.** It receives, packages, and hands a reference to Kubernetes.
A malicious artifact compromises the pod it runs in, never the agent or the cluster.

## Development

```bash
go test ./...          # 132 tests
go test -race ./...    # the race detector, which this code is written against
go vet ./...
gofmt -l .             # should print nothing
```

Tests need no cluster and no registry: `client-go`'s fake clientset and `go-containerregistry`'s
in-process registry are both real implementations backed by memory, so the tests exercise genuine
patch documents, watch handling, and registry traffic.

Three direct dependencies - `client-go`, `k8s.io/api`, `go-containerregistry` - and nothing else.
Everything the standard library can do, it does.

## License

MIT. See [LICENSE](LICENSE).
