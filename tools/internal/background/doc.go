// Package background 定义一个小巧的、面向生产者的后台任务契约，供 shell
// （bash、pwsh）与 terminal 工具把异步工作交给宿主提供的任务管理器。
//
// 该包刻意设为 internal：具体实现位于 tools/jobs（它导入本包），而
// tools/builtin 与 tools/terminal 只依赖这里的接口，因此这三个包彼此不导入，
// 由宿主把它们接在一起：
//
//	background := jobs.NewManager(jobs.WithPolicy(policy))
//	bash := builtin.NewShellWithOptions(root, timeout, builtin.WithBackgroundJobs(background))
//	terminals := terminal.NewManager(terminal.WithBackgroundJobs(background))
//
// 管理器负责任务身份、生命周期、受限输出环、等待、取消与完成通知；生产者只
// 提供进程 argv，或一个不透明操作加上它的增量输出源。
//
// # 核心类型
//
//   - Manager：宿主挂接到 shell/terminal 的入口，提供 StartProcess 与
//     StartOperation，均立即返回 Job 句柄。
//   - ProcessSpec：描述进程型任务（命令文本、argv、目录、环境、超时、输出
//     上限与 PolicyChecked 标记）。
//   - OperationSpec / Operation / Outcome / Source：描述终端发送这类不透明
//     操作，管理器把 Source 的输出泵入任务的受限输出环，生产者通过 Done
//     交付唯一终态。
//   - JobView / Notice / Kind / Status：不可变投影、完成通知与 DSH 兼容的
//     任务种类/状态。
//
// 生产者的命令校验可以选择自己做（PolicyChecked=true），或交给管理器用其
// 沙箱策略的 shell 语法检查完成；无论哪种方式，管理器都会重新应用环境、
// 工作目录与只读约束。
package background
