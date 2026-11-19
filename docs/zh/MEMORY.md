# 记忆与溢出结果

两个可选能力让 agent 的状态能活过一次上下文窗口。它们都是**公开包里的契约**加参考
实现，宿主可以换成自己的存储。

| 能力 | 契约 | 参考实现 |
| --- | --- | --- |
| 长期记忆 | `memory.Wiki`（`github.com/zeokeel/zeokeel-rig/memory`） | `internal/memwiki`（磁盘文件、进程内）与 `session/sqlite`（会话库里的 `rig_memory_pages`，**一张**表） |
| 超大工具结果 | `rig.SpillStore`（`SpillPolicy.Store`） | 默认是磁盘目录；`session/sqlite` 的 `Spill()` 提供库内实现，由 `spill_read` 读回 |

## 长期记忆（`memory`）

渐进式 wiki：`Index` 每页只返回一行元数据（`name — title — summary`），`Read` 才
取某一页正文，`Search` 在标题/摘要/正文里匹配但同样只返回元数据。目的是：模型扫
几十页只要一两 KB，只为真正读的那几页付正文的钱。

```go
wiki := store.Memory()                     // session/sqlite
// 或者用内置实现：memwiki.NewFileWiki(...) / memwiki.NewMemoryWiki()

if err := wiki.Write(ctx, memory.Page{
    Name:    "build-flaky-tests",
    Title:   "构建偶发失败",
    Summary: "哪些测试偶发失败、怎么重试",
    Content: "# 构建偶发失败\n……",
}); err != nil { /* ErrInvalidName / ErrSummaryRequired / ErrSummaryTooLong */ }

metas, _ := wiki.Index(ctx)        // 只有元数据，没有正文
page, err := wiki.Read(ctx, "build-flaky-tests")
```

写后端前值得知道的规则：

- 页名是 slug `[a-z0-9-]{1,64}`，**不会**被静默改写；
- 摘要必填（索引只显示摘要），上限 `MaxSummaryBytes`；
- `Write` 覆盖已有页时往 `## Log` 段追加 `- YYYY-MM-DD rewritten (was N bytes)`；
  `Append` 只追加（它不是重写，不记这一行）；
- `Append` 到不存在的页返回 `ErrNotFound`，**不会**顺手建页；
- `Index` 返回**全部**页：截断（以及"… and N more"那句）是工具层的事；
- `Search` 排序是"标题命中 > 摘要命中 > 只在正文命中"，同档按最近修改。正文过滤会
  读每条候选行（全表扫描）；个人客户端的记忆规模下够用，要更快得建全文索引，那属于
  另一次契约变更。

## 溢出结果（`rig.SpillStore`）

工具结果超过 `SpillPolicy.ThresholdBytes` 时，agent 会把它换成一段摘要；模型看到
的是 `ref:` 句柄而不是正文。

```go
a, _ := kit.New(kit.Config{
    Spill: &rig.SpillPolicy{Store: store.Spill(), MaxFiles: 200},
})
```

- `Save` 存下并返回句柄，`Load` 读回来，`GC(keep)` 保留最新 keep 条；
- **读回路径**：`tools/full.All` 在 `AllOptions.SpillStore` 非 nil 时注册
  `spill_read`（kit 把 `SpillPolicy.Store` 透传过去），所以模型看到的 `ref:`
  永远有一条回去的路。自行组装工具集的宿主需要提供等价的读回入口；
- **回收**：文件后端每次写入后按 `MaxFiles` 修剪；Store 后端每次 `Save` 之后调用
  一次 `GC(MaxFiles)`（见 `agent/spill.go`）。不经过 agent 层、直接使用
  `SpillStore` 的宿主需要自己定期调 `GC`。
