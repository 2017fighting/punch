# punch

A **declared CGNAT punch** for a homelab cluster: a `Punch` custom resource says *"make `<target>`
reachable on `<bindPort>`"*, a controller in the cluster owns the API and the status, and an agent on
the router owns the mapping.

Two binaries, one Go module:

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

## Build

```bash
go build ./...
go test ./...

# the router binary (static, no cgo)
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags='-s -w' -o bin/punchd ./cmd/punchd
```

CI publishes `ghcr.io/2017fighting/punch-controller` and the `punchd` binaries as workflow artifacts.
