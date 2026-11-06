# ZeoKeel Rig — architecture

> Companion: the module root package [rig](../doc.go), [README](../README.md), and the research
> behind this split in the ZeoKeel repo (`docs/research/rig-sdk/`).

## 1. Layers

```
consumer product (e.g. ZeoKeel desktop app)
  process host + kernel assembly + product modules + UI
        │ imports
        ▼
ZeoKeel Rig (this repo) — pure toolset, zero dependencies
  rig (module root) · agent · provider · tools · gate · session
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

## 3. Extension contracts (module root, `package rig`)

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
- runtime plugin loading, HMR, OS-level sandboxing (the `sandbox` package enforces an
  in-process policy, not a kernel/container boundary).

## 5. Security and trust

Rig runs in the product's binary; every tool and provider is code you compiled in and
trust. Built-in file tools enforce a root sandbox (`os.Root`), shell execution is bounded
by output caps and timeouts, and the network URL guard refuses loopback/private/metadata
targets by IP literal. Two limits callers must understand:

- the command allowlist is **best effort**: `bash` runs `/bin/sh -c`, so a command that
  chains other commands can reach programs outside the allowlist. The approval `Gate` is
  the real shell boundary;
- the URL guard does not resolve hostnames, so a DNS alias can still point at a private
  address. Products that expose `web_fetch` to untrusted prompts should add a resolving
  `DialContext` or run behind a network egress proxy.

Low-level filesystem writes go through `os.Root`/`OpenPath` to prevent traversal and
symlink escapes; the root `rig` package never imports another rig package (CI enforces
this).

## 6. Versioning

- `v0.x`: the API may change between minor releases; additive within a release line,
  deprecations announced.
- `v1.0.0`: the root `rig` package is frozen; a breaking change gets a new module major
  version (`github.com/zeokeel/zeokeel-rig/v2`), never a versioned subdirectory.
- Rig has no dependency matrix: it depends only on the Go standard library.
