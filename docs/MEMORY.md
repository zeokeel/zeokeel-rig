# 记忆与上下文

本文说明 Rig 的记忆/上下文子系统：模型怎样浏览、检索、写入和巩固跨会话的知识，
宿主怎样换后端，以及它和旧 `memory.Wiki` 的关系。读者是想给 agent 接长期记忆的
工程师，或要给记忆写新后端/新抽取器的宿主开发者。

设计目标来自 OpenViking 的"上下文数据库"：把记忆、资源、技能统一到一棵
`mem://` 虚拟文件系统里，每个条目带 L0 摘要 / L1 概览 / L2 正文，检索先扫摘要再
按需读正文。实现是纯 Go、stdlib-only、零新增依赖，所有模型能力（embedding、
rerank、摘要、抽取、意图分析）都是**可选注入**；默认不联网、不调模型。

## 1. 两层关系：v2 是默认，旧 Wiki 是兼容面

| 层 | 契约 | 实现 | 状态 |
| --- | --- | --- | --- |
| v2 上下文数据库 | `memoryvfs.Store`（`FS`/`Retriever`/`Memory`/`Sessions`/`Rules`/`Versions`/`Maintenance`/`Packer`） | `internal/memvfs` 文件后端 + `internal/memsearch` 索引 + `internal/memextract` 抽取 | **默认** |
| 旧 wiki | `memory.Wiki`（Index/Read/Write/Append/Delete/Search） | `internal/memwiki`；v2 上用 `memory.WikiFromStoreV2` 适配 | 兼容保留 |

`kit.New` 的默认路径是 v2：`<stateDir>/memory/<workspaceID>/v2`（目录 0700、条目
0600）。打开失败会回落到旧 wiki，原因由 `Agent.MemoryStoreError()` 报出；显式注入
`Config.Memory`（旧 Wiki）时则不建 v2。两者都设时 `Config.MemoryStore` 优先。

首次打开 v2 时，如果旧 wiki 目录里有 `pages/*.md`，`tools/memory` 会把它们一次性
迁到 `mem://user/self/memories/pages/<name>`（`Type=pages`，标签
`migrated=wiki`）。迁移结束写 `<legacyDir>/.v2-migrated`（个别页失败也写，避免
每次打开重扫、已删旧页复活），之后不再扫描旧页。失败页只记日志；要重试请删除
标记后重启，或显式调用 `memory.MigrateWiki(store, dir)`。旧目录原样保留，可回滚。

## 2. 数据模型

### 2.1 URI 与类别

```
mem://user/self/memories/<type>/<slug>      记忆（九类）
mem://user/self/memories/pages/<slug>       旧 wiki 页迁移落点
mem://user/self/peers/<peer>/memories/...   按 peer（项目/工作区）分区
mem://user/self/rules/<slug>                编译出的行为规则
mem://resources/<path>                      导入的文本资源
mem://skills/<name>                         技能文档
mem://sessions/<segment>/<last-at>-<hash>  会话归档（segment=清洗后的会话 ID）
```

`Kind` = `memory` / `resource` / `skill` / `rule` / `session` / `dir`。URI 只允许
小写字母、数字、`.`、`_`、`-`，禁止空段与 `..`，最长 512 字节；解析失败返回
`memoryvfs.ErrInvalidURI`。

### 2.2 九类记忆

`profile`、`preferences`、`entities`、`events`、`cases`、`trajectories`、
`experiences`、`tools`、`skills`。写入时 `Type` 必须是合法 slug；默认抽取器只会
自动产出前七类，`tools`/`skills` 由宿主或模型显式写。

### 2.3 三层内容与目录 sidecar

| 层 | 字段 | 上限 | 用途 |
| --- | --- | --- | --- |
| L0 | `Summary` | 512 字节 | 索引、召回、列表 |
| L1 | `Overview` | 16 KiB | 导航、rerank、注入 |
| L2 | `Content` | 1 MiB | 按需读取 |

条目还带 `Tags`、`Links`、`Source`、`Created/Updated`、`Version`、`Pinned`、
`ExpiresAt`、`LastAccess`。目录本身也有 L0/L1 sidecar 与 `Freshness`（直接子项数、
采样数、待刷新变更数；超过 32 个直接子项时用稳定采样）。

## 3. 存储布局与可靠性

```
<stateDir>/memory/<workspaceID>/v2/
  entries/<uri path>.json     一条一文件（0600）
  dirs/<uri path>.json        目录 sidecar
  sessions/<id>/pending.jsonl             待提交消息（<id> 是原始会话 ID 的目录）
  entries/sessions/<segment>/<last-at>-<hash>.json  会话归档
  snapshots/<id>.json                     快照清单
  trash/<id>.json                         删除前快照
  meta.json                               version + checkpoints
  index/vectors.json          可选向量（启用 Embedder 时）
```

- 每条写入是"同目录临时文件 → chmod 0600 → fsync 文件 → rename → fsync 目录"；
  进程崩溃或掉电不会留下半条。
- 加载时损坏文件被隔离（`LoadIssues()` 可查），不会让整个存储打不开。
- 单进程内 `RWMutex` 串行化写入；**不提供跨进程锁**（单用户单进程是产品边界）。
  多客户端共享同一目录时，最后一次写赢。
- 配额默认 20000 条 / 256 MiB；超限返回 `ErrQuotaExceeded`。会话归档与
  Working Memory 是系统条目，默认既不计配额、也不参与 GC（`GCOptions.IncludeSessions`
  显式打开才清理）；否则配额满会让 Commit 永久失败、pending 永远排不空。`GC` 先按 TTL 过期，
  再按衰减分数淘汰（半衰期默认 90 天），`Pinned` 永不淘汰；删除前写 trash 快照。
  默认装配**不会自动跑 GC**：宿主应定期调用 `Maintenance.GC`（会话归档默认不被
  GC 淘汰，`IncludeSessions=true` 才清理）。
- 版本：每次 Write/Remember/Delete 递增 `Version`；`Snapshot` 复制清单，
  `Diff` 走统一 diff，`Restore` 原子替换；会重建索引，并在启用 `Embedder` 时自动重算向量
  （重算失败返回错误，但状态已恢复）。

## 4. 写入与巩固

### 4.1 `write` 与 `remember`

- `write` 是覆盖写（不存在则创建），要求 L0 摘要；`append` 只追加正文，页必须
  已存在。
- `remember` 是面向记忆的高层写入：校验类型 → 在同一 scope/type 下找最相似的
  5 条 → `memextract.Merge` 分档：
  - 相似度 ≥ 0.90：视为重复，不写新条目（返回已有 URI）；
  - 0.65–0.90：合并（追加正文、保留更长摘要、合并 Tags/Links）；
  - 其余：新建。
- 合并/去重用的是 token Jaccard，不调用模型；想更聪明就注入自己的
  `memoryvfs.Extractor` 或直接 `write`。

### 4.2 会话捕获与提交

- `Sessions.Capture` 把 user/assistant/tool 消息幂等追加到
  `sessions/<id>/pending.jsonl`。
- `Sessions.Commit`：归档 pending（URI `mem://sessions/<segment>/<last-at>-<batch-hash>`；
  同一批重试覆盖同一归档）→ 调 `Extractor`（默认 `internal/memextract`，只抽用户
  明确表达的句子）→ 候选逐条 `Remember` → 每次成功 Commit 都写 checkpoint，
  `WorkingMemory=true` 时额外写 overview → 成功后清空 pending。任一步失败保留
  pending，下次重放。
- 每个会话第一次 Run 会重放上次进程崩溃留下的 pending（`MemoryCapture` 开时），
  与官方 "session start replay" 对齐。
- 工具 `commit` 不传 `session_id` 时使用当前工具调用所属的会话（`agent.ToolSessionID`）；
  不在工具调用内才退回 `default`。
- kit 默认在每次 `Run` 结束后捕获；pending token 超过
  `Config.MemoryCommitTokens`（默认 4000）或上下文发生压缩时自动 commit；
  `Shutdown`/`Runtime.Close` 会冲刷所有捕获过的会话。
- 默认抽取器是保守的启发式（中英文线索 + 句子长度 + 代码块/URL 过滤），
  宁可漏记也不把整段对话灌进记忆；要模型级抽取就注入 `Extractor`。

## 5. 检索

```
Query → 意图计划（thinking/context）→ 过滤 → BM25(+向量) → 融合 → 可选 rerank
      → 阈值 → 分页 → token 预算装层 → 结果
```

- `find`：单查询；`search`：`Mode=thinking/context` 时允许 `IntentAnalyzer`
  （nil 时用"查询 + 最近会话关键词"扩展，最多 3 条）。
- 过滤：`Scope`（URI 前缀）、`Kinds`、`Types`、`Tags`、`Levels`（命中层级）。
- BM25：倒排索引，字段加权 title 3 / L0 2 / L1 1.5 / L2 1，`k1=1.2,b=0.75`；
  空查询按 `Updated` 降序。
- 向量：注入 `Embedder` 后，查询与条目向量做余弦；没注入时**关闭向量通道**，
  只走 BM25。确定性 hash 向量只用于测试/管线演示（`Query.UseHashVector` 显式开启），
  不会自动混进真实向量空间。
- 融合：默认 RRF（k=60），也可用加权归一化；`Reranker` 对前 2×limit 精排。
- 阈值：`Threshold` 作用在**绝对压缩分** `score/(1+score)` 上（0 表示不过滤），
  不随本次最大值变化；因此 `0.99` 可以表达"没有足够相关的命中"。rerank 的
  0..1 分会映射到 0..0.5，调用方按同一口径设阈值。
- 分页：`Offset`/`Limit`；`TokenBudget` 按 L0→L1→L2 逐层装入，超预算降级/丢弃，
  结果里的 `Tokens` 是估算值。
- `IncludeBody=false`（默认）时结果只回元数据；正文要另发 `read`。

## 6. 规则、版本、导入导出

- `CompileRules` 把指令/来源拆成规则条目写到 `mem://user/self/rules`；
  `SearchRules` 用 token 覆盖率 + 强约束词加成打分，`threshold` 过滤。
- `Snapshot`/`Snapshots`/`Diff`/`Restore` 提供显式版本；工具动作
  `snapshot`/`history`/`restore`。
- `Export`/`Import` 是宿主 API（`memoryvfs.Packer`），JSON bundle；
  模型工具只暴露 `stats`，不直接导出整库。

## 7. 模型看到的 `memory` 工具

一个常驻工具，20 个动作：

| 动作 | 作用 |
| --- | --- |
| `index` / `ls` / `tree` | 列目录（`index` 列整个 `mem://user/self/memories` 子树） |
| `read` | 按 L0/L1/L2 读一条 |
| `write` / `append` / `delete` | 覆盖写 / 追加 / 删除 |
| `search` / `find` | 旧版查找（BM25，固定 memories 范围）/ 范围+类型+标签+层级+token 预算 |
| `remember` / `forget` | 合并式写入 / 删除 |
| `tag` | `k=v`、`k+=1`、`k-=1` |
| `commit` | 归档会话并抽取（`mode=working/full` 带工作记忆） |
| `rules` | 编译（`text`）或检索（`query` + `mode=search`） |
| `stats` | 条数/字节/token/分类型 |
| `mv` / `mkdir` | 移动 / 建目录 |
| `snapshot` / `history` / `restore` | 版本 |

整套工具声明（名称+描述+Schema）2385 字节 ≈ 595 tokens（常驻成本）；描述只写"一句话 + 动作列表"，细节
交给运行时错误。`read` 单次 8 KiB、响应 32 KiB；正文超限会被截断并在结果里注明，
需要完整长文时按主题拆页（`write` 覆盖或 `append` 追加）。

## 8. kit 默认接线

- `Config.MemoryStore`：注入自己的 `memoryvfs.Store`；nil 时按工作区自动开文件后端。
- `Config.MemoryOptions`：`WithEmbedder`/`WithReranker`/`WithSummarizer`/
  `WithExtractor`/`WithIntentAnalyzer`/`WithTTL`/`WithMaxEntries`/…；默认全确定性。
  注入的 `Summarizer`/`Embedder` 可以回读同一个 Store：Write/Commit/CompileRules/
  Import/Remember/Append 都在锁外调用它们。
- `Config.MemoryCapture`（三态，nil=开）与 `Config.MemoryCommitTokens`（默认 4000）。
- `Config.MemoryCaptureToolResults`（三态，nil=开）控制 tool 结果是否进 pending；
  `Config.MemorySkillCatalog`（三态，nil=开）控制首轮是否注入 `<available-skills>`。
- `Config.MemoryRecall`（三态，nil=开）与 `Config.MemoryRecallTokens`（默认 1200）：
  每个会话第一次 Run 额外注入 profile/preferences 索引；之后每次 Run 用最后一条
  用户输入召回相关记忆，作为一条 system 消息注入本轮；失败/无命中静默跳过。默认
  零配置没有 Embedder，召回走 BM25；要语义召回需注入 Embedder。
  捕获按批次 nonce 区分（重复的 "continue" 不会被合并）；单条正文 >256 KiB 截断；空
  `SessionID` 不捕获。
- `Agent.MemoryStore()` / `Agent.MemoryStoreError()`；
  `Agent.MemoryWiki()` 仍返回旧 Wiki（v2 上自动用适配器）。兼容视图按 slug
  投影：不同 memory type 下同名 slug 只保证一条可见（以树序第一条为准），
  新代码应直接用 `MemoryStore` 按 URI 操作。
- `Runtime` 按工作区共享一份 store；`Runtime.Close` 先让各 Agent 冲刷 pending，
  再关自己打开的 store。宿主注入的 store 归宿主关闭。

## 9. 实测：相对旧 wiki 的检索收益

`internal/membench` 是可复现基准（`go test ./internal/membench -run
TestBenchmarkReport -v`，seed=42）：60 会话 × 30 轮，780 条条目，600 条查询。
easy 集查询带唯一实体名；hard 集只给 2–3 个描述性属性、没有实体名，并为每条
gold 配一条"只有实体不同"的近重复干扰句。

### 9.1 easy 集（结构化字段 + BM25 排序）

| arm | recall@5 | MRR | avg tokens | p95 (ms) |
| --- | --- | --- | --- | --- |
| 旧 wiki 子串 | 0.250 | 0.250 | 7.6 | 0.77 |
| BM25 | **1.000** | **1.000** | 155.6 | 1.37 |
| 混合（hash+RRF） | 0.973 | 0.903 | 155.0 | 5.32 |

### 9.2 hard 集（纯词法上限）

| arm | recall@5 | MRR | avg tokens | p95 (ms) |
| --- | --- | --- | --- | --- |
| 旧 wiki 子串 | 0.275 | 0.114 | 36.4 | 0.69 |
| BM25 | **0.783** | **0.339** | 151.3 | 1.76 |
| 混合（hash+RRF） | 0.775 | 0.330 | 151.6 | 6.69 |

分层注入的代价（同一排名，只换注入层级）：easy L0 155 token → L0+L1 388 →
全量 786；hard L0 152 → L0+L1 347 → 全量 540，recall 不变。

**怎么读这组数**：

- easy 的 1.000 主要证明**索引/分词/字段权重正确**：查询都带唯一实体名（df=1），
  BM25 必然命中；它不等于"语义检索能力"。把实体词隐去后召回会显著下降。
- hard 暴露纯词法上限：查询没有实体名，属性词在池子里复用。注意 gold 与近重复
  twin 的 L0/L1 **逐字相同**，而 `rerank` 的输入正是 `Summary+Overview`，所以
  **Reranker 在这份数据上也无法区分**；"加 Reranker 就能跨过"不成立。要跨过需要
  能利用实体/来源/时间等 twin 未共享信号的检索器。twin 的 URI 排在 gold 前，
  因此 hard 的 MRR 结构上限约 0.5。
- avg tokens 跨臂不可比：baseline 经常 0 命中，token 少不代表更省；p95 是亚毫秒
  墙钟采样，同机两次可差 2×，只看量级。
- 混合臂的 hash 向量只是管线演示，不代表语义增益；它在这组数据上甚至略低于
  纯 BM25，报告里如实保留。

## 10. 与 OpenViking 官方模型的差异

- **L0/L1 的落点**：官方是目录级 sidecar（`.abstract.md`/`.overview.md`），普通文件
  不各自生成 L0/L1；Rig 额外给每条记忆保存 `Summary/Overview`，同时保留目录 sidecar。
  对记忆条目这是超集（检索可以直接命中一条记忆），但不是逐字节复刻。
- **检索默认值**：官方是向量优先 + 可选 rerank；Rig 默认纯 Go 的 BM25，注入
  `Embedder`/`Reranker` 后才启用向量/精排。零配置不联网、不调模型。
- **模型抽取**：官方用服务端 VLM/LLM；Rig 默认双语启发式，宿主可注入 `Extractor`。
- **会话启动**：Rig 首轮注入 profile/preferences 索引并重放 pending；官方 DSH 插件
  还会注入 skills catalog、可关闭 tool-result 捕获。Rig 没有 skills catalog。
- **工具面**：Rig 是原生 `memory` 工具，不是 MCP 的 `mcp__openviking__*` 前缀。
- **多租户/peer/ACL**：产品边界外；Rig 单用户、按工作区分区。

## 11. 不适用（产品边界）

- 多租户/账号/ACL/加密密钥托管：Rig 是单用户、单进程 SDK。
- HTTP 服务/Gateway/Studio/Web UI/Docker/Helm：不是本仓库的交付物。
- PDF/Office/音视频解析、飞书/Git 连接器：只接受文本/Markdown/代码。
- Prometheus/OTel：只在 `Stats` 暴露本地数字，不引观测依赖。

## 12. 相关文档

- [docs/CONFIG.md](CONFIG.md) —— `MemoryStore`/`MemoryOptions`/`MemoryCapture` 全字段
- [docs/TOOLS.md](TOOLS.md) —— `memory` 工具在工具目录里的口径
- [docs/ARCHITECTURE.md](ARCHITECTURE.md) —— 分层、契约与模块约束
- [docs/sqlite.md](sqlite.md) —— 会话持久化与 `storage` 聚合契约
- [CHANGELOG.md](../CHANGELOG.md) —— 本功能的变更与迁移说明