# ZeoKeel Rig

**A pure Go agent toolset** — model providers, a streaming ReAct loop, tools,
approval gates and session history. Zero dependencies: standard library only.

Rig is the reusable agent core extracted from [ZeoKeel](https://github.com/zeokeel).
It does **not** include the module runtime or process assembly: products keep that
layer (ZeoKeel uses [zeokeel-kernel](https://github.com/zeokeel/zeokeel-kernel) for
compile-time module assembly). Import just the packages you need — `agent`, `tool`,
`provider/openai`, `gate`, `session`, `contracts/v1` — and build your own binary.

```go
policy := sandbox.New(
	sandbox.WithRoots("/workspace"),
	sandbox.WithCommandAllowlist("sed", "rg", "go", "git"),
	sandbox.WithNetwork(false),
)
reg := tool.NewRegistry(builtin.NewWithPolicy(policy)...)
reg.Register(web.NewFetcher(policy)) // web_fetch (allow network in the policy first)
prov, _ := openai.New(openai.Config{DefaultModel: "gpt-4o-mini"})
a, _ := agent.New(v1.AgentSpec{
	Provider: prov,
	Model:    "gpt-4o-mini",
	Tools:    reg.Tools(),
	Gate:     gate.AllowTools("read_file", "list_dir", "glob", "grep", "web_fetch"),
	History:  session.NewMemory(),
})
res, err := a.Run(ctx, v1.RunRequest{
	SessionID: "s1",
	Input:     []v1.Message{v1.NewMessage(v1.RoleUser, "list the repo and summarize it")},
	Emit:      func(ev v1.Event) { fmt.Print(ev.Text) },
})
```

## Install

```sh
go get github.com/zeokeel/zeokeel-rig@v0.6.0
```

Requires Go 1.27+. **No third-party dependencies** — `go list -m all` on a
consumer only shows this module.

## Repository layout

```
contracts/v1/      stable extension surface (standard library only)
agent/             streaming ReAct loop
provider/openai/   OpenAI-compatible streaming provider
sandbox/           agent sandbox policy (roots, commands, env, network, caps)
gate/              approval gates
session/           in-memory History
tools/             package tools: the tool registry (name lookup, specs, invoke)
  builtin/         read_file/write_file/edit_file/list_dir/glob/grep/bash (+pwsh)
  editor/          str_replace_editor (view/create/str_replace/insert)
  image/           read_image (multimodal ToolResult.Parts)
  terminal/        terminal_open/send/read/signal/close (unix PTY, pipe fallback)
  jobs/            job_start/output/kill/list/wait (background processes)
  subagent/        subagent tool (one in-process child agent)
  workflow/        workflow tool (sequential/parallel subagent steps + templating)
  team/            team_spawn/send/wait/list/close/interrupt (managed subagents)
  web/             web_fetch + pluggable web_search
  mcp/             MCP stdio + Streamable HTTP client, tools/resources/prompts
  lsp/             LSP stdio client + lsp_* tools
  misc/            working_directory + todo_write
examples/          runnable examples
docs/              architecture + tool catalog
```

## Packages

| Package | What it is |
| --- | --- |
| `contracts/v1` | Stable extension surface: messages, streaming chunks, `Provider`, `Tool`, `Gate`, `History`, agent events, `Agent`/`AgentSpec`. Standard library only. |
| `agent` | Streaming ReAct loop: model rounds, parallel tool calls, approval gate, iteration cap, cancellation, synchronous event stream. |
| `provider/openai` | OpenAI-compatible streaming provider (SSE, tool-call deltas, reasoning, usage, retries). |
| `sandbox` | Agent sandbox policy: allowed roots, read-only, command allow/deny lists, env allowlist, network + URL guard, output/timeout caps, denied globs. |
| `tools` + `tools/builtin` | Tool registry and built-in tools: `read_file`, `write_file`, `edit_file`, `list_dir`, `glob`, `grep`, `bash` — sandboxed via `builtin.NewWithPolicy`. |
| `tools/web` | `web_fetch` (HTTP(S), redirect/size caps, HTML→text) and `web_search` (pluggable `Searcher`). |
| `tools/mcp` | MCP stdio client (JSON-RPC: initialize, tools/list, tools/call, ping) and MCP tools as `v1.Tool`. |
| `tools/lsp` | LSP stdio client and `lsp_hover` / `lsp_definition` / `lsp_diagnostics` / `lsp_symbols` tools. |
| `tools/misc` | `working_directory` and in-memory `todo_write`. |
| `gate` | Approval gates: `AllowAll`, `DenyAll`, `AllowTools(...)`, `Func(...)`. |
| `session` | In-memory conversation `History`. |
| `examples/agent`, `examples/custom-tool` | Runnable examples (the agent example needs model credentials). |

## Extension points

1. **Provider** — implement `contracts.Provider` (streaming), or use `provider/openai`
   for any OpenAI-compatible gateway.
2. **Tool** — implement `contracts.Tool` (`Name/Description/Schema/Invoke`) and register it
   (see `examples/custom-tool`).
3. **Gate** — implement `contracts.Gate` to approve/deny tool calls.
4. **History** — implement `contracts.History` for durable sessions.

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

## Compatibility and stability

- `contracts/v1` is the compatibility generation: additive changes only within v1;
  a breaking change gets `contracts/v2`.
- v0.x may break; `v1.0.0` freezes the API. See [CHANGELOG.md](CHANGELOG.md).
- Go 1.27+; no dependency matrix to track. See [COMPATIBILITY.md](COMPATIBILITY.md).

## Roadmap

- **v0.5.0** (this tag): real unix PTY terminal, MCP Streamable HTTP transport,
  and in-process orchestration — `agent.NewRunner` (`v1.SubagentRunner`),
  `subagent`, `workflow` and `team_*` tools. See [docs/TOOLS.md](docs/TOOLS.md).
- **v0.4.0**: complete agent tool surface — `str_replace_editor`, multimodal
  `read_image`, stateful `terminal_*` and `job_*`, `pwsh`, MCP resources/prompts,
  plus the full sandbox/MCP/web/LSP surface from v0.3.
- **v0.6.0** (this tag): the legacy agent tool surface is complete — host tools,
  settings, memory, scheduling, worktrees, session/inspect/report, ralph and the
  legacy tool names — plus the `tools/catalog` coverage gate (66 static tools,
  61 legacy names, 5 server-bound, MCP dynamic names).
- **v0.7**: MCP SSE subscriptions/OAuth, workflow DSL (conditions/loops),
  Windows ConPTY, cross-process subagents, compaction/SQLite history,
  `tools/full` convenience presets.

## License

MIT. See [LICENSE](LICENSE).
