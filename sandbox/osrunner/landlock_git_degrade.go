package osrunner

import "sync/atomic"

// Landlock 后端的 git 元数据降级开关（**历史 API，已无渲染效果**）。
//
// # 历史背景
//
// 早期版本为了让 `git commit` 在 Landlock 下可用，把 `.git` 整根降级为可写；
// 传 false 则回到"严格展开"（.git 及其控制面只读、`git commit` 不可用）。
//
// # 现在
//
// DSH 对齐后 Landlock 的授权就是 `--rw <workspace>`：工作区**整根**可写，没有任何
// "根内只读子路径"的展开，`.git` 自然可写（与 bwrap / Seatbelt / Windows 一致）。
// 开关不再影响渲染，保留这两个导出函数只为源码兼容：
//
//   - WithLandlockGitDegrade 只记录一个进程级布尔值；
//   - LandlockGitDegradeEnabled 返回它（默认 true）。
//
// 代码里不要再依赖它做判断：需要"某个子路径只读"请用进程内围栏
// （`Policy.CheckWritablePath`）或命令风险层。

// landlockGitDegrade 是进程级开关；默认 true（保持既有行为）。
var landlockGitDegrade atomic.Bool

func init() { landlockGitDegrade.Store(true) }

// WithLandlockGitDegrade 记录一个进程级布尔值（默认 true）。
//
// Deprecated: 该开关不再影响任何渲染——对齐 DSH 后 Landlock 授权工作区整根，
// 没有"降级"这一步。保留函数只为源码兼容。
func WithLandlockGitDegrade(enabled bool) { landlockGitDegrade.Store(enabled) }

// LandlockGitDegradeEnabled 返回当前开关值。
func LandlockGitDegradeEnabled() bool { return landlockGitDegrade.Load() }
