// Package fsguard 提供受限于一棵目录树的安全文件操作层。
//
// 它解决的是"校验与落盘之间的窗口"：用 filepath 拼接 + os.Rename 时，攻击者（或
// 并行进程）可以在"校验路径合法"与"真正写入"之间把某一段换成指向根外的符号链接
// （TOCTOU）。fsguard 把所有操作都经 os.Root 执行——由内核在每次系统调用时保证
// 路径不逃出根、符号链接不得指向根外——因此不存在"检查时合法、使用时逃逸"的窗口。
//
// # 语义决定（调用方必须知道）
//
//   - 路径一律是**根相对**的：绝对路径、以 ".." 开头的路径返回 ErrEscape；"." 表示
//     根本身，读操作允许，写操作（Write/Rename/Remove/MkdirAll/Chmod）返回 ErrEscape。
//   - 读操作**跟随**符号链接（与 os.Root 一致）：Stat/ReadFile 看到的是链接目标。
//   - WriteFileAtomic 写**符号链接的目标**（与 write_file 语义一致）：先 Lstat，若
//     目标是链接则读出链接文本、按根相对解析（绝对路径或逃出根的相对路径 →
//     ErrEscape），再对**解析后的目标**做原子写。链接本身保持不变。
//   - Rename/Remove 作用于**路径本身**，不跟随链接：移动/删除的是那个符号链接，
//     而不是它指向的文件（与 mv/rm 一致，也与 tools/patch 的语义一致）。
//   - Chmod 拒绝符号链接（返回错误），不会悄悄改掉链接目标的权限：os.Root.Chmod
//     本身是跟随的，那会让"给这个路径改权限"意外作用到别处。要改目标权限就显式
//     传目标路径。
//   - 原子写 = 同目录临时文件 → Chmod → Rename；任何一步失败都会清理临时文件，
//     调用方不会留下半成品或垃圾文件。
//
// # 并发
//
// Root 可被多个 goroutine 同时使用（os.Root 本身并发安全）。原子写使用互不相同的
// 临时文件名，因此并发写同一目标只会"最后完成者获胜"，不会写出撕裂的内容。
//
// # 残余风险
//
//   - os.Root.Chmod/Chown/Chtimes 在 Unix 上有已知竞态：如果目标在操作进行时被换成
//     符号链接，操作可能作用在链接本身而不是目标上（见 os 包文档）。fsguard 的
//     WriteFileAtomic 把 Chmod 施加在自己刚创建的临时文件上（名字随机、且以
//     O_EXCL 创建），因此不暴露这个竞态；Chmod 虽然会先 Lstat 拒绝符号链接，但
//     Lstat 与 Chmod 之间仍有微小窗口（stdlib 限制，无法消除）。
//   - 逃逸错误靠 stdlib 的固定错误文案 "path escapes from parent" 归一化（os 没有
//     导出该哨兵）；Go 若改文案，本包的归一化会失效（测试会在 CI 里立刻发现）。
//   - FIFO/socket/设备文件会被 OpenFile 阻塞或产生副作用：fsguard 不判断文件类型，
//     需要"只读普通文件"的调用方应自己 Stat 检查（或在 ReadFileLimit 之外加限制）。
package fsguard
