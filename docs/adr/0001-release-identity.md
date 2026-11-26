# Release identity is a repository-wide `vX.Y.Z` tag

**Status**: accepted (2026-11-26)

punch ships two binaries from one Go module, so one `vX.Y.Z` tag names a release and both binaries move
together. CI used to publish `main` on every push to the branch; that stopped: only `refs/tags/v*`
publishes now, and the tag is baked into both binaries (`-X main.version`) so a running process can
answer `punchd -version`. The reason is on the consumer side — a rolling tag is an identity
nothing can follow. home-ops' Renovate can only compare `vX.Y.Z` (and its built-in `kubernetes` manager
does not scan `infrastructure/` at all, so that repo carries a customManager row for this one file), and a
pin with no version signal ages silently, which is the failure ADR-0017 already recorded twice.

## Consequences

- `main` is retired: a branch build still produces artifacts but publishes no image, and the
  leftover `:main` tag in the registry was deleted. Anything that wants bytes pins a tag, or the
  `sha-<short>` tag that mirrors it.
- `latest` stays, and means "the newest release", not "the newest commit": `docker/metadata-action`'s
  default `flavor: latest=auto` adds it to any semver tag, so it moves only when a `vX.Y.Z` is cut.
  It is a convenience alias and never a pin — Renovate tracks the version tags, and home-ops pins
  `:vX.Y.Z` explicitly.
- The OpenWrt feed keeps a **commit** pin, not a tag pin: the tag's commit as the sha, its date as
  `PKG_VERSION`. `apk` orders by that date only, and `2026.11.26` sorts above `0.1.0`, so a version-number
  switch would read as a downgrade on every installed device. The feed follows releases by moving the pin.
- That pin's `PKG_LICENSE` can finally be filled in (MIT): a release nobody may legally build is not a
  release, and it was the only reason the feed left the field empty.
- Tags are never moved or re-pushed: `apk` and the registry both treat the tag as identity. A bad release
  is followed by `v0.1.1`, not by rewriting `v0.1.0`.

## Considered options

- **Per-binary tags** (`punch-controller/v0.1.0`, `punchd/v0.1.0`): truer to independent lifecycles, but the
  two have never shipped apart, and the CI filter, the metadata extraction and the Renovate regex would each
  grow a variant for no benefit yet.
- **Date tags** (`2026.11.26`): matches the feed's `PKG_VERSION` shape, but Docker and Renovate read a date
  as an enormous major version, so it needs its own versioning rule while still saying nothing about a
  breaking change.
- **No tags at all, pin digests**: the smallest possible CI change, but then home-ops tracks content while
  nobody can name the release — and `punchd` on a router, which has no image tag to lean on, would have no
  identity at all.
