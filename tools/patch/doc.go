// Package patch 提供 apply_patch 工具：一次原子地应用多文件补丁。
//
// 与逐条替换的 edit_file / str_replace_editor 不同，apply_patch 接收一份
// **补丁文本**（统一 diff 或 Codex 风格补丁），一次调用可以新增、修改、删除、
// 移动多个文件。它的核心承诺是：
//
//   - **全有或全无**：先解析、校验并在内存里算出每个文件的新内容；任何一步失败
//     （补丁语法错、路径不合法、上下文匹配不到或有歧义、文件是二进制……）都
//     一个文件都不改。
//   - **上下文容错**：hunk 里的上下文行按"exact → 忽略行尾空白 → 忽略所有空白"
//     三级回退匹配，因此缩进被编辑器改过、尾随空格被清掉的文件仍可打补丁；
//     三级都匹配不上才报错，并同时给出**期望上下文**与**文件实际内容附近的
//     行号**。同一级出现多处匹配即视为歧义，直接报错，绝不猜。
//   - **路径不许逃逸**：绝对路径、".." 逃逸、符号链接逃逸（EvalSymlinks 之后
//     不在工作区根内）都会被拒绝；工作区根本身不能被删除或替换。
//   - **最小副作用**：保留原文件权限（新文件 0644）、保持原换行风格（CRLF 文件
//     写回 CRLF）、未命中的行逐字节保留。
//
// # 精确契约（易误解的三条）
//
//   - **符号链接**：更新（Update File）会**写穿**符号链接——写入落在链接目标上，
//     链接本身保留；删除（Delete File）与纯移动（Move to 且无 hunk）作用于**链接
//     本身**，与 `rm`/`mv` 一致（带 hunk 的移动是"写新内容 + 删源"）。
//     逃出工作区根的链接一律拒绝；**绝对路径的链接目标也一律拒绝**，即使它指回
//     根内——这是 `os.Root` 在系统调用层的判定（见下文残余风险）。
//   - **纯插入 hunk 不幂等**：只有 "+" 行、没有任何上下文行的 hunk（统一 diff 的
//     `@@ -l,0 +m,n @@`）按行号插入，同一补丁重试会**重复插入**并且仍然报成功。
//     带上下文的 hunk（Codex 风格，以及正常带 context 的统一 diff）在重试时会因为
//     上下文已变而报错，因此是安全的重试单位。
//   - **规模上限**：补丁文本 ≤ 1 MiB（DefaultMaxPatchBytes）、单个目标文件 ≤ 16 MiB
//     （DefaultMaxFileBytes，超过时提示改用 write_file 重写）、单补丁文件数 ≤ 200
//     （DefaultMaxFiles）。三者的超限错误分别对应 WithMaxPatchBytes /
//     WithMaxFileBytes / WithMaxFiles。
//
// # 安全边界与残余风险
//
// 所有针对补丁路径的文件 I/O 都经 internal/fsguard（`os.Root`）执行：读、写、
// 建目录、删除、重命名都是系统调用级的遍历安全操作，因此"校验之后把某个目录换成
// 指向根外的符号链接"这类 TOCTOU 竞态**不会**让写入落到根外（句柄绑定在打开时的
// 目录上）。补丁层保留的 filepath 校验只用于**快速预检 + 可读文案**，不是边界。
//
//   - **残余（Unix）**：`os.Root.Chmod` 有已知竞态（目标在操作中被换成符号链接时
//     可能作用到链接本身）。本工具不单独 Chmod 补丁路径——权限位随
//     WriteFileAtomic 的 perm 参数落定——只在回滚写回时同样走 WriteFileAtomic，
//     因此不额外暴露该竞态。
//   - **绝对符号链接**：`os.Root` 拒绝绝对链接目标（即使指回根内），所以这类
//     文件可以被读取但不能被本工具更新/删除；迁移前基于 filepath 的实现能处理
//     这种布局。需要支持时请把链接改成根内相对形式（`ln -s real.md link.md`）。
//   - **大小写去重是平台近似**：同一补丁内的路径去重在 darwin/windows 上按小写键
//     比较（与 APFS/HFS+/NTFS 的默认行为一致），其它平台按字节比较。挂载参数可以
//     改变真实语义（macOS 的 case-sensitive APFS、Linux 的 vfat/NTFS），此时去重
//     判断可能偏松或偏紧——真正的一致性由 fsguard 的系统调用层保证。
//   - **沙箱策略是可选的**：见 WithPolicy。不传策略时工具只做"工作区根内"围栏，
//     不会遵守只读模式或 deny glob——宿主必须显式传入与其它文件工具相同的
//     *sandbox.Policy，否则它会成为绕过这些规则的写入口。
//
// # 支持的两种格式
//
// 统一 diff（可多文件，a/ b/ 前缀自动剥离）：
//
//	--- a/src/a.go
//	+++ b/src/a.go
//	@@ -10,3 +10,4 @@
//	 context
//	-old
//	+new
//	+extra
//	--- /dev/null
//	+++ b/src/new.go
//	@@ -0,0 +1,2 @@
//	+hello
//
// Codex 风格（更省 token，且能表达移动）：
//
//	*** Begin Patch
//	*** Add File: src/new.go
//	+hello
//	*** Update File: src/a.go
//	@@ func main() {
//	-	old
//	+	new
//	*** Move to: src/b.go
//	*** Delete File: src/old.go
//	*** End Patch
//
// 一个补丁里同一个文件只能出现一次（不合并、不重复应用），出现两次直接报错。
// 各操作按补丁里出现的顺序处理；结果文本按同样顺序列出每个文件的 Op 与
// +N/-M 行数统计。
//
// # 构造与接线
//
//	apply := patch.New("/srv/workspace")                 // 普通模式
//	dryRun := patch.New("/srv/workspace", patch.WithDryRun(true))
//	capped := patch.New(root, patch.WithMaxPatchBytes(256<<10))
//	guarded := patch.New(root, patch.WithPolicy(policy))  // 遵守只读模式/deny glob
//
// 工具只接受**相对**工作区根的路径；root 必须是已存在的目录（不存在时调用会
// 返回可读的错误结果，而不是 panic）。dry_run 只影响落盘，解析与匹配照常执行，
// 因此可以用它先验证补丁再真正应用。
//
// 接入宿主时应当把与其它文件工具同一个 *sandbox.Policy 传进来（WithPolicy）：
// 策略不通过（只读模式、deny glob、保护区）的补丁会整体拒绝，一个文件都不改。
package patch
