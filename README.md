# ZeoKeel Rig

[![CI](https://github.com/zeokeel/zeokeel-rig/actions/workflows/ci.yml/badge.svg)](https://github.com/zeokeel/zeokeel-rig/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/zeokeel/zeokeel-rig.svg)](https://pkg.go.dev/github.com/zeokeel/zeokeel-rig)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)
[![Release](https://img.shields.io/github/v/release/zeokeel/zeokeel-rig)](https://github.com/zeokeel/zeokeel-rig/releases)

**A pure Go agent toolset** — model providers, a streaming ReAct loop, tools,
permission modes and approval gates, and session history. One small
dependency: [`mvdan.cc/sh/v3`](https://pkg.go.dev/mvdan.cc/sh/v3) (POSIX shell
parser, used only by the sandbox policy); everything else is the Go standard
library. No cgo: builds and tests run with `CGO_ENABLED=0`.

Rig is the reusable agent core extracted from [ZeoKeel](https://github.com/zeokeel).
It does **not** include the module runtime or process assembly: products keep that
layer (ZeoKeel uses [zeokeel-kernel](https://github.com/zeokeel/zeokeel-kernel) for
compile-time module assembly). The stable contract types live in the module root
(`package rig`). For the shortest path use the [`kit`](kit) facade: `kit.New` wires
an OpenAI-compatible provider, the full sandboxed tool set, a safe default gate and
history in **one import**. Import the subpackages (`agent`, `tools`,
`provider/openai`, `gate`, `session`) when you want to assemble your own binary.

## Quickstart

The code below is `examples/quickstart/main.go`: one import, one constructor. CI
compiles it on every push and checks this README block still matches the file, so the
first thing you copy always builds.

```go
// Command quickstart is the smallest complete agent built from the Rig SDK: the
// kit facade wires an OpenAI-compatible provider, the full sandboxed tool set, a
// safe read-only approval gate and in-memory history in one import.
//
// It needs credentials to talk to a model:
//
//	OPENAI_API_KEY=... go run ./examples/quickstart "list the repo and summarize it"
//
// The provider defaults to https://api.openai.com/v1. Set Config.BaseURL for an
// OpenAI-compatible gateway and Config.Model for the model. Without an API key
// the program still builds; the first run reports the provider error.
package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/zeokeel/zeokeel-rig/kit"
)

func main() {
	prompt := strings.Join(os.Args[1:], " ")
	if prompt == "" {
		prompt = "List the files in this directory and summarize them."
	}

	// One import, one constructor: kit.New defaults to the full tool set
	// sandboxed to the working directory, shell execution disabled (no command
	// allowlist), no network, and a gate that only allows
	// read_file/list_dir/glob/grep. Supply Config.Gate/CommandAllowlist to widen
	// what the agent may do.
	a, err := kit.New(kit.Config{
		Model: "gpt-4o-mini",
		// For an OpenAI-compatible gateway:
		// BaseURL: "https://api.deepseek.com/v1", APIKeyEnv: "DEEPSEEK_API_KEY",
	})
	if err != nil {
		fail(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// RunText returns the aggregated assistant text; emit streams it live.
	_, result, err := a.RunText(ctx, "quickstart", prompt, func(event kit.Event) {
		if event.Type == kit.EventTextDelta {
			fmt.Print(event.Text)
		}
	})
	if err != nil {
		fail(err)
	}
	fmt.Println()
	fmt.Printf("finish=%s iterations=%d tokens=%d\n", result.FinishReason, result.Iterations, result.Usage.TotalTokens)
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "quickstart:", err)
	os.Exit(1)
}
```

Run it against any OpenAI-compatible endpoint with credentials:

```sh
OPENAI_API_KEY=... go run ./examples/quickstart "list the repo and summarize it"
```

## Install

```sh
go get github.com/zeokeel/zeokeel-rig@v0.11.0
```

Requires Go 1.27+ and builds with cgo disabled (`CGO_ENABLED=0`). The module has
exactly one dependency, the POSIX shell parser `mvdan.cc/sh/v3`, imported only by
the `sandbox` policy layer; the module root (`package rig`) remains
standard-library only, a leaf of the import graph, and imported by every other
package (CI enforces the dependency policy, the cgo-free build and the stdlib
root).

## Repository layout

```
*.go (root)        package rig: stable extension surface (standard library only)
agent/             streaming ReAct loop, subagent runner/fork, structured output
provider/openai/   OpenAI-compatible streaming provider
sandbox/           agent sandbox policy (roots, commands, env, network, caps)
gate/              approval gates
kit/               one-import facade: New/Config/Agent/RunText + contract aliases
session/           in-memory History
tools/             tool registry (name lookup, specs, invoke)
  audit/           modern tool-surface audit gate (reference vs Rig)
  builtin/         read_file/write_file/edit_file/list_dir/glob/grep/bash (+pwsh)
  catalog/         coverage ledger: Static / Legacy / Modern / ServerBound
  editor/          str_replace_editor (view/create/str_replace/insert)
  full/            All(options): builds every static tool in one call
  hosttools/       ask_user/present/plan_review/exit_plan_mode/goal
  image/           read_image (multimodal ToolResult.Parts)
  inspect/         inspect (sessions/events/workspace/stats)
  internal/        internal toolutil helpers (not importable from outside the module)
  jobs/            job_start/output/kill/list/wait (background processes)
  lsp/             LSP stdio client + lsp_* tools
  mcp/             MCP stdio + Streamable HTTP client, tools/resources/prompts
  memory/          memory tool over rig.MemoryStore
  misc/            working_directory, todo_write (+ legacy todo)
  ralph/           ralph (iterate a child agent until DONE/limit)
  report/          report (markdown session report inside the sandbox root)
  review/          code_review / auto_review over rig.Reviewer
  schedule/        schedule_create/list/update/delete
  session/         session_search/trace/event_* over rig.SessionLog
  settings/        settings_get / settings_set
  skill/           skill (list/search/load) + filesystem SkillStore
  subagent/        subagent + subagent_fork (one in-process child agent)
  team/            team_* tools + team_task_* shared task board
  terminal/        terminal_open/send/read/signal/close/list (unix PTY, pipe fallback)
  web/             web_fetch + pluggable web_search
  workflow/        workflow tool (sequential/parallel subagent steps + templating)
  worktree/        enter_worktree / exit_worktree / list_worktrees
examples/          runnable examples: quickstart (kit), agent, custom-tool
docs/              ARCHITECTURE, TOOLS, COVERAGE, AUDIT-REPORT, CONFIG, VERSIONING, RELEASE
scripts/           sync-version.sh + legacy tool-surface audit
.github/           CI, tag-driven release workflow, issue/PR templates, dependabot
Makefile           developer tasks (make help / make ci / make fuzz / make bench)
```

## Packages

| Package | What it is |
| --- | --- |
| `rig` (module root) | Stable extension surface: messages, streaming chunks, `Provider`, `Tool`, `Gate`, `History`, agent events, `Agent`/`AgentSpec`, host/store interfaces. Stdlib only; imported by every other package. |
| `agent` | Streaming ReAct loop: model rounds, parallel tool calls, approval gate, iteration cap, cancellation, synchronous event stream; subagent runner, fork and structured output. |
| `kit` | One-import facade: `kit.New(kit.Config{...})` wires provider + tools + gate + history + agent; `Agent.Run`/`Agent.RunText` (streams events, returns aggregated text); re-exports the contract types so quickstarts need one import. |
| `provider/openai` | OpenAI-compatible streaming provider (SSE, tool-call deltas, reasoning, usage, retries). |
| `sandbox` | Agent sandbox policy: allowed roots, read-only, command allow/deny lists, env allowlist, network + URL guard, output/timeout caps, denied globs. |
| `gate` | Approval gates: `AllowAll`, `DenyAll`, `AllowTools(...)`, `Func(...)`. |
| `session` | In-memory conversation `History`. |
| `tools` | Tool registry: name-based lookup, deterministic specs, concurrency-safe snapshot API. |
| `tools/full` | `All(options)`: every static tool in one slice (hosts that want the complete registry start here). |
| `tools/catalog` | Coverage ledger + `Check`/`AuditLegacy`/`AuditModern`; the release gate. |
| `tools/audit` | Modern tool-surface audit (reference/mapped/gaps) enforced by a test. |
| `tools/builtin` | `read_file`, `write_file`, `edit_file`, `list_dir`, `glob`, `grep`, `bash`, `pwsh` — sandboxed via `builtin.NewWithPolicy`. |
| `tools/editor` | `str_replace_editor`: view/create/str_replace/insert. |
| `tools/image` | `read_image`: magic-byte MIME, image in `ToolResult.Parts`. |
| `tools/terminal` | `terminal_*`: real unix PTY with pipe fallback, ANSI strip, Ctrl-C. |
| `tools/jobs` | `job_*`: background processes with ring buffers and exit codes. |
| `tools/web` | `web_fetch` (HTTP(S), redirect/size caps, HTML→text) and `web_search` (pluggable `Searcher`). |
| `tools/mcp` | MCP stdio + Streamable HTTP client; tools/resources/prompts/templates as `rig.Tool`. |
| `tools/lsp` | LSP stdio client and `lsp_hover` / `lsp_definition` / `lsp_diagnostics` / `lsp_symbols`. |
| `tools/subagent` | `subagent` and `subagent_fork` (in-process child agent). |
| `tools/workflow` | Sequential/parallel subagent steps with `{{prev}}`/`{{steps.name}}` templating. |
| `tools/team` | Managed subagents (`team_*`) plus the `team_task_*` shared task board. |
| `tools/ralph` | Iterate a child agent until `DONE` or the round limit. |
| `tools/skill` | `skill` (list/search/load) over filesystem `SKILL.md` stores. |
| `tools/review` | `code_review` / `auto_review` over `rig.Reviewer` (incl. `SubagentReviewer`). |
| `tools/hosttools` | Host UI interfaces with in-memory defaults: `ask_user`, `present`, `plan_review`, `exit_plan_mode`, `goal`. |
| `tools/settings` | `settings_get` / `settings_set` over `rig.Settings`. |
| `tools/memory` | `memory` over `rig.MemoryStore`. |
| `tools/schedule` | `schedule_*` CRUD over `rig.Scheduler`. |
| `tools/session` | `session_*` tools over `rig.SessionLog`. |
| `tools/inspect`, `tools/report` | Diagnostics and markdown session report. |
| `tools/worktree` | Git worktree tools, sandbox-checked. |
| `tools/misc` | `working_directory`, `todo_write` (+ legacy `todo`). |
| `examples/quickstart`, `examples/agent`, `examples/custom-tool` | Runnable examples (the agent ones need model credentials). |

## Gates (CI)

Correctness is a test result, not a claim. Every push and pull request runs:

| Gate | What it enforces |
| --- | --- |
| `gofmt`, `build`, `vet` | formatting and compilation |
| `test`, `test -race`, `-coverprofile` | behavior and data races |
| coverage floors | **80% total statement coverage** and **>=70% function coverage for every package with tests** (a total-only number cannot mask an under-tested package) |
| `lint` | staticcheck v0.8.2 and govulncheck v1.9.0 |
| cross-OS | windows/darwin/freebsd cross-compile **and** `go vet` with `GOOS` set (compile-checks the tests too); the unit suite itself runs on Linux (hosted macOS/Windows runners are unavailable for this repository) |
| `tools/catalog` + `tools/full` | every `catalog.Static` tool is built by `full.All`, every built tool is listed, legacy names are built or server-bound, no duplicates |
| `tools/audit` | the modern agent tool surface is mapped with no undocumented gaps |
| root `rig` package | standard-library only **and** a leaf of the import graph (no rig subpackage imports) |
| dependency policy | the **built** import set is exactly `mvdan.cc/sh/v3` (parser); the module root stays stdlib-only |
| cgo-free | every package has zero `CgoFiles`; all builds/tests run with `CGO_ENABLED=0` (the race-detector step is the only cgo tooling use) |
| fuzz smoke | 10 fuzz targets (SSE/JSON-RPC/LSP framing, shell lexer, URL/path policy) run 5s each |
| benchmark smoke | all benchmarks run once to catch panics/regressions |
| README quickstart | the README code block matches `examples/quickstart/main.go` |

The tag-driven `release` workflow reuses the **same** CI workflow through
`workflow_call`, so a tag runs exactly the merge gates; it also verifies that the
tag is signed (`.github/allowed_signers`), that `rig.Version` matches the tag and
that the tagged commit is on `main`, runs a clean-room `go get`/`go mod verify`
smoke of the published module, and creates a **draft** GitHub Release from the
CHANGELOG section. A maintainer publishes the draft after the post-release
re-audit with the `publish-release` workflow (see
[docs/RELEASE.md](docs/RELEASE.md)). Release tags are annotated and SSH-signed;
`main` is protected by required CI checks and a `v*` tag ruleset.

## Security

The built-in tools enforce an in-process policy (not an OS sandbox): file access is
rooted with `os.Root`, shell tools reject control operators, command/process
substitution (even inside double quotes), leading environment assignments and
path-form command names by default (`sandbox.WithAllowShellOperators` is the
documented opt-out), and `web_fetch` resolves and pins validated public IPs to
close DNS-rebinding SSRF — including a fetcher-side guard for policies without
`web.HostResolver` (`web.WithAllowPrivateHosts` is the local-target opt-out).
`terminal_send` lines are checked like shell commands and the terminal shell
binary must be allow-listed; the shell policy models POSIX quoting, escapes and
backslash-newline line continuations with one shared parser, so a substitution
split by a continuation is still rejected. LSP inbound headers are bounded at
64 KiB before any allocation. The approval `Gate` remains the real boundary for
irreversible actions. Permission modes (`read-only`, `workspace-write`,
`danger-full-access`) and the approval policy (`ask`/`auto`/`never`) mirror the
DeepSeek Harness model: they are in-process fences over the tool implementations,
**not a kernel boundary**. For untrusted models run the host in a container or VM
and keep approvals enabled. Everything is cgo-free (`CGO_ENABLED=0`). Please
report vulnerabilities privately — see [SECURITY.md](SECURITY.md).

## Extension points

1. **Provider** — implement `rig.Provider` (streaming), or use `provider/openai`
   for any OpenAI-compatible gateway.
2. **Tool** — implement `rig.Tool` (`Name/Description/Schema/Invoke`) and register it
   (see `examples/custom-tool`), or use `tools/full.All` for the batteries-included set.
3. **Gate** — implement `rig.Gate` to approve/deny tool calls.
4. **History** — implement `rig.History` for durable sessions.
5. **Host interfaces** — implement `rig.Asker`/`Presenter`/`PlanController`/`GoalStore`/
   `MemoryStore`/`Scheduler`/`SessionLog`/`Reviewer` to swap the in-memory defaults.

## Permission modes

Rig follows the same coarse permission model as DeepSeek Harness: one sandbox
mode plus one approval policy. Set them once in `kit.Config` (or build
`sandbox.New(sandbox.WithMode(...))` and a `rig.Gate` yourself):

| Mode | File mutations | Shell commands |
| --- | --- | --- |
| `read-only` | denied | denied |
| `workspace-write` (default) | only inside the workspace root | only allow-listed commands |
| `danger-full-access` | unrestricted | unrestricted (explicit denylist still applies) |

Approval policy (`kit.Config.Approval`, or your own `rig.Gate`): `ask` (default:
the built-in gate allows read-only tools and denies the rest; supply a prompting
Gate for interactive ask), `auto` (allow every call), `never` (deny every call).
These are tool-level fences, not a kernel boundary: for untrusted models run the
host in a container/VM (for example Docker, as OpenHands does) and keep approvals
on. See [docs/CONFIG.md](docs/CONFIG.md).

## Composition with a kernel-based product

Assembly, module lifecycle, dependency injection and preflight are deliberately **not**
in Rig. A product that wants "assemble modules from a config" keeps that layer itself;
ZeoKeel does it with `zeokeel-kernel`:

```
consumer product (e.g. ZeoKeel)
  kernel (compile-time module assembly) + product modules + UI
        │ imports
        ▼
ZeoKeel Rig  ← pure toolset (this repo; one parser dependency, cgo-free)
```

That split keeps every Rig consumer free of a module runtime they may not want, while
products still get the kernel's preflighted assembly.

## Documentation

- [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) — layers, request flow, contracts, security notes.
- [docs/TOOLS.md](docs/TOOLS.md) — the complete tool catalog.
- [docs/COVERAGE.md](docs/COVERAGE.md) — how the coverage gates work and how to add a tool.
- [docs/AUDIT-REPORT.md](docs/AUDIT-REPORT.md) — modern-surface audit (reference vs Rig).
- [docs/CONFIG.md](docs/CONFIG.md) — configuration conventions and defaults (Config structs vs options, model precedence).
- [docs/VERSIONING.md](docs/VERSIONING.md) — semantic import versioning and the deprecation policy.
- [docs/RELEASE.md](docs/RELEASE.md) — release checklist and tag/release automation.
- [CONTRIBUTING.md](CONTRIBUTING.md) · [SECURITY.md](SECURITY.md) · [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md) · [SUPPORT.md](SUPPORT.md)

## Platform support

- **Linux**: every package; the full unit suite runs on Linux in CI.
- **macOS, FreeBSD**: cross-compiled and `GOOS`-vetted in CI; the unit suite on
  these platforms is not currently run in hosted CI (macOS/Windows hosted
  runners are not available for this repository — disclosed limitation).
- **Windows**: the SDK (contracts, `agent`, providers, file/editor/image tools,
  MCP/LSP clients, `pwsh`, sessions, stores) is cross-compiled and `GOOS`-vetted;
  the `bash`, `jobs` and `terminal_*` tools require a unix `/bin/sh` and report a
  clear configuration error on Windows (use the `pwsh` tool; Windows ConPTY
  support is not implemented in this release). Runtime coverage on Windows is a
  disclosed gap, not a claim.

OS-level isolation is intentionally **out of scope** for the SDK: run the host
process in a container or VM when the model is untrusted (Docker/Seatbelt/
bubblewrap are deployment choices, as in OpenHands and Claude Code). The
permission modes above are the in-process fence.

Non-Linux runtime coverage is a documented limitation of this release line; the
enforced cross-platform gates are cross-compile plus `GOOS` vet.

## Compatibility and stability

- The module root (`package rig`) is the compatibility surface: additive changes
  only within a module major version; a breaking contract change after v1.0.0
  gets a new module major version (`/v2` import path), never a versioned
  subdirectory.
- v0.x may break; `v1.0.0` freezes the API. See [CHANGELOG.md](CHANGELOG.md).
- Go 1.27+; one dependency (`mvdan.cc/sh/v3`, POSIX shell parser), no cgo
  (`CGO_ENABLED=0`). See [COMPATIBILITY.md](COMPATIBILITY.md).
- Release tags are annotated and SSH-signed; `sum.golang.org` records the
  published module checksums (the release workflow runs `go mod verify` on a
  clean consumer).
- `main` requires the `test`+`lint` CI checks, signed commits and linear
  history; force-pushes and deletions are rejected and a `v*` tag ruleset
  protects published tags. This is a single-maintainer project: the
  administrator retains an emergency bypass (`enforce_admins=false`), there is
  no second reviewer, and GitHub commit verification requires registering the
  SSH signing key on the account. The `release` environment is a manual
  `workflow_dispatch` step, not a required-reviewer gate. See
  [docs/RELEASE.md](docs/RELEASE.md) for the exact status and remaining limits.
- License: MIT from v0.7.0 onward; v0.6.0 and earlier tags are Apache-2.0.

## Roadmap

- **v0.11.0** (current release): scope correction — removes the unreleased
  OS-sandbox layer and instead mirrors the DeepSeek Harness permission model:
  `read-only` / `workspace-write` / `danger-full-access` modes plus
  `ask` / `auto` / `never` approval policy (in-process fences; OS isolation is a
  deployment concern for containers/VMs). Keeps the POSIX `mvdan.cc/sh` parser,
  the cgo-free contract and the completed standard-library modernization.
  The unreleased v0.10.1 tag is superseded; see CHANGELOG.
- **v0.10.0**: OS-sandbox experiment (removed again in v0.11.0), the shell policy
  now parses with the established `mvdan.cc/sh/v3` POSIX parser (replacing the
  hand-written scanner; line continuations, quoting and comments are the shell's
  own semantics), a standard-library cleanup across the toolset (`slices`, `maps`,
  `cmp`, builtin `min`/`max`, `errors.AsType`, `strconv`, `slices.Clone/Delete`,
  `sync.WaitGroup.Go`), and a cgo-free CI contract (`CGO_ENABLED=0`). Breaking:
  the module now has one dependency. See CHANGELOG.
- **v0.9.4**: security/correctness patch — the shell policy
  now models POSIX line continuations (closing the v0.9.3 residual bypass),
  `sandbox.ParseShellCommand` is the single validation+argv parser (bash, jobs,
  terminal), single unterminated LSP headers are bounded before allocation,
  documentation-truth fixes (platform/CI claims, versioning, host-interface
  legend) and honest repository-protection limits.
- **v0.9.3**: release-engineering patch — the tag signature verification in the
  release workflow now also uses the tag *name* for `git tag -v` (follow-up to
  the v0.9.2 ref-restore fix). No SDK code changes; behaviour identical to
  v0.9.2.
- **v0.9.2**: release-engineering patch — restores the annotated tag object
  before signature verification (no SDK code changes; behaviour identical to
  v0.9.1).
- **v0.9.1**: security/correctness hotfix — quoted command
  substitution and env/path hijacks rejected, fetcher-side SSRF guard, bounded
  LSP headers and tool fan-out, LSP/terminal path and allowlist enforcement,
  replayable history on cancel/gate error, team spawn deadlock, full CI gate
  reuse in releases, signed tags, draft→publish release flow, 80%/70% coverage
  floors, staticcheck+govulncheck, cross-compile + `GOOS` vet (Linux runtime
  only; hosted macOS/Windows runners are unavailable for this repository).
- **v0.9.0**: root-package contracts (`package rig`), the one-import `kit`
  facade, shell-operator and SSRF hardening, `tools/internal` helper layer, CI
  fuzz/bench/coverage gates, governance + tag-driven release automation.
- **v0.8.0**: modern-surface audit closed — MCP resource templates,
  `subagent_fork`, structured child output, 74 static tools,
  `tools/audit` with `missing=0`.
- **v0.7.0**: modern tool surface — `skill`, team task board (`team_task_*`),
  full goal lifecycle (`goal action=update`), code review (`code_review`/`auto_review`),
  `tools/audit` + `catalog.Modern`; MIT from this tag onward.
- **v0.6.0**: legacy agent tool surface complete (host tools, settings, memory,
  scheduling, worktrees, session/inspect/report, ralph, legacy names) plus the
  `tools/catalog` coverage gate.
- **v0.5.0**: real unix PTY terminal, MCP Streamable HTTP transport, in-process
  orchestration (`agent.NewRunner`, `subagent`, `workflow`, `team_*`).
- **Next**: MCP SSE subscriptions/OAuth, workflow DSL (conditions/loops), Windows
  ConPTY, PTY resize, cross-process subagents, compaction/SQLite history.

## License

MIT. See [LICENSE](LICENSE).
