// Package subagent 把进程内子代理运行器（agent.NewRunner）暴露为模型可调用的同步委派
// 工具：模型把任务交给子代理，并等待子代理的最终回答。
//
// 注意：v0.12 起 tools/full.All 默认注册的是 tools/team 中 DSH 兼容的异步委派工具
// （spawn_teammate / subagent / subagent_fork / wait_agent 等）。本包保留的是“调用即
// 阻塞、返回完整结果”的同步一次性委派工具，供需要阻塞语义的宿主手动组装。
//
// # 模型可见工具
//
//   - subagent {prompt 必填, name 可选, model 可选, max_iterations 可选, fork 可选,
//     output_schema 可选}：启动一个新上下文子代理并等待其结束。fork=true 时子代理以父
//     会话已完成历史的副本作为种子（要求 runner 配置了父会话）；output_schema 要求子
//     代理通过结构化输出返回 JSON 结果，结果写入 SubagentResult.Structured，并由工具
//     渲染在文本摘要之后。
//   - subagent_fork {prompt 必填, name 可选, model 可选, max_iterations 可选}：与
//     subagent 相同，但强制 fork=true。
//
// 子代理看不到当前对话（fork 除外），因此 prompt 必须是自包含的完整任务；子代理不能
// 反问，默认也不能递归启动更多子代理——runner 会过滤编排工具并施加深度/并发限制。
//
// # 组装
//
// tools := subagent.Tools(runner) 返回 subagent 与 subagent_fork 两个工具；
// New/NewTool、NewForkTool/ForkTool 是单项构造器。runner 为 nil 时工具仍可构造，调用
// 返回可读的配置错误：
//
//	reg := tools.NewRegistry(subagent.Tools(runner)...)
//
// # 权限
//
// 工具只通过 runner 启动子代理，不直接访问文件、网络或 shell；子代理的权限来自 runner
// 对父 AgentSpec 的复制，实际工具调用仍经过 agent 层 rig.Gate 审批。
package subagent
