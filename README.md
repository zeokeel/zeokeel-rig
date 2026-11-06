# ZeoKeel Rig

**A pure Go agent toolset** — model providers, a streaming ReAct loop, tools,
approval gates and session history. Zero dependencies: standard library only.

Rig is the reusable agent core extracted from [ZeoKeel](https://github.com/zeokeel).
It does **not** include the module runtime or process assembly: products keep that
layer (ZeoKeel uses [zeokeel-kernel](https://github.com/zeokeel/zeokeel-kernel) for
compile-time module assembly). Import just the packages you need — `agent`, `tool`,
`provider/openai`, `gate`, `session`, `contracts/v1` — and build your own binary.

```go
prov, _ := openai.New(openai.Config{DefaultModel: "gpt-4o-mini"})
reg := tool.NewRegistry(builtin.NewBuiltins("/workspace", 30*time.Second)...)
a, _ := agent.New(v1.AgentSpec{
	Provider: prov,
	Model:    "gpt-4o-mini",
	Tools:    reg.Tools(),
	Gate:     gate.AllowTools("read_file", "list_dir", "glob", "grep"),
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
go get github.com/zeokeel/zeokeel-rig@v0.2.0
```

Requires Go 1.27+. **No third-party dependencies** — `go list -m all` on a
consumer only shows this module.

## Packages

| Package | What it is |
| --- | --- |
| `contracts/v1` | Stable extension surface: messages, streaming chunks, `Provider`, `Tool`, `Gate`, `History`, agent events, `Agent`/`AgentSpec`. Standard library only. |
| `agent` | Streaming ReAct loop: model rounds, parallel tool calls, approval gate, iteration cap, cancellation, synchronous event stream. |
| `provider/openai` | OpenAI-compatible streaming provider (SSE, tool-call deltas, reasoning, usage, retries). |
| `tool` + `tool/builtin` | Tool registry and seven built-in tools: `read_file`, `write_file`, `edit_file`, `list_dir`, `glob`, `grep`, `bash` (root sandbox, output caps, timeouts). |
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

- **v0.2.0** (this tag): pure toolset — agent loop, provider, tools, gates, history,
  examples; module runtime removed (moved back to the product).
- **v0.3**: compaction helpers, subagents, retry policy, SQLite history (optional
  nested module), MCP tools.

## License

Apache-2.0. See [LICENSE](LICENSE).
