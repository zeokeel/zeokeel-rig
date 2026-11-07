// Package builtin 提供 ZeoKeel Rig 的内置工具：六个文件工具
// （read_file、write_file、edit_file、list_dir、glob、grep）与两种 shell 工具
// （bash、pwsh）。它们刻意只依赖 Go 标准库、保持无状态：每次调用都重新打开工具
// 根，调用之间不保留文件句柄或工作目录，因此同一工具可安全并发使用。
//
// # 文件工具
//
// 每个文件工具在构造时绑定到一个根目录（或 sandbox.Policy）。路径参数可以是
// 相对根的路径，也可以是根内的绝对路径；解析到根外的路径一律拒绝，包括包含
// ".." 的路径和指向根外的符号链接。实现使用 os.Root 在系统调用相邻层校验每个
// 路径分量（并拒绝逃逸的符号链接），而不是自行重实现路径规范化。使用 os.Root
// 时需要注意：符号链接必须是相对链接；即使指向根内的绝对符号链接也会被拒绝，
// 因此这些工具只接受相对链接。
//
// 各文件工具的模型可见参数与上限：
//
//   - read_file：path（必填）、offset（1 基起始行）、limit（最大行数）；输出
//     上限为 MaxReadLines 行、每行 MaxReadLineBytes 字节、总计 MaxReadBytes。
//   - write_file：path、content；自动创建缺失的父目录，已有文件会被覆盖。
//   - edit_file：path、old_string、new_string、replace_all（可选）；默认要求
//     old_string 恰好出现一次，缺失或存在歧义都返回错误。
//   - list_dir：path（可选，默认工具根）；目录在前（带尾部 /）、文件在后，
//     各自按名称排序，最多 MaxDirEntries 项。
//   - glob：pattern（必填）、path（可选）；* 与 ? 在单个路径段内匹配，**
//     可跨任意多段；结果按路径排序并以 MaxGlobMatches 为上限。
//   - grep：pattern（必填，Go 正则）、include（可选文件名 glob）、path
//     （可选）；返回 file:line:text，最多 MaxGrepMatches 行；大于
//     MaxGrepFileBytes 的文件会被跳过，以免读取二进制与生成物。
//
// # Shell 工具
//
// bash 以 /bin/sh -c 运行命令；pwsh 以
// <binary> -NoProfile -NonInteractive -Command 运行脚本。两者都合并
// stdout/stderr、把输出限制为 MaxShellOutputBytes 字节、在结果末尾追加退出码，
// 并把超时视为工具错误（非零退出按 DSH 语义作为结果返回，供模型解读）。
// shell 工具是文档化的例外：它只依据根（或策略）校验 workdir；shell 命令仍可
// 使用绝对路径或启动其他程序，因此标准库无法沙箱化 shell。审批 gate
// （rig.Gate）才是 shell 的真正边界，根目录只是便利与误配置防护，不是安全沙箱。
//
// # 沙箱策略与权限模式
//
// NewFS/NewShell/NewBuiltins 保留 v0.2 行为：单根、不做命令或环境过滤、以审批
// 作为 shell 边界。NewWithPolicy(p) 在 sandbox.Policy 之上构建同样的工具：
// 多根、权限模式、命令允许/拒绝列表、环境过滤、输出/超时限制与被拒绝的路径
// glob。策略模式下：
//
//   - 文件工具通过 Policy.ResolvePath/OpenPath 解析每个路径（os.Root 约束）；
//   - 权限模式为 read-only 时，write_file/edit_file 拒绝运行，glob/grep/
//     list_dir 隐藏被拒绝的路径；
//   - bash/pwsh 使用策略的命令校验、环境白名单、超时上限与输出上限。
//
// 权限模式通过 sandbox.WithMode 选择：read-only 禁止文件变更并禁用 shell；
// workspace-write（默认）只允许在根内写文件、只允许执行允许列表中的命令；
// danger-full-access 去掉根与允许列表围栏（显式拒绝列表仍然生效）。nil policy
// 表示 sandbox.New()，即拒绝一切。
//
// # 后台执行
//
// 挂接宿主提供的后台任务管理器（BackgroundJobs，即 *jobs.Manager）后，bash 与
// pwsh 支持 DSH 后台执行：run_in_background 立即返回任务 id；前台命令达到超时
// 后会被提升为后台任务，而不是被杀死。宿主通过 NewShellWithOptions/
// NewWithPolicyOptions 的 WithBackgroundJobs 选项或 PwshConfig.BackgroundJobs
// 完成接线；未挂接时工具仅支持前台执行。
//
// # 输出上限
//
// 面向模型的文本都有上限，防止单次调用淹没上下文。上限以导出常量给出；被截断
// 的输出以 "[truncated: ...]" 提示结尾，说明截掉了什么。
//
// # 基本用法
//
//	policy := sandbox.New(
//		sandbox.WithRoots("/srv/workspace"),
//		sandbox.WithMode(sandbox.ModeWorkspaceWrite),
//		sandbox.WithCommandAllowlist("git", "go", "ls"),
//	)
//	background := jobs.NewManager(jobs.WithPolicy(policy))
//	all := builtin.NewWithPolicyOptions(policy, builtin.WithBackgroundJobs(background))
//	registry := tools.NewRegistry(all...)
package builtin
