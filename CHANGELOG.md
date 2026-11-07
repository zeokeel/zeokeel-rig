# Changelog

## Unreleased

### Added

### Changed

### Deprecated

### Removed

### Fixed

### Security

## v0.10.1 — 2026-11-07

Native, dependency-free Linux OS sandbox (no external binary) and completion of
the standard-library modernization across code and tests.

### Added

- **Native Linux OS sandbox backend `landlock` (default).** `sandbox.NewOSExec`
  now re-execs the host binary with a hidden marker; a small `init()` runner
  applies Landlock write confinement (workspace + `WritableDirs` + `/tmp` +
  `/dev/null`/`tty`) and, when `AllowNetwork` is false, a seccomp filter denying
  `socket(AF_INET|AF_INET6)` and `io_uring_setup`, then `execve`s the real
  command. Pure Go standard library: **no bwrap/system binary, no cgo, no new
  module dependency**. The PTY terminal transport now works under this backend
  (previously it fell back to pipe).
- Integration tests: native filesystem confinement, seccomp network denial (with
  `AllowNetwork` enabled as the control), and PTY-under-sandbox. The real-bwrap
  integration tests remain for the optional backend.
- `Backend: "bwrap"` stays available; auto mode falls back to bwrap only when
  Landlock is unavailable and bwrap exists, otherwise it fails closed with
  `sandbox.ErrOSExecUnavailable`.

### Changed

- `sandbox.OSExec` Linux default backend is now `landlock` (`Backend()` reports
  it). No API removal; `bwrap` is opt-in/fallback.
- Completed standard-library modernization across packages and tests:
  `sort`→`slices` (30 hand-written sort lines deleted), `append([]T(nil),…)`→
  `slices.Clone` (54), `errors.As`→`errors.AsType` (14), `os.IsNotExist`→
  `errors.Is(err, fs.ErrNotExist)` (11), hand-rolled `itoa` removed (3),
  `fmt.Sprintf("%d")`→`strconv.Itoa/FormatInt` (5), `HasPrefix`+`TrimPrefix`→
  `strings.CutPrefix` (7), manual clamps→builtin `min` (3), `bytes.Index`→
  `bytes.Cut`, `slices.Delete/Contains`, `maps.Keys`+`slices.Sorted`,
  `sync.WaitGroup.Go`, `atomic.Int64`. No mechanical stdlib-replacement pattern
  remains in non-generated code.
- Docs/README/SECURITY/ARCHITECTURE/CONFIG updated: Linux needs no installed
  sandbox helper (Landlock ABI>=1, Linux 5.13+; network filter linux/amd64+arm64);
  macOS keeps the system-provided `sandbox-exec` (a pure-Go Seatbelt backend would
  require cgo, which is forbidden); Windows uses the native restricted token +
  built-in `icacls` low-integrity labels (network not isolated).

### Security

- Removes the bubblewrap install/PATH dependency from the default Linux sandbox
  path (smaller install and supply-chain surface); Landlock fails closed when the
  kernel does not support it.
- seccomp also blocks `io_uring_setup`, closing the "create a socket without the
  socket syscall" bypass.
- Landlock/seccomp are applied before `execve` in a re-exec'd copy of the host
  binary; restrictions are inherited across exec and cannot be dropped.

### Notes for operators

- Linux: kernel 5.13+ with Landlock enabled; on other architectures or kernels
  without Landlock, use `Backend: "bwrap"` or leave network enabled (Landlock-only
  mode needs no seccomp arch support).
- macOS: `sandbox-exec` is part of macOS; pure-Go native Seatbelt is not possible
  without cgo. Windows: filesystem/privilege confinement only, no network
  isolation (documented).

## v0.10.0 — 2026-11-07

Adds an opt-in OS-level process sandbox, replaces the hand-written shell scanner
with the established `mvdan.cc/sh/v3` POSIX parser, modernizes the toolset onto
the Go standard library, and makes cgo-free builds a CI contract.

### Breaking

- **One module dependency.** Rig now requires `mvdan.cc/sh/v3 v3.14.1` (pure Go,
  no transitive build dependencies) for shell parsing. `go list -m all` on a
  consumer shows this module plus that dependency; the module root stays
  standard-library-only and every build remains cgo-free (`CGO_ENABLED=0`).
- **Shell policy behavior.** The parser now models the shell's complete POSIX
  grammar, so `$((...))` arithmetic expansion is allowed when it contains no
  command substitution (previously rejected wholesale), comments are handled like
  the shell, and process substitution is reported as a POSIX parse error. The
  `ParseShellCommand`/`CheckShellCommand` signatures are unchanged; argv[0] may
  now be quoted (`'git' status` works) as long as it is a static literal.
- **Legacy constructors stay outside the sandbox.** `builtin.NewShell`,
  `jobs.NewManager()` and `terminal.NewManager()` without `WithPolicy` still spawn
  without policy or OS confinement (unchanged, documented); use the policy-aware
  constructors with `sandbox.WithOSExec`.

### Added

- **`sandbox.NewOSExec` / `sandbox.WithOSExec` / `kit.Config.OSExec`**: opt-in
  OS-level process sandbox for bash, pwsh, jobs, terminal and worktree git.
  - Linux: bubblewrap (`bwrap`), unprivileged; writable roots bind-mounted,
    read-only `/`, private `/tmp`, unshared PID/net/user namespaces, network off
    unless `AllowNetwork`.
  - macOS: `sandbox-exec` Seatbelt profile; writes limited to workspace/writable
    dirs + temp, network denied unless allowed.
  - Windows: restricted token + low integrity + `icacls` labels on writable
    roots; privileges dropped, writes confined to the workspace. **Network is not
    isolated on Windows** (use a container/VM for untrusted models).
  - Other platforms: fail closed with `sandbox.ErrOSExecUnavailable`.
  - All backends are cgo-free: system helper via `os/exec` on Linux/macOS, the
    standard library restricted-token `SysProcAttr` on Windows.
- `sandbox.ParseShellCommand` is now the single validation+argv parser shared by
  bash, pwsh, jobs and terminal (the three duplicated splitters are gone).
- `FuzzParseShellCommand` remains the parser fuzz target; new integration tests
  execute real `bwrap` sandboxed commands (writes inside/outside, network
  namespace) and construct the full 74-tool surface with an OS sandbox attached.
- `kit.Config.OSExec` (cgo-free convenience wiring; must be attached via
  `sandbox.WithOSExec` when a custom `Config.Policy` is used).

### Changed

- Shell parsing: the ~300-line hand-written lexer/continuation scanner is replaced
  by `mvdan.cc/sh/v3` AST validation plus a small POSIX line-continuation
  pre-pass; quoting, escapes, comments and continuation semantics come from the
  maintained parser.
- **Standard-library modernization across tools**: `sort` → `slices.Sort` /
  `slices.SortFunc` / `slices.SortStableFunc` (with `cmp.Compare`/`cmp.Or`),
  `append([]T(nil), …)` → `slices.Clone`, manual caps → builtin `min`/`max`,
  `sort.Ints`-dead-work removal, `slices.Contains`/`slices.Delete`,
  `maps.Keys` + `slices.Sorted`, `strconv.FormatInt` instead of hand-rolled itoa,
  `errors.Is(…, fs.ErrNotExist)`, `cmp.Or` in the provider, `sync.WaitGroup.Go`
  and `atomic.Int64` in the agent/workflow fan-out.
- CI: workflow-wide `CGO_ENABLED=0`; new **cgo-free** gate (asserts no package has
  `CgoFiles` and runs the suite with cgo disabled) and **dependency policy** gate
  (built imports are exactly the parser); the race step is the only cgo tooling
  use and is documented as such. Release workflow also runs cgo-free.
- `Makefile`: `CGO_ENABLED ?= 0` exported for all gates; `make race` opts into
  cgo tooling explicitly; `make deps` now enforces the dependency policy.
- `docs/CONFIG.md` documents `sandbox.NewOSExec` and Windows' network limitation;
  README/ARCHITECTURE/SECURITY/TOOLS/COMPATIBILITY/CONTRIBUTING/PR template were
  updated for the dependency, OS sandbox and cgo-free contract.

### Security

- **OS-level confinement (opt-in)** for every process-spawning model-facing tool;
  the in-process policy and approval Gate stay the policy/approval layers.
- The POSIX shell parser is now a maintained, fuzz-tested library
  (`mvdan.cc/sh/v3`) rather than a hand-rolled scanner; line continuations are
  still normalized to the shell's own behavior before AST validation.
- Supply chain: one pure-Go dependency, no cgo, `govulncheck` covers it, module
  checksums remain in `go.sum`/`sum.golang.org`.

### Docs

- README: dependency/cgo/OS-sandbox sections, platform support matrix for the
  sandbox backends, CI gate table, release history.
- `docs/ARCHITECTURE.md`: OS-sandbox layer and cgo-free contract; intention list
  updated (no longer claims OS sandboxing is absent).
- `docs/RELEASE.md`: release gates include the dependency-policy/cgo-free gates.
- `docs/VERSIONING.md`: current release line `v0.10.x`.

### Notes for consumers

- `go get github.com/zeokeel/zeokeel-rig@v0.10.0` is a drop-in upgrade for
  library consumers; the extra module is a pure-Go parser with no cgo or
  transitive build dependencies. To use the OS sandbox, construct
  `sandbox.NewOSExec` and attach it via `sandbox.WithOSExec` (or
  `kit.Config.OSExec`); install `bubblewrap` on Linux. Windows sandboxing confines
  filesystem writes/privileges but not the network.

## v0.9.4 — 2026-11-07

Security and correctness patch for the shell policy and LSP framing, plus
documentation-truth and release-gate corrections. No breaking changes: the
module path and root contract are unchanged (`rig.Version` reports `0.9.4`).

### Added

- `sandbox.ParseShellCommand(command string) ([]string, error)`: the single
  validation + word-splitting parser used by the built-in `bash`, `jobs` and
  `terminal_send` tools. `CheckShellCommand` remains as the validation-only
  API and delegates to the same parser.
- Fuzz target `FuzzParseShellCommand` (registered in CI) covering quotes,
  escapes, line continuations, comments and substitution constructs.

### Security

- **POSIX line continuations are modeled by the shell policy.** v0.9.1 rejected
  quoted `$(...)`/backticks but `/bin/sh` removes backslash-newline before
  tokenizing, so `echo "$\<newline>(id)"` (and the `${x:-$\<newline>(id)}`
  variant) still reached `/bin/sh` as `"$(id)"` and executed `id` with an
  allowlist of `echo`. The parser now strips backslash-newline outside single
  quotes with the shell's quote rules and validates the joined text; the
  regression corpus covers quoted, unquoted and parameter-expansion variants.
- **One parser for validation and argv[0].** The previous three copies of a
  quote-unaware splitter are gone; a divergence between the validator and the
  allowlist check can no longer be introduced per tool.
- **LSP headers are bounded before allocation, including one unterminated
  line.** v0.9.1 capped multi-line headers, but `bufio.Reader.ReadString`
  allocated the whole line before the cap applied (a 96 MiB stream without a
  newline grew the heap by ~92 MiB in the re-audit). The reader now uses
  `bufio.Reader.ReadLine` under a hard 64 KiB budget and a 128-line cap.

### Changed

- Shell comments that begin a word are handled like the shell. `$((...))`
  arithmetic expansion and parentheses inside `${...}` remain rejected
  conservatively because they may contain command substitution; pass
  `sandbox.WithAllowShellOperators` when a host needs full shell syntax and
  accepts the advisory allowlist.

### Docs

- Removed the "Windows/macOS test matrix" claim from README, `docs/RELEASE.md`
  and the release workflow comment: CI runs the unit suite on Linux and
  cross-compiles + `GOOS`-vets windows/darwin/freebsd (hosted macOS/Windows
  runners are unavailable for this repository).
- Updated `docs/VERSIONING.md` current release line, `docs/TOOLS.md`
  host-interface legend, `docs/ARCHITECTURE.md` shell/LSP security sections,
  `tools/lsp/doc.go` limits, and the README CI/security/platform text.
- Documented the repository-protection limits honestly (single maintainer,
  `enforce_admins=false`, GitHub commit verification needing
  `admin:ssh_signing_key`, release environment without a required reviewer);
  see the new "Repository protection status" section in `docs/RELEASE.md`.

### Notes for consumers

- `go get github.com/zeokeel/zeokeel-rig@v0.9.4` is a drop-in upgrade from
  v0.9.3. Scripts that relied on `echo "$((1+2))"` passing the allowlist policy
  need to quote the expansion result or compute it outside the shell; this is
  the conservative documented behavior since v0.9.1.

## v0.9.3 — 2026-11-07

Release-engineering patch; the SDK code is identical to v0.9.1/v0.9.2.

### Fixed

- **Tag signature verification, follow-up.** v0.9.2 restored the annotated tag
  object but passed the full ref (`refs/tags/vX.Y.Z`) to `git tag -v`, which
  resolves names under `refs/tags/` itself and therefore failed with *"tag not
  found"*. v0.9.3 passes the plain tag name after restoring and checking the tag
  object, so the release workflow verifies the SSH signature against
  `.github/allowed_signers` and proceeds to the clean-room smoke and draft
  release.

### Notes for consumers

- `go get github.com/zeokeel/zeokeel-rig@v0.9.3` behaves exactly like v0.9.1;
  this patch only changes how the GitHub Release is produced.

## v0.9.2 — 2026-11-07

Release-engineering patch; the SDK code is identical to v0.9.1.

### Fixed

- **Release workflow signature verification.** `actions/checkout` rewrites
  `refs/tags/<tag>` to the commit SHA for tag events, so the verify job saw a
  lightweight tag and failed with *"cannot verify a non-tag object of type
  commit"*; this blocked creation of the v0.9.1 GitHub Release (the tag and the
  module itself were already published and immutable). The workflow now
  force-fetches the annotated tag object from the remote, requires it to be of
  type `tag`, and only then runs `git tag -v` against
  `.github/allowed_signers`.

### Notes for consumers

- `go get github.com/zeokeel/zeokeel-rig@v0.9.2` behaves exactly like v0.9.1;
  this patch only changes how the GitHub Release is produced.

## v0.9.1 — 2026-11-07

Security and correctness hotfix for the v0.9 line, plus release-engineering and
documentation-truth fixes. No breaking changes: `rig.Version` reports `0.9.1`,
the module path and the root contract surface are unchanged.

### Security

- **Shell allowlists are now enforced against quoted command substitution.**
  v0.9.0 rejected unquoted `$(...)`/backticks but allowed them inside double
  quotes, where `/bin/sh` still executes them: `echo "$(id)"` bypassed an
  allowlist that did not include `id`. `sandbox.CheckShellCommand` now rejects
  command substitution `$(...)`, backticks and process substitution `<(...)`/
  `>(...)` anywhere except single quotes, and the check is applied by `bash`,
  `pwsh`, `jobs` and `terminal_send`.
- **Environment-assignment and path-form command hijacks are rejected.**
  `PATH=. tool`, `LD_PRELOAD=... tool` and `./tool`/`/path/tool` could make a
  model-provided command resolve to a workspace binary while the allowlist only
  saw an allow-listed basename. `sandbox.CheckCommand` now rejects leading
  `VAR=value` assignments and path-form command names: allowlisted commands are
  bare names. Documented caveat: allowlisting an interpreter (`sh`, `env`,
  `xargs`, `python`, `eval`, ...) still yields arbitrary code execution.
- **`web_fetch` SSRF guard covers policies without `web.HostResolver`.**
  `web.PolicyFunc`/custom `CheckURL` policies previously fell back to the
  standard dialer, so a public name resolving to loopback was dialed. The
  fetcher now resolves and validates every dialed host itself and rejects
  loopback/private/link-local/CGNAT/cloud-metadata addresses for every non-nil
  policy; `web.WithAllowPrivateHosts()` is the explicit opt-out for local
  targets (used by the tests and by hosts fetching local mirrors).
- **LSP message headers are bounded** (64 KiB) before allocation, closing the
  unbounded `ReadMIMEHeader` allocation (a hostile server could make the client
  allocate hundreds of MiB before the 16 MiB message cap applied).
- **LSP tools gained an installable path policy.** `lsp.WithPathPolicy` (wired by
  `tools/full.All` from the sandbox policy) validates model-provided paths before
  the tool reads a file, so `lsp_*` can no longer read arbitrary files outside
  the sandbox and leak them through language-server diagnostics.
- **`terminal_*` is bounded consistently with its documentation.** The shell
  binary must pass the command allowlist and each `terminal_send` line runs the
  same operator/substitution and command checks as the non-interactive shell.
- **Agent tool fan-out is bounded.** One model round now executes at most 8 tool
  bodies in parallel (`maxParallelToolCalls`) instead of one goroutine per call;
  a response with thousands of tool calls can no longer fan out thousands of
  concurrent executions.
- **Approval gates see normalized tool names.** A leading space in a
  model-provided tool name no longer slips past name-matching gates or the
  allow-always cache; dispatch, events, history and the gate all use the
  trimmed name.

### Fixed

- **Cancelled and gate-error tool rounds keep history replayable.** A run
  cancelled mid-tool, cancelled in the approval gate, or aborted by a gate error
  used to persist an assistant `tool_calls` message with no `role=tool` answers;
  the next request on that session was rejected (HTTP 400) forever. Every tool
  call is now answered with a tool result (synthetic cancellation/denial results
  where needed), and history writes use a non-cancelled context.
- **`team_spawn` no longer deadlocks at the runner concurrency limit.** The team
  manager held its lock while `SubagentRunner.Start` waited for capacity, so the
  fifth spawn (runner limit 4, member limit 8) blocked `team_list`/`team_wait`/
  `team_close` and every other team tool forever. Spawn reserves a slot, releases
  the lock and starts the child with the caller's context; the member limit also
  counts in-flight spawns.
- CI race fixed in the new coverage gate and lint job versions are pinned.

### Changed

- **Release workflow reuses the full CI gate set.** `.github/workflows/ci.yml` is
  now a reusable workflow (`workflow_call`) and `.github/workflows/release.yml`
  calls it, so the tag build runs exactly the merge gates (gofmt, build, vet,
  tests, race, coverage floors, fuzz, benchmarks, catalog/audit gates,
  zero-dependency and root-leaf policy, README drift, cross-compile + `GOOS`
  vet, examples, staticcheck, govulncheck).
- **Signed tags, tag-on-main check and clean-room smoke before a draft release.**
  The release workflow verifies the annotated tag's SSH signature against
  `.github/allowed_signers`, verifies the tagged commit is on `main`, runs
  `scripts/sync-version.sh --require-tag`, does a clean-room `go get`+
  `go mod verify` of the published module, and creates a **draft** GitHub
  Release. `publish-release.yml` (manual dispatch, `confirm_reviewed=yes`)
  publishes the draft after the post-release re-audit. `contents: write` is
  scoped to the publish job; all actions are pinned by commit SHA.
- **Coverage is gated per package, not only in total.** CI and `make ci` enforce
  80% total statement coverage *and* >=70% function coverage for every package
  with tests (`scripts/coverage-floor.sh`), so a large well-tested package cannot
  mask an under-tested one.
- **CI gained a `lint` job (staticcheck v0.8.2, govulncheck v1.9.0)** and
  cross-compile now also runs `go vet` with `GOOS` set (windows/darwin/freebsd),
  which compile-checks the tests too; `gofmt`/`make fmt` scan tracked Go files
  only. Hosted macOS/Windows runners are unavailable for this repository, so
  non-Linux runtime coverage is a documented limitation (README "Platform
  support"), not a claim.
- **Unix-only tools fail clearly on Windows**: `bash`, `jobs` and `terminal_*`
  report a configuration error instead of a missing-`/bin/sh` crash; use the
  `pwsh` tool. Platform support is documented in README/ARCHITECTURE/TOOLS.
- `report` closes its `os.Root` handle so the report file is not left locked.
- **Documentation truth pass.** Corrected the v0.9.0 shell/web_fetch hardening
  claims, the `terminal_*` security text, the `tools/full` and `docs/TOOLS.md`
  "in-memory defaults" overclaim, the CONTRIBUTING coverage contradiction, the
  RELEASE/VERSIONING process text, the `doc.go` import-graph claim, the
  `examples/custom-tool` package reference, the COMPATIBILITY Go-min rows and
  the AUDIT-REPORT snapshot note. Non-routable `dev@zeokeel.local` contacts were
  replaced with the private GitHub advisory channel (the only private channel).
- **Retroactive GitHub Releases** were created for v0.1.0–v0.8.0 from their
  CHANGELOG sections, so every published tag now has release notes. Tags are
  immutable and were not moved.

### Deprecated

- The 61 legacy tool-name aliases tracked by `catalog.Legacy` (for example
  `spawn_team`/`list_teams`/`wait_team`/`close_team`, `send_message`,
  `interrupt_agent`, `list_subagent_models`, `todo`) are **deprecated as of
  v0.9.1** in favor of the modern names (`team_spawn`, `team_list`, `team_wait`,
  `team_close`, `team_send`, `team_interrupt`, `todo_write`, ...). They keep
  working for at least one minor release (v0.10.0) per the deprecation policy in
  [docs/VERSIONING.md](docs/VERSIONING.md); removal is planned for v0.10.0.

## v0.9.0 — 2026-11-06

Root-package contracts, a one-import `kit` facade, security hardening, and the
engineering shell around the SDK (CI gates, fuzzing, governance, release automation).

### Breaking

- The contract package moved from `contracts/v1` to the module root: the stable
  extension surface is now `package rig` at `github.com/zeokeel/zeokeel-rig`.
  Migrate imports (`v1 "…/contracts/v1"` → `"github.com/zeokeel/zeokeel-rig"`) and
  references (`v1.Tool` → `rig.Tool`). Versioning is module-level: after v1.0.0 a
  breaking contract change gets a new major version at the import path (`.../v2`),
  never a versioned subdirectory.
- Unused sentinels `rig.ErrDenied` and `rig.ErrNoStream` were removed; the four
  remaining sentinels are all returned by the implementation.

### Added

- `kit`: one-import facade — `kit.New(kit.Config{Model: ...})` wires an
  OpenAI-compatible provider, `full.All` tools, a safe read-only default gate and
  in-memory history; `Agent.Run`/`Agent.RunText` (aggregated text + event stream);
  contract aliases and `kit.Version`.
- `rig.Version`: the module version is queryable from code and checked against the
  release tag (`scripts/sync-version.sh`).
- `tools/internal/toolutil`: shared helper layer for tool packages (JSON args,
  results, JSON-Schema builders, UTF-8 truncation). Tool packages went from 131
  helper definitions / 864 lines to 11 shared implementations.
- Fuzz targets (9) and benchmarks (15) for the SSE, MCP, LSP, URL and path parsers
  and the registry/agent hot paths; CI runs a 5s-per-target fuzz smoke and a
  benchmark smoke.
- Governance: `SECURITY.md`, `CONTRIBUTING.md`, `CODE_OF_CONDUCT.md`,
  `SUPPORT.md`, `CODEOWNERS`, issue/PR templates, dependabot, `.editorconfig`,
  `.gitattributes` and a `Makefile` that mirrors CI.
- Release automation: tag-driven `.github/workflows/release.yml` verifies the tag
  against `rig.Version`, re-runs the gates, extracts the matching CHANGELOG
  section and creates the GitHub Release; `scripts/sync-version.sh` enforces the
  invariant locally and in CI.
- Docs: `docs/VERSIONING.md`, `docs/RELEASE.md`, `docs/CONFIG.md`; the README was
  rewritten around the one-import quickstart and a complete package list.

### Changed

- Security hardening (still an in-process policy; the approval `Gate` remains the
  boundary):
  - shell tools now reject unquoted shell control operators (`;`, `|`, `&&`, `$()`,
    backticks, redirects, newlines), so an allow-listed first command cannot chain
    another program. `sandbox.WithAllowShellOperators` is the documented opt-out
    that makes the allowlist advisory-only; the same check is wired into the jobs
    and pwsh tools. Terminal sessions document that the allowlist does not bound
    commands typed into an interactive shell.
  - `web_fetch` resolves and validates every dialed host itself and pins the
    validated public IP when the policy implements `web.HostResolver` (as
    `sandbox.Policy` does), closing the DNS-rebinding SSRF gap; redirects are
    covered too.
- `go.mod` declares `go 1.27` instead of a patch-pinned `1.27.3`.
- README quickstart is now `examples/quickstart` (one import) and CI diffs the
  README block against it so it cannot rot.
- `docs/ARCHITECTURE.md` documents the enforced sandbox behavior and residual
  limits; the stale `tool` package name and removed assembly references are gone.
- MCP client version now follows `rig.Version` instead of a stale `0.3.0`.
- CI: gofmt, modern-surface audit, root-package stdlib/leaf gate, coverage floor
  (80%), fuzz smoke, benchmark smoke, cross-compilation (windows/darwin/freebsd)
  and the README drift check; the job gained `permissions: contents: read`,
  concurrency and a timeout.

### Fixed

- CI: the "contracts must stay standard-library only" step compared the module's
  own import path with `grep '\.'` and failed on every run since v0.1.0; it now
  filters `go list -deps` by the `Standard` field and excludes the package itself
  (the root package after the move).
- Coverage gate: `TestToolCatalogCoverage` now also fails when `full.All` builds a
  tool missing from `catalog.Static` (nine v0.7/v0.8 tools had been outside the
  ledger; 74 static names = 74 built tools).
- `sandbox.CheckPath` no longer trims whitespace from an already-resolved path, so
  legal file names ending in a space agree with `ResolvePath` (found by fuzzing).
- `agent` documents model precedence (`RunRequest.Model` > `AgentSpec.Model`; the
  provider default applies only when calling the provider directly).
- README/docs: duplicate "(this tag)" roadmap entries, partial layout/package
  tables and the `OPENAI_BASE_URL` misinformation are fixed.

### Removed

- Local `.probe/` development scripts are untracked and ignored; the legacy audit
  script requires the old-repo path instead of defaulting to a maintainer machine.
- Dead `rig.ErrDenied` / `rig.ErrNoStream` sentinels (see Breaking).

## v0.8.0 — 2026-11-06

Audit-gap close: remote MCP templates, subagent fork and structured output.

### Added

- `tools/mcp`: `ListResourceTemplates` / `mcp_list_resource_templates`
  (resources/templates/list with cursor pagination, uriTemplate dedupe;
  server-bound like the other MCP tools).
- `agent.NewRunner`: `RunnerOptions.ParentSessionID` (+ optional ParentHistory)
  for fork; `SubagentSpec.Fork` seeds a child with a copy of the parent session
  history, `SubagentSpec.OutputSchema` gives the child a `structured_output`
  tool validated against a JSON-Schema subset and returns
  `SubagentResult.Structured`.
- `tools/subagent`: `fork` and `output_schema` parameters; new `subagent_fork`
  tool (fork forced on).
- `contracts/v1`: `SubagentSpec.Fork`, `SubagentSpec.OutputSchema`,
  `SubagentResult.Structured` (additive).
- `tools/audit` + `docs/AUDIT-REPORT.md`: three gaps closed; the remaining 6 are
  reclassified as host/kernel-bound (cordis inspection, plugin manager/market,
  workspace dependencies, PTC run_code).
- `tools/catalog` + `tools/full.All`: 74 static tools, 62 modern names, no
  catalog gaps.

## v0.7.0 — 2026-11-06

Modern-surface gap close.

### Added

- `tools/skill`: `skill` tool (list/search/load) with a filesystem `SkillStore`
  (`*/SKILL.md`, top-level `*.md`, `$ZEOKEEL_SKILLS`,
  `<root>/.zeokeel/skills`, `~/.zeokeel/skills`).
- `tools/team` (`tasks.go`): shared task board - `team_task_create`,
  `team_task_list`, `team_task_get`, `team_task_update` over
  `v1.TeamTaskManager` with pending/in_progress/completed states, owner claim,
  blocked-by dependency cycles and an in-memory default manager.
- `tools/hosttools`: `goal action=update` with complete/blocked/pause/resume/edit
  via the optional `v1.GoalUpdater`; the in-memory Host implements the state
  machine (blocked reason retained).
- `tools/review`: `code_review` and `auto_review` over `v1.Reviewer`, plus
  `SubagentReviewer` (child-agent reviewer with `VERDICT:`/`FILE:LINE` parsing).
- `tools/audit`: modern tool-surface audit (`Modern`/`DocumentedGaps`,
  `Missing`) and `docs/AUDIT-REPORT.md`.
- `contracts/v1`: `GoalUpdater`/`GoalUpdateAction`, `TeamTask`,
  `TeamTaskFilter`, `TeamTaskUpdate`, `TeamTaskManager`.
- `tools/catalog`: `Modern`, `DocumentedGaps`, `AuditModern`; the coverage test
  now checks static + legacy + modern names.

### Changed

- `tools/full.All`: new `Skill`, `TeamTasks`, `Reviewer` options; the full
  registry is now 73 static tools.
- Relicensed from Apache-2.0 to MIT (`LICENSE`, `README.md`).

## v0.6.0 — 2026-11-06

The legacy agent tool surface is complete, with a coverage gate that keeps it
complete.

### Added

- `tools/hosttools`: `ask_user`, `present`, `plan_review`, `exit_plan_mode`,
  `goal` built on the new `contracts/v1` host interfaces (Asker/Presenter/
  PlanController/GoalStore) with an in-memory default `Host`.
- `tools/settings`: `settings_get` / `settings_set` over `v1.Settings`.
- `tools/memory`: `memory` tool (add/list/search/remove/clear) with an
  in-memory `v1.MemoryStore` default.
- `tools/schedule`: `schedule_create/list/update/delete` with an in-memory
  `v1.Scheduler` default (cron evaluation stays in the host).
- `tools/worktree`: `enter_worktree` / `exit_worktree` / `list_worktrees`
  (git worktree, sandbox-checked, `git worktree list --porcelain` parsing).
- `tools/session`: `session_search`, `session_trace`, `session_event_read`,
  `session_event_search`, `session_event_trace` over `v1.SessionLog`, plus an
  in-memory log implementation.
- `tools/inspect` (`inspect`) and `tools/report` (markdown session report
  written inside the sandbox root).
- `tools/ralph`: iterate a child agent via `v1.SubagentRunner` until DONE or
  the round limit.
- `tools/team`: legacy tool names (`spawn_team`, `list_teams`, `team_status`,
  `close_team`, `wait_team`, `send_message`, `interrupt_agent`,
  `list_subagent_models`) mapped onto the existing manager.
- `tools/terminal`: `terminal_list` (sessions with transport/cwd/state).
- `tools/lsp`: legacy single `lsp` tool with action routing.
- `tools/misc`: legacy `todo` alias sharing the `todo_write` store.
- `tools/full`: `All(options)` aggregating every static tool.
- `tools/catalog`: coverage ledger (`Static`, `Legacy`, `ServerBound`,
  `Dynamic`), `Check` and `AuditLegacy`; `tools/full` runs them as
  `TestToolCatalogCoverage` so a missing legacy tool fails CI.
- `contracts/v1/extensions.go`: host interfaces and store contracts
  (Asker, Presenter, PlanController, GoalStore, MemoryStore, SkillStore,
  Scheduler, SessionLog, Reviewer + types).

### Notes

- 66 static tools, 61 legacy names, 5 server-bound (MCP resources/prompts and
  the legacy `lsp` tool), dynamic `mcp__<server>__<tool>` names.
- Zero dependencies, standard library only.

## v0.5.0 — 2026-11-06

Real terminal, remote MCP and in-process orchestration. Still zero dependencies.

### Added

- `tools/terminal`: real unix PTY transport (`/dev/ptmx` + ioctl, setsid/setctty,
  interactive shell); `terminal_send` uses a quiet period instead of a marker, ANSI
  is stripped by default, Ctrl-C is delivered as `0x03`; pipe transport remains as a
  fallback (Windows/other platforms) and keeps its previous semantics.
- `tools/mcp`: Streamable HTTP transport — POST JSON-RPC, `Mcp-Session-Id`,
  `MCP-Protocol-Version`, JSON or SSE responses, session-expiry errors, concurrency
  and cancellation; `Config{Transport,URL,Headers,HTTPClient}` and `StartHTTP`.
  The existing stdio transport is unchanged.
- `contracts/v1/subagent.go`: `SubagentSpec`, `SubagentResult`, `Subagent`,
  `SubagentRunner` (additive).
- `agent.NewRunner`: `v1.SubagentRunner` with depth (`MaxDepth`) and concurrency
  (`MaxConcurrent`) limits, tool exclusion (`ExcludedTools`, defaults include the
  orchestration tools) and per-child isolated history.
- `tools/subagent`: `subagent` tool (one child agent, blocking result).
- `tools/workflow`: `workflow` tool — sequential/parallel subagent steps with
  `{{prev}}` / `{{steps.<name>}}` templating.
- `tools/team`: managed subagents — `team_spawn/team_list/team_send/team_wait/
  team_last/team_close/team_interrupt` with id/name resolution, member limits and
  lifecycle state machine.

### Changed

- `agent.DefaultExcludedTools` now lists the shipped orchestration tool names
  (`subagent`, `workflow`, `team*`) so child agents cannot recurse by default.

## v0.4.0 — 2026-11-06

Complete agent tool surface, still zero dependencies.

### Added

- `tools/editor`: `str_replace_editor` — view/create/str_replace/insert with line
  numbers, sandbox-root checks, read-only enforcement and output caps.
- `tools/image`: `read_image` — magic-byte MIME detection (PNG/JPEG/GIF/WEBP/BMP)
  returning the image in the new `contracts/v1.ToolResult.Parts` (the agent delivers
  attachments as a follow-up user message).
- `tools/terminal`: stateful shell sessions — `terminal_open/send/read/signal/close`
  over a persistent pipe-based shell (marker-terminated commands; not a PTY yet).
- `tools/jobs`: background processes — `job_start/output/kill/list/wait` with ring
  buffered output, exit codes, signals and cleanup.
- `tools/mcp`: resources/prompts support — `ListResources/ReadResource/ListPrompts/
  GetPrompt` and the `mcp_list_resources` / `mcp_read_resource` / `mcp_list_prompts` /
  `mcp_get_prompt` tools.
- `tools/builtin`: `pwsh` tool (pwsh/powershell detection, `-NoProfile
  -NonInteractive -Command`, policy-aware).
- `contracts/v1.ToolResult.Parts` for multimodal tool attachments (additive).

### Changed

- Repository layout: tool packages moved from `tool/` to `tools/` (package `tools`),
  with implementations in `tools/{builtin,editor,image,terminal,jobs,web,mcp,lsp,misc}`.
- `tools.Registry.Unregister(name)` for dynamically composed tool sets.

## v0.3.0 — 2026-11-06

Complete agent tool surface (still zero dependencies).

### Added

- `sandbox`: policy layer for agent tools — allowed roots (symlink-safe `os.Root`
  resolution), read-only mode, command allow/deny lists, environment allowlist,
  network switch with private-address URL guard, output/timeout caps, denied globs.
- `tools/builtin`: `NewWithPolicy(*sandbox.Policy)` runs the full built-in toolset under
  the policy (existing `NewFS`/`NewShell`/`NewBuiltins` keep working).
- `tools/web`: `web_fetch` (HTTP(S), redirect/size caps, HTML→text) and `web_search`
  (pluggable `Searcher`, no built-in vendor).
- `tools/mcp`: standard-library MCP client over stdio JSON-RPC 2.0 — initialize
  handshake, paginated tools/list, tools/call (text/image/audio/resource), ping,
  concurrent calls, cancellation, graceful close — plus `Tools()` adapting MCP tools
  into Rig tools (`mcp__<server>__<tool>`).
- `tools/lsp`: standard-library LSP client (Content-Length framing, initialize,
  hover/definition/documentSymbols/diagnostics) and `lsp_*` tools.
- `tools/misc`: `working_directory` (root-checked cwd) and in-memory `todo_write`.
- `docs/TOOLS.md`: full old-agent tool catalog mapped to Rig / product / roadmap.

## v0.2.0 — 2026-11-06

**Breaking: Rig is now a pure toolset with zero dependencies.**

### Removed (moved back to the ZeoKeel product repo)

- `assembly`, `host`, `kernelx`, `keys`, `modules/appinfo` — compile-time module
  assembly, preflight/rollback, headless host and the appinfo module now live in the
  product that uses the kernel; Rig no longer depends on `zeokeel-kernel`.
- `examples/minimal`, `examples/custom-module` (module-assembly examples).

### Added

- `examples/agent` — provider + built-in tools + gate + history + streaming loop.
- `examples/custom-tool` — a third-party `Tool` implementation, registry and invoke.

### Changed

- `go.mod` has no `require` entries; `go list -m all` shows only this module.

## v0.1.0 — 2026-11-06

First public tag of the headless agent SDK.

### Added

- `contracts/v1`: messages/content parts, tool calls, streaming chunks, `Provider`,
  `Tool`, `Gate`, `History`, agent events, `Agent`/`AgentSpec`, sentinel errors.
  Standard library only; the API compatibility generation.
- `agent`: eino-free streaming ReAct loop — model rounds, parallel tool calls,
  approval gate, iteration cap, cancellation, synchronous event stream, history
  persistence.
- `provider/openai`: OpenAI-compatible streaming provider — SSE parsing, tool-call
  deltas, reasoning content, usage, 429/5xx retries, context cancellation.
- `tools` + `tools/builtin`: registry and seven built-in tools (`read_file`,
  `write_file`, `edit_file`, `list_dir`, `glob`, `grep`, `bash`) with root sandbox,
  output caps and timeouts.
- `gate`: `AllowAll`, `DenyAll`, `AllowTools`, `Func`.
- `session`: in-memory `History`.
- `assembly` + `modules/appinfo`: compile-time registry, embedded headless/minimal
  assemblies, kernel preflight/rollback, build-identity module.
- `host`: headless runner with flags, settings file, logger, signals and graceful
  shutdown.
- `kernelx` (public): kernel descriptors/service keys/registry/effect helpers for module authors;
  `keys`: shared service keys (`AppInfoKey`, `SettingsKey`, `LoggerKey`).
- `examples/minimal`, `examples/custom-module`.
