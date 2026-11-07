// Package worktree 提供 enter_worktree、exit_worktree 与 list_worktrees 三个工具：受
// sandbox.Policy 约束的 git worktree 管理。
//
// 主沙箱根目录必须是 Git 仓库根目录。所有 Git 变更都通过系统 `git` 可执行文件完成（绝不
// 手工改文件），从而让 Git 始终是 worktree 元数据的唯一事实来源。工具路径经
// Policy.ResolvePath 解析，因此目录穿越、符号链接逃逸与被禁 glob 会在命令执行前被拒绝。
//
// # 模型可见工具
//
//   - enter_worktree {path 可选, branch 可选}：基于当前 HEAD 新建分支与 worktree。path 是
//     沙箱内的检出目录（必须尚不存在），省略时使用 <repository>/.rig/worktrees/<branch>；
//     branch 省略时由 path 基名推导或自动生成。主沙箱根必须是 Git 仓库根且仓库至少有一个
//     提交；未提交的修改留在主检出中。结果报告创建的路径、分支与基线提交。
//   - exit_worktree {path 必填, keep_branch 可选}：移除已登记的 worktree 检出，默认保留其
//     分支；keep_branch=false 时同时删除分支。
//   - list_worktrees {}：以 PATH/BRANCH/HEAD/STATE 表格列出仓库的 Git worktree；主检出
//     标记 main，位于 .rig/worktrees 下的链接检出标记 managed，并标出 detached、bare、
//     locked、prunable 等情况。只读。
//
// # 组装、限制与权限
//
// 通过 worktree.Tools(policy) 构造（nil policy 为失败关闭：sandbox.New 拒绝所有路径）；
// tools/full.All 已自动注册。单次 git 调用默认 30s 超时、上限 2 分钟；git 输出与 stderr
// 摘录、工具响应均有字节上限。只读策略下 enter/exit 被拒绝。若 `git` 未安装、路径逃逸
// 沙箱或工作区不是 Git 仓库，工具返回可读的模型错误（ToolResult.IsError=true），而不是 Go
// 错误。工具不使用网络，审批在 agent 层由 rig.Gate 控制。
package worktree
