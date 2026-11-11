# Changelog

## v0.15.1 — 2026-11-11

Windows 沙箱临时目录的根因修复：改用 Windows 自带 temp，并放行定位它所需的非敏感环境变量。

### Fixed

- **helper 环境被裁剪导致候选为空**：命令的 `CommandEnv` 默认只放行
  `PATH`/`HOME`/`LANG`，Windows helper 因此看不到 `TEMP`/`TMP`/
  `LOCALAPPDATA`/`USERPROFILE`/`SystemRoot`；`os.TempDir()` 又回退到
  `C:\WINDOWS`，导致错误信息 `tried` 为空。现在 helper 的临时目录按 Windows
  自己的机制选择：`RIG_SANDBOX_TEMP` → `GetTempPath2W`/`GetTempPathW` →
  用户 Known Folder 的 `LocalAppData\Temp` → `os.TempDir()`；用 Win32
  `GetWindowsDirectoryW` 过滤 Windows 目录（即使 `SystemRoot` 也被裁剪）。
  不再把私有 temp 放进工作区。
- **默认环境变量允许列表**：`sandbox.Policy.CommandEnv` 默认放行 Windows 的
  `TEMP`/`TMP`/`TMPDIR`/`LOCALAPPDATA`/`APPDATA`/`USERPROFILE`/`SystemRoot`/
  `SystemDrive`/`WINDIR`/`COMSPEC`/`PATHEXT`/`USERNAME`/`USERDOMAIN`/`OS`
  （均非敏感）；API key/token/凭据仍会被丢弃。
- **`job_start` Windows 命令校验**：策略按 shell 的裸基名（如 `pwsh.exe`）
  校验允许列表，仍用 `LookPath` 返回的绝对路径启动，修复“命令名不能是路径”。

### 验证

- `go test ./...`、`go vet ./...`、`GOOS=windows` 交叉编译；`make ci` 全量。

## v0.15.0 — 2026-11-11

Windows 命令执行修复 + Windows 文件/终端/后台任务支持；无破坏性变更。

### Fixed

- **Windows ACL runner 临时目录**：私有临时目录原用 `os.TempDir()`；Windows 在
  `TEMP/TMP` 未设置时回退到 `C:\WINDOWS`，普通用户不可写，helper 在授权前
  以 `windows-acl-run: create private temp directory ...` / 退出码 127 失败，
  所有命令都无法执行。现在按 `RIG_SANDBOX_TEMP`、`TEMP`/`TMP`、
  `%LOCALAPPDATA%\Temp`、`os.UserCacheDir()\Temp`、`%USERPROFILE%\AppData\Local\Temp`、
  `os.TempDir()` 的顺序选第一个可写候选，并剔除 SystemRoot/WINDIR 之下的路径；
  功能探测与真实执行共用同一套选择逻辑。受限环境可用 `RIG_SANDBOX_TEMP`
  显式指定可写目录。
- **Windows 路径分隔符**：`write_file` 的父目录创建改用 `filepath.Dir`（原来
  `path.Dir` 不识别 `\`，嵌套路径不建目录）；`glob`/`grep` 传给
  `os.Root.FS()` 的路径统一 `filepath.ToSlash`（Windows 的 `fs.FS` 拒绝反斜杠）。

### Added

- **`terminal_*` Windows**：默认 shell 从硬编码 `/bin/sh` 改为按平台选择
  （Windows 为 `powershell.exe`）；ConPTY 仍不实现，但 pipe transport 可用。
- **`job_start` Windows**：后台任务在 Windows 上通过 `pwsh`/`powershell`
  （最后 `cmd.exe`）执行，策略命令允许列表仍生效。

### 验证

- Linux 全量 `go test ./...`、`go vet ./...`、`GOOS=windows` 交叉编译；
  -race 与 Windows 侧由消费方复验（仓库无 Windows runner）。

## v0.14.1 — 2026-11-11

v0.14.0 的 release lint 修复 + macOS Seatbelt 拒绝自检；无破坏性变更。

### Added

- **Seatbelt 拒绝自检（denial self-test）**：macOS runner 的功能探测在
  `sandbox-exec -p <profile> -- true` 之后，再用只读 profile 尝试写工作区外的临时
  文件。写入成功说明 Seatbelt 没有按预期拒绝 `file-write*`（“API 还在但行为静默
  变弱”），此时 `New` 失败并返回 `sandbox.ErrSandboxUnavailable`，绝不把不可信的
  runner 交给工具。纯逻辑测试用假 `sandbox-exec` 覆盖了“行为正常 / 未强制 / 无拒绝
  地退出”三种情形。

### Fixed

- 把 Windows helper 的失败前缀/退出码常量移到 `//go:build windows` 文件：
  v0.14.0 的这两个常量在 Linux 构建下被 staticcheck 判为 U1000（unused），导致
  release 门禁失败；无行为变更。

### Security

- 见上：Seatbelt denial self-test。

## v0.14.0 — 2026-11-11

可选的纯 Go OS 沙箱 runner（additive，默认关闭）；无破坏性变更。v0.11.0 曾移除
未发布的 OS 沙箱实验；本版以 **opt-in 子包** 的形式重新引入，核心策略层保持
进程内围栏，默认行为不变。

### Added

- **`sandbox.Runner` 接缝**：`Confine(argv, policy)` 返回 `sandbox.ConfinedCommand`
  （受限 argv、`Enforcement`、`DenialSignatures`）；`sandbox.ErrSandboxUnavailable`
  表示本机无法强制请求的模式，调用方必须 fail closed。
- **`sandbox/osrunner`（可选、纯 Go、无 cgo、零新增依赖）**：
  - Linux：bwrap 优先，回退自带的 Landlock 启动器（raw syscall 444/445/446，
    ABI 1–5 协商，`PR_SET_NO_NEW_PRIVS` 后 `exec`）；
  - macOS：系统 `sandbox-exec`（Seatbelt/SBPL）；
  - Windows：Write-Restricted 受限令牌 + 目录 ACL 的隐藏 helper（`partial`：
    只约束写/删）；
  - 功能探测 + fail-closed；`Config.RunnerCommand` 自定义 runner；
  - 宿主在 main 开始调用 `osrunner.MaybeRunHelper`（Linux/Windows 需要）。
- **工具接入**：`tools/builtin`（bash/pwsh）、`tools/jobs`、`tools/terminal`、
  `tools/worktree` 新增 `WithRunner`；`runner == nil` 时行为与 v0.13.x 完全一致。
- 文档：[docs/SANDBOX-OS.md](docs/SANDBOX-OS.md)。

### Changed

- `AGENTS.md` 放宽定位：OS 沙箱以 opt-in 的 `sandbox/osrunner` 子包存在，纯 Go、
  默认关闭；核心 `sandbox` 策略层仍不得依赖平台代码。
- README/SECURITY/ARCHITECTURE/COMPATIBILITY 更新 OS runner 的边界说明。

### Deprecated

### Removed

### Fixed

- 稳定 `tools/jobs` 的 awaited-settlement 测试：改用短生命周期但非瞬时的命令，避免
  job 在 waiter 注册前结算造成的 `-race` 抖动（测试修复，无行为变更）。

### Security

- OS runner 只治理文件效果（写/删/创建），不约束读、网络、进程可见性或设备；
  Windows 为 `partial`；macOS 依赖已弃用的 `sandbox-exec`；Linux 老内核 ABI 为
  `partial`。默认关闭，未显式启用时没有任何行为变化。

## v0.13.2 — 2026-11-09

Windows 命令 allowlist 的平台语义补丁；无破坏性变更。

### Added

### Changed

### Deprecated

### Removed

### Fixed

- **Windows 命令 allowlist 忽略 `.exe` 与大小写**：`Policy.CheckCommand` 现在按
  平台语义比较命令名——Windows 上 `"pwsh"`/`"pwsh.exe"`（及大小写变体）与
  `argv[0]` 的基名双向等价，`WithCommandAllowlist("pwsh", "powershell")` 不会再
  把工具传入的 `pwsh.exe`/`powershell.exe` 误判为 not in the allowlist；denylist
  使用同样的语义。非 Windows 平台行为不变（不剥 `.exe`、大小写敏感）。平台判断
  参数化为 `normalizeCommandName`/`commandNameMatches` 以便在任意宿主上测试两个
  分支。

### Security

## v0.13.1 — 2026-11-08

OpenAI 兼容 provider 的流截断检测补丁，并修复文档/版本一致性问题；无破坏性
变更。

### Added

- **流完整性校验**：`provider/openai.Config.RequireCompleteStream`。开启后，若事件流在
  `finish_reason` 与 `[DONE]` 都未出现前就关闭，`Recv` 返回新的
  `openai.ErrIncompleteStream`（回答可能被截断）；默认关闭，保持对直接关闭
  连接、不发送终止事件的 OpenAI 兼容服务器的兼容性。

### Changed

### Deprecated

### Removed

### Fixed

- `openai.ErrIncompleteStream` 现在同时 wrap `io.ErrUnexpectedEOF`，调用方可用
  标准 `errors.Is` 判断截断流。
- 补测“部分内容已返回后再返回 `ErrIncompleteStream`”的契约；`FuzzStreamDecode`
  同时覆盖 `RequireCompleteStream=false/true` 两种模式。
- `docs/CONFIG.md` / `docs/zh/CONFIG.md` 的 `openai.Config` 默认值表补齐
  `DefaultReasoningEffort` 与 `RequireCompleteStream`，说明截断流不会被自动重试。
- 修正 `docs/VERSIONING.md` / `docs/zh/VERSIONING.md` 的 release line/current
  version（`v0.13.x` / `v0.13.1`）。

### Security

## v0.13.0 — 2026-11-07

补齐 `02-sdk-gaps` 审计中的 SDK 缺失能力（additive，无破坏性变更）。

### Added

- **上下文压缩**：根包 `rig.Compactor`/`EstimateTokens`/`SpillPolicy`/
  `CheckpointStore`/`HistoryReplacer`；新增 `session/compact`：
  `WindowCompactor`（system 保留、最近窗口、旧图片卸载、tool_calls/tool 结果
  成块）与 `LLMCompactor`（模型摘要 + 超时/失败回退）。`agent.AgentSpec` 与
  `kit.Config` 增加 `ContextBudget`/`Compactor`；`kit.Agent.CompactNow` 支持
  手动压缩并持久化。
- **工具结果落盘（spill）**：`rig.SpillPolicy` + `kit.Config.Spill`；超过阈值
  的 tool 结果写入文件，模型看到稳定摘要与路径。
- **中断恢复 checkpoint**：根包 `Checkpoint`/`CheckpointPhase`/`CheckpointStore`；
  `agent.RecoverInterruptedTurn` 为中断回合补“结果未知、未重放”的 tool 结果与
  assistant 标记；agent 在模型请求前、工具执行前、正常结束写检查点（fail-closed）。
- **审批 Gate 辅助**：`gate.Ask(rig.Asker)`（allow/allow-always/deny、
  `ErrApprovalExpired`）与 `gate.Review(ToolReviewer)`（模型审前审批、超时/
  错误回退到人工/基础 gate、allowlist、observer）。
- **LSP references/implementation**：`lsp_references`/`lsp_implementation` 工具、
  legacy `lsp` action、`UTF16Len`/`ValidatePosition` 位置校验；LSP 服务端能力
  四态解析，未声明能力时返回可读错误。
- **MCP 通知流**：HTTP GET SSE、`OnNotification`/`OnNotificationError`、
  `Last-Event-ID` 断线恢复、`Close` 时 session DELETE；无处理器时不发起 GET。
- **Goal round driver**：`agent.GoalRoundDriver`（每会话预算、active-only 继续、
  prompt 生成，线程安全）。
- **工具集预设**：`tools/presets`（minimal/standard/all）+ `kit.Config.ToolPreset`。
- **Reasoning effort**：`provider/openai.Config.DefaultReasoningEffort` 与
  `kit.Config.ReasoningEffort`（low/medium/high；`Request.Extra` 可覆盖）。
- **owner 作用域清理**：`terminal.WithOwner`/`CloseAllOwned`、`jobs.WithOwner`/
  `CloseAllOwned`，owner 显示在 session/job 列表。

### Changed

- `gate` 包文档修正“内置门禁都是无状态的”表述（新增 Ask/Review gate）。
- `tools/lsp` `Tools()` 4→6；legacy `lsp` action 4→6（配置 LSP client 时）。
  静态 `full.All` 仍为 **81** 个工具（LSP 工具仍 server-bound）。
- 文档同步：README、docs/TOOLS、docs/CONFIG、docs/zh、AI-SDK、CHANGELOG。

### Notes

- `gate.Review` 的拒绝原因通过 `ReviewOutcome`/observer 暴露；agent 当前工具错误
  文本仍是固定 deny 文案（未新增 reason 通道）。
- 压缩只作用于内存对话；持久化替换需要 History 实现 `HistoryReplacer`
  （`session.Memory` 已实现）。
- 已知限制：`_test.go` 注释仍保留英文；模型可见 Description/Schema/错误文案
  保持英文。

### Fixed

### Security

## v0.12.1 — 2026-11-07

文档与 AI 可读性 release；**无代码行为变更**。

### Added

- 中文优先文档：`README.md` 改为中文主文档（`README.en.md` 保留英文），新增
  `docs/zh/` 全量指南（TOOLS/CONFIG/ARCHITECTURE/SECURITY/COVERAGE/
  VERSIONING/RELEASE/COMPATIBILITY）与中文索引。
- AI 使用入口：`AGENTS.md`、`llms.txt`、`docs/zh/AI-SDK.md`（组装方式、工具
  用法/能力、宿主依赖、权限与常见陷阱）。
- 机器可读工具目录：`cmd/rigdoc` 从真实 `tools/full.All()` 生成
  `docs/ai/tools.json` 与 `docs/ai/tools.md`（82 条：81 静态 + opt-in
  `run_code`，含 JSON Schema、分类、DSH 对应、可用性）。
- CI 门禁：`go run ./cmd/rigdoc -check` 防止 AI 目录漂移；`cmd/rigdoc` 测试
  同时校验文档计数（81 static / 65 catalog Modern / 55 audit Modern /
  v0.12.1）。
- 中文包文档与注释：全部 41 个包的包文档为中文 `doc.go`/包注释，公开 API 与
  内部注释中文化（仅注释变化，AST/测试验证无代码/字符串变更）。

### Changed

- 文档一致性修复：81 静态工具/65 catalog Modern/55 audit Modern、v0.12.x 支持
  线、唯一依赖 `mvdan.cc/sh/v3`、`CGO_ENABLED=0`、无 OS 沙箱声明；历史
  `docs/AUDIT-REPORT.md` 标注为 v0.8 快照；README/en/docs/zh 交叉链接与
  quickstart 代码块同步。
- 英文旧文档加中文优先 banner，指向 `README.md` 与 `docs/zh/`。

### Notes

- **模型可见的工具 Description/JSON Schema/错误文案保持英文**，因为它们是
  模型接口与 DSH 对齐的一部分；中文解释放在包文档与 `docs/zh/`。
- `_test.go` 注释不在 pkg.go.dev 渲染范围内，本批未强制中文化。
- 文档与代码冲突时以代码为准；中文文档为默认阅读版本，英文原文保留参考。


## v0.12.0 — 2026-11-07

DSH capability sync (second batch): asynchronous subagents/team control, typed
scheduling, revision-CAS goals, background jobs/terminal/bash, and opt-in PTC.

### Added

- **Async subagents and DSH team names** (`tools/team`): `subagent` and
  `subagent_fork` now use the async team manager: they return a target id
  immediately and deliver a completion notice through the new
  `rig.NoticeSource` capability, which the agent appends as a user message at
  the next step. Added `spawn_teammate`, `list_agents` and `wait_agent` (next
  team change; `noProgress` when nothing can progress); `send_message` remains
  non-blocking; `team_*` and legacy names remain as extensions.
- **Typed scheduling** (`tools/schedule`): `at`, `after_seconds`,
  `every_seconds`, `daily`, `weekly` and `cron` selectors with time-zone and
  cron canonicalization, `title`/`name` handling, and legacy free-form
  `schedule` strings kept; canonical stored grammar is documented in the
  package.
- **DSH goal tools** (`tools/hosttools`): `get_goal`, `create_goal`,
  `update_goal` with exact id+revision compare-and-set, actions
  edit/pause/resume/complete/blocked and `blocked_reason`; implemented over the
  optional `hosttools.GoalRevisionStore` (the in-memory host implements it).
  The legacy `goal` tool remains.
- **Background execution** (`tools/jobs`, `tools/builtin`, `tools/terminal`):
  new internal `tools/internal/background` producer contract; jobs support
  process and terminal-send kinds, DSH statuses/notices, `job_output` wait/
  `timeout_ms`, `job_kill` reason and kind/label listing. `bash`/`pwsh` accept
  `description`, `run_in_background` and foreground-timeout promotion;
  `terminal_*` accept DSH `sessionId`/`text`/`submit`/`offset`/`count`/`type`/
  `name`, paginated reads, SIG* signals and background sends. `full.All` wires
  the jobs manager into the shell and terminal tools and drains job completion
  notices through `rig.NoticeSource`.
- **PTC** (`tools/ptc` + `kit.Config.PTC`): opt-in `run_code` tool that executes
  model-authored Python 3/Node.js in a **system-installed runtime** (subprocess
  bridge over stdin/stdout, no embedded JS engine), exposing existing tools as
  `tools.<name>` with every inner call checked by the configured gate.

### Changed

- `tools/full.All` now builds **81 static tools** (previously 75): async
  subagent/team names and the three goal tools. `run_code` is an opt-in kit
  extension, not a static tool.
- `present` (first batch) remains DSH-compatible file declaration;
  `present_artifact` remains the artifact extension.
- `workflow` remains the Go-native declarative orchestrator (sequential/parallel
  steps, templating); DSH's JavaScript workflow/PTC program is intentionally not
  embedded. PTC instead executes user code in an installed runtime.
- Docs/catalog/audit mappings updated (static 81, catalog Modern 65, audit
  Modern 55; reference=63/mapped=57/gaps=6 unchanged).

### Security

- PTC inner tool calls pass through the same `rig.Gate` as ordinary calls; the
  host can restrict which tools are visible via `ptc.Config.Tools`.
- `sandbox_permissions`/`justification` are accepted on bash/pwsh/terminal
  schemas but always rejected with DSH's "no sandboxing executor" error: Rig has
  no per-call escalation executor.

### Notes / remaining gaps

- Still DSH-host-/kernel-specific: `load_workspace_dependencies`,
  `cordis_inspect_*`, `plugin_manager`, `plugin_market`.
- Not implemented from DSH details: output spill files (ring-only), owner-scoped
  terminal backend registry, DSH goal authority/driver rules (direct-human,
  autonomous-round threshold), DSH cron "no future occurrence" rejection and
  strict `additionalProperties` rejection; `run_code` uses Python/Node system
  runtimes instead of DSH's embedded PTC runtime.
- `run_code` is an opt-in kit extension rather than a `full.All` static tool.

## v0.11.0 — 2026-11-07

Scope correction. The OS-sandbox experiment is removed; Rig now mirrors the
DeepSeek Harness permission model: coarse sandbox modes plus an approval policy.
OS isolation is a deployment concern (container/VM), not an SDK feature.

### Breaking

- **Removed the opt-in OS-sandbox API**: `sandbox.NewOSExec`,
  `sandbox.OSExecOptions`, `sandbox.OSExec`, `sandbox.ErrOSExecUnavailable`,
  `sandbox.WithOSExec`, `(*sandbox.Policy).OSExec`, `(*sandbox.Policy).CommandContext`
  and `kit.Config.OSExec`. The `v0.10.1` tag (native Landlock/seccomp experiment)
  was never released and is superseded by this release; consumers of `v0.10.0`
  that attached an OS sandbox should switch to a permission Mode + approval and
  run the host in a container/VM for untrusted models.
- Shell/git/terminal processes are again plain `os/exec` calls; the PTY transport
  no longer has a sandbox fallback branch.

### Added

- **Permission modes** (`sandbox.Mode`, `sandbox.WithMode`):
  `ModeReadOnly` (deny file mutations and shell execution),
  `ModeWorkspaceWrite` (default: writes only under the workspace roots, shell only
  via the allowlist) and `ModeDangerFullAccess` (no root/allowlist fence; explicit
  denylist still applies). Unknown modes fail closed.
- **Approval policy** in `kit`: `ApprovalAsk` (default fail-safe gate: read-only
  tools allowed, the rest denied), `ApprovalAuto` (allow every call) and
  `ApprovalNever` (deny every call), plus `kit.Config.Mode`/`kit.Config.Approval`
  and `kit.Mode*`/`kit.Approval*` aliases. A non-nil `Config.Gate` always wins.
- Tests for mode semantics (read-only, workspace-write, danger-full-access and
  invalid-mode fail-closed) and kit mode/approval validation.

### Changed

- README/SECURITY/ARCHITECTURE/CONFIG/TOOLS now document the permission model and
  state plainly that these are in-process fences, not a kernel boundary; the
  removed OS-sandbox sections are replaced by "run the host in a container/VM".
- Kept from v0.10.0: `mvdan.cc/sh/v3` POSIX parser (single validation+argv
  parser for bash/pwsh/jobs/terminal), the cgo-free contract (`CGO_ENABLED=0`),
  and the completed standard-library modernization.

### Security

- The project no longer claims an OS-level sandbox. Permission modes constrain
  the shipped tool implementations; the approval Gate remains the boundary for
  irreversible actions, and untrusted models require container/VM isolation.

### Notes for consumers

- `v0.10.0` OS-sandbox users: remove `sandbox.WithOSExec` / `kit.Config.OSExec`
  and choose a `Mode` + `Approval`; for strong isolation run the host in Docker or
  a VM (as OpenHands does) and keep approvals enabled.
- `v0.10.1` was tagged during development but never published; its release notes
  are intentionally superseded by this section.

## v0.10.1 — unreleased (superseded by v0.11.0)

Native, dependency-free Linux OS sandbox (no external binary) and completion of
the standard-library modernization across code and tests.

### Added

- **Native Linux OS sandbox backend `landlock` (default).** `sandbox.NewOSExec`
  now re-execs the host binary with a hidden marker; a small `init()` runner
  applies Landlock write confinement (workspace + `WritableDirs` + `/tmp` +
  `/dev/null`/`tty`) and, when `AllowNetwork` is false, a seccomp filter denying
  `socket(AF_INET|AF_INET6)` and `io_uring_setup`, then `execve`s the real
  command. Pure Go standard library: **no bwrap/system binary, no cgo, no new
  module dependency**. The PTY terminal transport now works under this backend
  (previously it fell back to pipe).
- Integration tests: native filesystem confinement, seccomp network denial (with
  `AllowNetwork` enabled as the control), and PTY-under-sandbox. The real-bwrap
  integration tests remain for the optional backend.
- `Backend: "bwrap"` stays available; auto mode falls back to bwrap only when
  Landlock is unavailable and bwrap exists, otherwise it fails closed with
  `sandbox.ErrOSExecUnavailable`.

### Changed

- `sandbox.OSExec` Linux default backend is now `landlock` (`Backend()` reports
  it). No API removal; `bwrap` is opt-in/fallback.
- Completed standard-library modernization across packages and tests:
  `sort`→`slices` (30 hand-written sort lines deleted), `append([]T(nil),…)`→
  `slices.Clone` (54), `errors.As`→`errors.AsType` (14), `os.IsNotExist`→
  `errors.Is(err, fs.ErrNotExist)` (11), hand-rolled `itoa` removed (3),
  `fmt.Sprintf("%d")`→`strconv.Itoa/FormatInt` (5), `HasPrefix`+`TrimPrefix`→
  `strings.CutPrefix` (7), manual clamps→builtin `min` (3), `bytes.Index`→
  `bytes.Cut`, `slices.Delete/Contains`, `maps.Keys`+`slices.Sorted`,
  `sync.WaitGroup.Go`, `atomic.Int64`. No mechanical stdlib-replacement pattern
  remains in non-generated code.
- Docs/README/SECURITY/ARCHITECTURE/CONFIG updated: Linux needs no installed
  sandbox helper (Landlock ABI>=1, Linux 5.13+; network filter linux/amd64+arm64);
  macOS keeps the system-provided `sandbox-exec` (a pure-Go Seatbelt backend would
  require cgo, which is forbidden); Windows uses the native restricted token +
  built-in `icacls` low-integrity labels (network not isolated).

### Security

- Removes the bubblewrap install/PATH dependency from the default Linux sandbox
  path (smaller install and supply-chain surface); Landlock fails closed when the
  kernel does not support it.
- seccomp also blocks `io_uring_setup`, closing the "create a socket without the
  socket syscall" bypass.
- Landlock/seccomp are applied before `execve` in a re-exec'd copy of the host
  binary; restrictions are inherited across exec and cannot be dropped.

### Notes for operators

- Linux: kernel 5.13+ with Landlock enabled; on other architectures or kernels
  without Landlock, use `Backend: "bwrap"` or leave network enabled (Landlock-only
  mode needs no seccomp arch support).
- macOS: `sandbox-exec` is part of macOS; pure-Go native Seatbelt is not possible
  without cgo. Windows: filesystem/privilege confinement only, no network
  isolation (documented).

## v0.10.0 — 2026-11-07

Adds an opt-in OS-level process sandbox, replaces the hand-written shell scanner
with the established `mvdan.cc/sh/v3` POSIX parser, modernizes the toolset onto
the Go standard library, and makes cgo-free builds a CI contract.

### Breaking

- **One module dependency.** Rig now requires `mvdan.cc/sh/v3 v3.14.1` (pure Go,
  no transitive build dependencies) for shell parsing. `go list -m all` on a
  consumer shows this module plus that dependency; the module root stays
  standard-library-only and every build remains cgo-free (`CGO_ENABLED=0`).
- **Shell policy behavior.** The parser now models the shell's complete POSIX
  grammar, so `$((...))` arithmetic expansion is allowed when it contains no
  command substitution (previously rejected wholesale), comments are handled like
  the shell, and process substitution is reported as a POSIX parse error. The
  `ParseShellCommand`/`CheckShellCommand` signatures are unchanged; argv[0] may
  now be quoted (`'git' status` works) as long as it is a static literal.
- **Legacy constructors stay outside the sandbox.** `builtin.NewShell`,
  `jobs.NewManager()` and `terminal.NewManager()` without `WithPolicy` still spawn
  without policy or OS confinement (unchanged, documented); use the policy-aware
  constructors with `sandbox.WithOSExec`.

### Added

- **`sandbox.NewOSExec` / `sandbox.WithOSExec` / `kit.Config.OSExec`**: opt-in
  OS-level process sandbox for bash, pwsh, jobs, terminal and worktree git.
  - Linux: bubblewrap (`bwrap`), unprivileged; writable roots bind-mounted,
    read-only `/`, private `/tmp`, unshared PID/net/user namespaces, network off
    unless `AllowNetwork`.
  - macOS: `sandbox-exec` Seatbelt profile; writes limited to workspace/writable
    dirs + temp, network denied unless allowed.
  - Windows: restricted token + low integrity + `icacls` labels on writable
    roots; privileges dropped, writes confined to the workspace. **Network is not
    isolated on Windows** (use a container/VM for untrusted models).
  - Other platforms: fail closed with `sandbox.ErrOSExecUnavailable`.
  - All backends are cgo-free: system helper via `os/exec` on Linux/macOS, the
    standard library restricted-token `SysProcAttr` on Windows.
- `sandbox.ParseShellCommand` is now the single validation+argv parser shared by
  bash, pwsh, jobs and terminal (the three duplicated splitters are gone).
- `FuzzParseShellCommand` remains the parser fuzz target; new integration tests
  execute real `bwrap` sandboxed commands (writes inside/outside, network
  namespace) and construct the full 74-tool surface with an OS sandbox attached.
- `kit.Config.OSExec` (cgo-free convenience wiring; must be attached via
  `sandbox.WithOSExec` when a custom `Config.Policy` is used).

### Changed

- Shell parsing: the ~300-line hand-written lexer/continuation scanner is replaced
  by `mvdan.cc/sh/v3` AST validation plus a small POSIX line-continuation
  pre-pass; quoting, escapes, comments and continuation semantics come from the
  maintained parser.
- **Standard-library modernization across tools**: `sort` → `slices.Sort` /
  `slices.SortFunc` / `slices.SortStableFunc` (with `cmp.Compare`/`cmp.Or`),
  `append([]T(nil), …)` → `slices.Clone`, manual caps → builtin `min`/`max`,
  `sort.Ints`-dead-work removal, `slices.Contains`/`slices.Delete`,
  `maps.Keys` + `slices.Sorted`, `strconv.FormatInt` instead of hand-rolled itoa,
  `errors.Is(…, fs.ErrNotExist)`, `cmp.Or` in the provider, `sync.WaitGroup.Go`
  and `atomic.Int64` in the agent/workflow fan-out.
- CI: workflow-wide `CGO_ENABLED=0`; new **cgo-free** gate (asserts no package has
  `CgoFiles` and runs the suite with cgo disabled) and **dependency policy** gate
  (built imports are exactly the parser); the race step is the only cgo tooling
  use and is documented as such. Release workflow also runs cgo-free.
- `Makefile`: `CGO_ENABLED ?= 0` exported for all gates; `make race` opts into
  cgo tooling explicitly; `make deps` now enforces the dependency policy.
- `docs/CONFIG.md` documents `sandbox.NewOSExec` and Windows' network limitation;
  README/ARCHITECTURE/SECURITY/TOOLS/COMPATIBILITY/CONTRIBUTING/PR template were
  updated for the dependency, OS sandbox and cgo-free contract.

### Security

- **OS-level confinement (opt-in)** for every process-spawning model-facing tool;
  the in-process policy and approval Gate stay the policy/approval layers.
- The POSIX shell parser is now a maintained, fuzz-tested library
  (`mvdan.cc/sh/v3`) rather than a hand-rolled scanner; line continuations are
  still normalized to the shell's own behavior before AST validation.
- Supply chain: one pure-Go dependency, no cgo, `govulncheck` covers it, module
  checksums remain in `go.sum`/`sum.golang.org`.

### Docs

- README: dependency/cgo/OS-sandbox sections, platform support matrix for the
  sandbox backends, CI gate table, release history.
- `docs/ARCHITECTURE.md`: OS-sandbox layer and cgo-free contract; intention list
  updated (no longer claims OS sandboxing is absent).
- `docs/RELEASE.md`: release gates include the dependency-policy/cgo-free gates.
- `docs/VERSIONING.md`: current release line `v0.10.x`.

### Notes for consumers

- `go get github.com/zeokeel/zeokeel-rig@v0.10.0` is a drop-in upgrade for
  library consumers; the extra module is a pure-Go parser with no cgo or
  transitive build dependencies. To use the OS sandbox, construct
  `sandbox.NewOSExec` and attach it via `sandbox.WithOSExec` (or
  `kit.Config.OSExec`); install `bubblewrap` on Linux. Windows sandboxing confines
  filesystem writes/privileges but not the network.

## v0.9.4 — 2026-11-07

Security and correctness patch for the shell policy and LSP framing, plus
documentation-truth and release-gate corrections. No breaking changes: the
module path and root contract are unchanged (`rig.Version` reports `0.9.4`).

### Added

- `sandbox.ParseShellCommand(command string) ([]string, error)`: the single
  validation + word-splitting parser used by the built-in `bash`, `jobs` and
  `terminal_send` tools. `CheckShellCommand` remains as the validation-only
  API and delegates to the same parser.
- Fuzz target `FuzzParseShellCommand` (registered in CI) covering quotes,
  escapes, line continuations, comments and substitution constructs.

### Security

- **POSIX line continuations are modeled by the shell policy.** v0.9.1 rejected
  quoted `$(...)`/backticks but `/bin/sh` removes backslash-newline before
  tokenizing, so `echo "$\<newline>(id)"` (and the `${x:-$\<newline>(id)}`
  variant) still reached `/bin/sh` as `"$(id)"` and executed `id` with an
  allowlist of `echo`. The parser now strips backslash-newline outside single
  quotes with the shell's quote rules and validates the joined text; the
  regression corpus covers quoted, unquoted and parameter-expansion variants.
- **One parser for validation and argv[0].** The previous three copies of a
  quote-unaware splitter are gone; a divergence between the validator and the
  allowlist check can no longer be introduced per tool.
- **LSP headers are bounded before allocation, including one unterminated
  line.** v0.9.1 capped multi-line headers, but `bufio.Reader.ReadString`
  allocated the whole line before the cap applied (a 96 MiB stream without a
  newline grew the heap by ~92 MiB in the re-audit). The reader now uses
  `bufio.Reader.ReadLine` under a hard 64 KiB budget and a 128-line cap.

### Changed

- Shell comments that begin a word are handled like the shell. `$((...))`
  arithmetic expansion and parentheses inside `${...}` remain rejected
  conservatively because they may contain command substitution; pass
  `sandbox.WithAllowShellOperators` when a host needs full shell syntax and
  accepts the advisory allowlist.

### Docs

- Removed the "Windows/macOS test matrix" claim from README, `docs/RELEASE.md`
  and the release workflow comment: CI runs the unit suite on Linux and
  cross-compiles + `GOOS`-vets windows/darwin/freebsd (hosted macOS/Windows
  runners are unavailable for this repository).
- Updated `docs/VERSIONING.md` current release line, `docs/TOOLS.md`
  host-interface legend, `docs/ARCHITECTURE.md` shell/LSP security sections,
  `tools/lsp/doc.go` limits, and the README CI/security/platform text.
- Documented the repository-protection limits honestly (single maintainer,
  `enforce_admins=false`, GitHub commit verification needing
  `admin:ssh_signing_key`, release environment without a required reviewer);
  see the new "Repository protection status" section in `docs/RELEASE.md`.

### Notes for consumers

- `go get github.com/zeokeel/zeokeel-rig@v0.9.4` is a drop-in upgrade from
  v0.9.3. Scripts that relied on `echo "$((1+2))"` passing the allowlist policy
  need to quote the expansion result or compute it outside the shell; this is
  the conservative documented behavior since v0.9.1.

## v0.9.3 — 2026-11-07

Release-engineering patch; the SDK code is identical to v0.9.1/v0.9.2.

### Fixed

- **Tag signature verification, follow-up.** v0.9.2 restored the annotated tag
  object but passed the full ref (`refs/tags/vX.Y.Z`) to `git tag -v`, which
  resolves names under `refs/tags/` itself and therefore failed with *"tag not
  found"*. v0.9.3 passes the plain tag name after restoring and checking the tag
  object, so the release workflow verifies the SSH signature against
  `.github/allowed_signers` and proceeds to the clean-room smoke and draft
  release.

### Notes for consumers

- `go get github.com/zeokeel/zeokeel-rig@v0.9.3` behaves exactly like v0.9.1;
  this patch only changes how the GitHub Release is produced.

## v0.9.2 — 2026-11-07

Release-engineering patch; the SDK code is identical to v0.9.1.

### Fixed

- **Release workflow signature verification.** `actions/checkout` rewrites
  `refs/tags/<tag>` to the commit SHA for tag events, so the verify job saw a
  lightweight tag and failed with *"cannot verify a non-tag object of type
  commit"*; this blocked creation of the v0.9.1 GitHub Release (the tag and the
  module itself were already published and immutable). The workflow now
  force-fetches the annotated tag object from the remote, requires it to be of
  type `tag`, and only then runs `git tag -v` against
  `.github/allowed_signers`.

### Notes for consumers

- `go get github.com/zeokeel/zeokeel-rig@v0.9.2` behaves exactly like v0.9.1;
  this patch only changes how the GitHub Release is produced.

## v0.9.1 — 2026-11-07

Security and correctness hotfix for the v0.9 line, plus release-engineering and
documentation-truth fixes. No breaking changes: `rig.Version` reports `0.9.1`,
the module path and the root contract surface are unchanged.

### Security

- **Shell allowlists are now enforced against quoted command substitution.**
  v0.9.0 rejected unquoted `$(...)`/backticks but allowed them inside double
  quotes, where `/bin/sh` still executes them: `echo "$(id)"` bypassed an
  allowlist that did not include `id`. `sandbox.CheckShellCommand` now rejects
  command substitution `$(...)`, backticks and process substitution `<(...)`/
  `>(...)` anywhere except single quotes, and the check is applied by `bash`,
  `pwsh`, `jobs` and `terminal_send`.
- **Environment-assignment and path-form command hijacks are rejected.**
  `PATH=. tool`, `LD_PRELOAD=... tool` and `./tool`/`/path/tool` could make a
  model-provided command resolve to a workspace binary while the allowlist only
  saw an allow-listed basename. `sandbox.CheckCommand` now rejects leading
  `VAR=value` assignments and path-form command names: allowlisted commands are
  bare names. Documented caveat: allowlisting an interpreter (`sh`, `env`,
  `xargs`, `python`, `eval`, ...) still yields arbitrary code execution.
- **`web_fetch` SSRF guard covers policies without `web.HostResolver`.**
  `web.PolicyFunc`/custom `CheckURL` policies previously fell back to the
  standard dialer, so a public name resolving to loopback was dialed. The
  fetcher now resolves and validates every dialed host itself and rejects
  loopback/private/link-local/CGNAT/cloud-metadata addresses for every non-nil
  policy; `web.WithAllowPrivateHosts()` is the explicit opt-out for local
  targets (used by the tests and by hosts fetching local mirrors).
- **LSP message headers are bounded** (64 KiB) before allocation, closing the
  unbounded `ReadMIMEHeader` allocation (a hostile server could make the client
  allocate hundreds of MiB before the 16 MiB message cap applied).
- **LSP tools gained an installable path policy.** `lsp.WithPathPolicy` (wired by
  `tools/full.All` from the sandbox policy) validates model-provided paths before
  the tool reads a file, so `lsp_*` can no longer read arbitrary files outside
  the sandbox and leak them through language-server diagnostics.
- **`terminal_*` is bounded consistently with its documentation.** The shell
  binary must pass the command allowlist and each `terminal_send` line runs the
  same operator/substitution and command checks as the non-interactive shell.
- **Agent tool fan-out is bounded.** One model round now executes at most 8 tool
  bodies in parallel (`maxParallelToolCalls`) instead of one goroutine per call;
  a response with thousands of tool calls can no longer fan out thousands of
  concurrent executions.
- **Approval gates see normalized tool names.** A leading space in a
  model-provided tool name no longer slips past name-matching gates or the
  allow-always cache; dispatch, events, history and the gate all use the
  trimmed name.

### Fixed

- **Cancelled and gate-error tool rounds keep history replayable.** A run
  cancelled mid-tool, cancelled in the approval gate, or aborted by a gate error
  used to persist an assistant `tool_calls` message with no `role=tool` answers;
  the next request on that session was rejected (HTTP 400) forever. Every tool
  call is now answered with a tool result (synthetic cancellation/denial results
  where needed), and history writes use a non-cancelled context.
- **`team_spawn` no longer deadlocks at the runner concurrency limit.** The team
  manager held its lock while `SubagentRunner.Start` waited for capacity, so the
  fifth spawn (runner limit 4, member limit 8) blocked `team_list`/`team_wait`/
  `team_close` and every other team tool forever. Spawn reserves a slot, releases
  the lock and starts the child with the caller's context; the member limit also
  counts in-flight spawns.
- CI race fixed in the new coverage gate and lint job versions are pinned.

### Changed

- **Release workflow reuses the full CI gate set.** `.github/workflows/ci.yml` is
  now a reusable workflow (`workflow_call`) and `.github/workflows/release.yml`
  calls it, so the tag build runs exactly the merge gates (gofmt, build, vet,
  tests, race, coverage floors, fuzz, benchmarks, catalog/audit gates,
  zero-dependency and root-leaf policy, README drift, cross-compile + `GOOS`
  vet, examples, staticcheck, govulncheck).
- **Signed tags, tag-on-main check and clean-room smoke before a draft release.**
  The release workflow verifies the annotated tag's SSH signature against
  `.github/allowed_signers`, verifies the tagged commit is on `main`, runs
  `scripts/sync-version.sh --require-tag`, does a clean-room `go get`+
  `go mod verify` of the published module, and creates a **draft** GitHub
  Release. `publish-release.yml` (manual dispatch, `confirm_reviewed=yes`)
  publishes the draft after the post-release re-audit. `contents: write` is
  scoped to the publish job; all actions are pinned by commit SHA.
- **Coverage is gated per package, not only in total.** CI and `make ci` enforce
  80% total statement coverage *and* >=70% function coverage for every package
  with tests (`scripts/coverage-floor.sh`), so a large well-tested package cannot
  mask an under-tested one.
- **CI gained a `lint` job (staticcheck v0.8.2, govulncheck v1.9.0)** and
  cross-compile now also runs `go vet` with `GOOS` set (windows/darwin/freebsd),
  which compile-checks the tests too; `gofmt`/`make fmt` scan tracked Go files
  only. Hosted macOS/Windows runners are unavailable for this repository, so
  non-Linux runtime coverage is a documented limitation (README "Platform
  support"), not a claim.
- **Unix-only tools fail clearly on Windows**: `bash`, `jobs` and `terminal_*`
  report a configuration error instead of a missing-`/bin/sh` crash; use the
  `pwsh` tool. Platform support is documented in README/ARCHITECTURE/TOOLS.
- `report` closes its `os.Root` handle so the report file is not left locked.
- **Documentation truth pass.** Corrected the v0.9.0 shell/web_fetch hardening
  claims, the `terminal_*` security text, the `tools/full` and `docs/TOOLS.md`
  "in-memory defaults" overclaim, the CONTRIBUTING coverage contradiction, the
  RELEASE/VERSIONING process text, the `doc.go` import-graph claim, the
  `examples/custom-tool` package reference, the COMPATIBILITY Go-min rows and
  the AUDIT-REPORT snapshot note. Non-routable `dev@zeokeel.local` contacts were
  replaced with the private GitHub advisory channel (the only private channel).
- **Retroactive GitHub Releases** were created for v0.1.0–v0.8.0 from their
  CHANGELOG sections, so every published tag now has release notes. Tags are
  immutable and were not moved.

### Deprecated

- The 61 legacy tool-name aliases tracked by `catalog.Legacy` (for example
  `spawn_team`/`list_teams`/`wait_team`/`close_team`, `send_message`,
  `interrupt_agent`, `list_subagent_models`, `todo`) are **deprecated as of
  v0.9.1** in favor of the modern names (`team_spawn`, `team_list`, `team_wait`,
  `team_close`, `team_send`, `team_interrupt`, `todo_write`, ...). They keep
  working for at least one minor release (v0.10.0) per the deprecation policy in
  [docs/VERSIONING.md](docs/VERSIONING.md); removal is planned for v0.10.0.

## v0.9.0 — 2026-11-06

Root-package contracts, a one-import `kit` facade, security hardening, and the
engineering shell around the SDK (CI gates, fuzzing, governance, release automation).

### Breaking

- The contract package moved from `contracts/v1` to the module root: the stable
  extension surface is now `package rig` at `github.com/zeokeel/zeokeel-rig`.
  Migrate imports (`v1 "…/contracts/v1"` → `"github.com/zeokeel/zeokeel-rig"`) and
  references (`v1.Tool` → `rig.Tool`). Versioning is module-level: after v1.0.0 a
  breaking contract change gets a new major version at the import path (`.../v2`),
  never a versioned subdirectory.
- Unused sentinels `rig.ErrDenied` and `rig.ErrNoStream` were removed; the four
  remaining sentinels are all returned by the implementation.

### Added

- `kit`: one-import facade — `kit.New(kit.Config{Model: ...})` wires an
  OpenAI-compatible provider, `full.All` tools, a safe read-only default gate and
  in-memory history; `Agent.Run`/`Agent.RunText` (aggregated text + event stream);
  contract aliases and `kit.Version`.
- `rig.Version`: the module version is queryable from code and checked against the
  release tag (`scripts/sync-version.sh`).
- `tools/internal/toolutil`: shared helper layer for tool packages (JSON args,
  results, JSON-Schema builders, UTF-8 truncation). Tool packages went from 131
  helper definitions / 864 lines to 11 shared implementations.
- Fuzz targets (9) and benchmarks (15) for the SSE, MCP, LSP, URL and path parsers
  and the registry/agent hot paths; CI runs a 5s-per-target fuzz smoke and a
  benchmark smoke.
- Governance: `SECURITY.md`, `CONTRIBUTING.md`, `CODE_OF_CONDUCT.md`,
  `SUPPORT.md`, `CODEOWNERS`, issue/PR templates, dependabot, `.editorconfig`,
  `.gitattributes` and a `Makefile` that mirrors CI.
- Release automation: tag-driven `.github/workflows/release.yml` verifies the tag
  against `rig.Version`, re-runs the gates, extracts the matching CHANGELOG
  section and creates the GitHub Release; `scripts/sync-version.sh` enforces the
  invariant locally and in CI.
- Docs: `docs/VERSIONING.md`, `docs/RELEASE.md`, `docs/CONFIG.md`; the README was
  rewritten around the one-import quickstart and a complete package list.

### Changed

- Security hardening (still an in-process policy; the approval `Gate` remains the
  boundary):
  - shell tools now reject unquoted shell control operators (`;`, `|`, `&&`, `$()`,
    backticks, redirects, newlines), so an allow-listed first command cannot chain
    another program. `sandbox.WithAllowShellOperators` is the documented opt-out
    that makes the allowlist advisory-only; the same check is wired into the jobs
    and pwsh tools. Terminal sessions document that the allowlist does not bound
    commands typed into an interactive shell.
  - `web_fetch` resolves and validates every dialed host itself and pins the
    validated public IP when the policy implements `web.HostResolver` (as
    `sandbox.Policy` does), closing the DNS-rebinding SSRF gap; redirects are
    covered too.
- `go.mod` declares `go 1.27` instead of a patch-pinned `1.27.3`.
- README quickstart is now `examples/quickstart` (one import) and CI diffs the
  README block against it so it cannot rot.
- `docs/ARCHITECTURE.md` documents the enforced sandbox behavior and residual
  limits; the stale `tool` package name and removed assembly references are gone.
- MCP client version now follows `rig.Version` instead of a stale `0.3.0`.
- CI: gofmt, modern-surface audit, root-package stdlib/leaf gate, coverage floor
  (80%), fuzz smoke, benchmark smoke, cross-compilation (windows/darwin/freebsd)
  and the README drift check; the job gained `permissions: contents: read`,
  concurrency and a timeout.

### Fixed

- CI: the "contracts must stay standard-library only" step compared the module's
  own import path with `grep '\.'` and failed on every run since v0.1.0; it now
  filters `go list -deps` by the `Standard` field and excludes the package itself
  (the root package after the move).
- Coverage gate: `TestToolCatalogCoverage` now also fails when `full.All` builds a
  tool missing from `catalog.Static` (nine v0.7/v0.8 tools had been outside the
  ledger; 74 static names = 74 built tools).
- `sandbox.CheckPath` no longer trims whitespace from an already-resolved path, so
  legal file names ending in a space agree with `ResolvePath` (found by fuzzing).
- `agent` documents model precedence (`RunRequest.Model` > `AgentSpec.Model`; the
  provider default applies only when calling the provider directly).
- README/docs: duplicate "(this tag)" roadmap entries, partial layout/package
  tables and the `OPENAI_BASE_URL` misinformation are fixed.

### Removed

- Local `.probe/` development scripts are untracked and ignored; the legacy audit
  script requires the old-repo path instead of defaulting to a maintainer machine.
- Dead `rig.ErrDenied` / `rig.ErrNoStream` sentinels (see Breaking).

## v0.8.0 — 2026-11-06

Audit-gap close: remote MCP templates, subagent fork and structured output.

### Added

- `tools/mcp`: `ListResourceTemplates` / `mcp_list_resource_templates`
  (resources/templates/list with cursor pagination, uriTemplate dedupe;
  server-bound like the other MCP tools).
- `agent.NewRunner`: `RunnerOptions.ParentSessionID` (+ optional ParentHistory)
  for fork; `SubagentSpec.Fork` seeds a child with a copy of the parent session
  history, `SubagentSpec.OutputSchema` gives the child a `structured_output`
  tool validated against a JSON-Schema subset and returns
  `SubagentResult.Structured`.
- `tools/subagent`: `fork` and `output_schema` parameters; new `subagent_fork`
  tool (fork forced on).
- `contracts/v1`: `SubagentSpec.Fork`, `SubagentSpec.OutputSchema`,
  `SubagentResult.Structured` (additive).
- `tools/audit` + `docs/AUDIT-REPORT.md`: three gaps closed; the remaining 6 are
  reclassified as host/kernel-bound (cordis inspection, plugin manager/market,
  workspace dependencies, PTC run_code).
- `tools/catalog` + `tools/full.All`: 74 static tools, 62 modern names, no
  catalog gaps.

## v0.7.0 — 2026-11-06

Modern-surface gap close.

### Added

- `tools/skill`: `skill` tool (list/search/load) with a filesystem `SkillStore`
  (`*/SKILL.md`, top-level `*.md`, `$ZEOKEEL_SKILLS`,
  `<root>/.zeokeel/skills`, `~/.zeokeel/skills`).
- `tools/team` (`tasks.go`): shared task board - `team_task_create`,
  `team_task_list`, `team_task_get`, `team_task_update` over
  `v1.TeamTaskManager` with pending/in_progress/completed states, owner claim,
  blocked-by dependency cycles and an in-memory default manager.
- `tools/hosttools`: `goal action=update` with complete/blocked/pause/resume/edit
  via the optional `v1.GoalUpdater`; the in-memory Host implements the state
  machine (blocked reason retained).
- `tools/review`: `code_review` and `auto_review` over `v1.Reviewer`, plus
  `SubagentReviewer` (child-agent reviewer with `VERDICT:`/`FILE:LINE` parsing).
- `tools/audit`: modern tool-surface audit (`Modern`/`DocumentedGaps`,
  `Missing`) and `docs/AUDIT-REPORT.md`.
- `contracts/v1`: `GoalUpdater`/`GoalUpdateAction`, `TeamTask`,
  `TeamTaskFilter`, `TeamTaskUpdate`, `TeamTaskManager`.
- `tools/catalog`: `Modern`, `DocumentedGaps`, `AuditModern`; the coverage test
  now checks static + legacy + modern names.

### Changed

- `tools/full.All`: new `Skill`, `TeamTasks`, `Reviewer` options; the full
  registry is now 73 static tools.
- Relicensed from Apache-2.0 to MIT (`LICENSE`, `README.md`).

## v0.6.0 — 2026-11-06

The legacy agent tool surface is complete, with a coverage gate that keeps it
complete.

### Added

- `tools/hosttools`: `ask_user`, `present`, `plan_review`, `exit_plan_mode`,
  `goal` built on the new `contracts/v1` host interfaces (Asker/Presenter/
  PlanController/GoalStore) with an in-memory default `Host`.
- `tools/settings`: `settings_get` / `settings_set` over `v1.Settings`.
- `tools/memory`: `memory` tool (add/list/search/remove/clear) with an
  in-memory `v1.MemoryStore` default.
- `tools/schedule`: `schedule_create/list/update/delete` with an in-memory
  `v1.Scheduler` default (cron evaluation stays in the host).
- `tools/worktree`: `enter_worktree` / `exit_worktree` / `list_worktrees`
  (git worktree, sandbox-checked, `git worktree list --porcelain` parsing).
- `tools/session`: `session_search`, `session_trace`, `session_event_read`,
  `session_event_search`, `session_event_trace` over `v1.SessionLog`, plus an
  in-memory log implementation.
- `tools/inspect` (`inspect`) and `tools/report` (markdown session report
  written inside the sandbox root).
- `tools/ralph`: iterate a child agent via `v1.SubagentRunner` until DONE or
  the round limit.
- `tools/team`: legacy tool names (`spawn_team`, `list_teams`, `team_status`,
  `close_team`, `wait_team`, `send_message`, `interrupt_agent`,
  `list_subagent_models`) mapped onto the existing manager.
- `tools/terminal`: `terminal_list` (sessions with transport/cwd/state).
- `tools/lsp`: legacy single `lsp` tool with action routing.
- `tools/misc`: legacy `todo` alias sharing the `todo_write` store.
- `tools/full`: `All(options)` aggregating every static tool.
- `tools/catalog`: coverage ledger (`Static`, `Legacy`, `ServerBound`,
  `Dynamic`), `Check` and `AuditLegacy`; `tools/full` runs them as
  `TestToolCatalogCoverage` so a missing legacy tool fails CI.
- `contracts/v1/extensions.go`: host interfaces and store contracts
  (Asker, Presenter, PlanController, GoalStore, MemoryStore, SkillStore,
  Scheduler, SessionLog, Reviewer + types).

### Notes

- 66 static tools, 61 legacy names, 5 server-bound (MCP resources/prompts and
  the legacy `lsp` tool), dynamic `mcp__<server>__<tool>` names.
- Zero dependencies, standard library only.

## v0.5.0 — 2026-11-06

Real terminal, remote MCP and in-process orchestration. Still zero dependencies.

### Added

- `tools/terminal`: real unix PTY transport (`/dev/ptmx` + ioctl, setsid/setctty,
  interactive shell); `terminal_send` uses a quiet period instead of a marker, ANSI
  is stripped by default, Ctrl-C is delivered as `0x03`; pipe transport remains as a
  fallback (Windows/other platforms) and keeps its previous semantics.
- `tools/mcp`: Streamable HTTP transport — POST JSON-RPC, `Mcp-Session-Id`,
  `MCP-Protocol-Version`, JSON or SSE responses, session-expiry errors, concurrency
  and cancellation; `Config{Transport,URL,Headers,HTTPClient}` and `StartHTTP`.
  The existing stdio transport is unchanged.
- `contracts/v1/subagent.go`: `SubagentSpec`, `SubagentResult`, `Subagent`,
  `SubagentRunner` (additive).
- `agent.NewRunner`: `v1.SubagentRunner` with depth (`MaxDepth`) and concurrency
  (`MaxConcurrent`) limits, tool exclusion (`ExcludedTools`, defaults include the
  orchestration tools) and per-child isolated history.
- `tools/subagent`: `subagent` tool (one child agent, blocking result).
- `tools/workflow`: `workflow` tool — sequential/parallel subagent steps with
  `{{prev}}` / `{{steps.<name>}}` templating.
- `tools/team`: managed subagents — `team_spawn/team_list/team_send/team_wait/
  team_last/team_close/team_interrupt` with id/name resolution, member limits and
  lifecycle state machine.

### Changed

- `agent.DefaultExcludedTools` now lists the shipped orchestration tool names
  (`subagent`, `workflow`, `team*`) so child agents cannot recurse by default.

## v0.4.0 — 2026-11-06

Complete agent tool surface, still zero dependencies.

### Added

- `tools/editor`: `str_replace_editor` — view/create/str_replace/insert with line
  numbers, sandbox-root checks, read-only enforcement and output caps.
- `tools/image`: `read_image` — magic-byte MIME detection (PNG/JPEG/GIF/WEBP/BMP)
  returning the image in the new `contracts/v1.ToolResult.Parts` (the agent delivers
  attachments as a follow-up user message).
- `tools/terminal`: stateful shell sessions — `terminal_open/send/read/signal/close`
  over a persistent pipe-based shell (marker-terminated commands; not a PTY yet).
- `tools/jobs`: background processes — `job_start/output/kill/list/wait` with ring
  buffered output, exit codes, signals and cleanup.
- `tools/mcp`: resources/prompts support — `ListResources/ReadResource/ListPrompts/
  GetPrompt` and the `mcp_list_resources` / `mcp_read_resource` / `mcp_list_prompts` /
  `mcp_get_prompt` tools.
- `tools/builtin`: `pwsh` tool (pwsh/powershell detection, `-NoProfile
  -NonInteractive -Command`, policy-aware).
- `contracts/v1.ToolResult.Parts` for multimodal tool attachments (additive).

### Changed

- Repository layout: tool packages moved from `tool/` to `tools/` (package `tools`),
  with implementations in `tools/{builtin,editor,image,terminal,jobs,web,mcp,lsp,misc}`.
- `tools.Registry.Unregister(name)` for dynamically composed tool sets.

## v0.3.0 — 2026-11-06

Complete agent tool surface (still zero dependencies).

### Added

- `sandbox`: policy layer for agent tools — allowed roots (symlink-safe `os.Root`
  resolution), read-only mode, command allow/deny lists, environment allowlist,
  network switch with private-address URL guard, output/timeout caps, denied globs.
- `tools/builtin`: `NewWithPolicy(*sandbox.Policy)` runs the full built-in toolset under
  the policy (existing `NewFS`/`NewShell`/`NewBuiltins` keep working).
- `tools/web`: `web_fetch` (HTTP(S), redirect/size caps, HTML→text) and `web_search`
  (pluggable `Searcher`, no built-in vendor).
- `tools/mcp`: standard-library MCP client over stdio JSON-RPC 2.0 — initialize
  handshake, paginated tools/list, tools/call (text/image/audio/resource), ping,
  concurrent calls, cancellation, graceful close — plus `Tools()` adapting MCP tools
  into Rig tools (`mcp__<server>__<tool>`).
- `tools/lsp`: standard-library LSP client (Content-Length framing, initialize,
  hover/definition/documentSymbols/diagnostics) and `lsp_*` tools.
- `tools/misc`: `working_directory` (root-checked cwd) and in-memory `todo_write`.
- `docs/TOOLS.md`: full old-agent tool catalog mapped to Rig / product / roadmap.

## v0.2.0 — 2026-11-06

**Breaking: Rig is now a pure toolset with zero dependencies.**

### Removed (moved back to the ZeoKeel product repo)

- `assembly`, `host`, `kernelx`, `keys`, `modules/appinfo` — compile-time module
  assembly, preflight/rollback, headless host and the appinfo module now live in the
  product that uses the kernel; Rig no longer depends on `zeokeel-kernel`.
- `examples/minimal`, `examples/custom-module` (module-assembly examples).

### Added

- `examples/agent` — provider + built-in tools + gate + history + streaming loop.
- `examples/custom-tool` — a third-party `Tool` implementation, registry and invoke.

### Changed

- `go.mod` has no `require` entries; `go list -m all` shows only this module.

## v0.1.0 — 2026-11-06

First public tag of the headless agent SDK.

### Added

- `contracts/v1`: messages/content parts, tool calls, streaming chunks, `Provider`,
  `Tool`, `Gate`, `History`, agent events, `Agent`/`AgentSpec`, sentinel errors.
  Standard library only; the API compatibility generation.
- `agent`: eino-free streaming ReAct loop — model rounds, parallel tool calls,
  approval gate, iteration cap, cancellation, synchronous event stream, history
  persistence.
- `provider/openai`: OpenAI-compatible streaming provider — SSE parsing, tool-call
  deltas, reasoning content, usage, 429/5xx retries, context cancellation.
- `tools` + `tools/builtin`: registry and seven built-in tools (`read_file`,
  `write_file`, `edit_file`, `list_dir`, `glob`, `grep`, `bash`) with root sandbox,
  output caps and timeouts.
- `gate`: `AllowAll`, `DenyAll`, `AllowTools`, `Func`.
- `session`: in-memory `History`.
- `assembly` + `modules/appinfo`: compile-time registry, embedded headless/minimal
  assemblies, kernel preflight/rollback, build-identity module.
- `host`: headless runner with flags, settings file, logger, signals and graceful
  shutdown.
- `kernelx` (public): kernel descriptors/service keys/registry/effect helpers for module authors;
  `keys`: shared service keys (`AppInfoKey`, `SettingsKey`, `LoggerKey`).
- `examples/minimal`, `examples/custom-module`.
