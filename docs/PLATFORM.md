# 平台化规划（v2）

> **本文件是唯一来源。** v1（含一次 §9 补丁）已作废——它同时保留了错误契约与已推翻的结论，
> 两份内容互相矛盾。v2 只写**核实过**的东西；不确定的一律标"未验证"，不写成承诺。

## 0. 边界

| 层 | 归谁 | 内容 |
|---|---|---|
| 业务逻辑 | 宿主应用 | 对话策略、提示词、工具编排、UI、项目/任务等业务实体 |
| 运行时与状态 | 本 SDK | 会话与消息、检查点与崩溃恢复、事件、记忆、溢出结果、花费、检索、配额机制、迁移与导出、安全关停 |
| 不做 | 都不做 | 账号鉴权、云端同步、密钥托管、应用级加密（见 §7）、工作流编排 |

判据：**"这份数据丢了，agent 这个回合还能不能正确继续？"** 不能 → SDK；能（只是体验差）→ 宿主。

按此判据：**会话/消息、检查点、溢出结果**是必需；**事件、花费、记忆、检索**是可选能力
（事件写失败按设计不该打断 Run；花费与检索只是功能；记忆下回合可重建）。这个区分决定接口形状（§3）。

## 1. 目标形态

```go
st, err := sqlite.Open(sqlite.Options{Path: statePath}) // 子模块；内存态聚合实现尚不存在（见 §5 步 2）
if err != nil { ... }
defer app.Shutdown(ctx)   // 注意：不是 st.Close()——回合、后台任务、事件都要先收尾（§4）

app := kit.New(kit.Config{
	Provider: p,
	Storage:  st,   // 目标：一个字段
	Asker:    ui,
})
```

现状基线（不要再写错）：`kit.Config` 里与存储相关的字段有 **9 个**
（`Spill`/`CheckpointStore`/`History`/`SessionStoreDir`/`SessionStore`/`SessionLog`/`Meter`/`Memory`/`EventWriter`），
其中 **只有** `SessionStore` 是具体类型 `*store.Dir`，其余已经是接口。

## 2. 分层与铁律

| 层 | 包 | 依赖 | 作用 |
|---|---|---|---|
| 0 | 根 module：`rig` | **stdlib only** | 最小契约叶子（`History`/`SessionLog`/`CheckpointStore`/`SpillStore`/`Message`…） |
| 1 | 根 module：`storage`（新增） | stdlib + 本仓 `rig`/`memory` | 聚合契约 `storage.Storage` |
| 2 | 根 module：`session/store`、`session`（内存 History）、`internal/memwiki` | stdlib only | 零依赖实现 |
| 3 | 子模块 `session/sqlite` | `modernc.org/sqlite` | 参考实现：一个库装全部 |
| 4 | 宿主 | 任意 | 只写业务 |

**铁律**：

1. 根 `rig` 包**不得 import 本仓库任何子包**（`internal/rigdoc` 的
   `TestRootPackageStaysStdlibOnly` 硬拦；实测加一行 `import .../memory` 即红）。
   → 所以聚合契约不能放根包，必须放 `storage` 包。
2. 根 module 永不 import `session/sqlite`。
3. 新增的 `storage` 包要登记进 `internal/rigdoc` 的 `leafCapabilities` 与 `docs/MODULES.md`。
4. `storage` **不得 import `session/store`**（layer 1 依赖 layer 2 会成环，`storage` 的
   依赖面只有 stdlib + `rig` + `memory`）。推论：目录后端的适配器只能放在
   `session/store` 侧（或独立小包），`Dir` 上也不能有 `func (d *Dir) Storage() storage.Storage`
   这类反向构造函数。

## 3. 契约（已用编译探针验证）

只能声明**已有实现具备**的方法，否则验收永远不绿：

```go
package storage

// Storage 是"一个库装一份会话状态"的最小必需面（只读能力优先落地）。
type Storage interface {
	History() rig.History              // sqlite views.go:26 ✅
	Checkpoints() rig.CheckpointStore  // views.go:29 ✅
	Memory() memory.Wiki               // memory.go:19 ✅（可选能力，见 §0 判据）
	Spill() rig.SpillStore             // spill.go:19 ✅（注意是 Spill，不是 Spills）
	Close() error
}
```

实测：把该集合指向 `*sqlite.Store`，`go vet` 通过（复核 2026-11-19：exit 0）；
`*store.Dir` 不行（缺 `Checkpoints`，见下）。**可选能力不进主接口**：事件、花费、会话
列表、租约各做成一个独立小接口，由**能力位**（`storagetest.Capability`/`Capabilities`，
`type Capability struct` 与 `type Capabilities map[string]Capability`；清单 A §14-③-3）
声明谁存在——一旦塞进主接口，第二个实现就
永远不达标。**三处已知缺口**：

| 缺口 | 现状 | 修法 |
|---|---|---|
| `SessionLog()` 返回类型 | 声明返回的是包内自有接口 `sqlite.SessionLog`（= `rig.SessionLog` + `AppendEvent`，`views.go:19-32`）：方法集是超集，所以 `var l rig.SessionLog = st.SessionLog()` **能编译**；但**签名**不匹配任何声明 `SessionLog() rig.SessionLog` 的契约（探针：`have SessionLog() sqlite.SessionLog`） | 方法改返回 `rig.SessionLog`（`AppendEvent` 移到 `*Store` 上），多出的写方法留在具体类型；**注意它不在 §3 的五方法里**，所以不阻塞只读契约的落地 |
| 事件写入 | 只有 `kit.Config.EventWriter io.Writer` → NDJSON（`events/emitter.go`） | 改 kit 侧 eventBridge，让写入也进存储；并定义"写失败算不算 turn error" |
| 追加签名 | 两实现都是单条 `Append(ctx,id,rig.Message)` | 契约里一律用已有的 `AppendBatch(ctx,id, ...rig.Message)`；**不要**定义变参 `Append` |

**不要做的事**（都被实测否决）：

- ❌ 在契约里写 `Sessions() SessionState` 并引用 `store.Meta`/`store.Filter` → `storage` 就得
  import `session/store`，违反 §2 铁律 4（而适配器已放在 `session/store` 侧，两边一对就
  **成环**）；
- ❌ 断言 `var _ storage.Storage = (*store.Dir)(nil)` → `store.Dir` 没有 events/checkpoints/spill，
  实测 `missing method Checkpoints`。要支持目录实现就用**适配器**，且它只能放在
  `session/store` 侧（§2 铁律 4：`storage` 不 import `session/store`）：
  `store.NewDirStorage(*store.Dir)` → `store.DirStorage`（`dirstorage.go:62,71`），断言写成
  `var _ storage.Storage = store.DirStorage{}`。适配器绑定一个默认会话 ID
  （`store.DefaultSessionID`）：`Dir.History(ctx, id)`（`history.go:47`）与无参
  `History() rig.History` 签名不兼容；`Checkpoints()/Memory()/Spill()` 返回 nil——目录
  后端确实没有这三项能力；
- ❌ 用嵌入（embedding）拼契约 → 现有契约里 `Load` 出现 4 次、`Save`/`Append` 各 3 次、
  `Search`/`Delete` 各 2 次，**同名不同签名无法共存**。

## 4. 四件必须做、且优先于接口改造的底层修复

1. **生命周期**：`kit.Agent` 今天**没有** `Close`/`Shutdown`，也没有 `jobs.Manager` 访问器。
   宿主若直接 `st.Close()`，正在跑的回合会拿到 `ErrClosed` → 回合判失败 → 用户看到
   "回答到一半消失"。需要 `app.Shutdown(ctx)`：取消 run → 排空事件 → `wal_checkpoint(TRUNCATE)` → 关存储。
2. **多进程**：`agent` 每个 Run 开头自动 `RecoverInterruptedTurn`，同库第二个进程会把第一个进程
   **正在跑**的回合当成崩溃残留并补合成消息；而热路径 `AppendBatch` 没有 busy 重试
   （只有 `Create`/`SaveCost` 有）。需要**会话级租约**。
3. **降级与迁移**：`coreVersion > build` 直接报错，而 Export 只能由新版本执行 → 回滚后既打不开
   也导不出。且迁移只有 `CREATE TABLE IF NOT EXISTS`，**没有 ALTER 路径** → 第一次给老表加列就
   静默不升级。需要：`CanOpen` 预检、三选一可执行动作（升级/先备份/只读打开）、显式 ALTER 路径。
4. **权限与备份**：库文件按 umask（通常 0644），而记忆是 0700/0600 → 同机其他账户可读全部会话。
   `Close` 不 checkpoint、无 `VACUUM INTO` → 用户拷 `.db` 会漏 `-wal` 得到 malformed。
   需要：`chmod 0600` 三件套（`-wal`/`-shm`）、`Backup(path)` 与 `Close` 内的 checkpoint。

## 5. 执行顺序

| 步 | 内容 | 为什么这个顺序 |
|---|---|---|
| **0** | **只动发布面、不动契约**：删 `session/sqlite/go.mod:22` 的 `replace`、把 `:6` 的 `require v0.24.0` 换成本次要发的 `vX.Y.Z`（`v0.24.0` 从未打过 tag，见清单 B §10）、补根 tag 与 `session/sqlite/vX.Y.Z`（现在 `git tag -l 'session/sqlite/*'` 为空）、修 `sqlite-consumer-smoke`（`ci.yml:305` 现在 stage 不到根 module、`GOPROXY` 只有 `file://` 拉不到第三方依赖、版本号写死 `v0.24.0`）、让"漏打 tag"CI 红 | 现在"多实现分叉即红"是**假的**：根 module 不能 import sqlite，根 CI 的 `go test ./...` 跑不到子模块。而且这一步**必须**早于"子模块 import `storage`"——否则本地靠 replace 编过、下游 `go get` 解析不到那个版本（清单 B §8 已实测） |
| **1** | 落地 §3 的**只读**契约 `storage.Storage` + 能力位（`storagetest.Capability`/`Capabilities`）+ 可选小接口；**同批**：先写 `storagetest` 基线用例再让实现达标、`session/store` 侧的 `DirStorage` 适配器、`session/sqlite` 的 `SessionLog()` 先改成 `rig.SessionLog`（清单 B §12-1 的 13 个调用点）、`internal/rigdoc` 的 `leafCapabilities` 与 `docs/MODULES.md` 登记 | 已验证可实现，风险最低。`storagetest` 的"两实现共用基线"要有对象才存在（契约 + 适配器），所以它和契约**同批**而不是更早（清单 B §12-4）；名字与模块表必须与包同批，否则 `go test ./internal/rigdoc` 与文档对不上（清单 A §16-D11） |
| **2** | 注入点接口化：`SessionStore *store.Dir` → 接口（`store.Store` 没有 `History` → 类型断言或加方法）；同步改 `Agent.SessionStore()` 返回类型；内存聚合实现（今天不存在，要新写） | 单独一版，宿主只迁移一次 |
| **3** | §4 的生命周期：`Shutdown` / `wal_checkpoint` / `Backup` | 止"回答消失"，用户可感知 |
| **4** | 写路径收编（events / checkpoints / costs）+ 定"事件写失败算不算 turn error" | 先定语义再改代码 |
| **5** | 配额**只做机制**：默认全关、`Prune(ctx, DryRun)` 可预览、超限不静默删用户数据 | 防 SDK 默认删数据 |
| **6** | `Export`/`Import` 往返校验 → 最小示例（**第三个 module**，否则拖依赖进零依赖门禁）→ 宿主删代码 | 收尾；出口是 0.x 改 schema 的唯一安全网 |

**发版节奏**：四次小版本，每次只破坏一处。原计划两版里 S1 与 S3 改同一个字段，宿主会迁移两次。

## 6. 事实基线（v1 写错过的，不要再写）

| 说法 | 实际 |
|---|---|
| "宿主要拼 5 个字段" | 9 个（见 §1） |
| "默认往某目录写 NDJSON" | 不存在；零配置 `history = session.NewMemory()` |
| "默认不持久化" | **相反**：零配置会建**文件 wiki** 并写盘（`~/.local/state/rig/memory/<workspaceID>`），有测试钉死 |
| "Open 时自动 WAL checkpoint" | `Close` 里没有 `wal_checkpoint`；且 Open 是包级构造器，不是接口方法 |
| "回收一半靠宿主记得调" | spill GC 已自动（Save 后按 `MaxFiles`）；缺的是 events 与 VACUUM |
| "任何别的实现都装不上去" | 只有 `SessionStore` 是具体类型，其余五个已是接口 |
| "`session.Store` / `session.Dir` / `rig.Storage` / `rig.CostSink` / `rig.NewMemory()`" | 都不存在；实际是 `session/store.Store`、`store.Dir`，且上述 `rig.*` 全仓 0 命中 |
| "内存态 + 目录态两套默认实现" | 只有 `session.Memory`（History）与 `memwiki.MemoryWiki`（Wiki）；聚合形态的内存实现**要新写** |
| "宿主可删数百行存储代码" | **未验证**：仓库 examples 仅 221 行、0 处存储调用，无宿主可度量 |
| "目录后端 `Title` 重启后也丢" | **不准确**：`Title` 能恢复（`store.go:346-349`）；真正丢的是 `Workspace`/`Model`（恒空）与 `Messages`（恒 0） |

## 7. 加密：已决定**不做应用级加密**

- SQLite 本身无内置加密；透明加密依赖 SQLCipher 分支，而本项目使用的纯 Go 驱动
  `modernc.org/sqlite` **不含 SQLCipher**（模块内 `sqlcipher` 命中数 0），`PRAGMA key` 无效。
- 引入 SQLCipher 需要 cgo，破坏 `CGO_ENABLED=0` 与三平台可构建性 → **否决**。
- 结论：依赖操作系统全盘加密作为唯一加密手段；SDK 只做 §4-4 的文件权限收紧（0600），
  不引入密码、不做字段加密（字段加密会直接摧毁 `LIKE` 检索能力）。

## 8. 开放决策（未定，不写进实现）

1. 工作区分区：`store.Dir` 重启后 **`Meta.Workspace` / `Meta.Model` 恒空、`Meta.Messages` 恒 0**
   （`store.go:340-345` 的 `scan()` 与 `store.go:890-897` 的 `recoverIndex` 只填 ID/CreatedAt/
   UpdatedAt/Bytes；`Title` 不丢，由 `WithScanContents` 默认开启恢复，`store.go:346-349`），
   而 `sqlite` 自动建会话写死 `workspace_id=''` → 与"记忆按工作区分目录"口径不一致，
   "按工作区保留/统计"无法实现。
2. 保留策略的具体数值与触发时机（机制归 SDK，数值归宿主，默认永不删）。
3. 是否把 `session/store` 降级为 dev/test 后端（生产只认 sqlite）——它仍是唯一零依赖入门路径。
4. 事件写失败的语义（计入 turn error / 只记诊断）。

## 9. 验收门禁

每一步都必须同时满足：

1. `go test ./...`（根 module，含 `rigdoc` 守护）与子模块 `go test ./... -count=10` + `-race`；
2. 三平台 `GOOS=linux|darwin|windows CGO_ENABLED=0 go build ./...`；
3. `go run ./cmd/rigdoc -check`、`gofmt -l`、`staticcheck`；
4. 若改动了公共面：`CHANGELOG` 有迁移说明，`docs/` 与 `docs/zh/` 同步；
5. 若改动了 `session/sqlite` 依赖或公共方法：`sqlite-consumer-smoke`（干净下游 `go get`）必须绿。

## 10. 钉子测试登记

> 「钉子」= 改行为必红、必须同步改的断言（清单 A §13）。**本节是唯一登记处**：
> 每条都要能回答两件事——"它守的是**用户可见契约**还是**某个具体实现**"、
> "架构改错时它会不会红"。
>
> 处置只有三种：
>
> | 处置 | 判据 |
> |---|---|
> | **保留** | 守用户可见契约（工具面计数、零配置行为、状态目录落盘、seq 连续…） |
> | **改写为能力断言** | 守某个具体实现（"必须是 `*store.Dir`"、"必须恰好两次 Save"）→ 改成"必须是某个接口/能力" |
> | **降级为能力位测试** | 它鼓励架构上要消灭的行为（同一目录多实例、把 bug 当契约）→ 只记录不判定，写清谁在守正确的那条路 |
>
> 复核日期 2026-11-19；行号按复核后的文件。

### 10.1 八条钉子的处置

| # | 测试名 | 文件:行 | 守什么 | 处置 | 理由 |
|---|---|---|---|---|---|
| 1 | `TestAudit2CToolSurfaceComposition`（原 `TestAudit2CToolCountTable`） | `kit/toolfilter_internal_test.go:105`（`39` 在 `:108`） | 零配置**注册面**：39 个工具，且必须是这 39 个名字 | **保留**（升级为名字集合） | 工具面是发布口径（`AGENTS.md` 的 39/49、`docs/TOOLS.md`），改了就红是**功能**。原断言只有 `base != 39` 且那个 39 是**算出来的**（`personal − cfgUnavailableTools − …`）：换掉一个工具仍 39、kit 绕过体检表多注册一个仍 39。现在同时断言计数 + 组合律（`New().RegisteredTools()` 名字集 == 预设 − 体检 − 无快照的 `undo` − Grants 关掉的 2 项 + PTC 的 `run_code`） |
| 2 | `TestAudit2CToolSurfaceCapabilityDeltas` | `kit/toolfilter_internal_test.go:148` | 接上某项宿主能力后，**恰好**多出/摘掉那几个具名工具 | **改写为能力断言** | 原断言是 `count(...) != base+5` 一类算术：它守的是"5 这个数字"，把 5 个 `session_*` 之一换成别的名字仍然 +5；而模型看到的是名字。"接了能干什么"才是契约 |
| 3 | `TestWiringConflictingStoreConfigsAreRejected`（原 `TestWiringSessionStoreRejectsExplicitHistory`） | `kit/wiring_test.go:193` | 存储三字段里**任意两项**同时设置必须报错（不是静默忽略一个），文案要点名冲突字段；任意**单项**必须被接受 | **保留**（升级为全组合表） | 原用例只测 `History + SessionStoreDir` 一对，而 kit 是**两段 if 分支**管三对组合（`kit/kit.go:790-801`）：另两对哪天被写成"后者覆盖前者"，宿主会拿到一个能跑但写错后端的 agent。"静默忽略配置"是这里最贵的事故形态。正面对照（单项可用）保证检查没写宽 |
| 4 | `TestProductDefaultMeterAndSessionStore`（实例同一性段落） | `kit/audit_product_defaults_test.go:143`（能力断言在 `:170`） | 宿主能拿回一个**可列会话**的句柄，且它看到的就是 agent 写入的同一份会话 | **改写为能力断言** | 原断言 `a.SessionStore() != st` 钉的是"必须原样返回 `*store.Dir` 指针"——`SessionStore` 接口化、或返回同底层适配器就红，而用户可见行为没变（PLATFORM §5 步 2 正是这一步）。现在：①编译期 `var listable store.Store = a.SessionStore()`；②宿主的 `st.List` 与句柄的 `List` 必须看到同一会话。**放宽了什么**：不再要求指针相等 → 适配器/包装器被允许；**谁在守这条路**：`store.Store` 能力 + 两个句柄内容一致（行为可观测的"同一实例"），宿主持有同一后端这一事实由 `SessionStoreDir`/`SessionStore` 装配本身保证，`Storage` 化后由 `Storage()` 访问器承担 |
| 5 | `TestMemoryWikiDefaultsToWorkspaceFileWiki` / `IsolatedPerWorkspace` / `Injection` | `kit/memory_wiki_test.go:65` / `:103` / `:126`（严格断言在 `:141`） | 零配置**写盘**：落 `<stateDir>/rig/memory/<workspaceID>/pages/<name>.md`、跨实例可续用、不写进工作区、按工作区隔离；注入实现时**彻底不建文件 wiki** | **保留** | 这是零配置行为（PLATFORM §6 已更正过旧说法：默认**会**写盘）。注入那条同时是"将来 `Storage.Memory()` 注入会不会偷偷建文件 wiki"的取证用例，本轮把它从"`<state>/rig/memory` 下没有页面（且 `ReadDir` 出错也算通过）"收紧成"**整棵状态目录一个条目都没有**"，并补"内存实现确实收到了这次写入" |
| 6 | `TestAudit2HEventSequenceIsConsistent` / `TestAudit2HEventErrorIsRecordedAndRunEnded` / `TestAudit2HConcurrentEmitKeepsSeqContiguous` | `kit/audit_2h_events_test.go:59` / `:231` / `:357` | `seq` 从 1 起严格连续（UI 增量渲染与去重的前提），失败也要有 `error` + `run.end` | **保留**（+ 提为唯一判据函数 + 新增"一条 agent 一条流"） | `seq` 连续是用户可见契约，但原判据在四个用例里各写一遍、且只覆盖串行 Run。现在统一走 `auditAssertSeqContiguous`（`:345`），并新增 `TestAudit2HRunsShareOneContiguousStream`（`:279`）：并发 Run 必须共用**一条**流。这条钉的是架构面——写路径收编进存储后，"每会话/每 Run 一个 sink"会让两条流各自从 1 开始，UI 直接错乱 |
| 7 | `TestAgentWritesCheckpoints` / `TestAgentRecoversInterruptedToolCall` / `TestAgentCheckpointSaveFailureFailsClosed` | `agent/checkpoint_integration_test.go:54` / `:77` / `:113` | 检查点形状（`Messages` + `PriorMessages` + `Phase`）与恢复时的合成消息；`Save` 失败 fail-closed | **改写为能力断言**（本轮**未应用**：文件在 `agent/`，不在本轮范围） | `len(phases) != 2` 把"恰好两次 Save"当契约——多一次（比如新增 `tool_result` 阶段的检查点）就红，而恢复契约只要求"首个 `Requested`、末个 `Completed`、每次 `Save` 的 `Messages` 非空、`PriorMessages` 单调"。补丁见 §10.5 |
| 8 | `TestCatalogShape` / `TestGeneratedFilesAreCurrent` / `TestDocumentationCounts` | `internal/rigdoc/rigdoc_test.go:19` / `:72` / `:91` | `docs/ai/tools.json`、`tools.md` 与 `docs/TOOLS.md`/`COVERAGE.md`/`zh/COVERAGE.md`/`CHANGELOG.md` 的计数与工具面一致（`rigdoc -check` 的内容层） | **保留**（升级为名字集合） | 生成物过期是"红在别人机器上"的头号原因，且数字类文档必须与工具面同批改（清单 A §11）。原 `TestCatalogShape` 只比**个数**：目录里少一个真工具、多一个幻觉工具，计数一抵消就绿——而 `docs/ai` 是喂给模型的，多一个不存在的工具就是教模型调用不存在的东西。现在按名字逐个比对 `full.All + run_code` |

### 10.2 复核中新增/改判的护栏

| 测试名 | 文件:行 | 守什么 | 为什么加 |
|---|---|---|---|
| `TestWiringTwoAgentsSameStoreDir`（降级） | `kit/wiring_test.go:911` | 同一 `SessionStoreDir` 上的多 agent：目标形态（都能写）是否达成；会话存在后创建的 agent 必须能写；未归类的失败形态仍 fatal | 原实现把 `session not found` 当 `t.Fatalf` 的**期望值**——等于要求这个 bug 继续存在，而"同一目录多实例"正是 PLATFORM §5 步 2/步 3 要消灭的用法。**放宽了什么**：不再断言缺陷存在/不存在二选一；**谁在守这条路**：对照组（会话已存在时创建的 agent 必须能写）仍硬断言，未知失败形态仍 fatal；正确形态由**会话租约**守（本条目的"待建立"） |
| `auditAssertSeqContiguous`（判据唯一化） | `kit/audit_2h_events_test.go:345` | `seq` 连续这一条不变量的**唯一定义** | 原来四个用例各写一遍循环；入库通道落地时要对同一形状再断言一遍，届时复用同一函数，避免两套判据漂移 |
| `auditInternalDelta` / `auditInternalRegistered` / `auditInternalExpectedNames`（新 helper） | `kit/toolfilter_internal_test.go:190` / `:222` / `:248` | 走**真实** `New()` 的注册面，与组合律比对；名字级增减 | 计数断言测不到"kit 自己多注册/少注册了什么"；只有真的建一个 agent 才测得到 |

### 10.3 「架构改错」取证：新断言真的会红（旧断言不会）

做法：把 HEAD 导出到干净树，注入一条**真实事故形态**的突变，再分别用改造前/后的断言跑同一用例。

| 突变（架构改错） | 旧断言 | 新断言 |
|---|---|---|
| 体检表漏摘 `web_search`（`Searcher` 为 nil 时它必然失败）+ 顺手多摘 `read_image`：注册**仍 39 个**，名字变了 | 🟢 绿（计数不变） | 🔴 红：`注册面与组合律不一致：多出 [web_search]，少了 [read_image]` |
| 只保留 `History + SessionStoreDir` 那条互斥检查（回到加 `SessionStore` 之前） | 🟢 绿 | 🔴 红：`kit.New 接受了互相冲突的存储配置（History + SessionStore）` |
| `SessionStore()` 接口化 + 返回**同底层适配器**（PLATFORM §5 步 2 的合法改造） | 🔴 红：`SessionStore() 没有返回宿主传入的实例` | 🟢 绿（能力 + 行为同一） |
| 注入记忆实现时仍往状态目录写一个标记文件（落在 `<state>/rig/`，不是 `pages/`） | 🟢 绿（只看了 `<state>/rig/memory`，且把 `ReadDir` 出错当通过） | 🔴 红：`kit 仍然在状态目录里建了东西…: [rig]` |
| 每个 Run 换一个新事件汇（两条流各自从 `seq=1` 开始） | 🟢 绿（当时没有这条用例） | 🔴 红：`并发 Run 共用一条事件流：第 1 条事件 seq = 1, want 2` |
| `BuildCatalog` 把最后一个真工具换成不存在的 `*_phantom`（目录总数不变） | 🟢 绿（只数个数） | 🔴 红：`少了 [request_sandbox_access]，多了 [request_sandbox_access_phantom]` |

### 10.4 已失效的钉子（断了却不报错）

| 位置 | 现象 | 影响 / 建议 |
|---|---|---|
| `kit/toolfilter_internal_test.go:149`（原） | **半失效**：`base != 39` 的 39 是 test 侧复刻的算术（预设 − 体检 − undo − grants），不是 `New()` 的返回值。kit 绕过体检表注册/漏注册时它仍绿 | 已由 §10.1-1 的组合律修复（`auditInternalRegistered` 走真实 `New()`） |
| `docs/TOOLS.md:18` | **过期且无人守**：写「`kit.New(kit.Config{})` **registers** … **37**」，实测 **39**（`SandboxGrants` 默认开后含 `sandbox_status`/`request_sandbox_access`）。`TestDocumentationCounts` 只校验同表里的 "Static tools built by **81**" 那一行 | 文档撒谎，且改 `docs/TOOLS.md` 不会红。建议把 `TestDocumentationCounts` 扩到这一行（本轮未改：`docs/TOOLS.md` 不在本轮范围，且该数字与 `AGENTS.md` 口径要同批统一） |
| `docs/COVERAGE.md`（`go test ./tools/audit` 示例输出那行） | **过期且无人守**：写 `(static tools=80)`，实测 `(static tools=81)` | 同上，属数字类文档同步（清单 A §11/D12） |
| `kit/wiring_test.go:926`（原 `TestWiringTwoAgentsSameStoreDir`） | **已失效**：底层修复落地后走 `t.Skip`，用例从此永远"绿"，不会提示"目标形态是否真的达成" | 已改为通过 + `Logf`（§10.2）；真正的护栏（会话租约）待建 |

### 10.5 待建立 / 本轮不做

| 项 | 现状 | 下一步 |
|---|---|---|
| 检查点形状（钉子 7） | 仍是 `len(phases) != 2`，在 `agent/`，超出本轮测试范围 | 改成：首个 `Requested`、末个 `Completed`、每个 `Save` 的 `Messages` 非空且 `PriorMessages` 单调不减、完成检查点的最后一条消息 == 模型输出。去掉"恰好 2 次" |
| 入库通道的 `seq` | 目前只对 `Config.EventWriter`（NDJSON 导出通道）断言 | 写路径收编进存储（PLATFORM §5 步 4）后，对"入库后读回"的事件序列复用 `auditAssertSeqContiguous` 再断言一遍——**换写入端却沿用旧断言，等于测的和跑的不是一条路** |
| 事件写失败算不算 turn error（§8.4） | `EventError()` 只覆盖导出通道 | 语义定下来之后，同一处补"入库失败"的判据（`TestAudit2HEventErrorIsRecordedAndRunEnded` 是它的形状模板） |
| 会话租约（§5 步 2/3） | 无守护：多 agent 同库今天能跑通，但"两个进程同时写同一会话"没有租约挡 | 租约落地时给 `TestWiringTwoAgentsSameStoreDir` 补一条"第二个实例被明确拒绝，而不是静默写坏" |
| 能力位测试（`Capabilities()`） | `storage` 侧刚起步（`Capabilities` 已定义） | 契约基线用例应放进 `storagetest`（清单 A §14-③-6），两实现共用；本节只登记，不重复实现 |
