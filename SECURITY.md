# Security Policy

ZeoKeel Rig is a pure-Go SDK (zero module dependencies, standard library only)
that runs the agent loop and its tools **inside the host process**. This
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
| `0.9.x` | ✅ latest patch only |
| `0.8.x` | ❌ upgrade to `0.9.x` |
| `< 0.8` | ❌ unsupported |

The Go minimum for the supported line is **Go 1.27** (declared in `go.mod`); a
consumer on an older toolchain should upgrade Go before upgrading Rig.

## Reporting a vulnerability

**Never open a public issue, pull request or discussion for a suspected
vulnerability.** Use GitHub's private vulnerability reporting:

1. Go to <https://github.com/zeokeel/zeokeel-rig/security/advisories/new> (the
   repository **Security** tab → **Report a vulnerability**).
2. If private reporting is unavailable for your account, email the maintainers
   at **dev@zeokeel.local** with a subject beginning `SECURITY:`. Encrypt only
   if you have a previously exchanged key; never send credentials or live
   tokens.
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

### In scope

- **Sandbox policy bypasses** in shipped code: path traversal / symlink escapes
  through `sandbox.ResolvePath`, glob or grep hidden-file leaks, command
  allowlist bypasses, and URL/SSRF policy bypasses in `tools/web`.
- **Command execution**: the `bash`/`pwsh`/terminal/job tools running something
  the configured policy should have denied.
- **Network policy**: `web_fetch` reaching loopback, private, link-local or
  cloud-metadata addresses when the policy forbids them, including through
  redirects or DNS rebinding.
- **Crashes from untrusted input**: a panic or unbounded resource use triggered
  by model output, tool arguments, an SSE/JSON-RPC/LSP frame, or a URL.
- **Credential handling**: leaking API keys or environment variables through
  tool output, command environments or errors.

### Security hardening in v0.9.0

The v0.9 line closes two attack paths that earlier releases only mitigated
best-effort. The authoritative behavior is documented in the package docs and
`docs/ARCHITECTURE.md`; the short version:

- **Shell command allowlists** (`sandbox.CheckCommand` and the `bash` tool) now
  reject unquoted shell control operators by default when an allowlist is
  configured — `;`, newlines, `|`, `&`, `$()`, backticks, redirections and
  subshells cannot be used to smuggle a second command past a one-word
  allowlist. Hosts that genuinely need pipelines can opt in through
  `sandbox.WithAllowShellOperators()`, which is documented as a weak boundary
  and should be combined with an approval gate.
- **`web_fetch` SSRF protection**: the URL policy resolves hostnames and checks
  every resolved IP (loopback, private, link-local and metadata ranges) before
  connecting, and the dialer connects to the validated IP so a second DNS
  answer cannot redirect the connection. Redirect hops are validated too.
  A host still decides *whether* the network is allowed:
  `sandbox.WithNetwork(true)` only permits public HTTP(S) destinations.

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

## Supply chain

- The module has **zero module dependencies**; CI fails if `go list -m all`
  reports anything beyond the module itself, and the root `rig` package is
  checked to import only the standard library.
- GitHub Actions dependencies are tracked by Dependabot
  (`.github/dependabot.yml`), pinned by major version in workflows.
- Releases are cut from annotated tags by `.github/workflows/release.yml`, which
  re-runs the CI gates and verifies `version.go` against the tag before
  publishing; the workflow has `contents: write` only for creating the release.
  See [docs/RELEASE.md](docs/RELEASE.md).
- Report a supply-chain concern through the same private channel as any other
  vulnerability.

## Code of conduct

Reports and follow-ups are covered by our
[Code of Conduct](CODE_OF_CONDUCT.md). Harassment or conduct issues go to
dev@zeokeel.local.
