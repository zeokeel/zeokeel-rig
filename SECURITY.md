# Security Policy

> 中文文档以 [README.md](README.md) 与 [docs/zh/](docs/zh/README.md) 为主；本文件保留英文原文作为参考，对应中文版见 [docs/zh/SECURITY.md](docs/zh/SECURITY.md)。

ZeoKeel Rig is a pure-Go, cgo-free SDK (one dependency: the `mvdan.cc/sh/v3` POSIX
shell parser, used only by the sandbox policy layer) that runs the agent loop and
its tools **inside the host process**. This
document describes which versions receive security fixes, how to report a
vulnerability privately, and what the built-in sandbox policy does and does not
guarantee.

## Supported versions

Security fixes land on the latest patch of the current minor line only. Because
the module is still pre-1.0, there are no long-term support branches; consumers
should pin an exact version (see [docs/VERSIONING.md](docs/VERSIONING.md)) and
upgrade deliberately.

| Version | Supported |
| --- | --- |
| `0.14.x` | ✅ latest patch only |
| `0.13.x` | ❌ upgrade to `0.14.x` |
| `0.12.x` | ❌ upgrade to `0.14.x` |
| `≤ 0.10` | ❌ unsupported |

The Go minimum for the supported line is **Go 1.27** (declared in `go.mod`); a
consumer on an older toolchain should upgrade Go before upgrading Rig.

## Reporting a vulnerability

**Never open a public issue, pull request or discussion for a suspected
vulnerability.** Use GitHub's private vulnerability reporting:

1. Go to <https://github.com/zeokeel/zeokeel-rig/security/advisories/new> (the
   repository **Security** tab → **Report a vulnerability**). This is the only
   private channel; the repository has private vulnerability reporting enabled.
2. If the form is unavailable for your account, open a minimal public issue that
   says only "I need a private security channel" and mention `@zeokeel-dev`.
   Do **not** include vulnerability details, reproduction steps or credentials
   in a public issue.
3. Include as much of the following as you can:
   - affected version (`rig.Version` or the release tag) and the `go version` output;
   - operating system and, for sandbox issues, the policy configuration
     (`sandbox.New(...)` options) and whether an approval gate was installed;
   - a minimal reproduction (a Go test or small program) and the observed
     impact;
   - any suggested mitigation.

Do not include real API keys or private data in the report. Redact log output.

## What to expect

This is a volunteer-maintained open-source project; the targets below are best
effort, not a commercial SLA (see [SUPPORT.md](SUPPORT.md)):

- **Acknowledgement** of the report within 3 business days.
- **Initial assessment** (severity, affected versions, whether a fix is
  feasible) within 10 business days.
- A fix and a GitHub Security Advisory (GHSA) for confirmed issues, shipped in
  the next patch release of the supported line.
- **Coordinated disclosure**: we aim to publish the advisory within 90 days of
  the report (sooner when a fix is available). Please do not disclose publicly
  before the advisory unless we agree otherwise.
- Credit in the advisory unless you ask to stay anonymous.

We may close reports as out of scope if they only concern the items listed
under [Scope and threat model](#scope-and-threat-model); in that case we explain
why.

## Scope and threat model

Rig is an **in-process library**, not a hardened multi-tenant service. Code that
a host links into its own binary is trusted code. The built-in `sandbox` policy
constrains what the built-in tools may touch; it is **not an OS sandbox** and it
is not a sandbox for third-party tools.

The optional `sandbox/osrunner` add-on (default off) does provide OS-level
**file-effect** confinement on Linux/macOS/Windows through pure-Go platform
runners. It is still same-world confinement, not a container or VM: reads,
network, and process visibility stay unconstrained, Windows is `partial`, and
macOS depends on the deprecated `sandbox-exec`. Hosts opt in by calling
`osrunner.New` and injecting the runner with the tools' `WithRunner` option.

### In scope

- **Sandbox policy bypasses** in shipped code: path traversal / symlink escapes
  through `sandbox.ResolvePath`, glob or grep hidden-file leaks, command
  allowlist bypasses, and URL/SSRF policy bypasses in `tools/web`.
- **OS runner bypasses** in shipped `sandbox/osrunner` code: a confined command
  escaping the declared writable roots on the platform the runner claims to
  enforce (`full`), or a `Confine` error degrading to an unconfined spawn.
- **Command execution**: the `bash`/`pwsh`/terminal/job tools running something
  the configured policy should have denied.
- **Network policy**: `web_fetch` reaching loopback, private, link-local or
  cloud-metadata addresses when the policy forbids them, including through
  redirects or DNS rebinding.
- **Crashes from untrusted input**: a panic or unbounded resource use triggered
  by model output, tool arguments, an SSE/JSON-RPC/LSP frame (message headers are
  bounded at 64 KiB before allocation, including a single unterminated header
  line), or a URL.
- **Credential handling**: leaking API keys or environment variables through
  tool output, command environments or errors.

### Security hardening

The v0.9.1–v0.9.4 releases corrected the shell-allowlist boundary that v0.9.0
announced too broadly; v0.11.0 added the permission-mode fences. The authoritative behavior is documented in the package docs and
`docs/ARCHITECTURE.md`; the short version:

- **Shell command allowlists** (`sandbox.CheckShellCommand` + `CheckCommand`,
  used by `bash`, `pwsh`, `jobs` and `terminal_send`):
  - unquoted shell control operators (`;`, newlines, `|`, `&`, redirects,
    subshells) are rejected by default;
  - command substitution `$(...)` and backtick substitution are rejected
    **even inside double quotes** — `echo "$(id)"` executes in `/bin/sh`, so a
    quoted operator is not a literal;
  - process substitution (`<(...)`, `>(...)`) is rejected;
  - leading `VAR=value` environment assignments and path-form command names
    (`./tool`, `/bin/tool`) are rejected so `PATH=.`, `LD_PRELOAD=...` and a
    workspace binary cannot borrow an allow-listed basename;
  - allowlisted command names are matched as bare names against the policy.

  **Line continuations are modeled.** The parser removes POSIX backslash-newline
  continuations (outside single quotes) before evaluating the text, so
  `echo "$\<newline>(id)"` is rejected as the `$(...)` that `/bin/sh` will
  execute after joining the line. The same parser produces argv[0] for the
  allowlist check (bash, jobs and terminal_send share it), so validation and
  dispatch cannot disagree about quoting.

  `sandbox.WithAllowShellOperators()` opts out and makes the command allowlist
  advisory-only (a weak boundary; combine it with an approval gate). Allowlisting
  an interpreter (`sh`, `bash`, `env`, `xargs`, `python`, `eval`, ...) still
  gives that interpreter arbitrary code execution: do not allowlist interpreters
  for untrusted models. The allowlist is a policy layer for cooperative models,
  not a security boundary; the approval Gate is the boundary for untrusted models
  (and run the whole host in a container/VM when the model is untrusted).
- **`web_fetch` SSRF protection**: the fetcher resolves every dialed host itself
  and connects to a validated IP, so the checked address cannot change between
  validation and dial (DNS rebinding). Policies implementing
  `web.HostResolver` (such as `sandbox.Policy`) provide the validated addresses;
  every other non-nil policy (including `web.PolicyFunc`) gets a fetcher-side
  guard that rejects loopback, private, link-local, CGNAT and cloud-metadata
  addresses. `web.WithAllowPrivateHosts()` is the explicit opt-out for local
  targets; a nil policy installs no guard. Redirect hops are validated too.
  A host still decides *whether* the network is allowed:
  `sandbox.WithNetwork(true)` only permits public HTTP(S) destinations.
- **Permission modes (in-process fences, not an OS sandbox)**: `sandbox.Mode`
  provides `read-only` (file mutations and shell execution denied),
  `workspace-write` (default; writes only under the workspace roots, shell only
  via the command allowlist) and `danger-full-access` (no root/allowlist fence;
  explicit denylists still apply). `kit.Config.Approval` adds `ask` (default
  fail-safe read-only gate), `auto` (allow every call) and `never` (deny every
  call). These constrain the shipped tool implementations only: a hostile
  in-process tool, an allow-listed interpreter or an interactive terminal session
  can still do anything the host process can. The optional `sandbox/osrunner`
  add-on moves the **write/delete** part of the mode onto the OS (Linux
  bwrap/Landlock, macOS Seatbelt, Windows ACL), default off; it does not confine
  reads, network or process visibility, and its Windows rung is `partial`. Run
  the host in a container/VM for untrusted models — full OS isolation is a
  deployment layer, not an SDK feature.
- **`terminal_*`**: the shell binary must pass the command allowlist, and every
  `terminal_send` line is validated by the same operator/substitution and
  command checks as the non-interactive shell (shell builtins such as `cd` need
  to be allow-listed explicitly). Interactive shell state is still not a
  security boundary: use an approval gate, and run untrusted hosts in a
  container/VM.

### Out of scope

- Anything that requires running untrusted code in-process: custom `rig.Tool`,
  `rig.Provider`, `rig.Gate`, `rig.History`, searcher or skill implementations
  supplied by the host are trusted code by design.
- The approval `Gate`: a host that installs a permissive gate (or `nil`, which
  allows everything) has chosen to allow those calls. Gate policy is a host
  responsibility.
- OS, container or filesystem misconfiguration; running Rig as root; a workspace
  root that is writable by an untrusted party; `GOMAXPROCS`/kernel limits.
- The model endpoint or proxy itself, prompt-injection outcomes (an allowed tool
  doing something undesirable), billing/usage, or TLS interception by the host
  environment.
- Vulnerabilities in code the host links in addition to Rig (including
  `zeokeel-kernel` products), and missing TLS certificate pinning.
- Reports about older, unsupported versions without a demonstration on the
  supported line.
- Read, network, process-visibility or device confinement through
  `sandbox/osrunner`: its contract is file effects (write/delete/create) only,
  and the macOS rung depends on Apple keeping the deprecated `sandbox-exec`
  binary available.

## Supply chain

- The module has **exactly one dependency** — `mvdan.cc/sh/v3`, the POSIX shell
  parser used only by `sandbox`; CI pins the built import set and checks that
  the root `rig` package imports only the standard library.
- GitHub Actions are pinned to full commit SHAs in every workflow (Dependabot
  updates the pins in `.github/dependabot.yml`); the repository also requests
  SHA-pinned actions from GitHub.
- Releases are cut from **signed annotated tags** on `main` by
  `.github/workflows/release.yml`. The tag build reuses the complete CI workflow
  through `workflow_call` (so it runs exactly the merge gates), verifies the tag
  signature against `.github/allowed_signers`, verifies `version.go` against the
  tag and the tag's presence on `main`, runs a clean-room `go get`/`go mod
  verify` smoke of the published module, and then creates a **draft** GitHub
  Release. `contents: write` exists only on the publish job; a maintainer
  publishes the draft with the `publish-release` workflow after the
  post-release re-audit. See [docs/RELEASE.md](docs/RELEASE.md).
- The Go module proxy and `sum.golang.org` are the integrity anchor for the
  published artifact: `go mod verify` in the release smoke checks the downloaded
  module against the checksum database.
- Report a supply-chain concern through the same private channel as any other
  vulnerability.

## Code of conduct

Reports and follow-ups are covered by our
[Code of Conduct](CODE_OF_CONDUCT.md). Report harassment or conduct issues
through the private GitHub security-advisory form described above and mark the
report as a conduct concern; do not post details in a public issue.
