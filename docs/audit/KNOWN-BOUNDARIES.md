# 已知边界清单（KNOWN-BOUNDARIES）

本文件汇总**当前仍然存在**、且**刻意不修**的能力边界。每条都写明：边界是什么、影响什么、怎么缓解、有没有测试钉住。

判定原则：
- 修得掉且值得修的，进代码 + 测试（不在本清单里）；
- 修掉会破坏契约 / 需要平台专用代码 / 成本远超收益的，进本清单，并在对应包的 `doc.go` 里同样写明；
- **每条都必须有测试钉住**（`kn-XXX` 编号可在测试注释里搜到），否则就是"忘了的 bug"而不是"已知边界"。

| 编号 | 包 | 边界 | 影响 | 缓解 | 测试钉住 |
| --- | --- | --- | --- | --- | --- |
| KN-01 | `secrets` | 不做熵检测（无信息熵/字符分布打分） | 未列入模式表、也未被 `Add` 登记的自定义凭据形态不会被识别 | 宿主把当前凭据 `Add`/`AddNamed` 登记为已知明文 | `secrets/audit_patterns_test.go`（模式表 + 反例） |
| KN-02 | `secrets` | 内置模式全部单行 | YAML 的 `password:` 与缩进值分居两行、跨行拼接的凭据漏判 | 敏感值写成单行；或先自行拼接再 `Redact` | `secrets/audit_leak_test.go`（跨行/分割形态用例） |
| KN-03 | `secrets` | **数字型秘密不脱敏**：敏感键下的 number/bool/null 逐字节保留 | `{"pin":123456}` 里的数字原样输出 | 敏感值用**字符串**承载；`Add` 只对文本/字符串生效（不进数字字面量）；或上游序列化前替换 | `secrets/sweep_test.go: TestRedactJSONNumericSecretUnderSensitiveKeyIsPreserved` |
| KN-04 | `secrets` | 流式 `Writer`/`Reader` 单行上限 `MaxBuffered`=64 KiB | 没有换行的超长单行会被**强制冲刷**：有延迟，且 >64 KiB 的单条已知明文可能被切开（非流式不受影响） | 超大输出**先整体 `Redact` 再写**（无边界问题、无延迟）；或按行输出 | `secrets/sweep_test.go: TestWriterOversizeSingleLineRedactsButDelays`、`secrets/audit_leak_test.go: TestAuditKnownLimitation_OverMaxBufferedSecret` |
| KN-05 | `tools/search` | 硬链接是同一 inode 的另一个名字，可能在根之外；`lstat` 无法判断另一个名字在哪 | 内容检索可能读到根外的数据（不是路径逃逸） | `WithHardlinkGuard(true)` 整类跳过 `Nlink > 1` 的文件（默认关，保持兼容）；Windows 拿不到链接数，守卫退化为不跳过 | `tools/search/hardlink_test.go: TestHardlinkGuardSkipsMultiplyLinkedFiles` / `TestHardlinkGuardOffIndexesBothNames` |
| KN-06 | `tools/search` | 内容检索**每次调用现建内存 BM25 索引**，不分片、不缓存 | 内存峰值与"入选文件总字节数"同阶；大仓库有分配压力 | `WithMaxFileBytes`（默认 1 MiB）+ `WithMaxResults`（默认 20/上限 200）收窄；宿主侧先粗筛 | `tools/search/search_test.go`（上限/超时用例） |
| KN-07 | `mcpserver` | **已移除**（对齐"个人 agent 客户端 SDK"，2026-11-18） | 原边界：只实现 stdio 传输、无 HTTP/SSE | 产品本身是 MCP **客户端**（`tools/mcp` 保留）；需要把自己的工具暴露成 server 的宿主应自建 MCP server，再用 `full.AllOptions.MCPClients` 接回来 | `internal/rigdoc/surface_test.go: TestRemovedPackagesAreGone` |
| KN-08 | `mcpserver` | **已移除**（同上） | 原边界：在途调用无上限、每条请求一个 goroutine | 该类"被外部客户端调用"的并发压力不再属于本产品 | `internal/rigdoc/surface_test.go: TestRemovedPackagesAreGone` |
| KN-09 | `mcpserver` | **已移除**（同上） | 原边界：默认宽松协议（不校验 `jsonrpc`/`id`/initialize 顺序） | 协议严格性不再由本产品负责 | `internal/rigdoc/surface_test.go: TestRemovedPackagesAreGone` |
| KN-10 | `mcpserver` | **已移除**（同上） | 原边界：工具名形状默认只告警 | 同上 | `internal/rigdoc/surface_test.go: TestRemovedPackagesAreGone` |
| KN-11 | `internal/filewatch` | **已移除**（同上） | 原边界：轮询只能看到"扫描时刻"，两次扫描之间创建又删除的文件不可见 | 单 agent 场景由 git 覆盖；需要精确事件请用宿主自己的内核事件（inotify/FSEvents） | `internal/rigdoc/surface_test.go: TestRemovedPackagesAreGone` |
| KN-12 | `internal/filewatch` | **已移除**（同上） | 原边界：`MaxFiles` 触顶保留字典序前 N 个，可能产生 `created` 误报 | 同上 | `internal/rigdoc/surface_test.go: TestRemovedPackagesAreGone` |
| KN-13 | `session/store` | **单进程单实例**：索引是内存里的（`NewDir` 扫目录 + 之后由增删改维护），进程内只按 ID 分片加锁，**不做跨实例/跨进程协调** | 同一目录上的两个实例各自持有独立索引 → 看不到对方新建/写入的会话（**数据都在盘上不会丢**，但两边 `List` 的列表不完整、计数暂时偏小） | 一个目录只由一个 `Dir`/agent 实例使用；"重启后继续"只需关掉旧实例再 `NewDir`（索引就是扫目录，无需迁移） | `session/store/store.go`（包文档"并发"段）+ `session/store/layout_test.go`（布局/扫描用例）+ `kit/wiring_test.go: TestWiringTwoAgentsSameStoreDir`（同目录双实例的行为钉死） |
| KN-14 | ~~`sandbox/escalate`~~ | **已移除**（对齐"个人 agent 客户端 SDK"，2026-11-18）。原边界：Windows 孙进程不会被杀（`exec.CommandContext`+`WaitDelay` 只保证父进程不再等管道） | 原影响：命令派生的子进程可能在 broker 返回后继续存活；超时只保证不再等待 | 整包已删除：个人客户端放宽权限用 `danger-full-access` 或审批，不走"把命令送回宿主执行"的旁路 | `internal/rigdoc/surface_test.go: TestRemovedPackagesAreGone` |
| KN-15 | ~~`sandbox/escalate`~~ | **已移除**（同上）。原边界：工作目录 TOCTOU（`Dir` 在判定时与 spawn 前各解析一次，沙箱可写 `Roots`） | 原影响："解析之后、chdir 之前"把某段换成符号链接，命令仍可能在 Roots 之外执行 | 整包已删除（同上）；文件写入路径的同类问题由 `internal/fsguard`（`os.Root`）覆盖 | `internal/rigdoc/surface_test.go: TestRemovedPackagesAreGone` |
| KN-16 | `tools/patch` | Unix 上 `os.Root.Chmod` 有 stdlib 级竞态：`Lstat` 与 `Chmod` 之间目标被换成符号链接时可能作用到链接本身 | 极端并发下"改权限"可能落到非预期对象；不能用来防敌对本地进程 | 落盘走 `internal/fsguard`（`WriteFileAtomic` 把 Chmod 施加在自己 `O_EXCL` 创建的临时文件上，不暴露该竞态）；工具文档已声明"约束配合的模型，不是敌对进程" | `internal/fsguard` 用例 + `tools/patch/doc.go` 声明 |
| KN-17 | `tools/patch` | 不做大小写归一化：同一补丁里的路径去重按字节比较 | 大小写不敏感文件系统（macOS 默认、Windows）上 `A.md` 与 `a.md` 会被当成两个文件而同写一个物理文件 | 宿主在策略层拒绝同名不同大小写的路径 | `tools/patch/*_test.go`（路径规则用例） |

## KN-18（通则）：注入一个组件就取代一组配置

> **凡是"注入一个组件就取代一组配置"的地方，要么在 `New` 里做一致性校验（冲突即报错），
> 要么 `slog.Warn` 指明"哪几项已失效"。静默吸收配置是"假默认值"的另一种形态——
> 用户以为开启了保护，实际上没有。**

已按这条通则处理的（2026-11-18，审计 2B 修复轮）：

| 位置 | 冲突 | 当前行为 |
| --- | --- | --- |
| `kit.Config.Tools` + `Config.SandboxRunner` | 自建工具集不经过 OS 沙箱包装，但 PTC 仍用 runner | **构造期报错**（`kit: Config.Tools 与 Config.SandboxRunner 不能同时设置——…`） |
| `kit.Config.Tools` + `Searcher`/`SessionLog`/`GitUndo`/`UndoTracker`/`Asker` | 对应工具不会出现在自建工具集里（Asker 仍驱动审批门禁） | 一条 `slog.Warn` 列出失效项 |
| `provider/*.Config.HTTPClient` + `HeaderTimeout` | 注入的 client 上没有 `ResponseHeaderTimeout` | `*http.Transport` → 克隆补上；自定义 `RoundTripper` → `slog.Warn` |
| `provider/openai.Config.IncludeUsage` | 原本是假配置（设 false 也发 `stream_options`） | 改成 `*bool`：`nil`/`true` 发送，`false` 不发 |
| `provider/local.Config.APIKey` + `APIKeyEnv` | 显式 key 时不再读环境变量 | 已文档化（语义明确，不告警） |
| `kit.Config.Policy` + `Mode`/`AllowNetwork`/`CommandAllowlist` | 传入 policy 完全接管这三项 | 已文档化（语义明确，不告警） |

**待办（未修，建议下轮）**：`kit.Config.SandboxRunner` 在 `Tools != nil` 时已报错，但
`cfg.PTC` 分支仍会用它——如果将来允许"自建工具集 + PTC + runner"，需要重新审视这条错误的
粒度（目前选择"整体拒绝"）。

## 附：两条"文档与实现"需要留意的地方

1. `tools/patch/doc.go` 的"已知残余风险"说落盘用 `filepath` + `os.Rename`、没有 TOCTOU 保护；而 `tools/patch/path.go` 与 `apply.go` 说明所有 I/O 都经 `internal/fsguard` 的 `os.Root` 句柄。**实际实现是后者**，残余风险收敛到 `os.Root.Chmod` 的 stdlib 窗口（见 KN-16）。这段文档应当在下次改 `tools/patch` 时对齐（本次任务禁止改该包）。
2. `session/store` 的"不提供跨进程互斥"只写在包注释里，`kit.Config.SessionStoreDir` 的用户看不到。建议在 `kit` 的配置文档里补一句（本次任务禁止改 `kit`）。

## 变更记录

- 2026-11-17：首版。列出 17 条边界；其中 KN-03/04/05/09/10 在同轮工作里补了测试钉住（`secrets/sweep_test.go`、`tools/search/hardlink_test.go`、`mcpserver/strict_internal_test.go`），KN-05/09/10 同时补了开关（`WithHardlinkGuard`、`WithStrictProtocol`）。