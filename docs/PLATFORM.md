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

实测：把该集合指向 `*sqlite.Store`，`go vet` 通过。**三处已知缺口**：

| 缺口 | 现状 | 修法 |
|---|---|---|
| `SessionLog()` 返回类型 | `sqlite.SessionLog() SessionLog`（自有接口，含 `AppendEvent`）≠ `rig.SessionLog` | 方法改返回 `rig.SessionLog`，多出的写方法留在具体类型上 |
| 事件写入 | 只有 `kit.Config.EventWriter io.Writer` → NDJSON（`events/emitter.go`） | 改 kit 侧 eventBridge，让写入也进存储；并定义"写失败算不算 turn error" |
| 追加签名 | 两实现都是单条 `Append(ctx,id,rig.Message)` | 契约里一律用已有的 `AppendBatch(ctx,id, ...rig.Message)`；**不要**定义变参 `Append` |

**不要做的事**（都被实测否决）：

- ❌ 在契约里写 `Sessions() SessionState` 并引用 `store.Meta`/`store.Filter` → `storage` 与
  `session/store` 互相 import，**成环**；
- ❌ 断言 `var _ storage.Storage = (*store.Dir)(nil)` → `store.Dir` 没有 events/checkpoints/spill，
  实测 `missing method Checkpoints`。要支持目录实现就用适配器
  `storage.NewDirStorage(*store.Dir)`，断言写成 `var _ storage.Storage = storage.DirStorage{}`；
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
| **0** | 建 `storagetest`（stdlib-only 工厂 + `Capabilities()` + 两实现基线，**只加测试**）；清掉 `session/sqlite` 的 `replace`、补 `session/sqlite/vX` tag、让"漏打 tag"CI 红 | 现在"多实现分叉即红"是**假的**：根 module 不能 import sqlite，根 CI 的 `go test ./...` 跑不到子模块 |
| **1** | 落地 §3 的**只读**契约 `storage.Storage` | 已验证可实现，风险最低 |
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