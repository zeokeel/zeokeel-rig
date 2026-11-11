# Support

> 中文文档以 [README.md](README.md) 与 [docs/zh/](docs/zh/README.md) 为主；本文件保留英文原文作为参考（暂无对应中文译本）。

ZeoKeel Rig is maintained by volunteers. Support is **community best effort**:
there is no commercial support offering, no response-time SLA for usage
questions, and no private support channel.

## Where to ask

| Topic | Channel |
| --- | --- |
| Bug in the SDK | [GitHub Issues → Bug report](https://github.com/zeokeel/zeokeel-rig/issues/new?template=bug_report.yml) |
| Feature or API request | [GitHub Issues → Feature request](https://github.com/zeokeel/zeokeel-rig/issues/new?template=feature_request.yml) |
| Usage / "how do I…" | GitHub Issues (bug-report form is fine; mark it as a question in the title) or a discussion if the repository has discussions enabled |
| Security vulnerability | **Private only** — see [SECURITY.md](SECURITY.md); never a public issue |
| Code of conduct concern | private GitHub security-advisory form (see [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md)) |
| Maintainer contact for administrative matters | open an issue mentioning `@zeokeel-dev` |

Before opening an issue, search the existing issues and check:

- [README](README.md) — install, quickstart, repository layout.
- [docs/CONFIG.md](docs/CONFIG.md) — configuration conventions and defaults.
- [docs/TOOLS.md](docs/TOOLS.md) and [docs/COVERAGE.md](docs/COVERAGE.md) — the tool catalog.
- [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) — package layering and sandbox design.
- [docs/VERSIONING.md](docs/VERSIONING.md) and [COMPATIBILITY.md](COMPATIBILITY.md) — which versions are compatible.
- [CHANGELOG.md](CHANGELOG.md) — what changed between releases.
- Package docs on <https://pkg.go.dev/github.com/zeokeel/zeokeel-rig>.

## What to include in a request

A minimal, self-contained reproduction is the single most useful thing you can
provide. Please include:

1. The Rig version (`rig.Version` or the release tag from `go.mod`).
2. `go version` output and the operating system/architecture.
3. The relevant part of your `go.mod` (module path, replace directives).
4. The exact command that fails and the full error/stack trace (redact secrets).
5. For sandbox/tool issues: the policy configuration (`sandbox.New(...)` options,
   roots, network setting) and whether an approval `Gate` was installed.

## Supported versions

Only the latest patch release of the current `0.14.x` line receives fixes; see
[SECURITY.md](SECURITY.md#supported-versions) and
[docs/VERSIONING.md](docs/VERSIONING.md). Pin an exact version in your `go.mod`
and upgrade deliberately while the module is pre-1.0.

## What we cannot help with

- Debugging your model endpoint, gateway, proxy or provider credentials.
- Design reviews of products built on Rig beyond a public issue.
- Private or priority support; paid support is not offered.
- Hosts that disable the security mechanisms (for example passing a `nil` gate
  or opening the sandbox wider than they intend) — see the threat model in
  [SECURITY.md](SECURITY.md#scope-and-threat-model).

## Contributing

Fixes and improvements are welcome; see [CONTRIBUTING.md](CONTRIBUTING.md). The
fastest way to get a bug fixed is a pull request with a test that reproduces it.
