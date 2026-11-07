// Package workflow 把进程内子代理运行器封装为 workflow 工具：按顺序（或部分并行）的
// 步骤列表，每一步由一个全新的子代理执行；后续提示可以通过简单模板引用前序步骤的输出。
//
// 模板：
//
//	{{prev}}          上一个已完成步骤的输出
//	{{steps.<name>}}  指定名称步骤的输出
//
// 标记 parallel:true 的相邻步骤组成一个并发组；组内步骤只能看到组之前的输出，看不到同组
// 兄弟步骤的输出。runner 自身的并发上限仍会在 max_parallel 之上生效。
//
// # 模型可见工具
//
//   - workflow {steps 或 tasks: [{name?, prompt 必填, parallel?}], max_parallel 可选}：
//     steps 与 tasks 等价（tasks 是简单顺序工作流的别名）；max_parallel 默认 4、上限 8。
//     工具返回每个已完成步骤的输出；失败时返回已完成步骤以及失败步骤的错误。
//
// # 组装与依赖
//
// workflow.New(runner) / workflow.NewTool(runner) 构造工具；runner 为 nil 时调用返回
// 可读的配置错误。tools/full.All 会把本工具作为静态工具注册（需要 AllOptions.Runner）。
//
// # 权限与成本
//
// 工具自身不直接访问文件、网络或 shell，只驱动 runner 启动的子代理；子代理权限由 runner
// 决定，工具调用仍经过 agent 层 rig.Gate 审批。并行组会同时占用多个并发额度，宿主应
// 结合 runner 的 MaxConcurrent 控制成本。
package workflow
