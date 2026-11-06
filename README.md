# ZeoKeel Rig

**Headless agent SDK for Go** — compile-time modules, zero third-party dependencies.

ZeoKeel Rig is the agent runtime behind [ZeoKeel](https://github.com/zeokeel): model
providers, a streaming ReAct loop, tools, approval gates, session history and a
compile-time module assembly. It is headless by design: no UI toolkit, no database
driver, no agent framework. A third-party product imports the SDK, registers its own
modules, writes an `assembly.json`, and builds a single self-contained agent binary.

```go
// examples/minimal: the whole product
package main

import (
	"os"

	"github.com/zeokeel/zeokeel-rig/host"
)

func main() { os.Exit(host.Main(os.Args[1:])) }
```

```go
// An agent in ~20 lines: provider + tools + gate + loop
prov, _ := openai.New(openai.Config{DefaultModel: "deepseek-chat"})
tools := tool.NewRegistry(builtin.NewFS("/workspace")...)
a, _ := agent.New(v1.AgentSpec{
	Provider:      prov,
	Model:         "deepseek-chat",
	SystemPrompt:  "You are a helpful coding agent.",
	Tools:         tools.Tools(),
	Gate:          gate.AllowTools("read_file", "list_dir", "glob", "grep"),
	History:       session.NewMemory(),
	MaxIterations: 20,
})
res, err := a.Run(ctx, v1.RunRequest{
	SessionID: "s1",
	Input:     []v1.Message{v1.NewMessage(v1.RoleUser, "list the repo and summarize it")},
	Emit:      func(ev v1.Event) { fmt.Print(ev.Text) },
})
_ = res
```

## Install

```sh
go get github.com/zeokeel/zeokeel-rig@v0.1.0
```

Requires Go 1.27+. The SDK's only dependency is
[`github.com/zeokeel/zeokeel-kernel`](https://github.com/zeokeel/zeokeel-kernel)
(MIT, stdlib-only); UI toolkits, SQLite and eino are **not** in its dependency tree.

## Packages

| Package | What it is |
| --- | --- |
| `contracts/v1` | Stable extension surface: messages, streaming chunks, `Provider`, `Tool`, `Gate`, `History`, agent events, `Agent`/`AgentSpec`. Standard library only. |
| `agent` | Eino-free streaming ReAct loop: model rounds, parallel tool calls, approval gate, iteration cap, cancellation, event stream. |
| `provider/openai` | OpenAI-compatible streaming provider (SSE, tool-call deltas, reasoning, usage, retries). |
| `tool` + `tool/builtin` | Tool registry and built-in tools: `read_file`, `write_file`, `edit_file`, `list_dir`, `glob`, `grep`, `bash` with root sandbox, output caps and timeouts. |
| `gate` | Approval gates: `AllowAll`, `DenyAll`, `AllowTools(...)`, `Func(...)`. |
| `session` | In-memory conversation `History`. |
| `assembly` + `modules/appinfo` | Compile-time registry, embedded `assembly.json` products, preflight and rollback. |
| `host` | Headless process host: settings file, logger, signals, graceful shutdown, `--product` flag. |
| `kernelx` | Public: the single place that imports the kernel; descriptors, service keys, registry and effect helpers for module authors. |
| `keys` | Public service keys (`AppInfoKey`, `SettingsKey`, `LoggerKey`) for `Requires(...)` without importing implementation packages. |

## Extension points

1. **Provider** — implement `contracts.Provider` (streaming), or use `provider/openai`.
2. **Tool** — implement `contracts.Tool` (`Name/Description/Schema/Invoke`) and register it.
3. **Gate** — implement `contracts.Gate` to approve/deny tool calls.
4. **History** — implement `contracts.History` for durable sessions.
5. **Module** — implement a kernel module with `kernelx.Define(...)`, register it in your
   own registry (see `examples/custom-module`) and enable it in `assembly.json`. Modules are compiled in **together**
   with the host; "installing a plugin" means adding a dependency and rebuilding.

## Assembly model

Modules are compiled into the binary and selected/configured at startup:

```json
{"version":1,"entries":[
  {"id":"appinfo","module":"rig/appinfo","config":{"name":"my-agent","version":"1.0.0"}}
]}
```

- fixed at link time; no runtime code loading, no hot update (config change = restart);
- kernel preflight rejects unknown modules, duplicate instance IDs, missing providers,
  type mismatches, dependency cycles and bad config *before* anything starts;
- a required module that fails activation rolls the whole assembly back;
  optional modules are kept as failed and reported as warnings.

## Stability

`contracts/v1` is the compatibility generation: additive changes only within v1; a
breaking change gets `contracts/v2` and a deprecation window. The SDK follows semver
(`v0.x` may still break; `v1.0.0` freezes the API). Kernel compatibility is documented
in [COMPATIBILITY.md](COMPATIBILITY.md).

## Roadmap

- **v0.1.0** (this tag): streaming ReAct loop, OpenAI-compatible provider, 7 built-in
  tools, gates, in-memory history, assembly/preflight/rollback, headless host,
  appinfo module, examples.
- **v0.2**: SQLite session store (optional nested module), compaction, MCP tools,
  subagents, retry/policy package.
- **v0.3**: ACP headless adapter, LSP tools, multi-agent modules.

## License

Apache-2.0. See [LICENSE](LICENSE).
