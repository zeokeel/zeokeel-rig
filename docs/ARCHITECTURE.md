# ZeoKeel Rig — architecture

> 中文文档以 [README.md](../README.md) 与 [docs/zh/](zh/README.md) 为主；本文件保留英文原文作为参考，对应中文版见 [docs/zh/ARCHITECTURE.md](zh/ARCHITECTURE.md)。

> Companion: the module root package [rig](../doc.go), [README](../README.md) and
> [docs/AUDIT-REPORT.md](AUDIT-REPORT.md). The design was informed by the
> product-side audit that motivated extracting Rig; the portable summary lives in
> the audit report.

## 1. Layers

```
consumer product (e.g. ZeoKeel desktop app)
  process host + kernel assembly + product modules + UI
        │ imports
        ▼
ZeoKeel Rig (this repo) — pure toolset, one parser dependency, cgo-free
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
- runtime plugin loading, HMR, container/kernel sandboxing. The `sandbox` package
  enforces an **in-process permission policy** (`read-only` / `workspace-write` /
  `danger-full-access` modes plus an approval gate); OS-level isolation is a
  deployment concern: run the host in a container/VM for untrusted models.

## 5. Security and trust

Rig runs in the product's binary; every tool and provider is code you compiled in and
trust. The built-in tools enforce:

- **file tools** — a root sandbox (`os.Root`/`OpenPath`) that rejects traversal and
  symlink escapes;
- **shell tools** — an allow/deny command list (bypassed by
  `danger-full-access`), a bounded environment, output caps and timeouts, and one
  shared POSIX-aware parser (`sandbox.ParseShellCommand`) that
  models quoting, escapes and backslash-newline line continuations exactly as
  `/bin/sh` does before rejecting unquoted control operators, command/process
  substitution (`$(...)`, backticks, `<(...)` and `>(...)`) **even inside double quotes**,
  leading environment assignments (`PATH=.`, `LD_PRELOAD=...`) and path-form
  command names. An allow-listed first command therefore cannot chain or smuggle a
  second one, including through `"$\<newline>(id)"`, which `/bin/sh` joins into
  `"$(id)"`. `sandbox.WithAllowShellOperators` opts out and makes the command
  allowlist advisory-only. The allowlist is a policy layer for cooperative models,
  not an OS boundary; allowlisting an interpreter (`sh`, `env`, `xargs`,
  `python`, ...) gives arbitrary execution by design, and the approval Gate
  remains the boundary (run untrusted hosts in a container/VM);
- **LSP** — inbound message bodies are capped at 16 MiB and message headers are
  read with a 64 KiB byte budget *before* allocation (a single unterminated
  header line is rejected instead of buffered); LSP tools accept an optional
  path policy and `tools/full` derives it from the sandbox policy.
- **`web_fetch`** — HTTP(S) only, redirect/size caps, and a URL policy. The fetcher always
  resolves the dialed host itself and pins a validated IP, which closes the DNS-rebinding
  gap left by `CheckURL`'s no-DNS contract. Policies implementing `web.HostResolver`
  (as `sandbox.Policy` does) supply the validated addresses; every other non-nil policy
  gets a fetcher-side guard that rejects loopback/private/link-local/CGNAT/metadata
  addresses. `web.WithAllowPrivateHosts` is the explicit opt-out for local targets.

Residual limits: this is an in-process policy, not a kernel/container boundary. Shell
commands and tools run as the current user; the approval `Gate` remains the caller's
policy boundary for irreversible actions, and a tool that ignores the policy can do
anything the process can do. The `terminal_*` tools start an interactive shell: the
shell binary must be allow-listed and every `terminal_send` line is checked like a
non-interactive shell command, but interactive shell state (variables, builtins that
were allow-listed) and programs started inside the session are not further confined —
the same is true for any allow-listed interpreter or command. Use the Gate and run
the host in a container/VM when the model is untrusted.

The root `rig` package never imports another rig package and is standard-library only
(CI enforces both, together with the dependency policy and a cgo-free build).

Platform note: `bash`, `jobs` and `terminal_*` require a unix `/bin/sh`/PTY and are
Unix-only in this release; on Windows they report a clear configuration error and
the `pwsh` tool is the supported shell surface. CI runs the full unit suite on
Linux and cross-compiles + `GOOS`-vets windows/darwin/freebsd; hosted macOS and
Windows runners are not available for this repository, so their runtime coverage
is a disclosed limitation (see README.md "Platform support").

## 6. Versioning

- `v0.x`: the API may change between minor releases; additive within a release line,
  deprecations announced.
- `v1.0.0`: the root `rig` package is frozen; a breaking change gets a new module major
  version (`github.com/zeokeel/zeokeel-rig/v2`), never a versioned subdirectory.
- The module has exactly one dependency (`mvdan.cc/sh/v3`, the POSIX shell
  parser, imported only by `sandbox`); the root `rig` package stays
  standard-library only and has no dependency matrix.
