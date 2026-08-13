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

**Working prototype, verified against real infrastructure.** The pipeline above runs end to end: a
plugin jar deployed onto a live Paper server on k3s, digest-pinned, with the health gate passing on a
TCP probe - and a real automatic rollback with zero downtime when a rollout stalled. 212 tests pass
under `-race`.

Not yet built, and designed but absent: environment scopes (dev/staging/prod), promotion between them,
artifact signature verification, scoped RBAC with the agent running as a pod,
prebuilt release binaries, and a container image. Treat it as something that works rather than
something finished.

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

### 1. Describe your targets

A **target** is a Deployment that already exists, applied from a manifest by you. Lodestone addresses
targets; it does not create them. Targets live in a JSON file because the environment cannot express a
map:

```json
{
  "targets": {
    "dev-lobby": {
      "namespace":     "hideaway-dev",
      "deployment":    "lobby",
      "container":     "paper",
      "baseImage":     "localhost:5000/hideaway/lobby-base:current",
      "repo":          "localhost:5000/dev/lobby",
      "destPath":      "/plugins/app.jar",
      "credentials": [
        { "name": "cammy", "env": "LODESTONE_TOKEN_DEV_LOBBY_CAMMY" },
        { "name": "ci",    "env": "LODESTONE_TOKEN_DEV_LOBBY_CI" }
      ],
      "healthAddr":    "127.0.0.1:25565",
      "settleTimeout": "12m",
      "maxReplicas":   4
    }
  }
}
```

`namespace`, `deployment`, `container`, `baseImage` and `repo` are required.

**Credentials are identities, not just secrets.** Each one names who is deploying, and that name -
not anything the request claims - is what the ledger records. Give every person and every automation
its own, because the alternative costs you three things: you cannot tell who deployed, you cannot
revoke one person without rotating the secret for everyone, and a leak gives you no idea whose it was.
Prefer **`env`** over a literal `token` so the file can live in a ConfigMap and the secrets in a
Secret; a missing variable fails at startup rather than on someone's first deploy.

Two credentials may not share a token, and two may not share a name - either would make a deploy
impossible to attribute.

The older single-token form (`token` / `tokenEnv`, exclusive with `credentials`) still works and is
recorded as **`shared`**. That is deliberate: a secret several people hold cannot identify any of
them, and the ledger should say so rather than name a suspect.

Set **`settleTimeout` longer than the Deployment's `progressDeadlineSeconds`**, so Kubernetes gets to
report *why* a rollout failed before Lodestone stops watching. `maxReplicas` caps what one deploy may
scale to.

### 2. Run the agent

```bash
export LODESTONE_DATA_DIR=/var/lib/lodestone
export LODESTONE_TARGETS=/etc/lodestone/targets.json
export LODESTONE_KUBECONFIG=/etc/rancher/k3s/k3s.yaml   # unset = in-cluster

export LODESTONE_TOKEN_DEV_LOBBY_CAMMY=$(openssl rand -hex 32)
export LODESTONE_TOKEN_DEV_LOBBY_CI=$(openssl rand -hex 32)

lodestoned
```

With no targets file the agent serves `/status` and nothing else, which is a fine state for a fresh
install. Run `lodestoned -help` for the full reference.

### 3. Point the CLI at it

Each credential reaches exactly one target, so a context pairs an address, a token and that target:

```bash
lodestone login dev --api https://lodestone.example.com \
                    --token "$LODESTONE_TOKEN_DEV_LOBBY_CAMMY" \
                    --target dev-lobby
lodestone status
```

Config lives in `~/.config/lodestone/config.json` (mode `0600`, it holds a token). For CI, set
`LODESTONE_API`, `LODESTONE_TOKEN` and `LODESTONE_TARGET` instead and skip `login` entirely - the
environment overrides the config file completely, so a stray config on a build agent cannot redirect a
deploy.

### 4. Deploy

```bash
$ lodestone deploy build/libs/myplugin.jar
deploying build/libs/myplugin.jar to dev-lobby
  starting      deploying localhost:5000/dev/lobby@sha256:acac48… to deployment hideaway-dev/lobby (container paper)
  updating      replacing localhost:5000/dev/lobby@sha256:fa13ee…
  settling      waiting for the rollout to settle
  checking      gating on 1 health check(s)
  succeeded     deployed localhost:5000/dev/lobby@sha256:acac48…

artifact  sha256:d43ec8b762ba10b7487998a163c36589057b82114c5c714d726c806827edc2d1
image     localhost:5000/dev/lobby@sha256:acac48a1457fd604aaee022f1cd583eb1abb25e225bffbc08092a5bffc9b8164
base      localhost:5000/hideaway/lobby-base@sha256:9c1f2e3d4a5b6c7d8e9f0a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f
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

Pick how many instances a deploy produces - 1 for a UI change, 2 for cross-server data and fallback,
more for load testing:

```bash
lodestone deploy build/libs/myplugin.jar --replicas 2
```

Image and replica count move in a single patch, so Kubernetes starts one rollout that knows both.

**A rollback reverts exactly the fields the deploy set**, and says which either way:

```
rollout did not settle; replicas restored 4 -> 1
rollout did not settle; replicas left at 3 (not set by this deploy)
```

A deploy that changed the count has it restored - that is undoing the operation. A deploy that never
touched it leaves it alone, because undoing a capacity decision nobody made here would be worse. It is
reported both ways so nobody is left with a degraded target and no idea there is something outstanding.

## CLI

| Command | |
|---|---|
| `lodestone deploy <file>` | upload, package, push, roll out, streaming progress |
| `lodestone push <file>` | upload and record, without deploying |
| `lodestone ledger` | what has been published to a target, newest first |
| `lodestone status` | is the agent up, what version, which targets |
| `lodestone login [ctx]` | save an address, token and target as a named context |
| `lodestone contexts` | list configured contexts (never prints tokens) |
| `lodestone version` | client version |

Global flags: `--target <name>` to pick the workload, `--ctx <name>` to pick a context, `--api` /
`--token` to override it, `--replicas <n>` to scale, `--json` for machine-readable output.

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

Server-level settings come from the environment. Everything about a *target* comes from the targets
file, because a map does not fit in an environment variable.

| Variable | Default | |
|---|---|---|
| `LODESTONE_ADDR` | `0.0.0.0` | listen address |
| `LODESTONE_PORT` | `8080` | listen port |
| `LODESTONE_DATA_DIR` | `/var/lib/lodestone` | artifacts and the per-target ledgers |
| `LODESTONE_TARGETS` | - | path to the targets JSON. Unset means `/status` only |
| `LODESTONE_KUBECONFIG` | - | unset means in-cluster, then the usual rules |

Plus one variable per credential holding its token - e.g.
`LODESTONE_TOKEN_DEV_LOBBY_CAMMY`, or `LODESTONE_TOKEN_DEV_LOBBY` for the single-token form.

### Target fields

| Field | Default | |
|---|---|---|
| `namespace` | **required** | namespace of the Deployment |
| `deployment` | **required** | name of the Deployment |
| `container` | **required** | container to update |
| `baseImage` | **required** | image to append artifacts onto |
| `repo` | **required** | registry path to push builds to |
| `destPath` | `/plugins/app.jar` | where the artifact lands inside the image |
| `credentials` | - | named tokens permitted to deploy. `name` is recorded in the ledger; `env` names the variable holding the secret, or `token` is a literal |
| `tokenEnv` | - | single shared token from the environment. Recorded as `shared`. Exclusive with `credentials` |
| `token` | - | literal single token, for local use. Exclusive with `tokenEnv` and `credentials` |
| `healthURL` | - | HTTP GET must return 2xx |
| `healthAddr` | - | TCP connect must succeed (`host:port`) |
| `settleTimeout` | `10m` | how long to watch a rollout. Set above `progressDeadlineSeconds` |
| `maxReplicas` | `10` | cap on `--replicas` for this target |
| `retain` | `10` | ledger entries to keep, with their artifacts and manifests. Minimum 2 |

**Retention.** After a successful deploy, entries beyond `retain` are dropped along with their stored
jars and pushed manifests. The **deployed entry is never dropped** however old it is, and `retain` floors
at **2** - one entry is the running revision and the next is the rollback target, so a tighter window
would break rollback.

An artifact is only deleted once **no** target's ledger references it: the store is shared, so an
identical jar deployed to dev and prod is stored once, and pruning per target would take the file out
from under the other.

Manifests are unlinked from the registry and nothing more. The registry does not free the blobs until
`registry garbage-collect` runs, which wants it read-only - that is registry maintenance, not a deploy
agent's business. A registry with `REGISTRY_STORAGE_DELETE_ENABLED` unset rejects deletes; Lodestone
reports that once and carries on pruning the ledger and store.

Count retention in **world revisions, not deploys**. A plugin deploy adds a ~2.5 MB layer, but a new
base adds the whole world - layer sharing keeps everything else cheap, so what pins disk is how many
distinct bases your retained manifests still reference.

### CLI (`lodestone`)

| Variable | |
|---|---|
| `LODESTONE_API` | server address. Overrides the config file entirely, including `--ctx` |
| `LODESTONE_TOKEN` | bearer token |
| `LODESTONE_TARGET` | target to address |
| `LODESTONE_CONFIG` | config file path |
| `LODESTONE_VERSION` | version to stamp on the ledger entry |
| `LODESTONE_BY` | who to record as the publisher |

## HTTP API

| | | |
|---|---|---|
| `GET` | `/status` | public - health probes need it without a token. Reports the target names |
| `POST` | `/artifacts/{target}` | upload and record. `?version=` stamps the ledger |
| `GET` | `/artifacts/{target}` | read that target's ledger, newest first |
| `POST` | `/deploy/{target}` | the whole pipeline. Also accepts `?replicas=` |

All but `/status` need `Authorization: Bearer <token>`, and **each target's token reaches only that
target** - that is what stops a dev credential touching prod. An unknown target answers `404` rather
than `401`, so a caller with a valid token learns they typed the name wrong; target names are not
treated as secrets.

A second deploy to a target already deploying gets `423 Locked` and a `Retry-After`. Deploys to
*different* targets run concurrently.

`POST /deploy` returns one JSON object by default. Send `Accept: application/x-ndjson` for a stream of
one object per line, discriminated by `kind`:

```
{"kind":"event","phase":"settling","message":"waiting for the rollout to settle","at":"…"}
{"kind":"result","target":"dev-lobby","digest":"sha256:…","image":"…@sha256:…","deployed":true}
```

**A streamed deploy always returns HTTP 200**, even when it fails. A status code is sent with the first
byte of the body and cannot be revised, so the outcome lives in the final `result` line. Read that,
not the status.

## How it works

**Deploy by digest, never by tag.** The Deployment is pinned to `image@sha256:…`. Tags are mutable;
digests are not. This removes an entire class of "the thing running is not the thing you approved" -
and it means an unchanged artifact produces an unchanged digest, so redeploying it is a no-op.

> **What that buys you, concretely.** While this was being built, a base image was published with CRLF
> line endings in its entrypoint script, so every pod died with
> `exec /usr/local/bin/entrypoint.sh: no such file or directory`. The file was present and executable -
> the kernel was looking for an interpreter called `/bin/sh\r`.
>
> The fix was easy. Getting it *deployed* was not. The image was rebuilt and pushed **under the same
> tag**, and the node kept serving the cached broken layer, because `imagePullPolicy: IfNotPresent` did
> exactly what it says: same tag, already present, no pull. It took a bump to a fresh tag to shift it.
>
> That is the mutable-tag problem in one incident, and deploying by digest makes it unrepresentable. A
> new build is a new digest, so there is nothing for a node to mistake for what it already has.

**A moving base tag is allowed, and pinned in the record.** A dev target can set
`baseImage: registry/lobby-base:current`, so a new world revision needs no config change. The tag is
re-resolved to a digest on **every** build - never cached across builds - and that digest is recorded
in the ledger and returned in the deploy result as `baseImage`. So *"which world was this built on?"*
stays answerable after the tag has moved.

Staging and prod should pin an exact tag instead. A moving base means a world change can enter a deploy
nobody chose: fine while iterating, not fine in front of players.

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

**One deploy at a time, per target.** Interleaved deploys corrupt each other's rollback: if A replaces
X with B, then C replaces B with D, and A's rollout fails, A would roll back to X and silently destroy
C's healthy deploy. So a second deploy to the same target gets `423 Locked`. The lock is per target,
not global - two developers on different workloads have no reason to queue behind each other's
ten-minute rollout.

**A draining pod is not a failed rollout.** A rollout counts as settled once the desired number of
*new* pods are updated and available; it does not wait for old ones to disappear. `Status.Replicas`
includes terminating pods, so requiring it to match would block for the whole drain - and a workload
that saves state on SIGTERM can take minutes over it. Waiting would burn the settle timeout and roll
back a working deploy. `ProgressDeadlineExceeded` is the one authoritative failure signal; Lodestone's
own timeout means "we stopped watching", not "it broke".

**The agent never executes the artifact.** It receives, packages, and hands a reference to Kubernetes.
A malicious artifact compromises the pod it runs in, never the agent or the cluster.

## What Lodestone is not

**Lodestone ships new code to a fixed workload. It does not do dynamic provisioning** - creating and
destroying an instance per player group, per session, or per tenant. That is a separate concern, and
if you need it, Agones is the tool.

The line is: *"how many replicas of an existing target"* is a deploy parameter Lodestone accepts.
*"What targets exist"* is a manifest you apply yourself. Lodestone addresses many targets and manages
none of them.

**Draining is also not Lodestone's job.** A workload that needs to shut down gracefully does it with
`preStop` and `terminationGracePeriodSeconds` in its own pod spec. Lodestone's only obligations are to
make the rollout timeout configurable and to never mistake a terminating pod for a failed rollout - a
pod that legitimately drains for two minutes is not a rollback trigger.

## Development

```bash
go test ./...          # 187 tests
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
