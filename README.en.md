# ZeoKeel Rig

> [中文 README](README.md) · English entry point. Detailed documentation is written in Chinese; every link below points at the Chinese doc.

[![CI](https://github.com/zeokeel/zeokeel-rig/actions/workflows/ci.yml/badge.svg)](https://github.com/zeokeel/zeokeel-rig/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/zeokeel/zeokeel-rig.svg)](https://pkg.go.dev/github.com/zeokeel/zeokeel-rig)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)
[![Release](https://img.shields.io/github/v/release/zeokeel/zeokeel-rig)](https://github.com/zeokeel/zeokeel-rig/releases)

**A pure-Go SDK for personal agent clients.** One import gives you an OpenAI-compatible provider, a streaming ReAct loop, a permission-fenced tool set, a three-layer approval gate, session history and persistence, NDJSON event streams, cost metering, and optional orchestration tools.

Current release **v0.23.4** · **Go 1.27+** · `CGO_ENABLED=0` · exactly **one** dependency (`mvdan.cc/sh/v3`, the POSIX shell parser used only by the `sandbox` policy layer) · no cgo.

## What it is — and what it is not

Rig is the layer behind `main()` when you build a desktop, CLI, or local agent client: **single user, single process**, with sessions, memory, spend, and snapshots all belonging to one user on one machine. The module root package `rig` is standard-library only, so you can take one tool, the `gate` package, or a provider without pulling in the whole tree. Model-visible text (tool `Description`s, JSON schemas, error strings) is an interface and stays in English.

It does **not** provide a UI (no TUI/GUI; `present` and `ask_user` call host interfaces you implement), a process host (no supervision, tray, auto-update, or crash reporting), module assembly (no compile-time injection or plugin runtime), accounts (no login, quotas, cloud sync, or secret hosting), or a kernel sandbox — the built-in fence is an in-process policy check, and kernel boundaries are the opt-in `sandbox/osrunner`.

## Features

- **Agent loop.** Streaming ReAct with reasoning deltas, parallel tool calls, cancellation, and pluggable `Provider`. `kit` caps iterations at **200** (`DefaultSafeMaxIterations`); `agent` is unlimited by default. Repeat-call guards warn after 3 identical turns and return `rig: repeated call blocked` after 5. Context compaction is on by default (window × 0.6, floor 16k) and degrades instead of aborting.
- **Tool surface.** Counts depend on assembly: `kit.New(Config{})` registers **39** with **16** initially active; `full.All(AllOptions{Root, Policy})` builds **81** (**83** with `SandboxGrants`); the ledger `catalog.Static` has **83**; presets are personal **55** / standard **73** / minimal **8**. `docs/ai/tools.json` lists **84** (83 static + opt-in `run_code`). Dynamic loading is on by default — core 13 + `tool_search` + `memory` + `skill` start active and the model activates the rest; activation is append-only and per session, so the `tools` array stays byte-stable and the prefix cache keeps hitting.
- **Permissions.** Three modes (`read-only`, `workspace-write` default, `danger-full-access`) crossed with three approval presets (`ask` default, `auto`, `never`). `ask` is three-layer: read-only tools and commands run silently; dangerous commands (`rm -rf /`, `mkfs`, `curl … | sh`) are refused with the rule id and reason returned to the model; everything else asks `Config.Asker`. With no `Asker`, `kit` uses `gate.PersonalAskWithoutAsker()` — read-only passes, the rest is denied with "no approval channel available" instead of hanging. `auto` **is** `AllowAll()` (it does not classify commands); layer `gate.CommandRiskGate` yourself if you want dangerous commands refused. `never` denies everything. Command escalation (`sandbox_permissions:"require_escalated"` plus a `justification`) is honoured only after human approval and never changes the policy.
- **Sandboxing.** The default is an in-process fence: writable roots, command allow/deny lists, network switch, output and timeout caps, runtime write grants, and structured denial auditing. `sandbox/osrunner` is opt-in and off by default (Linux bubblewrap or pure-Go Landlock, macOS `sandbox-exec`, Windows restricted token + ACL) — pure Go, no cgo, fail-closed. Sandboxed non-dangerous shell commands skip the prompt but still emit `approval.response` with `reason=sandboxed`.
- **Sessions.** In-memory history by default, per-session NDJSON in `session/store`, SQLite in the `session/sqlite` submodule, and the `storage` aggregate contract. `History`, `SessionStoreDir`, and `SessionStore` are mutually exclusive; `Agent.Shutdown(ctx)` closes in-flight runs, events, storage, and background jobs in order.
- **Orchestration and providers.** `team` (`spawn_teammate`, `wait_agent`, `team_task_*`), `workflow` (Go-native steps, no JS engine), `ralph`, `review`; providers for OpenAI-compatible endpoints, `local`, `ollama` (`127.0.0.1:11434`), and `lmstudio` (`127.0.0.1:1234`).
- **Observability.** Versioned NDJSON `events`, `costmeter` (nano-USD integers, budget warnings), `modelprice`, approval events, sandbox denial auditing, and `Preflight()`.

## Requirements and installation

Go 1.27+, `CGO_ENABLED=0`, and exactly one dependency. The root `rig` package stays standard-library only.

```sh
export CGO_ENABLED=0
go get github.com/zeokeel/zeokeel-rig@v0.23.4
```

`session/sqlite` is a **separate submodule** (own `go.mod`, depends on `modernc.org/sqlite`) that the main module never imports; it ships with the next release, so treat it as unpublished today.

## Quickstart

The block below is `examples/quickstart/main.go` verbatim, Chinese comments included. CI compiles it and diffs the first Go block of the Chinese README against this file byte for byte.

```go
// Command quickstart 是用 Rig SDK 构建的最小完整 agent：kit 门面在一次 import 中
// 装配好兼容 OpenAI 的 provider、带沙箱策略的完整工具集、两档审批门禁（只读免问、
// 其余问人）以及内存历史。
//
// 与模型对话需要凭据：
//
//	OPENAI_API_KEY=... go run ./examples/quickstart "list the repo and summarize it"
//
// provider 默认使用 https://api.openai.com/v1。接入 OpenAI 兼容网关时设置
// Config.BaseURL，并用 Config.Model 选择模型。没有 API key 时程序仍可构建；
// 第一次运行会报告 provider 错误。
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

	// 一次 import、一个构造函数：kit.New 默认使用完整工具集（再经 personal 预设与
	// 零配置体检裁剪），沙箱只做**可写根围栏**（默认限定在工作目录、不联网）。
	// 策略层不设命令白名单（空 CommandAllowlist 时允许任意程序，否则 go test 这类
	// 命令根本跑不了），所以边界是审批而不是命令分类：approval=ask（默认）只有
	// 两档——只读工具免问，其余调用（写文件、bash/pwsh……）一律问 Config.Asker；
	// 默认不超时（一直等），没有 Asker 时 fail-closed 拒绝并说明"没有可用的审批
	// 通道"。approval=never 全拒，approval=auto 全放行。
	// 默认**不做**命令风险分类，也**不硬拒**危险命令；要这一层就用 Gate 字段显式
	// 叠加 gate.CommandRiskGate。
	// 想一次定好权限面用 PermissionPreset（read-only / workspace-write /
	// danger-full-access / auto）；需要扩大能力时传入 Config.Asker、Approval 或
	// CommandAllowlist。
	a, err := kit.New(kit.Config{
		Model: "gpt-4o-mini",
		// 接入 OpenAI 兼容网关时：
		// BaseURL: "https://api.deepseek.com/v1", APIKeyEnv: "DEEPSEEK_API_KEY",
	})
	if err != nil {
		fail(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// RunText 返回聚合后的助手文本；emit 会实时流出事件。
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

```sh
export OPENAI_API_KEY=sk-...
export CGO_ENABLED=0
go run ./examples/quickstart "list the files here and summarize them"
```

The zero-config agent is **usable but read-only**: `Approval` defaults to `ask` with no `Asker`, so file writes and every non-read-only command are refused with "no approval channel available" (read-only commands pass, dangerous ones are denied), and networking is off. The policy layer sets **no command allowlist** — an empty `CommandAllowlist` allows any program, otherwise `go test` could never run — so shell safety comes from command-risk classification plus approval. Pass `Config.BaseURL` / `Config.APIKeyEnv` to target an OpenAI-compatible gateway (for example `https://api.deepseek.com/v1`).

## Three ways to assemble

- **`kit.New(kit.Config{})`** — most personal clients; defaults and safety posture are already chosen. With `Tools == nil` it builds `full.All`, filters by preset, then drops tools whose host dependency is missing.
- **`full.NewRuntime(full.AllOptions{...})` + `agent.New(rig.AgentSpec{...})`** — full wiring (Host/Runner/Searcher/MCP/LSP/reviewer) plus a handle to close background jobs and terminals.
- **Individual packages** — one tool, one gate, one provider, or your own loop.

When assembling `full.All` by hand, always pass `Root` or a `Policy` with roots: otherwise you get **78** tools, no `apply_patch`/`search`/`undo`, and `no roots configured` from every file tool.

## Platforms

- **Linux** — the only platform running the full test matrix (`-race` and coverage floors included).
- **macOS / FreeBSD** — cross-compiled and vetted with `GOOS`; real macOS sandbox validation runs in a separate, non-blocking workflow.
- **Windows** — cross-compiled and vetted, plus a blocking real-Windows job for ACLs, restricted tokens, and job objects. `bash`, `jobs`, and `terminal_*` need a unix `/bin/sh`; use the **`pwsh`** tool instead (ConPTY, pipe fallback).

## Documentation

Detailed documentation is in Chinese:

- [docs/README.md](docs/README.md) — index and reading order
- [docs/GETTING-STARTED.md](docs/GETTING-STARTED.md) — install, assembly paths, common tasks, troubleshooting
- [docs/AI-SDK.md](docs/AI-SDK.md) — SDK guide for AI agents (+ [docs/ai/tools.json](docs/ai/tools.json))
- [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) — layers, request flow, contracts
- [docs/TOOLS.md](docs/TOOLS.md) — tool catalog by category
- [docs/CONFIG.md](docs/CONFIG.md) — every `kit.Config` field and default
- [docs/SANDBOX-OS.md](docs/SANDBOX-OS.md) — the opt-in OS runner
- [docs/MEMORY.md](docs/MEMORY.md) · [docs/sqlite.md](docs/sqlite.md) — memory, spill, and session persistence
- [docs/COVERAGE.md](docs/COVERAGE.md) · [docs/VERSIONING.md](docs/VERSIONING.md) · [docs/RELEASE.md](docs/RELEASE.md) — coverage gates, versioning, releasing

## Security

The built-in fence is an in-process policy layer, **not** a kernel boundary. Put untrusted models or untrusted input in a container or VM; layer `sandbox/osrunner` on top for OS-level file effects, but do not read it as permission to run arbitrary code. Report vulnerabilities privately via [SECURITY.md](SECURITY.md) — never in a public issue.

## License

MIT (since v0.7.0; earlier tags are Apache-2.0). See [LICENSE](LICENSE).