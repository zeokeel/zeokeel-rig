# ZeoKeel Rig — architecture

> Companion: [contracts/v1](../contracts/v1), [README](../README.md), and the research
> behind this split in the ZeoKeel repo (`docs/research/rig-sdk/`).

## 1. Layers

```
consumer product (e.g. ZeoKeel desktop app)
  process host + kernel assembly + product modules + UI
        │ imports
        ▼
ZeoKeel Rig (this repo) — pure toolset, zero dependencies
  contracts/v1 · agent · provider · tool · gate · session
```

Rig owns the **agent runtime primitives**; the product owns **composition**. This is
deliberate: a module runtime / DI container / config tree is not part of a toolset, and
forcing it on every Rig consumer would make the dependency tree heavier for no benefit.
ZeoKeel, for example, keeps `zeokeel-kernel` and its `assembly.json` in the product repo.

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

Threading: one run is single-goroutine for its event stream; parallel tool execution uses
goroutines internally and joins before the next round. A run is cancelled through its
`context.Context` and returns `ctx.Err()`. `Emit` is called synchronously; consumers that
need fan-out should buffer.

## 3. Extension contracts (`contracts/v1`)

| Interface | Implement to add |
| --- | --- |
| `Provider` / `Stream` | a model vendor (or wrap another API) |
| `Tool` | one capability the model can call |
| `Gate` | approval policy (allow/deny/allow-always per call) |
| `History` | durable or custom session storage |
| `Logger` / `Settings` | integrate your host logging/settings |

Messages are wire-shaped (roles, content parts, tool calls, tool-result messages) so a
provider can map them to any chat-completions-like API. Rig never stores provider SDK
types in the conversation.

## 4. What is intentionally absent

- module registry / compile-time assembly / preflight / rollback (product concern —
  ZeoKeel uses the kernel for it);
- UI toolkits, process hosts, config files, database drivers;
- runtime plugin loading, HMR, sandboxing (out of scope for an in-process toolset).

## 5. Security and trust

Rig runs in the product's binary; every tool and provider is code you compiled in and
trust. Built-in tools enforce a root sandbox for file tools; shell execution is bounded
by output caps and timeouts. Approval gates are the caller's policy boundary.

## 6. Versioning

- `v0.x`: API may change; `contracts/v1` additive changes only, deprecations announced.
- `v1.0.0`: `contracts/v1` frozen; breaking changes require `contracts/v2`.
- Rig has no dependency matrix: it depends only on the Go standard library.
