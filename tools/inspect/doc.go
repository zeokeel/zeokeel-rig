// Package inspect 提供只读的 inspect 工具：以紧凑、结构化的方式报告 agent 当前可见的
// 环境——会话历史、事件统计与工作区根目录。
//
// # 模型可见工具
//
//   - inspect {action 必填, session_id 可选, limit 可选}，action 取值：
//     sessions  列出已知会话（limit 默认 20、上限 200）
//     events    显示某个会话的近期事件（session_id 必填）
//     workspace 报告工作区根目录与文件/目录计数（limit 默认 1000、上限 10000）
//     stats     汇总事件类型、工具调用与错误（可用 session_id 限定会话）
//
// 单个响应上限 32 KiB，事件载荷摘录上限 200 字节；action=workspace 最多列出 12 个顶层
// 条目。
//
// # 宿主接口与降级
//
// 工具只依赖 rig.SessionLog（读取事件，绝不写入）与文件系统。SessionLog 为 nil 时，
// 会话类 action 降级为纯工作区快照而不是报错；若宿主实现还提供可选的 SessionLister
// 扩展（Sessions(ctx) ([]string, error)），inspect 会直接枚举会话，否则从 Search 结果
// 推断会话 id。构造 New(Options{Root, Log}) 永不失败，缺少配置由 Invoke 报告。
//
// # 权限
//
// 纯只读：不修改会话、事件或工作区文件；工作区遍历受 limit 与上限约束。工具不访问网络
// 或 shell。审批在 agent 层由 rig.Gate 控制。
package inspect
