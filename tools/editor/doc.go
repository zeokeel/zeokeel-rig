// Package editor 提供 str_replace_editor 工具：面向 agent 的沙箱文件查看器与
// 精确文本编辑器。
//
// 该工具通过一个 rig.Tool 暴露四个命令：
//
//   - view：以 1 基行号显示文件内容（"N<TAB>文本"）。可选 view_range
//     [start, end] 为包含端点的 1 基行区间，结束值 -1 表示文件末尾；目录不会
//     在此列出（请用 list_dir）。
//   - create：用 file_text 创建新文件；路径已存在时失败，修改已有文件请用
//     str_replace。
//   - str_replace：用 new_str 替换精确匹配的 old_str；除非 replace_all 为
//     true，否则 old_str 必须恰好出现一次，0 次或存在歧义都返回错误。成功时
//     报告替换次数与一行上下文的 diff。
//   - insert：在 1 基行号 insert_line 之后插入 new_str；insert_line 为 0
//     表示在文件开头插入。
//
// 所有路径都经绑定的 sandbox.Policy 解析：允许的根、拒绝 ".."、符号链接约束
// 与拒绝 glob 都会生效；实际 I/O 通过 os.Root 在系统调用相邻层再约束一次。
// 编辑是精确的：不做空白规范化、不使用正则。大于 MaxFileBytes（1 MiB）的文
// 件、二进制文件与目录都会被拒绝。
//
// # 构造与接线
//
//	policy := sandbox.New(
//		sandbox.WithRoots("/srv/workspace"),
//		sandbox.WithMode(sandbox.ModeWorkspaceWrite),
//		sandbox.WithDeniedGlobs("**/.env", "**/*.pem"),
//	)
//	editorTool := editor.New(policy)
//	registry := tools.NewRegistry(editorTool)
//
// New 是唯一构造函数。nil policy 表示 sandbox.New()，即 fail-closed 的默认
// 策略：拒绝所有路径，因此不给根时该工具不可用（文档化的“必须提供根沙箱”
// 约定）。
//
// # 权限模式
//
// 工具本身不读取模式，行为完全由传入的 sandbox.Policy 决定：
//
//   - read-only：view 仍可读取；create/str_replace/insert 返回工具错误，
//     因为策略为只读。
//   - workspace-write（默认）：允许在根内查看、创建与修改文件。
//   - danger-full-access：去掉根与允许列表围栏（显式拒绝 glob 仍然生效），
//     此时写入范围由部署的容器/VM 边界负责。
package editor
