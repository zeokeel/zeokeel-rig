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
| KN-07 | `mcpserver` | 只实现 **stdio 传输**，没有 HTTP/SSE | 不能被浏览器/远程客户端直接连接 | 由宿主在前面套一层 HTTP↔stdio 桥；或自带传输层 | `mcpserver/e2e_test.go`（stdio 端到端） |
| KN-08 | `mcpserver` | **没有在途调用上限**：每条请求一个 goroutine | 恶意/失控客户端可以打满 goroutine 与工具并发 | 反向代理/宿主侧限流；工具自身要有超时与并发控制 | `mcpserver/audit_lifecycle_test.go`（并发与收尾语义） |
| KN-09 | `mcpserver` | 默认**宽松协议**：不校验 `jsonrpc` 字面值、`id` 任意类型、`initialize` 前可用 `tools/call` | 非标客户端也能跑，但协议不合法时不会报错 | `WithStrictProtocol(true)` 收紧（-32600）；面向第三方客户端建议打开 | `mcpserver/strict_internal_test.go`（严格 vs 宽松两套） |
| KN-10 | `mcpserver` | 工具名形状默认只**告警**（stderr），不报错 | 不合规名字（含空格/点号/超 64 字符）在严格客户端上可能被拒 | `WithStrictProtocol(true)` 升级为错误；空名/重名在两种模式下都报错 | `mcpserver/strict_internal_test.go: TestToolNameValidationWarnsByDefaultErrorsInStrict` |
| KN-11 | `internal/filewatch` | 轮询只能看到"扫描时刻"，**两次扫描之间创建又删除**的文件完全不可见 | 短命临时文件不产生事件 | 缩小 `Interval`；需要精确捕获请用内核事件（inotify/FSEvents，本包刻意不引依赖） | `internal/filewatch/doc.go`（边界声明）+ `TestIntervalBoundsScanWork`（频率受控） |
| KN-12 | `internal/filewatch` | `MaxFiles` 触顶时保留字典序前 N 个 | 前 N 个里删掉一个后，会多纳入一个原本范围外的条目，**以 `created` 出现**（误报） | 看到 `Notice` 就别把 `created` 当真；调大 `MaxFiles` | `internal/filewatch/scan.go`（注释）+ `TestMaxFilesCapAndNoticeOnce` |
| KN-13 | `session/store` | **不提供跨进程互斥**（进程内按 ID 分片加锁） | 两个进程同写一个目录/同一 session 会互相覆盖历史 | 一个目录只给一个进程写；或宿主自己加文件锁 | `session/store/store.go:40`（包文档）+ `kit/audit_wiring_test.go`（同目录双实例用例） |
| KN-14 | `sandbox/escalate` | **Windows 孙进程不会被杀**：`exec.CommandContext`+`WaitDelay` 只保证父进程不再等管道 | 命令派生的子进程可能在 broker 返回后继续存活；超时只保证不再等待 | 宿主自行用 Job Object（Windows）/进程组（POSIX）收尾，或放进容器/VM | `sandbox/escalate/audit_limits_test.go`（超时/输出上限）+ 包文档"不封住的"段 |
| KN-15 | `sandbox/escalate` | **工作目录 TOCTOU**：`Dir` 在判定时与 spawn 前各解析一次，沙箱可写 `Roots` | "解析之后、chdir 之前"把某段换成符号链接，命令仍可能在 Roots 之外执行 | 执行后复核并标记（exit=-1 + 诊断行），把窗口压到最小；彻底封死需 `dirfd`+`fchdir` 平台代码（刻意不做） | `sandbox/escalate`（Dir 校验/复核用例） |
| KN-16 | `tools/patch` | Unix 上 `os.Root.Chmod` 有 stdlib 级竞态：`Lstat` 与 `Chmod` 之间目标被换成符号链接时可能作用到链接本身 | 极端并发下"改权限"可能落到非预期对象；不能用来防敌对本地进程 | 落盘走 `internal/fsguard`（`WriteFileAtomic` 把 Chmod 施加在自己 `O_EXCL` 创建的临时文件上，不暴露该竞态）；工具文档已声明"约束配合的模型，不是敌对进程" | `internal/fsguard` 用例 + `tools/patch/doc.go` 声明 |
| KN-17 | `tools/patch` | 不做大小写归一化：同一补丁里的路径去重按字节比较 | 大小写不敏感文件系统（macOS 默认、Windows）上 `A.md` 与 `a.md` 会被当成两个文件而同写一个物理文件 | 宿主在策略层拒绝同名不同大小写的路径 | `tools/patch/*_test.go`（路径规则用例） |

## 附：两条"文档与实现"需要留意的地方

1. `tools/patch/doc.go` 的"已知残余风险"说落盘用 `filepath` + `os.Rename`、没有 TOCTOU 保护；而 `tools/patch/path.go` 与 `apply.go` 说明所有 I/O 都经 `internal/fsguard` 的 `os.Root` 句柄。**实际实现是后者**，残余风险收敛到 `os.Root.Chmod` 的 stdlib 窗口（见 KN-16）。这段文档应当在下次改 `tools/patch` 时对齐（本次任务禁止改该包）。
2. `session/store` 的"不提供跨进程互斥"只写在包注释里，`kit.Config.SessionStoreDir` 的用户看不到。建议在 `kit` 的配置文档里补一句（本次任务禁止改 `kit`）。

## 变更记录

- 2026-11-17：首版。列出 17 条边界；其中 KN-03/04/05/09/10 在同轮工作里补了测试钉住（`secrets/sweep_test.go`、`tools/search/hardlink_test.go`、`mcpserver/strict_internal_test.go`），KN-05/09/10 同时补了开关（`WithHardlinkGuard`、`WithStrictProtocol`）。