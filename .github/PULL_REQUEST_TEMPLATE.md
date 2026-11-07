## Summary

<!-- What does this pull request change, and why? One or two sentences. -->

## Related issue

<!-- Fixes #123, or "none" for docs/chores. -->

## Change type

- [ ] Bug fix
- [ ] Feature (additive to the contract surface)
- [ ] Breaking change (contract surface / compatibility — see docs/VERSIONING.md)
- [ ] Documentation / governance
- [ ] Build / CI / tooling

## Testing

<!-- List the commands you ran, and any manual verification. -->

- [ ] `make fmt`
- [ ] `make build`
- [ ] `make vet`
- [ ] `make test`
- [ ] `make race`
- [ ] `make gate`

## Reviewer notes

<!-- Trade-offs, follow-ups, or areas that need a closer look. Delete if empty. -->

## Checklist

- [ ] Tests added or updated for the behavior change
- [ ] Public identifiers have documentation comments; `gofmt` is clean
- [ ] the dependency policy still holds (only `mvdan.cc/sh/v3`; root stdlib-only; `CGO_ENABLED=0` builds)
- [ ] The root `rig` package still imports only the standard library
- [ ] Documentation updated (`README.md`, `docs/`, and a `CHANGELOG.md` entry under `## Unreleased`)
- [ ] Breaking changes are marked `BREAKING` in `CHANGELOG.md` with a migration note
- [ ] No secrets, credentials, generated or vendored code added
- [ ] Security issues were reported privately per `SECURITY.md` (not in this PR)
