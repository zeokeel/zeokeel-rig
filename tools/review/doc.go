// Package review 提供代码评审工具（code_review、auto_review）以及一个由进程内子代理驱动
// 的 rig.Reviewer 实现（SubagentReviewer）。
//
// 工具只依赖冻结的 rig.Reviewer 契约：宿主可以接模型、接人工，或使用本包的
// SubagentReviewer。reviewer 为 nil 时会得到可读的配置错误，绝不会 panic。
//
// # 模型可见工具
//
//   - code_review {target 可选, kind 可选, prompt 可选, content 可选}：请求一次评审
//     （kind 默认 code，可选 diff/plan），格式化输出结论（verdict）、摘要与发现
//     （findings：位置、严重级别、消息）。target 是被评审材料的位置；当材料无法从
//     target 加载时用 content 直接提供。工具结果本身不是错误，是否处理由模型决定。
//   - auto_review {target 可选, prompt 可选, severity_threshold 可选}：评审后明确给出
//     “是否应阻塞进度”的结论。verdict 为 fail/block，或存在严重级别达到
//     severity_threshold（默认 warning；可取 info/warning/error）的发现时，文本会标记为
//     阻塞；工具结果同样不是错误。
//
// 输出有界：摘要 MaxSummaryChars、单条发现消息 MaxFindingMessageChars、最多渲染
// MaxFindingsDisplay 条发现。
//
// # 宿主接口
//
// 两个工具通过 Tools(reviewer) 构造并注册；宿主可以实现 rig.Reviewer 接自己的评审通道，
// 或使用 NewSubagentReviewer(runner) 让子代理执行评审：它用结构化提示词启动子代理、等待
// 最终回答，并把纯文本解析为 rig.ReviewResult（可选 "VERDICT: pass|warn|fail" 行、
// "FILE:LINE SEVERITY: message" 形式的发现行，其余文本作为摘要，解析器
// ParseReviewOutput 也对外导出）。
//
// # 权限
//
// 工具自身不访问文件、网络或 shell，只调用宿主注入的 reviewer；SubagentReviewer 通过
// runner 启动子代理，子代理权限继承父配置并仍经 agent 层 rig.Gate 审批。评审请求中的
// 被评审内容由调用方提供，宿主实现负责自己的访问控制。
package review
