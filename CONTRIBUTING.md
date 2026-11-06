# Contributing to ZeoKeel Rig

Thanks for helping improve Rig. The project is MIT-licensed, has **zero module
dependencies**, and aims to stay that way: a Go 1.27 toolchain is the only
prerequisite.

By participating you agree to follow the [Code of Conduct](CODE_OF_CONDUCT.md).
For security issues, stop and read [SECURITY.md](SECURITY.md) — never open a
public issue for a vulnerability.

## Prerequisites

- **Go 1.27 or newer** (`go version`). `go.mod` declares `go 1.27`, and CI uses
  that file (`actions/setup-go` with `go-version-file: go.mod`).
- `git`, `awk`/`diff` for the README drift check, and (optionally) `make` for
  the convenience targets.
- No third-party tooling is required: formatting is `gofmt`, static checks are
  `go vet`, and the lint/coverage gates are Go tests in the repository.

## Development setup

```sh
git clone https://github.com/zeokeel/zeokeel-rig.git
cd zeokeel-rig
make build     # or: go build ./...
go test ./... -count=1
```

The repository has no generated files, no vendoring and no code generation step.

## Local gates (required before a pull request)

Run the same commands CI runs. `make ci` runs all of them in order; each target
can also be run separately:

| Gate | Command | What it checks |
| --- | --- | --- |
| Formatting | `make fmt` (`gofmt -l .` must be empty) | gofmt has been applied (`gofmt -w .` to fix) |
| Build | `make build` (`go build ./...`) | every package compiles |
| Vet | `make vet` (`go vet ./...`) | standard static checks |
| Tests | `make test` (`go test ./... -count=1`) | behavior, including the catalog gates' prerequisites |
| Race | `make race` (`go test ./... -count=1 -race`) | data races in concurrent paths |
| Catalog gate | `make gate` (`go test ./tools/full -run TestToolCatalogCoverage` and `go test ./tools/audit`) | no tool disappeared from the ledger; modern surface still covered |
| Dependencies | `make deps` | `go list -m all` shows only the module; root `rig` imports stdlib only |
| README drift | `make quickstart` | the README quickstart block matches `examples/quickstart/main.go` |
| Version | `make version` (`bash scripts/sync-version.sh`) | `version.go` matches the release tag; skipped on an untagged dev checkout, fails on a mismatch |
| Aggregate | `make ci` | all of the above |

Optional but recommended for parser, input-facing or hot-path changes:

```sh
make fuzz                       # 3s smoke for every Fuzz target in the module
make fuzz FUZZTIME=10s          # longer run
make bench                      # runs every benchmark once
make cover                      # coverage profile + total (writes coverage.out, git-ignored)
```

There is **no percentage coverage threshold**. Coverage is enforced structurally:
the tool-catalog and modern-surface audits (`make gate`) fail when a tool or
capability disappears, and new exported behavior is expected to come with tests.
`make cover` is for inspecting the number, not for gaming it.

## Repository rules

- **Zero dependencies.** Do not add a `require` to `go.mod`; the standard library
  must be enough. CI fails on any module dependency.
- **The root package is a leaf.** `package rig` at the module root is the stable
  contract surface; it must import only the standard library (CI checks
  `go list -deps` for this) and must not import other Rig packages.
- **Public API is additive.** A breaking change to the contract surface is a
  versioning event, not a normal PR — see [docs/VERSIONING.md](docs/VERSIONING.md).
  Add fields/interfaces rather than changing existing ones; mark removals with
  `// Deprecated:` and a CHANGELOG note.
- **Documentation comments** are required for every exported identifier, and
  `gofmt` must be clean. Match the surrounding style: standard library only,
  no panics in library code except documented compile-time programming errors
  (e.g. duplicate tool names passed to `tools.NewRegistry`).
- **Keep the release state out of feature PRs.** Do not edit `version.go`,
  `CHANGELOG.md`'s release sections or `LICENSE` unless the change *is* the
  release/documentation task. New work goes under `## Unreleased` in
  `CHANGELOG.md`.
- **Never move, delete or force-push a published tag.** Tags are immutable;
  see [docs/RELEASE.md](docs/RELEASE.md).

## Adding a tool

Adding a model-facing tool touches the ledger, the aggregator and the docs. The
full checklist with the exact files lives in
[docs/COVERAGE.md](docs/COVERAGE.md#adding-a-tool-checklist); summary:

1. Implement it in a new `tools/<name>` package (standard library only),
   returning one or more `rig.Tool` implementations with stable model-facing
   names, descriptions and JSON Schemas.
2. Wire the constructor into `tools/full/full.go` (`All`), otherwise the
   catalog gate fails.
3. Register the name in `tools/catalog/catalog.go` (`Static`); if it reproduces
   a tool from the legacy agent surface add it to `Legacy`, or list it in
   `ServerBound` when it only exists with an MCP/LSP client.
4. Add tests in the tool package (schema, happy path, invalid arguments, policy
   errors). Run `make test`, `make race` and `make gate`.
5. Update `docs/TOOLS.md`; update `tools/audit` tables and
   `docs/AUDIT-REPORT.md` when the tool closes a modern-surface capability; add
   a `CHANGELOG.md` entry under `## Unreleased`.
6. Run `make ci` and make sure the catalog counts in
   [docs/COVERAGE.md](docs/COVERAGE.md) still match if the tool set changed.

## Commit messages

Use [Conventional Commits](https://www.conventionalcommits.org/) with an
imperative, lower-case summary:

```
<type>(<scope>): <summary>

<why this change is needed>
```

- Types: `feat`, `fix`, `docs`, `test`, `refactor`, `perf`, `build`, `ci`,
  `chore`, `revert`.
- Breaking changes: append `!` to the type/scope (`refactor(rig)!:`) **and** add
  a `BREAKING CHANGE:` footer describing the migration.
- Reference issues in the footer (`Fixes #123`), keep one logical change per
  commit, and do not mix unrelated renames with behavior changes.

## Pull requests

- Fork the repository and branch from `main` (`feat/...`, `fix/...`,
  `docs/...`).
- Fill in the pull-request template: summary, linked issue, change type, the
  exact commands you ran, and the checklist.
- Keep the PR focused; splitting a large change into reviewable commits is
  welcome. Maintainers may squash on merge.
- CI must be green; reviewers may ask for the optional fuzz/bench run on
  parser/input changes.
- Breaking changes need a `CHANGELOG.md` `BREAKING` entry with a migration note
  and, when the contract surface changes, a plan in the PR description.
- Do not add dependencies, generated code, vendored code or secrets.

## License

Contributions are accepted under the **MIT license**, inbound = outbound: by
submitting a pull request you license your contribution under the repository's
[MIT license](LICENSE), the same terms the project distributes the code under.
There is **no CLA** to sign and no copyright assignment.

Only submit code you have the right to contribute. If a contribution is based
on another project, its license must be compatible with MIT and the derivation
must be disclosed in the PR. Keep the existing `LICENSE` file and copyright
line; per-file license headers are not required.

## Where to ask

- Usage questions and bugs: GitHub Issues using the templates (see
  [SUPPORT.md](SUPPORT.md)).
- Security: privately via [SECURITY.md](SECURITY.md).
- Conduct: dev@zeokeel.local.
