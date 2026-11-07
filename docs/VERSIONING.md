# Versioning policy

ZeoKeel Rig follows [Semantic Versioning](https://semver.org/) and Go's
[module version numbering](https://go.dev/ref/mod#versions) (semantic import
versioning). This document explains what a release number promises, where the
version lives, and how tags are treated.

**Current state:** pre-1.0, release line `v0.9.x` (current `v0.9.4`, next minor
`v0.10.0`). The module path is `github.com/zeokeel/zeokeel-rig` and the stable
contract surface is the module root package `rig`. Until `v1.0.0`, minor
releases may still contain documented breaking changes; exact versions are what
consumers should pin.

## Where the version lives

| Source | Location | Role |
| --- | --- | --- |
| Release tag | git tag `vX.Y.Z` | **Authoritative.** This is what `go get mod@vX.Y.Z` resolves and what the Go module proxy and checksum database record. |
| `rig.Version` | `version.go` at the module root (`const Version = "X.Y.Z"`) | Runtime version for diagnostics and support requests. Must equal the tag with the leading `v` removed. |
| `CHANGELOG.md` | section `## vX.Y.Z — YYYY-MM-DD` | Human-readable release notes; the release workflow copies this section verbatim into the GitHub Release. |
| `COMPATIBILITY.md` | release-line table | Go minimum and dependency policy per release line. |
| `go.mod` | `go 1.27` | Minimum Go toolchain for the module. |

The tag and the constant are enforced together:

- `scripts/sync-version.sh` reads `const Version` from `version.go`, resolves the
  tag (explicit argument, `GITHUB_REF`, or `git describe --tags --exact-match`)
  and exits non-zero on a mismatch. `make version` runs it.
- `.github/workflows/release.yml` runs `scripts/sync-version.sh --require-tag`
  before publishing, so a tag that does not match `version.go` cannot produce a
  release.
- `version.go` **always equals the latest published release tag**. Do not bump it
  ahead of the tag in ordinary development: the constant is release state, not a
  development marker.

## Semantic import versioning

Go modules encode the major version in the import path:

- **v0 and v1** use the bare path `github.com/zeokeel/zeokeel-rig`. The module
  root is `package rig`; that package is the contract surface imported by
  consumers.
- **v2 and later** use `github.com/zeokeel/zeokeel-rig/v2` (respectively `/v3`,
  …). A breaking change after v1.0.0 creates a new major version at a new import
  path.
- **Never a versioned subdirectory.** There will be no `v1/`, `v2/` or
  `contracts/v1` generation directory in this module (the pre-v0.9
  `contracts/v1` package was moved to the module root precisely to follow this
  rule). Each major version is a separate module path with its own tags, not a
  folder inside one module.

Within a major version the contract surface evolves additively:

- new packages, tools, types, functions, struct fields and interfaces are
  additive (add struct fields at the end and use keyed literals; consumers
  should always use keyed literals);
- adding a method to an exported interface is breaking (it breaks external
  implementations), so capability growth goes through *optional* interfaces that
  callers type-assert, not by widening an existing interface;
- changing behavior without changing signatures can still be breaking in
  practice; non-trivial behavior changes are documented in `CHANGELOG.md`;
- removals and renames go through the [deprecation policy](#deprecation-policy).

### What counts as a breaking change

- Removing or renaming an exported identifier (package, type, function, method,
  constant, variable), or changing its signature/type.
- Removing or renaming an exported struct field; adding a field in the middle of
  a struct whose exported layout matters.
- Changing the meaning of a documented function, option or default in a way a
  reasonable consumer could depend on.
- Raising the minimum Go version or adding a module dependency.
- Changing the sandbox policy to be less restrictive by default (security
  relevant; treated as breaking even if the API is unchanged).

Adding new exported API, new tools, new optional config fields or new backend
implementations is not breaking.

## Pre-1.0 rules (`v0.x`)

Because the module is pre-1.0, the semantic versioning guarantee is weaker than
for `v1+`. The concrete rules for this project:

| Bump | Allowed contents | Compatibility promise |
| --- | --- | --- |
| `v0.Y.Z+1` (patch) | Bug fixes, documentation, performance, security fixes; additive exported API is allowed but should normally wait for a minor | Existing code compiles and behaves the same, except for corrected bugs |
| `v0.Y+1.0` (minor) | New features, new tools, new packages; **possibly breaking contract changes** | Breaking changes must be listed under `BREAKING` / `Removed` in `CHANGELOG.md` with a migration note |
| `v1.0.0` (major) | Stabilization; first compatibility commitment | From here on, semantic versioning applies strictly |

Consumers should:

- pin an exact version in `go.mod` (`require
  github.com/zeokeel/zeokeel-rig v0.9.0`) and upgrade deliberately;
- read the `## Unreleased` / release section of `CHANGELOG.md` before upgrading;
- prefer keyed struct literals (`rig.AgentSpec{Model: ...}`), which keep working
  when optional fields are added;
- treat the root contract surface as the stable integration point and avoid
  depending on internal details of other packages that the docs do not promise.

The v0.9 line specifically: the contract moved from `contracts/v1` to the module
root package `rig`, and the sandbox/network security hardening is documented in
[SECURITY.md](../SECURITY.md) and [docs/ARCHITECTURE.md](ARCHITECTURE.md).

## Post-1.0 rules

From `v1.0.0` on, strict semantic versioning applies:

- **Major** (`v2.0.0`): breaking changes; new import path
  `github.com/zeokeel/zeokeel-rig/v2`. The previous major continues to receive
  critical security and regression fixes for at least **6 months** after the
  successor major is released (maintainers may extend this). Fixes are shipped as
  `v1.x.y` tags on the old major path.
- **Minor** (`v1.Y.0`): additive API only; no breaking changes.
- **Patch** (`v1.Y.Z`): backwards-compatible fixes, documentation and security
  fixes only.

A new major is never created for cosmetic reasons, and an old major is never
deleted from the repository.

## Deprecation policy

- Deprecated API is marked in its documentation comment with Go's standard
  marker: `// Deprecated: use X instead.`
- It is listed in `CHANGELOG.md` (`Deprecated` category) and keeps working:
  - pre-1.0: for at least one minor release before removal;
  - post-1.0: until the next major version.
- Deprecated code stays tested until it is actually removed.
- Removal is `BREAKING` (pre-1.0) or lands in a new major (post-1.0), with a
  migration note.

## Tag immutability and retractions

Published tags are **immutable**:

- Never move, delete or force-push a published tag. The Go module proxy and the
  checksum database (`sum.golang.org`) cache tag content; a moved tag produces
  checksum mismatches and breaks existing builds for every consumer.
- Release tags are **annotated and signed** (`git tag -s vX.Y.Z -m "..."` with an
  SSH or GPG signing key listed in `.github/allowed_signers`). The release
  workflow verifies the signature with `git tag -v`; an unsigned or unverifiable
  tag fails the release before anything is created.
- A release tag must point at a commit on `main` and must be the commit that
  carries the release `CHANGELOG.md` section and the matching `version.go`.
  Publish the tag once; the release workflow verifies it and fails closed if the
  notes section is missing.

If a release is wrong:

1. Fix forward: land the fix on `main` and cut a new **patch** release
   (`vX.Y.Z+1`). Never re-tag.
2. If the release is so broken that consumers must not use it, add a
   [`retract`](https://go.dev/ref/mod#go-mod-file-retract) directive to `go.mod`
   in a later version, and publish a GitHub Security Advisory if the problem is
   security relevant. A retraction is a signal to `go` commands; it does not
   rewrite history.
3. If the GitHub Release itself is missing or malformed after a tag push, re-run
   the failed workflow job (see [docs/RELEASE.md](RELEASE.md)); the tag itself is
   still immutable.

Branch names, commit hashes, `version.go` on `main` and the current date are not
release identities — only the tag is.

## Pre-release versions

Tags of the form `vX.Y.Z-rc.N` (or `-beta.N`) are valid Go module versions and
follow the same tag rules. `go get` does not select pre-releases unless they are
requested explicitly. The release workflow creates a **draft** GitHub Release for
any `v*` tag and does not set `--prerelease`; publish pre-release tags by hand
(with `gh release edit --prerelease`) or extend the workflow before tagging.

## Go version and dependency policy

- The module requires **Go 1.27** (the `go 1.27` directive in `go.mod`). Raising
  the minimum is announced in `CHANGELOG.md` and `COMPATIBILITY.md`; while
  pre-1.0 it is treated as a compatibility-relevant change for consumers on older
  toolchains.
- The module has **zero module dependencies** and is standard-library only. The
  root `rig` package is a leaf that imports only the standard library (CI
  enforces both). New dependencies are not accepted unless the policy in this
  document and `CONTRIBUTING.md` changes first.
- `COMPATIBILITY.md` records the Go minimum per release line; `README.md` shows
  the current install tag.

## Changelog conventions

`CHANGELOG.md` follows [Keep a Changelog](https://keepachangelog.com/) and is the
single source of release notes:

- development work goes under `## Unreleased` with the standard categories
  (`Added`, `Changed`, `Deprecated`, `Removed`, `Fixed`, `Security`);
- a release replaces `Unreleased` with `## vX.Y.Z — YYYY-MM-DD` (em dash before
  the date is the house style; the release workflow matches the `## vX.Y.Z`
  prefix);
- there must be exactly **one** section per released version and exactly one
  `Unreleased` section at the top;
- breaking changes are marked `BREAKING`, `!` in the commit type, and include a
  migration note;
- the release workflow extracts this section verbatim; a release without a
  matching section fails.

The release procedure itself is in [docs/RELEASE.md](RELEASE.md).
