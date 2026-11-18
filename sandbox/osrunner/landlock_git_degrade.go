package osrunner

import "sync/atomic"

// Landlock 后端的 git 元数据降级开关。
//
// # 为什么需要这个开关
//
// Landlock 的规则是**并集**语义，没有否定规则：只要某条祖先规则授予了写权限，
// 更深、更严的子路径规则不会收窄它。因此"<ws> 可写，但 <ws>/.git/config 与
// <ws>/.git/hooks 只读"在这个后端上**无法表达**。而 `git commit` 必须在 .git 里
// 新建 index.lock 再改名，缺了整根可写就提交不了。
//
// 于是默认（true）把 `.git` 整根授予 rw —— 这是"git commit 能用"的必要条件，
// 代价是 `.git/hooks/**` 与 `.git/config` 在 **OS 层**可写。这个代价由
// RunnerLimits() 如实上报；真正拦住它的是命令风险层
// （sandbox.ClassifyCommand 的 `write.git.control` 规则：任何往 .git/hooks、
// .git/config、.git/config.worktree、.git/modules/**/config、.git/info 写内容的
// 命令都直接判危险）与进程内围栏（write_file/apply_patch）。
//
// 传 false 回到**严格展开**：`.git` 及其控制面只读，代价是 Landlock 后端下
// `git commit`（以及在已有子目录里新建文件）不可用。宿主应当在装配期调用一次。

// landlockGitDegrade 是进程级开关；默认 true（保持既有行为）。
var landlockGitDegrade atomic.Bool

func init() { landlockGitDegrade.Store(true) }

// WithLandlockGitDegrade 设置 Landlock 后端是否允许 git 元数据降级为整根可写。
//
// 默认 true。传 false 表示"宁可 git commit 不可用，也不要 .git/hooks 在 OS 层
// 可写"——适合把命令风险层当成唯一拦截手段、不希望有降级面的部署。
//
// 它是进程级设置（全局开关），不是 per-policy 配置：装配期调用一次即可。
func WithLandlockGitDegrade(enabled bool) { landlockGitDegrade.Store(enabled) }

// LandlockGitDegradeEnabled 返回当前开关值。
func LandlockGitDegradeEnabled() bool { return landlockGitDegrade.Load() }
