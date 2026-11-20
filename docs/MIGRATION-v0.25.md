# 从 v0.24 升到 v0.25 的迁移指南

这份文档是**给宿主升级用的**：v0.25 删掉了一批 API（脱敏、保护路径、拒绝 glob），
宿主编译器会先把它们逐个报错。别照报错一条条改——那是一条条往返。先跑扫描器：

```sh
go run github.com/zeokeel/zeokeel-rig/cmd/rigmigrate -C ./你的宿主目录
# 机器可读：-json；退出码 0 = 干净、1 = 有命中、2 = 用法/读取错误
```

扫描器一次列出**文件:行、旧符号、替代做法**，与下面的表格同源（表格里的每个旧符号
都在 `internal/rigmigrate` 的符号表里，守护测试会比对两边）。改完再编一次即可。

匹配口径是"import 是否真在本仓模块前缀下 + 接收者的声明类型"，不做类型推导：宿主
自己的 `github.com/host/secrets`、`github.com/host/internal/kit`，或本地 struct 上
的同名字段，都不会被误报；反过来，`cfg := load()` 这类拿不到类型的接收者可能漏报。
所以它是"一次列全常见写法"的加速器，不是编译器的替代品。

读者：正在把 Rig 从 v0.24 升到 v0.25 的宿主集成者。本文只讲破坏性变更与替代做法；
新增能力见 [../CHANGELOG.md](../CHANGELOG.md) 与 [CONFIG.md](CONFIG.md)。

## 1. 脱敏（`secrets` 包与 `Redactor` 整族）

SDK 不再做任何脱敏：工具结果、助手文本与推理、事件、错误、导出与日志都是**原文**。
单用户客户端的取向是"不偷偷改写用户自己的数据"，而半吊子脱敏只会让人误以为已经清洗过。
需要屏蔽凭据的宿主在自己的 UI/导出通道处理（见 [../SECURITY.md](../SECURITY.md)）。

| 旧 API | 新做法 | 备注 |
| --- | --- | --- |
| `github.com/zeokeel/zeokeel-rig/secrets` | （无替代）宿主导出/展示前自己清洗 | 包整体删除，含 22 条内置模式 |
| `agent.NewWithRedactor` | `agent.New(spec)` | 构造期不再接受脱敏器 |
| `agent.RedactMessages` | （无替代）删掉这次调用 | 没有任何脱敏层，历史文本原样进出 |
| `agent.RunnerOptions.Redactor` | （无替代）删掉这个字段 | 子 agent 与父 agent 一样不脱敏 |
| `secrets.RedactForExport` | （无替代）宿主导出前自己清洗 | 随包一起删除 |
| `kit.Config.Redactor` | `kit.Config.PermissionPreset` / `Mode` / `Approval` / `Policy` | 权限只由权限档、模式、审批与策略表达 |

## 2. 保护路径（`sandbox` 策略层不再有这一层）

策略层只剩**可写根围栏**：要"根内局部只读"就用可写根自己的 `ReadOnlySubpaths`
（`WithReadOnlySubpaths` / `WithWritableRoot(root, readOnly...)` / `ProtectGitMetadata`）；
要"按模式看住路径"用隐藏路径（见第 3 节）。`Preflight` 与 `sandbox_status` 里**没有**
"保护路径"这个概念，它们只按档位说可写面：`read-only` 只点名私有 scratch，没有配
scratch 就直说"没有可写目录"；`workspace-write` 点名工作区（以及系统临时目录）。
`sandbox_status` 的 `writableRoots` 数组同口径——`read-only` 下只有配了 scratch 才非空
（`tempWritable.writable` 是另一个布尔，说的是临时目录此刻是否真的可写）。

| 旧 API | 新做法 | 备注 |
| --- | --- | --- |
| `sandbox.WithProtectedPaths` | `sandbox.WithWritableRoot(root, readOnly...)` | 局部只读改由可写根表达 |
| `sandbox.WithProtectMissingPaths` | （无替代）删掉这个选项 | 没有保护路径就没有"缺失"这一说 |
| `sandbox.WorkspaceMetadataPaths` | （无替代）删掉这次调用 | 不再需要枚举"需要保护的位置" |
| `sandbox.WorkspaceNonGitMetadataPaths` | （无替代）删掉这次调用 | 同上 |
| `sandbox.Policy.AddProtectedPaths` | `sandbox.WithWritableRoot(root, readOnly...)` | 追加接口一起删除 |
| `sandbox.Policy.ProtectedPaths` | `sandbox.Policy.WritableRootsFor(mode)` 里的 `ReadOnlySubpaths` | 回读可写面时看每个根自己的只读子路径 |
| `sandbox.Policy.WriteDenyPaths` | `sandbox.Policy.WritableRootsFor(mode)` 里的 `ReadOnlySubpaths` | 旧口径是"减去保护路径后的可写面" |
| `sandbox.Policy.ProtectMissingPaths` | （无替代）删掉这个选项 | 已无缺失保护路径的概念 |
| `sandbox.Denial.ProtectedPaths` | `sandbox.Denial.WritableRoots` | 拒绝归因不再渲染保护路径，可写面看 `WritableRoots` |

## 3. 拒绝 glob（改用隐藏路径）

隐藏路径是**参与判定**的替代品：`WithHidden` / `Hidden` / `HiddenPatterns` 与
`sandbox.GlobMatch` 在读取、列目录、`glob`/`grep`/`search` 结果与写入上全部生效
（零配置入口是 `kit.Config.HiddenPaths`）。它只约束工具面，**不是** OS 边界。

`sandbox.Policy.DeniedPath` **保留**，但语义已收窄为"是否位于所有根之外"，不再是拒绝 glob。

| 旧 API | 新做法 | 备注 |
| --- | --- | --- |
| `sandbox.WithDeniedGlobs` | `sandbox.WithHidden(patterns...)` | 隐藏参与判定，不是记录型 no-op |
| `sandbox.Policy.DeniedGlobs` | `sandbox.WithHidden` / `sandbox.Policy.HiddenPatterns` | 拒绝 glob 整层删除 |

## 4. 可写授权校验（少一个参数）

`ValidateWritableGrantPath` 去掉了 `protected` 参数：授权校验只看可写根、静态可写根与
已授权项。要拒绝某个目录，请把它排除在可写面之外。

| 旧 API | 新做法 | 备注 |
| --- | --- | --- |
| `sandbox.ValidateWritableGrantPath` | `sandbox.ValidateWritableGrantPath(path, roots, configured, granted)` | 5 个实参 → 4 个实参 |
| `sandbox.ErrGrantProtected` | （无替代）按错误文案或 `WritableRootsFor` 判断 | 不再有"与保护路径冲突"这一类拒绝 |

## 5. `sandbox/osrunner` 的诊断字段

OS 后端从不渲染保护规则，所以"声明了保护"与"缺失保护"这两个字段没有意义；
保留字段只会造成"设了边界"的错觉。可写面诊断看 `Profile.WritableRoots`
（`[]sandbox.WritableRoot`，含每个根自己的只读子路径）与 `Profile.ReadWrite` /
`Profile.ReadOnly`，以及 `RuntimeReport` 的其余字段。

| 旧 API | 新做法 | 备注 |
| --- | --- | --- |
| `osrunner.Profile.Protected` | （无替代）删掉这个字段 | 后端不渲染保护规则 |
| `osrunner.Profile.MissingProtected` | （无替代）删掉这个字段 | 同上 |
| `osrunner.ErrMissingProtected` | （无替代）删掉这个判断 | 没有保护路径就不会有"缺失"错误 |

## 6. `kit` 的保护开关

`kit` 从不追加任何保护项，所以这些开关从来没有"关掉什么"的语义。权限只由
`kit.Config.PermissionPreset` / `Mode` / `Approval` / `Policy` 表达；凭据位置保护属于
部署层（容器/VM）或宿主自己的校验。

| 旧 API | 新做法 | 备注 |
| --- | --- | --- |
| `kit.Config.DisableProtection` | `kit.Config.PermissionPreset` / `Mode` / `Approval` / `Policy` | 没有可关的东西 |
| `kit.Config.ProtectPaths` | （无替代）删掉这个字段 | kit 不追加保护路径 |
| `kit.Config.ProtectWorkspaceMetadata` | （无替代）删掉这个字段 | kit 不追加保护路径 |
| `kit.HomeSecretPaths` | （无替代）宿主自己维护凭据位置 | 工具面不做凭据位置保护 |

## 7. `tools/full` 的保护开关

`AllOptions` 里的同名字段一起删除：`sandbox.Policy` 里不再有任何"保护"开关。

| 旧 API | 新做法 | 备注 |
| --- | --- | --- |
| `full.AllOptions.ProtectWorkspaceMetadata` | （无替代）删掉这个字段 | 与第 6 节同一层语义 |

## 8. 事件桥签名（`kit.NewEventSink`）

事件桥只做 `rig.Event` → `events.Envelope` 的映射与 NDJSON 写入，不改写内容，
所以 `redactor` 参数删除。

| 旧 API | 新做法 | 备注 |
| --- | --- | --- |
| `kit.NewEventSink` | `kit.NewEventSink(w)` | 旧调用是两个实参 `(w, redactor)`，现在是 `(w)` |

## 9. `session/sqlite`：核心 schema 升到 12

库文件从 v11 升到 v12（`rig_sessions.next_ordinal`）。升级是**单向**的：
先在旧版本里 `Export` 一份备份（或直接拷库文件），再用新版本 `Open`，它会分段迁移。
旧版本程序不要打开 v12 的库（会丢新字段）。

| 旧 API | 新做法 | 备注 |
| --- | --- | --- |
| v11 的 SQLite 库文件 | 先 `Export` 备份，再 `Open`（自动分段迁移） | 版本号只增不减；`CanOpen` 会给出可行/不可行与原因 |

## 顺带新增（无需迁移，可选采纳）

- `sandbox.WithScratchDir` + `kit.Config.ScratchDir`：`read-only`/`workspace-write`
  档下的私有可写临时目录（注入 `TMPDIR/TMP/TEMP`）。
- `sandbox.Policy.WritableRootsFor(mode)`：按模式回读"真的能写哪里"，与 `Preflight`、
  `sandbox_status` 同口径。
- `sandbox.Policy.CheckConfinedCommand`：执行侧已被 OS runner 兜住时的命令检查。
- `kit.Config.PromptSections`（追加系统提示词区段）与 `kit.Config.Aggregate`
  （`storage.Storage` 一次给出 History/Checkpoints/Memory/Spill）。
- `spillfile.New(dir)`：文件型 `rig.SpillStore`；`kit.EventSink`：导出的 NDJSON 事件桥。
- `session/sqlite`：管理面 `Store.DeleteMessages` / `TruncateMessages` / `Stats` /
  `DeleteCheckpoint`；导入导出对称的 `Store.ImportRows` 与
  `ExportOptions.IncludeEvents` / `IncludeCosts` / `IncludeCheckpoints`。

## 相关文档

- [../CHANGELOG.md](../CHANGELOG.md)：逐版本变更（含"Unreleased"）
- [VERSIONING.md](VERSIONING.md)：兼容承诺与破坏性变更的发布规则
- [CONFIG.md](CONFIG.md)：`kit.Config` 全字段与默认值
- [SANDBOX-OS.md](SANDBOX-OS.md)：可写根围栏与 OS 沙箱的边界
- [sqlite.md](sqlite.md)：SQLite 会话持久化的三档与版本升级