// Package ralph 提供 ralph 工具：让同一个子代理反复迭代，直到输出出现完成标记。
//
// ralph 把长任务拆成多轮：第 1 轮发送任务提示；之后每轮把上一轮输出和完成提示词回传给
// 同一个子代理（其会话被保留），直到回复包含独立的 DONE（或中文“完成”）标记，或达到
// 轮次上限。它适合需要多轮自我修正、且每轮应沿用已有上下文的收尾型任务。
//
// # 模型可见工具
//
//   - ralph {prompt 必填, completion_prompt 可选, max_rounds 可选, name 可选}：
//     prompt 是完整任务描述；completion_prompt 在每轮追加，可引用 {{output}} 与
//     {{round}}，默认 "If the task is complete, reply DONE"；max_rounds 默认 8、上限
//     32；name 仅用于日志。工具返回每轮摘要（单轮摘要上限 MaxRoundSummaryChars =
//     2000 字符）与最终输出。
//
// # 组装与依赖
//
// ralph.New(runner) / ralph.NewTool(runner) 构造工具；runner 是 rig.SubagentRunner
// （agent.NewRunner 提供默认实现）。runner 为 nil 时工具仍可构造，调用时返回可读的配置
// 错误。子代理的深度、并发与工具过滤由 runner 统一约束；tools/full.All 会把本工具作为
// 静态工具注册（需要 AllOptions.Runner）。
//
// # 权限与成本
//
// 工具自身不直接访问文件、网络或 shell，只驱动 runner 启动的子代理；子代理的实际权限
// 继承自 runner 对父配置的复制，工具调用仍经过 agent 层 rig.Gate 审批。多轮迭代会放大
// 模型与工具调用成本，宿主应结合轮次上限与超时控制使用。
package ralph
