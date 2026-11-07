// Package report 提供 report 工具：把一个会话（或没有会话日志时的工作区）汇总成一份
// Markdown 文件，写入 agent 的沙箱内。
//
// 工具对会话只读，仅写报告文件；所有文件系统访问都经过 sandbox.Policy，根目录之外的路径
// 或只读策略都会被拒绝并返回可读的模型错误。会话日志由宿主提供；没有日志时报告降级为
// 仅工作区摘要，而不是失败。
//
// # 模型可见工具
//
//   - report {path 可选, title 可选, session_id 可选, limit 可选, content 可选}：
//     path 是相对沙箱根的输出路径（默认 rig-report-<时间戳>.md，写入主根目录）；title 默认
//     "Rig report"；session_id 限定统计的会话（省略则汇总可搜索的跨会话事件，无日志则只
//     含工作区信息）；limit 为扫描事件数（默认 200、上限 2000）；content 会作为报告的
//     Content 段追加。返回写入路径、作用域、事件/工具调用/错误计数、字节数与词数。
//
// 报告列出最近事件（最多 recentEvents 条），单条事件载荷摘录上限 payloadExcerptBytes；
// 文件以 FileMode 写入；目录不存在时会自动创建。
//
// # 组装、依赖与权限
//
// 通过 New(Options{Log, Policy, Now}) 构造；Policy 在调用时必需（nil 时返回可读错误），Log
// 为 nil 时只汇总工作区。Now 可注入确定性时钟（测试用）。工具不访问网络或 shell；写入被
// sandbox.Policy 限制在允许的根目录内，只读策略下拒绝写文件。审批在 agent 层由 rig.Gate
// 控制。
package report
