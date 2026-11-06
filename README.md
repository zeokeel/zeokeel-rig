# ZeoKeel Rig

[![CI](https://github.com/zeokeel/zeokeel-rig/actions/workflows/ci.yml/badge.svg)](https://github.com/zeokeel/zeokeel-rig/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/zeokeel/zeokeel-rig.svg)](https://pkg.go.dev/github.com/zeokeel/zeokeel-rig)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

**A pure Go agent toolset** — model providers, a streaming ReAct loop, tools,
approval gates and session history. Zero dependencies: standard library only.

Rig is the reusable agent core extracted from [ZeoKeel](https://github.com/zeokeel).
It does **not** include the module runtime or process assembly: products keep that
layer (ZeoKeel uses [zeokeel-kernel](https://github.com/zeokeel/zeokeel-kernel) for
compile-time module assembly). Import just the packages you need — `agent`, `tools`,
`provider/openai`, `gate`, `session`, `contracts/v1` — and build your own binary.

## Quickstart

The code below is `examples/quickstart/main.go`: CI compiles it on every push, and a
CI step also checks this README block still matches the file, so the first thing you
copy always builds.

```go
// Command quickstart is the smallest complete agent built from the Rig SDK:
// an OpenAI-compatible provider, the sandboxed file tools, an approval gate,
// in-memory history and the streaming ReAct loop. It is the code shown in the
// README quickstart and the CI build compiles it on every push.
//
// It needs credentials to talk to a model:
//
//	OPENAI_API_KEY=... go run ./examples/quickstart "list the repo and summarize it"
//
// The provider defaults to https://api.openai.com/v1. To use an OpenAI-
// compatible gateway, set Config.BaseURL (the provider does not read an
// OPENAI_BASE_URL environment variable); set Config.DefaultModel for the model.
package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/zeokeel/zeokeel-rig/agent"
	v1 "github.com/zeokeel/zeokeel-rig/contracts/v1"
	"github.com/zeokeel/zeokeel-rig/gate"
	"github.com/zeokeel/zeokeel-rig/provider/openai"
	"github.com/zeokeel/zeokeel-rig/sandbox"
	"github.com/zeokeel/zeokeel-rig/session"
	"github.com/zeokeel/zeokeel-rig/tools"
	"github.com/zeokeel/zeokeel-rig/tools/builtin"
)

func main() {
	prompt := strings.Join(os.Args[1:], " ")
	if prompt == "" {
		prompt = "List the files in this directory and summarize them."
	}
	workdir, err := os.Getwd()
	if err != nil {
		fail(err)
	}

	// 1. Sandbox policy: file access rooted at the working directory, a small
	// command allowlist for bash, no network.
	policy := sandbox.New(
		sandbox.WithRoots(workdir),
		sandbox.WithCommandAllowlist("git", "go", "rg", "sed"),
		sandbox.WithNetwork(false),
	)

	// 2. Tool registry: the built-in file/shell tools enforce the policy.
	reg := tools.NewRegistry(builtin.NewWithPolicy(policy)...)

	// 3. Model provider: any OpenAI-compatible endpoint. The provider reads
	// OPENAI_API_KEY from the environment when Config.APIKey is empty.
	prov, err := openai.New(openai.Config{
		BaseURL:      "https://api.openai.com/v1", // override for a gateway
		DefaultModel: "gpt-4o-mini",
	})
	if err != nil {
		fail(err)
	}

	// 4. Agent: provider + tools + approval gate + session history.
	a, err := agent.New(v1.AgentSpec{
		Provider:      prov,
		Model:         "gpt-4o-mini",
		Tools:         reg.Tools(),
		Gate:          gate.AllowTools("read_file", "list_dir", "glob", "grep"),
		History:       session.NewMemory(),
		MaxIterations: 20,
	})
	if err != nil {
		fail(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// 5. Run one turn; events stream synchronously to Emit.
	_, err = a.Run(ctx, v1.RunRequest{
		SessionID: "quickstart",
		Input:     []v1.Message{v1.NewMessage(v1.RoleUser, prompt)},
		Emit: func(ev v1.Event) {
			if ev.Type == v1.EventTextDelta {
				fmt.Print(ev.Text)
			}
		},
	})
	if err != nil {
		fail(err)
	}
	fmt.Println()
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
go get github.com/zeokeel/zeokeel-rig@v0.8.0
```

Requires Go 1.27+. **No third-party dependencies** — `go list -m all` on a
consumer only shows this module. `contracts/v1` is standard-library only and a
leaf of the import graph (CI enforces both).

## Repository layout

```
contracts/v1/      stable extension surface (standard library only)
agent/             streaming ReAct loop, subagent runner/fork, structured output
provider/openai/   OpenAI-compatible streaming provider
sandbox/           agent sandbox policy (roots, commands, env, network, caps)
gate/              approval gates
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
  jobs/            job_start/output/kill/list/wait (background processes)
  lsp/             LSP stdio client + lsp_* tools
  mcp/             MCP stdio + Streamable HTTP client, tools/resources/prompts
  memory/          memory tool over v1.MemoryStore
  misc/            working_directory, todo_write (+ legacy todo)
  ralph/           ralph (iterate a child agent until DONE/limit)
  report/          report (markdown session report inside the sandbox root)
  review/          code_review / auto_review over v1.Reviewer
  schedule/        schedule_create/list/update/delete
  session/         session_search/trace/event_* over v1.SessionLog
  settings/        settings_get / settings_set
  skill/           skill (list/search/load) + filesystem SkillStore
  subagent/        subagent + subagent_fork (one in-process child agent)
  team/            team_* tools + team_task_* shared task board
  terminal/        terminal_open/send/read/signal/close/list (unix PTY, pipe fallback)
  web/             web_fetch + pluggable web_search
  workflow/        workflow tool (sequential/parallel subagent steps + templating)
  worktree/        enter_worktree / exit_worktree / list_worktrees
examples/          runnable examples: quickstart, agent, custom-tool
docs/              ARCHITECTURE, TOOLS, COVERAGE, AUDIT-REPORT
scripts/           helper scripts (legacy tool-surface audit)
```

## Packages

| Package | What it is |
| --- | --- |
| `contracts/v1` | Stable extension surface: messages, streaming chunks, `Provider`, `Tool`, `Gate`, `History`, agent events, `Agent`/`AgentSpec`, host/store interfaces. Standard library only. |
| `agent` | Streaming ReAct loop: model rounds, parallel tool calls, approval gate, iteration cap, cancellation, synchronous event stream; subagent runner, fork and structured output. |
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
| `tools/mcp` | MCP stdio + Streamable HTTP client; tools/resources/prompts/templates as `v1.Tool`. |
| `tools/lsp` | LSP stdio client and `lsp_hover` / `lsp_definition` / `lsp_diagnostics` / `lsp_symbols`. |
| `tools/subagent` | `subagent` and `subagent_fork` (in-process child agent). |
| `tools/workflow` | Sequential/parallel subagent steps with `{{prev}}`/`{{steps.name}}` templating. |
| `tools/team` | Managed subagents (`team_*`) plus the `team_task_*` shared task board. |
| `tools/ralph` | Iterate a child agent until `DONE` or the round limit. |
| `tools/skill` | `skill` (list/search/load) over filesystem `SKILL.md` stores. |
| `tools/review` | `code_review` / `auto_review` over `v1.Reviewer` (incl. `SubagentReviewer`). |
| `tools/hosttools` | Host UI interfaces with in-memory defaults: `ask_user`, `present`, `plan_review`, `exit_plan_mode`, `goal`. |
| `tools/settings` | `settings_get` / `settings_set` over `v1.Settings`. |
| `tools/memory` | `memory` over `v1.MemoryStore`. |
| `tools/schedule` | `schedule_*` CRUD over `v1.Scheduler`. |
| `tools/session` | `session_*` tools over `v1.SessionLog`. |
| `tools/inspect`, `tools/report` | Diagnostics and markdown session report. |
| `tools/worktree` | Git worktree tools, sandbox-checked. |
| `tools/misc` | `working_directory`, `todo_write` (+ legacy `todo`). |
| `examples/quickstart`, `examples/agent`, `examples/custom-tool` | Runnable examples (the agent ones need model credentials). |

## Gates (CI)

Three machine-checked gates run on every push, so "complete" is a test result,
not a claim:

1. **`tools/catalog` + `tools/full`** — `TestToolCatalogCoverage` asserts every
   `catalog.Static` name is built by `full.All`, every legacy name is either built
   or explicitly server-bound, every built tool is listed, and no name is duplicated.
2. **`tools/audit`** — `TestModernToolSurface` tracks the modern agent tool surface
   (reference → mapping → documented gaps) and fails on an undocumented missing tool.
3. **`contracts/v1` / zero dependencies** — the contracts package must stay
   standard-library only, and the module must have no third-party dependencies.

Plus `gofmt`, `build`, `vet`, `test`, `test -race`, cross-compilation for
windows/darwin/freebsd and the README/quickstart drift check.

## Extension points

1. **Provider** — implement `v1.Provider` (streaming), or use `provider/openai`
   for any OpenAI-compatible gateway.
2. **Tool** — implement `v1.Tool` (`Name/Description/Schema/Invoke`) and register it
   (see `examples/custom-tool`), or use `tools/full.All` for the batteries-included set.
3. **Gate** — implement `v1.Gate` to approve/deny tool calls.
4. **History** — implement `v1.History` for durable sessions.
5. **Host interfaces** — implement `v1.Asker`/`Presenter`/`PlanController`/`GoalStore`/
   `MemoryStore`/`Scheduler`/`SessionLog`/`Reviewer` to swap the in-memory defaults.

## Composition with a kernel-based product

Assembly, module lifecycle, dependency injection and preflight are deliberately **not**
in Rig. A product that wants "assemble modules from a config" keeps that layer itself;
ZeoKeel does it with `zeokeel-kernel`:

```
consumer product (e.g. ZeoKeel)
  kernel (compile-time module assembly) + product modules + UI
        │ imports
        ▼
ZeoKeel Rig  ← pure toolset (this repo, zero dependencies)
```

That split keeps every Rig consumer free of a module runtime they may not want, while
products still get the kernel's preflighted assembly.

## Documentation

- [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) — layers, request flow, contracts, security notes.
- [docs/TOOLS.md](docs/TOOLS.md) — the complete tool catalog.
- [docs/COVERAGE.md](docs/COVERAGE.md) — how the coverage gates work and how to add a tool.
- [docs/AUDIT-REPORT.md](docs/AUDIT-REPORT.md) — modern-surface audit (reference vs Rig).

## Compatibility and stability

- `contracts/v1` is the compatibility generation: additive changes only within v1;
  a breaking change gets `contracts/v2`.
- v0.x may break; `v1.0.0` freezes the API. See [CHANGELOG.md](CHANGELOG.md).
- Go 1.27+; no dependency matrix to track. See [COMPATIBILITY.md](COMPATIBILITY.md).
- License: MIT from v0.7.0 onward; v0.6.0 and earlier tags are Apache-2.0.

## Roadmap

- **v0.8.0** (current release): modern-surface audit closed — MCP resource
  templates, `subagent_fork`, structured child output, 74 static tools,
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
