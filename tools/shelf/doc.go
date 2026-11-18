// Package shelf 是一个**书架**原语：一排条目，取下某一本。它把"渐进式加载"抽成
// 可复用的工具：**目录先行、正文按需、输出有界**。
//
// # 它解决的问题
//
// 把一大堆内容塞给模型有两种坏方式：一次性全倒进上下文（窗口被吃光、注意力被稀释），
// 或者干脆不给（模型只能猜）。渐进式加载是第三条路：先给一份便宜的目录，让模型自己
// 决定要读哪一条，再按条取正文。仓库里已有的两个工具就是这个形态：
//
//   - `skill`：`list` / `search` / `load` —— 目录给 `- name: description`，`load` 才读正文；
//   - `memory`：`index` / `search` / `read` —— 目录给 `name — summary`，`read` 才读正文。
//
// `shelf`（书架）把它们共同的骨架抽出来，宿主只要实现 `Source`（"条目从哪来"），就得到一个
// 行为一致的渐进式工具，不必各自重写 action 解析、上限、截断与错误文案。
//
// # 名字：没有默认工具名
//
// `Options.ToolName` **必填**：不同的内容该有贴合自己的名字（`handbook`、`errata`、
// `metrics`、`skill`…），由宿主决定。本包刻意不提供默认名，也刻意**不占用**
// `browse` / `browser` / `page` / `navigate` 这些词——它们要留给以后真正的浏览器
// 集成，通用库不该抢走。
//
// # 契约（宿主必须遵守）
//
//   - **只有 `Load` 会返回正文**。`List` 与 `Search` 必须便宜：它们的产物会原样进
//     模型上下文，为排序去读所有正文等于放弃了渐进性。
//   - **`Search` 自己排序**。工具的职责不是替你决定权重（标题命中优先？时间倒序？
//     分页？），它只负责渲染与上限。返回顺序就是模型看到的顺序。
//   - **`Summary` 必填且 ≤200 字节**：目录里显示的就是它。没有摘要的条目在目录里
//     等于不存在（除非显式 `AllowEmptySummary`）。
//
// # 三个动作
//
//	action=list            目录（只给元数据，最多 MaxListed 条）
//	action=search&query=…  匹配目录（只给元数据，最多 MaxSearch 条）
//	action=load&name=…     单条**完整**正文（不做单条截断）
//
// 省略 `action` 时：给了 `name` 就当作 load，否则 list（"只传 name"的兼容形态）。
//
// # 为什么没有"单条正文上限"
//
// 刻意不做。大正文的截断边界由 **agent 的 spill/truncate 机制**负责（它知道模型
// 的窗口与预算）；本库如果再加一层 `MaxLoadBytes`，"读一页"就变成两种行为（有时读
// 全、有时读半），模型也无法判断自己拿到的是不是完整内容。这里只保留一个**全局**
// 输出安全网 `MaxOutputBytes`（默认 64 KiB，按整行 + UTF-8 安全截断），它的职责是
// "别把工具响应撑爆"，不是"限制你读多少"。
//
// 同理，`load` 没有分页/offset 参数：需要翻页时由宿主的读取工具提供，本库不重复造。
//
// # 用法
//
//	source := mySource{}                       // 宿主实现 List / Search / Load
//	tool, err := shelf.New(source, shelf.Options{
//		ToolName:    "browse",
//		Description: "Browse the team handbook; load one page at a time.",
//	})
//	if err != nil {
//		return err
//	}
//	// tool 就是 rig.Tool，可以直接放进 tools/full 之外的任何装配。
//
// # 给既有工具留的兼容开关
//
// `skill` 是本包的第一位消费者，它的协议（schema、`- name: description` 目录行、
// `…\n[truncated: output exceeded N bytes]\n` 截断后缀）必须逐字节不变。为此 Options
// 提供了几个**兼容用**逃生舱：`Schema`（整体覆盖）、`FormatItem`（目录行渲染）、
// `TrailingNewline`、`TruncateBytes`（按字节截断的旧模式）、`Texts`（空库/无匹配/
// 未知条目文案）、`AllowEmptySummary`。新工具用默认值即可，不需要碰它们。
//
// # 本包不是什么
//
// 它不提供存储：`Source` 可以是文件、数据库、远程 API，甚至 `rig.SkillStore` 的
// 适配器。它也不提供写语义（新增/覆盖/追加）——`memory` 有自己的 `write`/`append`
// 与 wiki 存储，写语义与存储强耦合，强行统一只会把两边都拧巴。
package shelf
