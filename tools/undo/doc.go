// Package undo 提供"撤销上一回合文件改动"的能力，以及一个面向用户的 undo 工具。
//
// # 产品定位
//
// 这是**个人 agent 客户端**里的"撤销按钮"：用户在 UI 上点一下，工作区回到本轮开始
// 之前的样子。模型只应在用户明确要求撤销时调用 undo 工具，不要主动用它收拾自己的改动
// （工具描述里写明了这一点）。
//
// 实现上不再自己做"全树快照 + 内容备份"——那种设计是为"不可信并发写入 + 崩溃一致性"
// 准备的，在单用户客户端里是过度实现。个人客户端**绝大多数**工作区是 git 仓库，而 git
// 已经提供了内容寻址存储、增量对象、大文件/二进制、.gitignore 与跨平台路径处理。因此
// 默认后端是 internal/gitsnap 的一层薄封装：把"回合"映射成 git 隐藏提交。
//
// 工作区不是 git 仓库时，用 NewFileTracker 挂**纯文件系统快照**（kit 里是
// `Config.UndoDir`）：契约完全一致（同样的 Turns/Changes/Revert），只是快照变成了
// `<Dir>/turns/<turn>/files/<rel>` 下的文件副本（清单在同一回合目录的 `manifest.json`
// 里）。两条后端都由 Tracker 统一编排回合号，调用方不需要
// 判断后端类型。
//
// # 工作方式
//
// 回合开始前调用 Take（或 Tracker.Begin）：git 后端用"复制用户的 index + `git add -A`"
// 生成一个工作区树，再用 `git commit-tree` 造一个**隐藏提交**（不移动 HEAD、不碰用户
// 分支、不改用户暂存区），并挂到 `refs/rig/undo/` 下避免被 gc 回收；文件后端则把工作区
// 里的普通文件复制到 `<Dir>/turns/<turn>/files/<rel>`（保留相对路径与权限位），并在
// `<Dir>/turns/<turn>/manifest.json` 写一份清单。清单与镜像分属两个命名空间，工作区里
// 叫 `manifest.json` 的文件不会与清单撞名。
//
// 之后：
//
//   - Changes：git 后端对比快照树与当前工作区树；文件后端对比清单与工作区（含大小/内容
//     与权限位）。两者都给出路径级变更（add/modify/delete，含增删行数）；
//   - Revert：把工作区恢复到快照——修改/删除的文件从快照写回，回合里新建的文件删除，
//     被删除的文件恢复（git 后端还会把 `git add` 过的新文件从暂存区移除）。
//
// 回退是路径级的、**只影响本回合**：快照之前的用户改动（哪怕是脏的、已暂存的）都在
// 快照里，因此不会被撤销。
//
// # 回合号与重启
//
// Tracker 的回合号从 1 开始递增。后端目录里有一份 `state.json`（`{"turn":N}`）记录
// 最近分配过的回合号：NewFileTracker 打开同一个 Dir 时会读回来继续（Dir 里已有的回合
// 目录也参与推算），git 后端在 Options.Dir 非空时同样持久化（kit 默认
// `<Root>/.rig/undo`）。Dir 为空时回合号只活在进程内，重启后从 1 重新开始。
//
// # 暂存区语义（重要，容易被误解）
//
// 快照记录的是**工作区内容**（复制用户 index 后跑 `git add -A`），不是用户当时的
// index 内容。因此回退把工作区与暂存区**都恢复成"回合开始前的工作区状态"**：
//
//   - 如果你在回合前 stage 了改动、并且工作区没有再改（index == worktree）：回退后
//     这份暂存内容仍然是暂存的，内容一致、状态不变；
//   - 如果你 stage 了 v2 之后又把工作区改成 v3（index != worktree，即"部分暂存"），
//     回退后工作区与 index 都是 v3——**v2 那份精细的暂存状态会被展平**（v2 的内容
//     仍在 git 对象库里，只是不再挂在 index 上）。
//
// 给用户的建议：**想保留精细的暂存状态，请在回合开始前先 `git commit` 或 `git stash`**；
// 只要工作区与 index 一致，撤销就不会影响你的暂存意图。
//
// TODO（暂不做）：精确恢复"回合前的 index"需要在快照里额外记录一条 index 树
// （`git stash create` 的第二父就是它），回退时对未提交路径 `git read-tree`。代价是
// 快照多一个对象 + 回退多一步，而单用户客户端里"部分暂存"属于少数场景，先按文档说明
// 处理。
//
// # 边界
//
//   - git 后端需要 git：非 git 工作区返回 ErrNotGitRepo，宿主可以改用 NewFileTracker
//     （IsGitRepo 可以提前判断）。
//   - `.gitignore` 语义由 git 负责：被忽略的文件既不进快照，也不会被回退删除。
//   - 工作区根可以是仓库的子目录：此时只覆盖该子树，变更路径相对工作区根。
//   - 子模块（gitlink）只报告、不回退（Change.Reason 说明原因）。
//   - 目录不跟踪：回退删除文件后可能留下空目录；目录本身不会新建或重建。
//   - 回退不跟随符号链接：快照里的文件如果在本回合被换成了符号链接/目录/设备文件，
//     它只会以 warning 列出（`TypeChange` + `Reason`），不会被写入或删除；父目录是
//     符号链接时同样拒绝写入（否则会写到工作区外）。只读（0444）目标可以回退——写回
//     走"先删再以 O_EXCL 创建"，权限位按清单恢复。
//   - 不 unstage 用户自己的改动（只处理本回合涉及的路径）；回退"恢复"的文件会同时写回
//     暂存区，因此 git status 会显示为本回合改动被撤销后的状态。
//   - 快照依赖工作区当时的 index 与工作区内容；如果回合中有人手工 rebase/reset 了
//     仓库，快照仍可回退文件内容，但不会回滚提交历史。
//   - 快照引用（`refs/rig/undo/*`）默认保留最近 128 个，每次 Take 自动修剪；宿主可用
//     Options.RefRetain 调小，或用 Prune（或 Tracker.Prune）在退出/清空历史时主动收紧。
//   - **文件后端的边界**：不做增量，每个回合复制一份完整工作区（保留 MaxTurns 个）；
//     `FileOptions.MaxBytes`（默认 4 MiB）以上的文件只记一条"未快照"，undo 结果里以
//     warning 列出且不会被改动；符号链接、设备文件等非常规文件不参与快照；二进制文件
//     照常复制，只是增删行数记为 0；跳过 `.git` 与快照目录自身。文件后端不知道
//     `.gitignore`（没有 git），工作区里所有普通文件都会被快照。
//   - 同一工作区只推荐**一个** undo 使用者（一个 Tracker / 一个进程）：git 后端的引用
//     与回合映射是进程内状态，两个进程各自 Begin 会互相覆盖对方的"最近回合"理解（文件
//     后端的回合目录共享；每次 Begin 都会重读 `state.json` 并取 max(内存, 磁盘)，因此
//     顺序的第二个实例不会撞号，但真正并发写仍是"最后写的赢"）。
//
// # 组装
//
//	tracker := undo.NewTracker(workspaceRoot, undo.Options{})            // git 后端
//	other, err := undo.NewFileTracker(workspaceRoot, undo.FileOptions{   // 非 git 后端
//		Dir: filepath.Join(workspaceRoot, ".rig", "undo"),
//	})
//	tools := []rig.Tool{undo.New(tracker)}
//	// 每回合开始前（宿主钩子）：
//	turn, err := tracker.Begin()
//	// 用户点"撤销"时：
//	changes, err := tracker.Revert(turn)
//
// kit 的 GitUndo 会在每个 Run 开始前自动 Begin；非 git 工作区用 UndoDir 走文件后端。
package undo
