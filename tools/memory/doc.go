// Package memory 提供 memory 工具与一个进程内 rig.MemoryStore 实现。
//
// 宿主可以通过 Tools(store) 注入自己的持久化实现（数据库、文件、远端服务）；store 为
// nil 时使用 NewMemoryStore——并发安全、进程内、重启即丢失。
//
// # 模型可见工具
//
// 唯一的 memory 工具按 action 操作：
//
//	{"action":"add","text":"...","scope":"workspace"}   新增（text 必填，scope 默认 workspace）
//	{"action":"list","scope":"workspace"}               列出，输出 "- <id> [<scope>] <text>"
//	{"action":"search","query":"release"}               文本子串搜索（大小写不敏感）
//	{"action":"remove","id":"mem-1"}                    按 ID 删除，未知 ID 为可读错误
//	{"action":"clear","scope":"workspace"}              清空指定 scope（省略则清空全部）并报告数量
//
// list/search 的输出受 DefaultMaxOutputBytes（32 KiB）上限保护；add/remove/clear 报告
// 变更结果。clear 基于 MemoryStore 接口的 List+Remove 实现，自定义 store 不需要额外方法。
//
// # 宿主接口与权限
//
// 工具依赖 rig.MemoryStore（List/Add/Remove/Search）；持久化、多会话隔离与访问控制由
// 宿主实现决定（默认实现是进程内单例）。工具不触碰文件、网络或 shell；审批在 agent 层
// 由 rig.Gate 控制。
package memory
