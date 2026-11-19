# 平台化规划：SDK 承担全部底层，应用只写业务逻辑

本文是施工单，不是愿景。每一条都带验收标准；没写验收标准的条目不得进入发布。

## 0. 边界（先把"不做什么"钉死）

| 层 | 归谁 | 内容 |
|---|---|---|
| **业务逻辑** | 宿主应用 | 对话策略、提示词、工具编排、UI、项目/任务等业务实体 |
| **运行时与状态** | 本 SDK | 会话与消息、事件流、检查点与崩溃恢复、记忆、溢出结果、花费记账、检索、配额与回收、迁移与导出、进程安全关停 |
| **明确不做** | 都不做 | 账号与鉴权、云端同步、密钥托管、加密容器。SDK 只提供导出/导入钩子让宿主自己接 |

判据一句话：**"这份数据如果丢了，agent 这个回合还能不能正确继续？"**
不能 → 归 SDK。能（只是体验变差）→ 归宿主。

## 1. 目标形态

宿主最终应当长这样——"存储"这个词从它的代码里消失：

```go
st, err := sqlite.Open(sqlite.Options{Path: statePath}) // 或 rig.NewMemory()：零持久化
if err != nil { ... }
defer st.Close()

app := kit.New(kit.Config{
	Provider: p,
	Storage:  st, // 一个字段 = 全部状态
	Asker:    ui,
})
```

**验收口径**：宿主的 `main` 里不出现路径拼接、不出现 schema、不出现关停顺序。

## 2. 分层（决定依赖面不被破坏）

| 层 | 包 | 依赖 | 作用 |
|---|---|---|---|
| 0 | 根 module：`rig` 契约 | stdlib only | `Storage`、`Session`、`SessionLog`、`CheckpointStore`、`SpillStore`、`memory.Wiki`、`CostSink` |
| 1 | 根 module：默认实现 | stdlib only | 内存态（测试/短会话）+ 目录态（`session/store`，降级为"零依赖可选实现"） |
| 2 | 子模块 `session/sqlite` | `modernc.org/sqlite` | 参考实现：一个库装全部 |
| 3 | 宿主应用 | 任意 | 只写业务 |

**铁律**：根 module 永不 import 第 2 层。宿主想要数据库就 `go get` 那一个子模块——
这是"零依赖"与"开箱即用"唯一能共存的方式。

## 3. 施工步骤（每步可独立合并、独立发版）

| # | 改动 | 为什么必须先做 | 验收标准 |
|---|---|---|---|
| **S1** | `kit.Config.SessionStore *store.Dir` → `store.Store`（接口） | 注入点现在是**具体类型**，任何别的实现都装不上去 | 用内存假实现跑通 kit 全流程；`TestKitAcceptsAnyStore` |
| **S2** | 新增**独立包** `storage`，里面放 `storage.Storage`（见 §3.1） | 现在宿主要自己拼 5 个字段并保证指向同一处，拼错就"事件在 A、会话在 B" | 契约只有一份定义；`session.Dir` 与 `sqlite.Store` 编译期满足（`var _ storage.Storage = …`） |
| **S3** | `kit.Config.Storage rig.Storage` 取代多个入口（旧字段 deprecated 保留一版） | 宿主从"5 个字段"降到"1 个字段" | 宿主示例行数对比表；`TestKitSingleStorageField` |
| **S4** | `session/sqlite` 满足 `Storage`；`sqlite.Open(Options{Path})` 一行可用 | 让"一库装全部"在**类型上**成立，而不只是实现里成立 | `var _ rig.Storage = (*sqlite.Store)(nil)`；跨会话检索与花费汇总走契约 |
| **S5** | 恢复、配额、回收内建进 `Storage` 语义 | 这些"丢了就继续不下去"，现在一半靠宿主记得调（`MaxFiles`、`GC`） | `Storage.Open` 自动完成 WAL checkpoint、孤儿溢出回收、检查点续跑；用例覆盖"进程被 kill 后重启" |
| **S6** | 检索/统计 API 上移为契约（跨会话搜索、事件按类型查、花费汇总、时间窗） | 现在只有 SQLite 子模块有查询；目录实现只写不查 | 所有实现跑**同一套 conformance 测试** |
| **S7** | 迁移与出口：`ImportDir`（旧→新）+ `ExportDir`/`ExportNDJSON`（新→文件） | 进得去出不来 = 用户被锁死；0.x 改 schema 时这是唯一安全网 | 往返测试：导出→导入→逐字节等价 |
| **S8** | 文档与最小示例 `examples/minimal-host`（≤40 行） | "SDK 把底层做好"必须能被 40 行证明 | 示例可 `go run`；存储章节只讲"选内存/选库"，不讲 schema |
| **S9** | 宿主迁移清单核对（见 §5） | 减负要有可核对清单，否则只是口号 | 每项对应一次删除 + 一个不红的功能 |
| **S10** | 发版：**v0.24.0**（S1–S4）、**v0.25.0**（S5–S7） | 每步都改公共面，合成大版本无法回滚 | 每版四 job 绿（含真机 Windows/macOS）+ 子模块 tag |

### 3.1 `storage.Storage` 的形状（已按现有签名对齐）

**不能放在根 `rig` 包**：`internal/rigdoc` 的 `TestRootPackageStaysStdlibOnly` 禁止根包 import
本仓库任何子包（"根包必须是 import 图的叶子"），而聚合契约需要 `memory.Wiki`。故新开
`storage` 包。

**不能用嵌入（embedding）**：现有的两套契约有**同名不同签名**的方法，Go 不允许嵌入冲突。

| 现有契约 | 方法 | 与谁冲突 |
|---|---|---|
| `store.Store` | `Load(ctx, id) (Meta, []Message, error)` | 与 `rig.History.Load(ctx, id) ([]Message, error)` **同名冲突** |
| `rig.History` | `Load` / `Append` | 同上 |
| `rig.SessionLog` | `Events` / `Search` / `Last` | 只读，**没有写入方法**（写入靠 `kit.Config.EventWriter`） |
| `rig.CheckpointStore` | `Load` / `Save` / `Clear` | `Load` 再次与上面冲突 |
| `rig.SpillStore` | `Save` / `Load` / `GC` | `Load`、`Save` 又与上面冲突 |
| `memory.Wiki` | `Index` / `Search` / `Read` / `Write` / `Append` / `Delete` | `Search`、`Append`、`Delete` 冲突 |

结论：聚合契约必须用**命名访问器**，每个访问器返回一个小接口：

```go
package storage

// Storage 是一个库装下"一份会话状态"的全部能力。
// 用访问器而不是嵌入：现有契约之间存在同名不同签名的方法（Load/Save/Search/Append/Delete）。
type Storage interface {
	// Sessions 是一个事务域：会话、消息、事件、检查点必须落在同一个事务边界内。
	Sessions() SessionState
	// Memory 是长期记忆（渐进式 wiki）。
	Memory() memory.Wiki
	// Spills 是超大工具结果的存储。
	Spills() rig.SpillStore
	// Costs 是花费记账。
	Costs() CostSink
	// Close 关闭全部后端；可重复调用，之后的访问器调用返回 ErrClosed。
	Close() error
}

// SessionState 收拢"某个会话的状态"，避免 Load/Save/Append 三套签名散落各处。
type SessionState interface {
	Create(ctx context.Context, meta store.Meta) (store.Meta, error)
	Append(ctx context.Context, id string, msgs ...Message) (store.Meta, error)
	Load(ctx context.Context, id string) (store.Meta, []Message, error)
	List(ctx context.Context, filter store.Filter) ([]store.Meta, error)
	Delete(ctx context.Context, id string) error

	Events(ctx context.Context, id string, from, to int64) ([]SessionEvent, error)
	AppendEvents(ctx context.Context, id string, events ...SessionEvent) error
	SearchEvents(ctx context.Context, query string, limit int) ([]SessionEvent, error)

	Checkpoint(ctx context.Context, id string) (*Checkpoint, error)
	SaveCheckpoint(ctx context.Context, id string, cp *Checkpoint) error
	ClearCheckpoint(ctx context.Context, id string) error
}
```

**对齐要点**（S2 落地时必须做）：
- `sqlite.Store` 现有方法名 `Memory()` / `Spill()` / `SaveCost()` 与契约不一致 →
  要么在 `sqlite` 上加 `Spills()` / `Costs()` 转发，要么把契约统一成现有名字；
- `rig.SessionLog` 是**只读**的，而 `kit` 现在通过 `EventWriter io.Writer` 写事件 ——
  这是**非对称**设计，S2 要给 `SessionState` 补 `AppendEvents`，让读写走同一个端口；
- `store.Store.Close` 与 `Storage.Close` 语义重复：把 `Close` 只留在 `Storage` 上，
  `SessionState` 不提供 `Close`。

## 4. 三条必须同时立的规矩

1. **schema 不是承诺**：`session/sqlite` 在 0.x 内明确"schema 不保证向后兼容；跨大版本请走
   Export→Import"。写进 README，不藏。
2. **默认不持久化**：`kit.New(Config{})` 用内存态；要盘必须显式注入。这样"零依赖"与
   "零存储假设"同时成立，也顺手消掉"默认往某目录写 NDJSON"这个隐式行为。
3. **一个实现必须过同一套 conformance**：`Storage` 的所有实现跑同一张用例表
   （会话幂等、并发写、崩溃续跑、检索字面量、GC 边界、关停语义）。此前 `Append` 丢写、
   写入假账、排序分叉，全部源于"没有跨实现一致性测试"。

## 5. 宿主减负清单（可核对）

| 宿主里的东西 | 处置 |
|---|---|
| 会话/消息落盘 + 索引 | 删 → 注入 `Storage` |
| 事件写盘 | 删 → `Storage` 内建 |
| 花费记账持久化 | 删 → `Storage` 内建 |
| 检查点与崩溃恢复 | 删 → `Storage` 内建 |
| 记忆页读写/索引/检索 | 删 → `Storage.Memory()` |
| 溢出结果文件管理 | 删 → `Storage.Spills()` + `spill_read` |
| 跨会话搜索/统计 | 删 → `Storage` 查询契约 |
| 用户设置、凭据、账号 | **留**（不属于 SDK） |
| 业务实体（项目/任务/文件） | **留**（可存进同一个库，但 schema 是 app 的） |
| 多设备同步/加密容器 | **留**（SDK 只给 Export/Import 钩子） |
| UI 视图索引 | **留**（建在 SDK 库上，决定权在 app） |

## 6. 风险

| 风险 | 处置 |
|---|---|
| SDK 变得过大（什么都管 = 什么都不精） | 用"丢了能不能继续"卡新增项；每加一项必须有 conformance 用例 |
| schema 承诺失控 | 0.x 明确不承诺 + Export/Import 兜底 + 子模块独立 tag |
| 多实现分叉 | S6 的 conformance 测试作为硬门禁，分叉即红 |
| S2/S3 破坏性改动 | 0.x 可接受；旧字段保留一版 deprecated，CHANGELOG 给迁移示例 |
| 真机 macOS runner 不稳 | 已拆成独立 workflow；S1–S10 的验收不依赖它成为阻塞门禁 |

## 7. 执行顺序

1. **S1 + S2 + S4**（地基，约一天）：修掉注入点类型错误、建立聚合契约、让参考实现编译期满足
   契约。做完后即可用一个最小示例验证"宿主只剩业务逻辑"。
2. **S3 + S5 + S6**（收敛使用面 + 底层自动化 + 跨实现一致性）。
3. **S7–S10**（出口、示例、宿主删除、发版）。

## 8. 加密存储（讨论中，未定稿）

**事实**（已核实）：SQLite 本身**没有**内置加密；透明加密依赖 SQLCipher 这类分支。
本仓库使用的纯 Go 驱动 `modernc.org/sqlite` **不含 SQLCipher**（模块内 `sqlcipher` 命中数为 0），
因此 `PRAGMA key` 无效、不存在"给个密码就能加密"的路径。

可选方案见 §8.1，选定后再补本节结论。

### 8.1 候选方案

| 方案 | 纯 Go | 代价 | 备注 |
|---|---|---|---|
| **A. 依赖操作系统全盘加密**（FileVault / BitLocker / LUKS） | 是 | 无 | 桌面应用的默认答案；SDK 侧零改动 |
| **B. 字段级加密**（AES-GCM，stdlib 即可） | 是 | **按密文无法 `LIKE`/排序/建索引** → S6 的搜索能力必须改成"明文字段 + 加密原文字段"双写，或对敏感字段放弃模糊搜索 | 只加密真正敏感的列（如消息正文）需要产品决策 |
| **C. 整文件加密 + 关库时重加密**（打开时解密到临时文件） | 是 | 崩溃会留下明文临时文件；大库开合慢；WAL 与临时文件必须一起清理 | 安全收益与实现复杂度不成比例 |
| **D. 换成 SQLCipher（cgo）** | **否** | 破坏 `CGO_ENABLED=0`；三平台各自预编译库；签名与公证成本 | 与仓库"纯 Go、无 cgo"的硬约束冲突 |
| **E. 明文字段 + 可搜索加密（blind index，HMAC）** | 是 | 只支持**精确匹配**，模糊搜索不可行 | 可与 B 组合：正文加密 + 关键词 blind index |

### 8.2 需要产品决策的三点

1. 加密目标是"防设备丢失"还是"防同机其他进程"？前者用 A 即可；后者必须 B/E。
2. 接受"加密字段不能模糊搜索"吗？不接受 → 只能选 A。
3. 密码是"启动解锁"还是"可取消的额外一层"？前者影响无人值守运行与崩溃恢复。