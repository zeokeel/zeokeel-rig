# 记忆与溢出结果

本文说明 Rig 里两类**可选**的"跨上下文状态"：模型自己维护的长期记忆 wiki，以及
放不进上下文窗口的超大工具结果（spill）。读者是想给 agent 接上记忆、或者要给记忆
写一个新后端的工程师——读完之后你应该知道 `memory.Wiki` 每个方法的语义、默认行为
落在哪里、以及哪些内容还没发布。

两类能力的共同形状：**契约在公共包里，实现可以换**。SDK 提供参考实现与默认接线，
宿主可以注入自己的后端（加密、远程、测试替身）。

| 能力 | 契约 | 参考实现 |
| --- | --- | --- |
| 长期记忆（渐进式 wiki） | `memory.Wiki`（包 `memory`） | `internal/memwiki` 的文件 wiki / 进程内 wiki；`session/sqlite` 的 `rig_memory_pages` 表（**未发布**） |
| 超大工具结果（spill） | `rig.SpillStore`（经 `rig.SpillPolicy.Store` 注入） | 目录里的文件（默认）；`session/sqlite` 的 `rig_spill` 表（**未发布**）；读回工具 `spill_read` |

## 1. `memory.Wiki` 契约

包 `memory` 只做三件事：定义数据模型（`Page`/`PageMeta`）、定义六个方法的接口
（`Wiki`）、把规则函数原样导出（`ValidateName`/`NormalizeSummary`/`AppendLog`/
`RewriteLog`/`SortMetas`/`ClampText`）。**规则的唯一来源是规则函数**：第二个实现
如果自己抄一遍校验与追加逻辑，第一次改规则时两个后端就会漂移，而模型看到的是
"同样的操作在文件后端能成、在数据库后端报错"。

接口要求：实现必须**并发安全**，且 `Index`/`Search` **只返回元数据**——正文只能
通过 `Read` 取。渐进披露就是靠这一条成立的：索引里放正文，等于每次看目录都付一遍
正文的 token。

### 1.1 六个方法

| 方法 | 语义 | 边界 |
| --- | --- | --- |
| `Index(ctx) ([]PageMeta, error)` | 返回**全部**页面的元数据（`Name`/`Title`/`Summary`/`Updated`/`Bytes`），最近修改降序、同时间按 `Name` 升序 | **不返回正文**；**不截断**——"最多显示 N 条"与 `… and N more` 是工具层的事（`MaxIndexResults` = 60）。在存储层写 `LIMIT` 会让"还有几页没显示"永远不出现 |
| `Read(ctx, name) (Page, error)` | 返回一页完整内容（含正文） | 页不存在返回 `ErrNotFound`；页名先过 `ValidateName` |
| `Write(ctx, page) error` | **覆盖**一页（不存在则创建） | `Summary` 必填；覆盖已有页时在正文的 `## Log` 段追加 `- YYYY-MM-DD rewritten (was N bytes)`；`Updated` 为零值时由存储层填当前时间（UTC） |
| `Append(ctx, name, text) error` | 把 `text` 追加到该页正文的 `## Log` 段，自动带 `- YYYY-MM-DD ` 前缀 | **页必须已存在**，不存在返回 `ErrNotFound`。"没有则创建"指的是 **Log 这一段**，不是整页；`Append` 不是重写，因此**不**记 `rewritten` 行 |
| `Delete(ctx, name) error` | 删除一页 | 不存在返回 `ErrNotFound` |
| `Search(ctx, query) ([]PageMeta, error)` | 在标题/摘要/正文里做大小写不敏感的子串查找，只返回元数据 | 排序：标题命中 > 摘要命中 > 正文命中；同档按最近修改降序、再按 `Name` 升序；最多 `MaxSearchResults` = 20 条。空查询返回 `nil` |

### 1.2 规则与上限

| 项 | 值 | 说明 |
| --- | --- | --- |
| 页名（slug） | `[a-z0-9-]{1,64}` | `MaxNameLength` = 64。**刻意不做自动改写**：记忆是模型自己写的，静默把 `Build Notes` 改成 `build-notes` 会让它之后用原名找不到 |
| 摘要 | 必填，≤ 200 **字节** | `MaxSummaryBytes` = 200；`NormalizeSummary` 去掉首尾空白并把任意空白序列折叠成一个空格（索引一行一条）。索引只显示摘要，没有摘要的页等于不可见 |
| 标题 | 可空 | `NormalizeTitle(name, title)`：空则回退到页名 |
| Log 段标题 | `## Log` | `LogHeading`。`Append` 与覆盖写都写进这一段；正文里已有该段就追加到段末（下一处 `## ` 之前），没有就在文末新建 |
| 索引条数上限 | 60 | `MaxIndexResults`，由工具层执行 |
| 搜索条数上限 | 20 | `MaxSearchResults`，由 `Search` 自己执行 |
| 截断 | UTF-8 边界 | `ClampText(text, limit)` 返回截断结果与原始字节数；直接 `text[:limit]` 会把一个多字节字符劈成两半 |

哨兵错误（调用方用 `errors.Is` 判断）：

| 错误 | 含义 |
| --- | --- |
| `memory.ErrInvalidName` | 页名不是 slug |
| `memory.ErrSummaryRequired` | 缺少摘要 |
| `memory.ErrSummaryTooLong` | 摘要超过 `MaxSummaryBytes` |
| `memory.ErrNotFound` | 页面不存在 |

### 1.3 模型看到的 `memory` 工具

契约是给实现者的；模型看到的是 `tools/memory` 包里的 `memory` 工具，一个工具六个
动作：`index` / `read` / `write` / `append` / `search` / `delete`。工具层负责三件事：

- `index` 只回 `name — summary (多久以前)`，最多 60 行，尾巴写
  `… and N more pages (use search to narrow)`；
- `read` 只给一页，正文超过 **8 KiB**（`DefaultMaxReadBytes`）截断并写明
  `page is N bytes`；单次响应总上限 32 KiB（`DefaultMaxOutputBytes`，按整行截断）；
- `search` 只回元数据行，最多 20 条。

8 KiB 与 32 KiB 这两道字节上限都可以用 `WithMaxReadBytes` / `WithMaxOutputBytes` 覆盖。`memory` 工具的
描述刻意只写"一句话总述 + 一行动作列表"：细节交给运行时错误去教（用错的那一刻
再教，比常驻描述里教便宜）。

## 2. 实现

### 2.1 文件 wiki（按工作区分区）

`internal/memwiki` 的 `FileWiki` 是默认实现。磁盘布局：

```
<stateDir>/memory/<workspaceID>/pages/<name>.md   一页一个文件（0600）
<stateDir>/memory/<workspaceID>/index.json        派生缓存（可重建，损坏即重建）
```

- **目录 0700、页面 0600**：记忆里可能有用户不愿分享的内容。
- **页面格式**：front-matter + Markdown 正文。front-matter 只有 `title` / `summary` /
  `updated` 三个字段，写成"每行一个 `key: value`"，值取整行剩余部分——本仓库只允许
  一个第三方依赖（`mvdan.cc/sh/v3`），所以不引 YAML 库，也不支持多行值。
- **损坏容忍**：front-matter 缺失或损坏时**不报错**，整份文件当正文、标题回退成
  页名——这样一页坏文件仍能被 `read`/`delete` 修掉，而不是从索引里消失。
- **原子写**：临时文件 + `rename`，因此崩溃不会留下半页；写操作在进程内串行
  （记忆写入是低频动作，不值得引入锁竞争）。
- **索引缓存**：`index.json` 带版本号与每个页面的 `size`/`mtime` 指纹；缺失、损坏、
  版本不符或任一页面变动就整体重建。缓存只是性能，不影响正确性。
- **`Search` 的读放大**：只有"标题/摘要都没命中"的页面才去读正文，避免全量 IO。

`stateDir` 与 `workspaceID` 的规则：

| 项 | 规则 |
| --- | --- |
| `stateDir` | `$XDG_STATE_HOME/rig`；否则 Windows `%LOCALAPPDATA%\rig`；其它平台 `~/.local/state/rig` |
| `workspaceID` | `<basename>-<sha256(canonicalRoot)[:8]>`；`canonicalRoot` = 绝对路径 + `EvalSymlinks` + `Clean` |
| 分区效果 | 不同仓库互不可见；同一仓库经符号链接/相对路径打开落到**同一份**记忆 |

**记忆不写进仓库**，两个理由：① 里面可能有不该提交给团队的内容（私有偏好、临时
结论、凭据片段）；② `.rig/` 出现在 `git status` 里会干扰"帮我提交"这类任务。

### 2.2 进程内 wiki

`MemoryWiki` 是同一套语义的内存实现，并发安全，用于测试与无盘环境。它与文件实现
共用 `internal/memwiki` 里的校验、排序与 Log 追加规则，所以两者行为一致。

### 2.3 `session/sqlite` 的 wiki（**未发布**）

`session/sqlite` 把记忆放进会话库的同一张表 `rig_memory_pages`（`name` 主键，外加
`title`/`summary`/`content`/`updated_at`/`bytes` 与 `updated_at DESC` 索引）。
它与文件后端共用 `memory` 包导出的规则函数，区别只在"落盘"换成了"落库"：

- `Index` **不在 SQL 里写 `LIMIT`**（契约要求返回完整目录，截断是工具层的事）；
- `Write` 把"读旧正文大小 + 写新正文"放进**一个事务**，并用 BUSY 重试包住整个
  事务（锁冲突时必须重开事务、重新读，否则重试用的是过期快照，等于把丢写换成
  慢速丢写）；
- `Append` 不记 `rewritten` 行（它本来就不是重写）。

> **发布状态**：`memory` 公共契约、`session/sqlite` 子模块都在 CHANGELOG 的
> 「Unreleased」里，v0.23.4 **没有**它们。详见 [docs/sqlite.md](sqlite.md) 的
> 发布状态一节。

### 2.4 怎么构造一个 wiki

契约在 `memory` 包，构造函数在 `tools/memory`（`internal/memwiki` 不可被仓外导入）：

```go
import (
    "context"

    "github.com/zeokeel/zeokeel-rig/memory"
    memorytools "github.com/zeokeel/zeokeel-rig/tools/memory"
)

// ① 按工作区分区的文件 wiki（kit 的默认行为）
wiki, err := memorytools.NewWorkspaceWiki("", root) // stateDir 为空 = 用户级默认状态目录
// ② 指定目录的文件 wiki
wiki, err = memorytools.NewFileWiki("/path/to/memory")
// ③ 进程内（测试 / 无盘环境）
wiki = memorytools.NewMemoryWiki()

if err := wiki.Write(context.Background(), memory.Page{
    Name:    "build-flaky-tests",
    Title:   "Flaky build tests",
    Summary: "哪些测试偶发失败、重试策略是什么",
    Content: "# Flaky build tests\n…",
}); err != nil {
    // ErrInvalidName / ErrSummaryRequired / ErrSummaryTooLong
}

metas, err := wiki.Index(context.Background())          // 只有元数据，没有正文
page, err := wiki.Read(context.Background(), "build-flaky-tests")
```

## 3. kit 的默认行为

`kit.New` 的记忆默认**开**，逻辑只有一段：

```go
// kit/kit.go 的等价逻辑（这里把 tools/memory 写成 memorytools 以便区分契约包）
memoryWiki := cfg.Memory                       // 显式注入优先
if memoryWiki == nil && root != "" {
    if wiki, err := memorytools.NewWorkspaceWiki("", root); err == nil {
        memoryWiki = wiki                    // <状态目录>/memory/<workspaceID>，0700/0600
    }
}
```

- `Config.Memory`（`memory.Wiki`）为 nil 且 `Root` 非空时，kit 用**工作区分区**的
  文件 wiki 打开记忆；`Root` 默认 `.`。
- 装配失败（例如状态目录不可写）**不阻断** `kit.New`：回落进程内 wiki，`memory`
  工具仍然可用；原因可通过 `(*kit.Agent).MemoryWikiError()` 拿到，建议 UI 提示用户。
- `memory` 工具是**常驻 active** 的（初始 16 个之一）：它不属于"宿主付了配置成本
  才拿到"的可选能力，而是产品默认。
- `(*kit.Agent).MemoryWiki()` 返回工具实际使用的 wiki，宿主可以用它预置或导出页面。

**关掉或替换**：

| 目标 | 做法 |
| --- | --- |
| 换后端（加密、远程、测试替身） | 实现 `memory.Wiki` 六方法，填 `kit.Config.Memory` |
| 换位置 | `memorytools.NewFileWiki(dir)` / `NewWorkspaceWiki(stateDir, root)` 后填 `Config.Memory` |
| 彻底不要 `memory` 工具 | 自定义 `kit.Config.Tools`（`Tools` 非 nil 时 kit 不构建默认集，也不做零配置体检）——工具不在集合里，就没有记忆能力 |

## 4. 溢出结果（spill）

当一条工具结果大到不该每轮重发时，agent 在**记录 tool 消息之前**用
`rig.SpillPolicy` 把它换成一段摘要：模型看到的是句柄（`ref:`）加预览，字节留在
存储里，需要时再读回来。

### 4.1 `rig.SpillPolicy`

| 字段 | 类型 | 默认 / 语义 |
| --- | --- | --- |
| `Dir` | `string` | 落盘目录（相对路径相对宿主工作目录）；为空时用 `<系统临时目录>/zeokeel-rig-spill` |
| `ThresholdBytes` | `int` | 触发落盘的最小内容字节数；`<= 0` 时用 16 KiB |
| `MaxFiles` | `int` | 保留的落盘结果上限（先进先出）；`<= 0` 不清理。**设了 `Store` 之后同样生效**（每条新结果写入后按它回收一次） |
| `Store` | `rig.SpillStore` | 非 nil 时结果存进宿主自己的存储（数据库行、对象存储），模型看到的是句柄而不是路径 |

### 4.2 模型看到什么

摘要的第一行是稳定前缀 `[tool result spilled to file]`（`rig.SpillSummaryPrefix`，
`rig.IsSpilledToolResult` 用它判断），后面按后端分叉：

```
[tool result spilled to file]
ref: spill-1763500000000-a1b2c3d4e5f6      ← Store 后端
bytes: 41234
preview: …前 240 字节，换行折叠成空格…
```

文件后端把 `ref:` 换成 `path: <绝对路径>`，其余完全一样。预览按 **rune** 截断：
预览是模型可见文案，切在字节中点会把一个汉字劈成两半。

### 4.3 读回：`spill_read`

句柄必须能读回来，否则"结果太大，已存起来"等于"结果永久丢失"。

- **注册条件**：只有宿主提供了 `SpillStore` 才注册。`tools/full.All`/
  `full.NewRuntime` 在 `AllOptions.SpillStore` 非 nil 时追加它；`kit` 从
  `Config.Spill.Store` 透传（`kit.Config.Spill` 为 nil 或 `Store` 为 nil 时不注册）。
- **可见性**：它是 **server-bound** 工具（`catalog.ServerBound` 的 7 个之一），
  **不进**初始 active 集——"真的需要时才用"的工具不该占常驻预算。模型要用它得
  先 `tool_search` 激活。
- **语义**：参数只有 `id`（就是 `ref:` 那一行）；单次最多 8 KiB，超了截断并说明
  `showing N of M bytes`；句柄不存在时返回包了 `rig.ErrSpillNotFound` 的错误
  （宿主可以 `errors.Is` 判断）。
- 自己组装工具集的宿主必须给模型一个等价的读回入口（或直接用自己的 UI 呈现）。

### 4.4 回收

- 文件后端：每次写入后按 `MaxFiles` 修剪（只保留最新的 `N` 个 `.txt`）。
- `Store` 后端：agent 层在**每次 `Save` 之后**调一次 `GC(MaxFiles)`——否则
  `MaxFiles` 在设了 `Store` 之后会静默失效，表无限增长而宿主以为自己设过上限。
  不经过 agent 层、直接用 `SpillStore` 的宿主必须自己定期 `GC`。

### 4.5 `session/sqlite` 的 spill（**未发布**）

`session/sqlite` 用 `rig_spill` 表实现 `rig.SpillStore`：`id` 是模型看到的唯一句柄，
`seq` 是 `AUTOINCREMENT` 主键，`GC(keep)` 按 `seq` 保留最新 `N` 条。为什么按 `seq`
而不是 `created_at`：同一毫秒写入的多条结果时间相同，按时间排序时"最新的是哪条"
不确定；`AUTOINCREMENT` 保证单调、不会因为删行而回退复用。

```go
// session/sqlite（未发布）：句柄从"文件路径"变成"一行记录"
a, _ := kit.New(kit.Config{
    Model: "gpt-4o-mini",
    Spill: &rig.SpillPolicy{Store: st.Spill(), MaxFiles: 200},
})
```

## 5. 迁移：旧的扁平 `rig.MemoryStore`

`rig.MemoryStore`（`List`/`Add`/`Remove`/`Search`）是 wiki 之前的扁平记忆接口。
它**保留**（宿主自定义实现仍能编译），但不再是推荐路径：

- `tools/memory.WikiFromStore(store)` 把扁平 store **投影**成 wiki：一条记忆 = 一页，
  页名由记忆 ID 生成（非 slug 字符转成 `-`），摘要取正文首行（截断到 200 字节），
  更新时间取 `Created`。
- 投影是**只读迁移通道**：`Write`/`Append` 只会往 store 里加新条目，不会覆盖旧条目
  ——`MemoryStore` 没有"覆盖"语义。
- `full.AllOptions.Memory` 是**历史字段**；同时设置 `Wiki` 时 `Wiki` 优先，`Memory`
  只作为只读迁移通道；两者都为 nil 时用进程内 wiki。
- `kit.Config` **没有**扁平字段，只有 `Memory memory.Wiki`：新代码直接实现/注入
  `memory.Wiki`。

迁移建议：一次性把扁平记忆读出来写成页（`WikiFromStore(...).Index` +
`wiki.Write`），然后把 `Config.Memory` 指向新 wiki；不要在两边同时写。

## 6. 相关文档

- [docs/sqlite.md](sqlite.md) —— 会话持久化三形态与 `storage` 聚合契约（含
  `session/sqlite` 的发布状态）
- [docs/CONFIG.md](CONFIG.md) —— `kit.Config` 全字段与默认值
- [docs/TOOLS.md](TOOLS.md) —— `memory` / `spill_read` 在工具目录里的口径
- [docs/ai/tools.json](ai/tools.json) —— 两个工具的精确 JSON Schema
- [docs/ARCHITECTURE.md](ARCHITECTURE.md) —— 分层与契约在架构中的位置