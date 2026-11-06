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
trust. The built-in tools enforce:

- **file tools** — a root sandbox (`os.Root`/`OpenPath`) that rejects traversal and
  symlink escapes;
- **shell tools** — an allow/deny command list, a bounded environment, output caps and
  timeouts, and, by default, rejection of unquoted shell control operators
  (`;`, `|`, `&&`, `$()`, backticks, redirects, newlines): an allow-listed first command
  cannot chain a second one. `sandbox.WithAllowShellOperators` opts out and makes the
  command allowlist advisory-only;
- **`web_fetch`** — HTTP(S) only, redirect/size caps, and a URL policy. When the policy
  implements `web.HostResolver` (as `sandbox.Policy` does), the fetcher resolves each
  dialed host itself, rejects loopback/private/link-local/metadata addresses and pins the
  validated IP, which also closes the DNS-rebinding gap left by `CheckURL`'s no-DNS
  contract.

Residual limits: this is an in-process policy, not a kernel/container boundary. Shell
commands and tools run as the current user; the approval `Gate` remains the caller's
policy boundary for irreversible actions, and a tool that ignores the policy can do
anything the process can do. The `terminal_*` tools start an interactive shell: the
command allowlist only bounds the shell binary, not the commands typed into it, so use
the Gate or an OS-level sandbox when the model is untrusted.

The root `rig` package never imports another rig package and is standard-library only
(CI enforces both).

## 6. Versioning

- `v0.x`: the API may change between minor releases; additive within a release line,
  deprecations announced.
- `v1.0.0`: the root `rig` package is frozen; a breaking change gets a new module major
  version (`github.com/zeokeel/zeokeel-rig/v2`), never a versioned subdirectory.
- Rig has no dependency matrix: it depends only on the Go standard library.
