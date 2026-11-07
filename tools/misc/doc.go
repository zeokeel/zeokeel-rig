// Package misc 提供若干自包含的小工具，把会话本地状态保存在内存中：
// working_directory（在工作区根目录内导航）与 todo_write（模型的待办清单）。
//
// 两个工具刻意有状态，但状态只存在于工具值（或其 store）中，绝不使用包级变量：宿主为每个
// agent 创建一个工具实例，工具可安全并发调用。状态是进程内、重启即丢失的，这与它们的用途
// 一致（一次运行内的草稿空间）。
//
// # 模型可见工具
//
//   - working_directory {path 可选}：省略 path 时查询当前目录（输出相对根目录的路径）；
//     给出 path 时切换到该已存在的目录。相对路径按当前目录解析，绝对路径必须位于根目录
//     内；不允许用 ".." 或符号链接逃逸根目录，报告给模型的路径始终相对根目录。
//   - todo_write {todos 必填}：用新列表整体替换会话待办清单并渲染为 "[ ]/[~]/[x]" 检查
//     清单；每项含 id、content 必填，status（pending/in_progress/completed）与可选
//     priority；传空数组清空清单，单项列表最多 MaxTodos=200 条。
//   - todo：todo_write 的旧名别名，两者共享同一个 TodoStore（用同一 store 构造时）。
//
// # 组装与权限
//
//	reg := tools.NewRegistry(
//		misc.NewWorkingDirectory(root),
//		misc.NewTodoTool(nil), // nil = 新建内存 store
//		misc.NewTodoAliasTool(nil),
//	)
//
// 工具不访问网络或 shell；working_directory 的文件系统访问被限制在构造时给定的根目录内
// （使用 os.Root 语义）。审批在 agent 层由 rig.Gate 控制。注意状态按工具实例保存：多会话
// 宿主应为每个会话单独构造工具，或由产品层自行做会话隔离。
package misc
