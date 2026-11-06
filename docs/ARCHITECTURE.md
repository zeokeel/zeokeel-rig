# ZeoKeel Rig — architecture

> Companion: [contracts/v1](../contracts/v1), [README](../README.md), and the research
> behind this layering in the ZeoKeel repo (`docs/research/rig-sdk/`).

## 1. Layers

```
consumer product (e.g. ZeoKeel desktop app)
  cmd + desktop host + UI + product modules + product assembly
        │ imports
        ▼
ZeoKeel Rig (this repo)
  contracts/v1 · agent · provider · tool · gate · session · assembly · host · modules
        │ imports (only here via internal/kernelx)
        ▼
github.com/zeokeel/zeokeel-kernel (MIT)
  Context/fiber/effect/service/event + module/ compile-time registry and preflight
```

Rules enforced in CI:

1. `contracts/v1` imports **only the standard library** — no kernel, no UI, no DB.
2. Only `kernelx` (public) imports the kernel; modules use `kernelx` aliases
   (`kernelx.Descriptor[T]`, `kernelx.ServiceKey[T]`, `kernelx.Define`, ...).
   Service keys shared with third-party modules live in `keys`; `contracts/v1` stays
   standard-library only.
3. The root module's dependency tree contains only the kernel. SQLite/UI/anything else
   must live in nested modules or product repos (not in v0.1).
4. Registration is explicit: a module is registered in `assembly.NewRegistry()` or a
   product registry, never with `init()` magic.

## 2. Request flow

```
caller ──RunRequest──▶ agent.Run
                         │  load History, append input
                         ▼
              ┌─ round loop (≤ MaxIterations) ──────────────────┐
              │  Provider.Stream(req)                           │
              │    chunks → events (text/reasoning/tool deltas) │
              │    final → assistant Message (+ tool calls)     │
              │  no tool calls → done                           │
              │  tool calls: Gate.Approve each → execute        │
              │    (parallel, results in call order)            │
              │  append tool Messages → next round              │
              └─────────────────────────────────────────────────┘
                         │
                         ▼
                    RunResult + Events (synchronous callback)
```

Threading: one run is single-goroutine for its event stream; parallel tool execution
uses goroutines internally and joins before the next round. A run is cancelled through
its `context.Context` and returns `ctx.Err()`. `Emit` is called synchronously and must
not block for long; buffer it in consumers that need fan-out.

## 3. Extension contracts (`contracts/v1`)

| Interface | Implement to add |
| --- | --- |
| `Provider` / `Stream` | a model vendor (or wrap another API) |
| `Tool` | one capability the model can call |
| `Gate` | approval policy (allow/deny/allow-always per call) |
| `History` | durable or custom session storage |
| module (`kernelx.Descriptor`) | a service/module wired into an assembly (see `examples/custom-module`) |
| `Logger` / `Settings` | integrate your host logging/settings |

Messages are wire-shaped (roles, content parts, tool calls, tool-result messages) so a
provider can map them to any chat-completions-like API. The SDK never stores provider
SDK types in the conversation.

## 4. Assembly and modules

- `assembly.NewRegistry()` registers the built-in modules explicitly.
- `assembly/assemblies/*.json` are embedded product configurations: instance tree
  (`id`/`module`/`config`/`children`/`disabled`/`optional`).
- `assembly.Validate(product)` runs kernel preflight without creating fibers;
  `assembly.Load(ctx, product)` validates, starts the tree, waits until quiescent and
  reports failures. Required failures roll the tree back; optional failures stay as
  failed instances with warnings.
- Modules own their registrations through kernel effects: services, listeners and
  whatever a module registered are unwound in reverse order on dispose.

## 5. Host

`host.Main` is the headless product entry: flags (`--product`, `--data-dir`,
`--version`, `--list-products`), settings file (atomic JSON write), logger
(stderr + optional file), signal handling and graceful shutdown. Desktop hosts are
product code: they embed the SDK instead of `host.Main`.

## 6. Security and trust

Compiled-in modules are fully trusted code (same binary, no sandbox). The SDK's trust
model for v0.x:

- official/built-in modules: reviewed in-tree;
- third-party modules: compile them into your own binary, review their code, pin
  versions;
- untrusted/remote capabilities: use out-of-process protocols (MCP/ACP/HTTP) in later
  versions, not in-process plugins.

Built-in tools enforce a root sandbox for file tools and output caps/timeouts for shell
tools; approval gates are the caller's policy boundary.

## 7. Versioning

- `v0.x`: API may change; `contracts/v1` additive changes only, deprecations announced.
- `v1.0.0`: `contracts/v1` frozen; breaking changes require `contracts/v2` and a
  migration note.
- Kernel compatibility: see [COMPATIBILITY.md](../COMPATIBILITY.md).
