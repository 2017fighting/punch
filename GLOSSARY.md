# punch

The declared CGNAT punch (home-ops ADR-0044): a `Punch` custom resource says *make `<target>` reachable
on `<bindPort>`*, and this repository implements the two halves that make it true — `punch-controller` in
the cluster, `punchd` on the router.

## Language

**Punch**:
The request itself — *make `<target>` reachable on `<bindPort>`* — and, by extension, the CR that carries
it and the mapping that satisfies it. One word covers the declaration, the running child, and the public
tuple it yields.
_Avoid_: port-forward, tunnel, NAT rule

**Punch Origin**:
The address + port pair the carrier maps, and therefore the thing a punch is measured against. It is
stable by construction because the socket lives on the router, where no SNAT hop rewrites the source port.
_Avoid_: public IP, external address

**Desired list**:
What the cluster declares — served at `GET /punches/desired` and fetched by the agent every tick. It is
the only declaration source; the router holds no local list of punches.
_Avoid_: config, inventory

**Observation**:
The agent's report of what it sees, per punch: `up` with the tuple, `pending`, or `down`, POSTed as a whole
snapshot every tick. The cluster owns the declaration and the clock (`lastSeenAt`, the deadline); the
agent owns the observation.
_Avoid_: status update, heartbeat

**Version tag**:
A `vX.Y.Z` git tag — this project's release identity. It is what CI bakes into both binaries
(`-X main.version`) and what becomes the controller image's tag; consumers pin it.
_Avoid_: release number (a tag names a commit; there is no separate release record)

**Rolling tag**:
A tag whose content moves under a fixed name instead of naming one release. `main` was one and is
retired (`docs/adr/0001`): it named a branch, so nothing could tell that it had fallen behind.
`latest` is still published, but it moves only when a release is cut — it is an alias for the newest
version tag, never a pin, and nothing pins it.
_Avoid_: branch tag

**Pin**:
A specific commit a consumer pins instead of following a branch. The OpenWrt feed pins a 40-hex sha (and
keeps its date for `apk` ordering); home-ops pins an image tag. Both name one release, each in its own
vocabulary.
_Avoid_: version (a pin is an identity, not a number)
