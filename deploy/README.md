# Running `lodestoned` under systemd

`lodestoned` is a plain long-running process, so any supervisor works. This is the one we run.

Two things here are not cosmetic:

- **`TimeoutStopSec=960s`.** The agent waits up to 15 minutes for a rollout in flight to finish
  before exiting. systemd's default stop timeout is 90 seconds, after which it sends `SIGKILL` -
  abandoning a half-applied rollout, with nothing in the agent's log to say why, because it never got
  to write one. Keep this above the largest `settleTimeout` across your targets, plus headroom.
- **Versioned binaries behind a symlink.** `ProtectHome=true` makes `/root` inaccessible to the
  service, so a binary living there fails at `203/EXEC` before the agent starts. Installing under
  `/usr/local/lib/lodestone` fixes that, and makes rollback a symlink repoint rather than a file
  shuffle that can only ever hold one previous version.

## Layout

```
/usr/local/lib/lodestone/lodestoned-0.7.0    the binaries, one per version
/usr/local/lib/lodestone/current             symlink -> the running version
/usr/local/bin/lodestone                     the CLI, if this host runs it too
/etc/lodestone/lodestoned.env                environment, mode 600 (holds tokens)
/etc/lodestone/targets.json                  target definitions (no secrets)
/var/lib/lodestone/                          artifact store and per-target ledgers
```

Nothing under `/root`, so the hardening directives and the install agree with each other - and
`/usr/local/bin` puts the CLI on `PATH`. Keeping the client out of `/root` matters for a second
reason: a sweep for "anything holding tokens" matches `/root/lodestone` (the CLI binary) as readily
as `/root/lodestone.env`, and the names are one character apart.

## Install

```bash
VERSION=0.7.0

sudo install -d -m 0755 /usr/local/lib/lodestone
sudo install -d -m 0755 /etc/lodestone
sudo install -d -m 0700 /var/lib/lodestone

# The binary, named for its version.
sudo install -m 0755 ./lodestoned /usr/local/lib/lodestone/lodestoned-$VERSION
sudo ln -sfn /usr/local/lib/lodestone/lodestoned-$VERSION /usr/local/lib/lodestone/current

# The environment file holds tokens, so it is not world-readable.
sudo touch /etc/lodestone/lodestoned.env
sudo chmod 0600 /etc/lodestone/lodestoned.env

sudo install -m 0644 deploy/lodestoned.service /etc/systemd/system/lodestoned.service
sudo systemctl daemon-reload
sudo systemctl enable --now lodestoned
```

Logs go to the journal rather than a file:

```bash
systemctl status lodestoned
journalctl -u lodestoned -f
```

## Upgrade

```bash
VERSION=0.8.0
sudo install -m 0755 ./lodestoned /usr/local/lib/lodestone/lodestoned-$VERSION
sudo ln -sfn /usr/local/lib/lodestone/lodestoned-$VERSION /usr/local/lib/lodestone/current
sudo systemctl restart lodestoned
```

`systemctl restart` sends `SIGTERM` and waits, so a deploy in flight completes first - which is why
`TimeoutStopSec` matters. Old versions stay on disk; prune them when you like.

## Rollback

```bash
ls -1 /usr/local/lib/lodestone/          # what is available
sudo ln -sfn /usr/local/lib/lodestone/lodestoned-0.7.0 /usr/local/lib/lodestone/current
sudo systemctl restart lodestoned
```

Rolling back the binary does not roll back the config. If the upgrade also changed
`targets.json` - adding a field an older binary does not know - restore that too, or the older
binary will reject the file and refuse to start.

## ⚠️ If your registry is a container on the same host

A local registry reachable from the cluster network is the setup Lodestone is built for, and it has a
boot-order trap that **fails silently**: the agent reports perfectly healthy right up until the first
push after a reboot.

To be reachable from pods, the registry publishes on the cluster bridge (`10.42.0.1:5000` under k3s
and flannel). That interface does not exist until the CNI has come up - which is **several seconds
after k3s reports active**. A registry container starting in that window fails to bind:

```
failed to bind host port for 10.42.0.1:5000: cannot assign requested address
```

Docker's own `restart=always` does **not** cover this. A container that fails to *start* is not a
run-then-exit, so Docker abandons it and leaves it `Exited`. The registry is then simply absent, and
nothing complains until a deploy tries to push.

Run the registry under systemd too, and give it a retry:

```ini
[Unit]
After=docker.service k3s.service
Requires=docker.service

[Service]
ExecStart=/usr/bin/docker start -a registry
ExecStop=/usr/bin/docker stop registry
Restart=on-failure
RestartSec=5s
```

**The retry is what does the work** - ordering alone is not sufficient, because "k3s is active" does
not mean "the bridge exists". Measured on one host: docker at `03:29:08`, k3s active at `03:29:13`,
bridge usable at `03:29:19`.

Verify by rebooting and then **running a deploy**, not by checking `systemctl status`. The agent
comes up healthy either way; only a push proves the registry is there.

## Notes

- **`EnvironmentFile` is read by the service manager**, outside the unit's namespace, so
  `ProtectHome` does not affect it. It lives in `/etc/lodestone` anyway, to keep configuration in one
  place.
- **`ProtectSystem=full`** makes `/usr`, `/boot` and `/etc` read-only. The agent only reads from
  those. To tighten to `strict`, which makes the whole filesystem read-only, add
  `ReadWritePaths=/var/lib/lodestone` - without it the agent cannot write the store or the ledgers.
- **The service runs as root** only because a k3s kubeconfig is root-readable. A dedicated user with
  a scoped kubeconfig is the real hardening; running the agent in-cluster with a ServiceAccount
  replaces both, and is the direction we are going.

## Version skew

The agent and the CLI are versioned and released together, but nothing requires them to match, and in
practice they drift - a daemon gets upgraded on a server while laptops keep whatever they installed.

**An older CLI against a newer agent is supported.** The API only grows: new response fields are
additive and older clients ignore what they do not know, new query parameters are optional, and no
existing field changes meaning. A `0.4.0` CLI against a `0.7.0` agent works.

**A newer CLI against an older agent is not.** It may send parameters the agent does not understand,
or expect fields it will not send. Upgrade the agent first.

Both print their version (`lodestone version`, `lodestoned -version`), and the agent reports its own
in `GET /status`, so a mismatch is always answerable.

One consequence worth stating: a field the agent stops sending is a **breaking** change for any CLI
that reads it, even though nothing in the wire format looks broken. Prefer adding over removing.
