// Package memory 提供 `memory` 工具：一个**渐进式**的 wiki 式长期记忆。
//
// # 为什么是 wiki，而不是"条目列表"
//
// 记忆必须是**常驻**能力（模型随时可能用到"上次这个测试是怎么修的"），但常驻不等于
// 一次全倒进上下文——20 条记忆的正文可能几百 KB，倒一次就把窗口吃掉。所以按 wiki
// 组织：一页一个主题，索引只给 `name — summary (多久以前)`，正文按需 read：
//
//	memory{action:"index"}                       → 目录（几十页也只有一两 KB）
//	memory{action:"read", name:"build-flaky"}    → 只取这一页（上限 8 KiB）
//	memory{action:"search", query:"flaky"}       → 缩小范围（只回 meta，最多 20 条）
//	memory{action:"write", name:"…", summary:"…", content:"…"}
//	memory{action:"append", name:"…", text:"…"}   → 追加到 "## Log" 段，带日期
//	memory{action:"delete", name:"…"}
//
// 三条硬约束把"渐进"落到实处：
//
//   - `index` **绝不返回正文**（只有 name/summary/年龄），最多 60 条，更多时尾巴写
//     `… and N more pages (use search to narrow)`；
//   - `read` 只给一页，超过 8 KiB 截断并显式说明"page is N bytes"；
//   - `search` 只回 meta 行，按"标题命中 > 摘要命中 > 正文命中"排序，最多 20 条。
//
// # 按工作区分区
//
// 记忆按工作区隔离（不同仓库互不可见，同一个仓库经不同路径打开落到同一份）：
//
//	<stateDir>/memory/<workspaceID>/pages/<name>.md
//	<stateDir>/memory/<workspaceID>/index.json
//
// 其中 `workspaceID = <base>-<sha256(canonicalRoot)[:8]>`，`canonicalRoot` 是
// `EvalSymlinks` + `Clean` 之后的绝对路径。`stateDir` 默认取 XDG 状态目录
// （`$XDG_STATE_HOME/rig`，否则 `~/.local/state/rig`；Windows 用
// `%LOCALAPPDATA%\rig`），可用 WithStateDir 语义的 NewWorkspaceWiki 覆盖。
//
// **记忆不写进仓库**：① 里面可能有不该提交给团队的内容（私有偏好、临时结论、
// 甚至凭据片段）；② `.rig/` 出现在 `git status` 里会干扰"帮我提交"这类任务。
//
// # 存储
//
// `NewFileWiki(dir)` 落盘（目录 0700、页面 0600——记忆是私人的）；`NewMemoryWiki()`
// 是进程内实现（测试与无盘环境）；`NewWorkspaceWiki(stateDir, root)` 直接按工作区
// 目录开一个。宿主也可以自己实现 `Wiki`（例如落到远程或加密存储）。
//
// 旧的扁平 `rig.MemoryStore` 接口**保留**（宿主自定义实现仍可编译使用）：
// `WikiFromStore(store)` 把一条记忆投影成一页，作为只读迁移通道。
//
// # 页面格式
//
//	front-matter（title/summary/updated）+ Markdown 正文
//
// 覆盖写会在 Log 段留下一行 `- YYYY-MM-DD rewritten (was N bytes)`；`append` 追加
// `- YYYY-MM-DD <text>`。`name` 只允许 `[a-z0-9-]{1,64}`，**不做静默改写**——否则
// 模型之后用原名会找不到自己写的东西。
package memory
