# punch

The declared CGNAT punch: `punch-controller` in the cluster, `punchd` on the router (home-ops ADR-0044).
`README.md` has the architecture, `GLOSSARY.md` the vocabulary, `docs/adr/` the decisions.

```bash
go vet ./... && go test ./...   # the whole test surface (no code generation, no local image build needed)
go build ./cmd/...              # both binaries; -X main.version is only set by a tagged CI build
```

The tag is the release identity — see `docs/adr/0001-release-identity.md`. Never move or reuse a tag.

## Agent skills

### Issue tracker

GitHub Issues in `2017fighting/punch`, via the `gh` CLI. See `docs/agents/issue-tracker.md`.

### Triage labels

The five canonical role names, unchanged (`needs-triage`, `needs-info`, `ready-for-agent`,
`ready-for-human`, `wontfix`). See `docs/agents/triage-labels.md`.

### Domain docs

Single-context: `GLOSSARY.md` + `docs/adr/` at the repo root. See `docs/agents/domain.md`.
