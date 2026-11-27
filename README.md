# punch

A **declared CGNAT punch** for a homelab cluster: a `Punch` custom resource says *"make `<target>`
reachable on `<bindPort>`"*, a controller in the cluster owns the API and the status, and an agent on
the router owns the mapping.

Two binaries, one Go module, no code generation:

| binary | runs on | job |
|---|---|---|
| `punch-controller` | the cluster (Deployment) | serves `GET /punches/desired` and `POST /punches/status`, stamps `lastSeenAt` with the **cluster's** clock, marks a punch `down` after the deadline, writes the `<name>-punch` ConfigMap, exports the metrics the alerts read |
| `punchd` | the router (static binary, `procd` service) | every 15 s: fetch desired state → reconcile `natmap` children (fixed `bindPort`, render the redirect) → POST the observed state. **An unreachable cluster is not an empty list**: it keeps running what it has |

The design is `home-ops` **ADR-0044** (`docs/adr/0044-declared-punch-router-agent-crd.md`); the
DNAT-rendering mechanism and its three traps were measured in
`docs/research/punch/01-dnat-rendering.md`. The API contract (the CRD, the ConfigMap's name rule and its
single key `punch.json`) lives in `home-ops/infrastructure/punch/00-crds.yaml` — this repository's job is
to implement it, not to redefine it.

## Why the agent is on the router

Cilium's egress-gateway SNAT rewrites the source port (measured: a pod bound to 27280 left the node as
`51674`), and a punch's redirect has to match the **LAN-side** port. A pod therefore cannot own a
statically-routed port. The router's own socket has no such hop, so the punch lives there and its
**Punch Origin** (the address + port pair the carrier maps) is stable by construction.

## Layout

```
api/v1alpha1/                 the CRD's Go mirror (spec/status, GVR/GVK, state + reason constants)
config/crd/                   VENDORED copy of home-ops' 00-crds.yaml — home-ops owns it
cmd/punch-controller/         the cluster binary
cmd/punchd/                   the router binary (static, no cgo, no k8s.io dependencies)
internal/controller/          desired list, status patch, ConfigMap writer, deadline loop, /metrics
internal/agent/               config, natmap children, nft renderer + read-back, status snapshot
```

`go test ./api/...` parses `config/crd/punch.raenzo.com_punches.yaml` and fails if the Go structs and the
CRD disagree, property for property — that is the "one source of truth" gate, since there is no
`controller-gen` here. When home-ops changes the CRD, re-copy it and fix the types.

## `punch-controller`

```bash
# flags (all optional but the token); the defaults are the contract
punch-controller \
  -listen :8080 \
  -token-env PUNCH_BEARER_TOKEN \
  -resync 15s \
  -deadline 60s
```

| flag | env | default | meaning |
|---|---|---|---|
| `-listen` | | `:8080` | address for the two API endpoints **and** `/metrics` (the Service's numeric `targetPort`) |
| `-token-env` | | `PUNCH_BEARER_TOKEN` | name of the env var holding the bearer token; **empty ⇒ refuses to start** (the HTTPRoute has no edge auth) |
| `-resync` | | `15s` | reconcile period: the deadline, the `bindPort` uniqueness rule, the ConfigMap lifecycle |
| `-deadline` | | `60s` | silence after which a punch is `down` (four missed agent ticks) |
| `-kubeconfig` | | — | local runs only; in-cluster config is used when empty |
| `-log-format` | | `text` | `text` or `json` (`log/slog`) |

Endpoints:

- `GET /punches/desired` → `{"punches":[{name,namespace,target,protocol,bindPort}]}` for every `Punch`
  in the cluster. `protocol` is resolved (empty ⇒ `TCP`).
- `POST /punches/status` → body
  `{"punches":[{name,namespace,state,publicIP,publicPort,innerPort,reason}]}`; the whole batch is
  validated before anything is written, unknown punches are skipped (counted in the response), and for
  each accepted entry the controller stamps `lastSeenAt` **from its own clock**, patches the CR's
  `/status` subresource, and:
  - `up` → writes `<name>-punch` (key `punch.json`, `{ip,port,inner_port,protocol}`, ownerReference to
    the CR with `controller: true`, in the CR's namespace);
  - `pending` or `down` → writes the SAME object with a **down document**, `{"ip":"","port":0,…}`,
    which is how "there is no mapping" is said. Not a deletion: kubelet never removes a deleted
    ConfigMap's files from a running pod's volume, so an app cannot see absence (ADR-0044 amendment,
    2026-11-26 — measured on this cluster before it was believed).

  Whichever it is, the write is skipped when the live object already carries that exact document, so
  a steady state costs no writes.
- `GET /metrics` → Prometheus text format, **unauthenticated** (scraped in-cluster; the HTTPRoute only
  routes `/punches/...`).

Both API endpoints require `Authorization: Bearer <token>`, compared in constant time.

### The metrics contract

Home-ops' alerts (`infrastructure/punch/vmrule.yaml`) read exactly these four families — renaming one is
a breaking change, not a cosmetic one:

```
punch_last_seen_timestamp_seconds{namespace,name}   unix seconds, CLUSTER clock; 0 if never seen
punch_state{namespace,name,state}                   one-hot over state ∈ {pending,up,down}
punch_agent_reports_total                           counter, status entries accepted since start
punch_configmap_writes_total                        counter, <name>-punch writes since start
```

`punch_state` is evaluated at scrape time from the CRs (the controller keeps no state of its own), so
`/metrics` and the CRs cannot disagree during the incident the alerts watch. The values are the
**evaluated** state: the 60 s deadline and the `bindPort` conflict rule are applied here too.

### The ConfigMap is a contract, not an implementation detail

For a `Punch` named `<name>`, the controller writes exactly one ConfigMap, `<name>-punch`, in the
Punch's namespace, with exactly one key, `punch.json`, holding natmap's document:

```json
{"ip":"120.244.157.23","port":38698,"inner_port":27257,"protocol":"tcp"}
```

Consumers open that key by name (`/punch/punch.json`) and read the CONTENT, never the presence:

```json
{"ip":"","port":0,"inner_port":27257,"protocol":"tcp"}
```

— that is the same key for a punch that is not up, and it is the only signal a running app can see
(kubelet keeps a deleted ConfigMap's last payload in the volume, so "the file disappeared" happens
nowhere). `CheckConfigMapName` refuses any other name, any other namespace, and any object that is not
derived from the CR — RBAC cannot express "the name is derived from a Punch", so the code does.

### State machine (one place, `internal/controller/state.go`)

| condition | state | reason |
|---|---|---|
| two `Punch` objects claim the same `bindPort` | `down` (both) | `bindPortConflict` |
| the agent never reported (`lastSeenAt` zero) | `pending` | the agent's own reason, if any |
| `now - lastSeenAt > deadline` | `down` | `no status from the punch agent for <N>s` |
| otherwise | what the agent said | what the agent said |

The ConfigMap exists from the moment the controller has seen the CR: `up` writes the public tuple,
`pending` and `down` write the down document, and a hand-edited document is repaired on the next
resync. Nothing deletes it except the CR's own deletion, through the `ownerReference`.

## `punchd`

`punchd` is a static binary (`CGO_ENABLED=0`). It needs `natmap` and `nft` on `PATH`; it does **not**
link the Kubernetes client libraries.

### Configuration

`-config /etc/punch/punch.conf` (default), `key = value`, `#` starts a comment. Unknown keys are
errors — a typo that silently becomes a default is the invisible contract ADR-0044 retires. Mode 0600,
because it holds the token.

```ini
# /etc/punch/punch.conf — installed by home-ops' router files/
cluster_url = https://punch.raenzo.com
token       = <same value as the controller's PUNCH_BEARER_TOKEN>
stun        = stun.m-online.net
http        = www.baidu.com
iface       = pppoe-wan
state_dir   = /var/run/punch
nft_path    = /usr/share/nftables.d/ruleset-post/punch.nft
fwmark      = 66          # optional: natmap -f 66, as the two production instances use
interval    = 15s         # optional
```

| key | required | default | meaning |
|---|---|---|---|
| `cluster_url` | yes | — | the Edge Gate base URL; endpoints are `<base>/punches/desired` and `/status` |
| `token` | yes | — | bearer token, sent on both calls |
| `stun` | yes | — | `natmap -s` |
| `http` | yes | — | `natmap -h`, the remote keepalive host |
| `iface` | yes | — | `natmap -i`, the WAN interface (`pppoe-wan`) |
| `state_dir` | no | `/var/run/punch` | notify script + one `<bindPort>.args` report per punch |
| `nft_path` | no | `/usr/share/nftables.d/ruleset-post/punch.nft` | fw4's auto-include |
| `fwmark` | no | unset | `natmap -f <fwmark>`; set it to keep the flow on the right routing table |
| `interval` | no | `15s` | reconcile period |

### Flags

| flag | meaning |
|---|---|
| `-config /etc/punch/punch.conf` | config file above |
| `-dry-run` | fetch the desired list, print the nft table and the status that would be posted; touch neither `nft` nor `natmap`, post nothing |
| `-interval` | override the config's interval |
| `-log-format` | `text` or `json` |
| `-version` | print the build's version (the release tag; `dev` outside a tagged CI build) and exit |

First manual run, before anything is installed:

```console
# punchd -config /etc/punch/punch.conf -dry-run
# punchd -dry-run: 1 desired punch(es) from https://punch.raenzo.com/punches/desired
# would be written to /usr/share/nftables.d/ruleset-post/punch.nft and applied with `nft -f`, then read back with `nft -j list table ip punch`
destroy table ip punch
table ip punch {
	chain prerouting {
		type nat hook prerouting priority dstnat - 5; policy accept;
		tcp dport 27257 dnat ip to 192.168.9.70:6881 comment "punch apps-qbittorrent-private/qb-private"
	}
}
# would run: natmap -k 30 -s stun.m-online.net -h www.baidu.com -b 27257 -4 -i pppoe-wan -e /var/run/punch/notify.sh -f 66
# would POST https://punch.raenzo.com/punches/status (not sent in -dry-run):
[ { "name": "qb-private", …, "state": "pending", "reason": "dry run: natmap not started" } ]
```

That `-dry-run` is also how the redirect is reviewed: the file it prints is the file the agent writes.

### A dead natmap is restarted, and the report goes with it

`natmap` has no heartbeat (ADR-0044 fact 4), so a child that exits is invisible until someone
looks. The agent owns that: on the tick that sees `Exited`, it drops the entry, **deletes the
dead child's `<bindPort>.args` report** (the replacement writes a new one only when it has a
mapping — keeping the old file would announce the dead child's public port), and starts a fresh
child. The wait before each attempt doubles from `RestartInitialDelay` (15 s) to
`RestartMaxDelay` (5 min), and a child that lived past `RestartHealthyFor` (2 min) resets it, so
one crash every few hours is retried at once while a crash loop is spaced out instead of becoming
a restart storm on a 15 s tick. Between the death and the replacement's first report the punch is
`pending` — "nothing announced yet" — which is the honest state, and the ConfigMap says so.

### A restart stops its children (and a SIGKILLed one is swept)

The natmap children are punchd's, and procd supervises only punchd. So on `ctx.Done()` — the
SIGTERM behind `service restart` and `apk upgrade` — the agent SIGTERMs every child's process
group, **waits for each to exit**, and deletes the report it left. The wait is the point: the
bind port is free when the process is gone, not when the signal was sent. Without it every
restart left the old generation's children behind holding `27257`/`27258`, and the new one
could not map at all — `[E] sock_connect src/hev-sock.c:60 Cannot assign requested address`,
while the punch still reported `up` off the orphan's report (issue #2, measured 2026-11-26).
The old report goes with the child it described, so the restart says `pending` — "nothing
announced yet" — instead of re-announcing a public port nothing owns. That is one rule, applied
at all three places a child stops: the shutdown above, the restart after a crash, and a punch
that is no longer desired.

A punchd that was SIGKILLed never runs that path (a `kill -9`, or procd's term timeout on a
punchd stuck in a tick). The next one therefore sweeps on startup, before its first tick: it
looks for `natmap` processes whose `-e` script is **its own notify path**, and SIGTERMs, then
SIGKILLs, each one — and deletes the report of every child it killed. The command line is the
marker because the router also runs unrelated natmap instances (their own `-e`, their own `-f
<fwmark>`), and killing every `/usr/bin/natmap` would take those down with it — the one thing
an init script's `stop_service` cannot decide.

Measured on the router (x86_64, ImmortalWrt 25.12.1) with the v0.2.2 build: `stopping` at
10:39:28.136, `stopped every natmap child` one millisecond later, the replacements started
120 ms after that, and the first tick reported `pending=2` at .375 — the next tick `up=2` on
the same public ports. A `kill -9` (children reparented to init) is cleaned up by procd's 5 s
respawn plus the same sweep: two WARN lines naming the pids, replacements bound, `up=2` on the
next tick.

### The redirect, and the three traps

One self-owned `table ip punch`, rendered into fw4's auto-include at `priority dstnat - 5`:

```
destroy table ip punch
table ip punch {
	chain prerouting {
		type nat hook prerouting priority dstnat - 5; policy accept;
		tcp dport 27257 dnat ip to 192.168.9.70:6881 comment "punch apps-qbittorrent-private/qb-private"
	}
}
```

The first line is load-bearing: `nft -f` treats an existing `table` as `add table` (idempotent) and then
**appends** the rules, so every `fw4 reload` would duplicate the redirect. `destroy` returns 0 for a
missing table where `delete` returns 1.

`punchd` then **reads the chain back** (`nft -j list table ip punch`) and fails loudly when an expected
rule is missing, points elsewhere, or appears twice — because a broken include rolls the whole `nft -f`
batch back while `fw4 reload` still exits 0. When the last punch goes away the table is removed
explicitly (`nft delete table ip punch`): `fw4 reload` only flushes `inet fw4`, so deleting the include
never deletes our table.

Two startup assertions, both from the research note:

1. `/etc/config/firewall` must not set `firewall.@defaults[0].auto_includes` to `0` — that silently stops
   fw4 from rendering the include, and `punchd` **refuses to start**;
2. the include may be missing because a firmware upgrade overwrote `/usr/share`: that is a warning plus a
   guarantee — `punchd` renders on its first tick unconditionally, so the file comes back.

### Running it under procd

```sh
#!/bin/sh /etc/rc.common
# /etc/init.d/punchd
USE_PROCD=1
START=96          # after fw4 (START=19) so the include's rules precede every punch process
STOP=10

start_service() {
	procd_open_instance punchd
	procd_set_param command /usr/bin/punchd -config /etc/punch/punch.conf
	procd_set_param respawn 3600 5 5
	procd_set_param stdout 1
	procd_set_param stderr 1
	procd_close_instance
}
```

No UCI, no `procd_set_config_changed firewall`: the agent applies its own table and never asks fw4 to
rebuild `inet fw4` for a punch change. `logread | grep -i punchd` carries both `punchd`'s structured log
and natmap's own stderr (children inherit the same descriptors).

### Observation rules

- Every 15 s `punchd` POSTs the **full snapshot**: one entry per desired punch, `up` with the discovered
  public tuple, `pending` while natmap has not announced one yet (`-e` is edge-triggered — no file is not
  a death), `down` when the child exited or the redirect is not in the table after a read-back.
- A failed fetch changes **nothing**: every natmap keeps running, no table is touched, and the snapshot is
  re-sent with `reason: cannot reach the cluster` (states unchanged). Only an explicit absence stops a
  punch.
- The controller owns `lastSeenAt`, the 60 s deadline and the `bindPort` uniqueness rule; the agent only
  reports what it sees.

## Build, test, publish

```bash
go vet ./...
go test ./...

# the controller image (also built by CI for amd64 + arm64)
docker build -t punch-controller .

# the router binary (static, no cgo)
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags='-s -w' -o bin/punchd ./cmd/punchd
```

CI (`.github/workflows/ci.yml`) runs `go vet`, `go test` and builds **both binaries for linux/amd64 and
linux/arm64** on every branch and PR, but **only a `v*` tag publishes**: it uploads `punchd` as the
workflow artifacts `punchd-linux-amd64` / `punchd-linux-arm64` and pushes
`ghcr.io/2017fighting/punch-controller` with `GITHUB_TOKEN`, under the tag `vX.Y.Z` (plus
`sha-<short>`). No branch tag: `:main` was retired because a rolling branch tag is an identity
nothing can follow. `:latest` still exists and follows the newest release (metadata-action's default
`flavor: latest=auto` adds it to any semver tag) — it is a convenience alias, never a pin; the
[Release](#release) section pins the tag.

### Release

The tag is the release identity (`docs/adr/0001-release-identity.md`): one repository-wide `vX.Y.Z` for
the module, so both binaries move together.

```bash
# the release commit is whatever main is when you decide to ship it
git tag -a v0.1.0 -m "v0.1.0"
git push origin v0.1.0
```

That push is the whole ritual. It builds with `-X main.version=v0.1.0`, publishes
`ghcr.io/2017fighting/punch-controller:v0.1.0`, and fails loudly if `-s -w` ever stops the binary from
answering `punchd -version` (the workflow runs it on the tag). Consumers pin the tag itself:
`home-ops` pins the image tag (`:v0.1.0`, Renovate-tracked), and the OpenWrt feed pins the tag's commit
and keeps a date as `PKG_VERSION` because `apk` orders by it only. **Nothing consumes a branch.**

A tag is never moved or re-pushed over: `apk` and the registry both treat the tag as identity, so a bad
`v0.1.0` is retracted by shipping `v0.1.1`, not by rewriting history.

What the tests cover: the CRD/Go parity gate; the state machine (pending/up/down, the deadline boundary,
`bindPortConflict`, deterministic order); the metrics text (exact contract lines, one-hot cardinality, no
family outside the four); bearer auth (missing/wrong/prefix/prefixed-suffix tokens); the ConfigMap
contract (name and namespace guard, single key, document bytes, ownerReference); the HTTP endpoints
against a fake apiserver (status patch, `lastSeenAt` from the controller's clock, ConfigMap created on
`up` / deleted on `down`, batch validation, unknown punches); the reconcile loop (deadline, pending,
ConfigMap kept when `up` and recreated when missing); the agent (config parsing, notify script and report
parsing, exact `natmap` argv, renderer output, read-back verification of all three traps, fetch-failure
behaviour, idempotent ticks, `-dry-run` touching nothing, the `auto_includes` assertion, a natmap that
exits being replaced with a backoff, the shutdown that stops every child, waits for it and clears its
report, the SIGKILL escalation, and the startup sweep that kills a SIGKILLed generation's natmaps and
nobody else's).

## License

MIT — see [LICENSE](LICENSE).
