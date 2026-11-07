// Package session 提供只读的会话历史工具，由 rig.SessionLog 支撑：session_search、
// session_trace、session_event_read、session_event_search、session_event_trace。
//
// 工具从不写会话状态。宿主提供自己的 SessionLog 实现（持久化存储）；NewMemoryLog 是一个
// 进程内实现，供测试、示例以及在无持久化存储时嵌入 Rig 使用，同时实现 tools/inspect 使用
// 的可选 SessionLister 扩展（Sessions）。
//
// # 模型可见工具
//
//   - session_search {query 必填, limit 可选}：搜索事件（大小写不敏感，匹配会话 id、
//     事件类型与载荷），返回匹配事件及载荷摘录；limit 默认 20、上限 100。
//   - session_trace {session_id 必填, from 可选, to 可选}：按 seq 区间输出一个会话的有序
//     事件轨迹（to=0 表示直到结尾）；最多打印 MaxTraceEvents=100 条事件。
//   - session_event_read {session_id 必填, seq 必填}：读取一条完整事件，载荷超过 4 KiB
//     时截断并提示。
//   - session_event_search {query 必填, limit 可选}：与 session_search 类似，但每条命中
//     附带载荷摘录，便于先定位再读全文。
//   - session_event_trace {seq 必填, session_id 可选, before 可选, after 可选}：显示目标
//     事件前后的上下文事件（before/after 默认 2、上限 10）；省略 session_id 时按 seq 跨
//     会话尽力解析所属会话。
//
// 所有输出上限 32 KiB。
//
// # 组装与降级
//
// Tools(log) 允许 log 为 nil：每个工具仍可构造，调用时返回可读的
// "no session log is configured" 工具错误，从而保持工具目录稳定。
//
// # 权限
//
// 工具只读会话历史，不写文件、不访问网络或 shell；审批在 agent 层由 rig.Gate 控制。
package session
