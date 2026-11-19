package full

// Runtime：把 full 构造出来的**后台句柄**交还给宿主。
//
// # 为什么必须有这个类型
//
// 旧行为：All 在内部 jobs.NewManager / terminal.NewManager，然后只把工具返回给
// 调用方，句柄就地丢弃。结果是一条后台 bash 或 PTY 会话活着，而**全仓没有任何
// 生产路径调用 CloseAll**（只有测试调）——宿主关窗口后子进程继续跑，Windows 上
// 还会锁住工作区。
//
// 现在：NewRuntime 与 All 构造同一套工具，额外把两个管理器交出来；Runtime.Close
// 是"一次关掉所有后台任务"的入口，可重复调用。
//
// # 与 All 的关系（向后兼容）
//
// All(opts) []rig.Tool 的签名**不变**，内部就是 NewRuntime 之后丢掉 Runtime（与
// 旧行为逐字相同）。需要关停能力的宿主改用 NewRuntime；不需要的宿主（例如只是
// 想要一份工具清单做覆盖检查）继续用 All。

import (
	"errors"
	"fmt"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/tools/jobs"
	"github.com/zeokeel/zeokeel-rig/tools/terminal"
)

// Runtime 是一次 full 构造的产物：工具面 + 它背后的后台句柄。
//
// 零值不可用（Tools 为空、两个管理器为 nil）；请用 NewRuntime 构造。
type Runtime struct {
	// Tools 与 All 返回的工具集逐字相同（顺序也相同）。
	Tools []rig.Tool

	jobs      *jobs.Manager
	terminals *terminal.Manager
}

// NewRuntime 构造完整工具集，并把后台句柄一并交给调用方。
//
// 调用方负责在关停时调用 Close()（或对 Jobs()/Terminals() 各自 CloseAll）；
// 不关闭的后果是后台命令/PTY 会话比进程活得更久。
//
// error 目前恒为 nil：构造过程没有可失败的分支（与 All 的历史行为一致）。保留它
// 是为了让宿主能在**一处**处理"组合不完整"这类未来诊断，而不必到时候改签名。
func NewRuntime(opts AllOptions) (Runtime, error) {
	return newRuntime(opts)
}

// Jobs 返回本 Runtime 的后台任务管理器（与 bash 的后台命令、job_* 工具共享）。
func (r Runtime) Jobs() *jobs.Manager { return r.jobs }

// Terminals 返回本 Runtime 的终端会话管理器。
func (r Runtime) Terminals() *terminal.Manager { return r.terminals }

// Close 关闭全部后台任务与终端会话，返回聚合错误。
//
// 语义与两个管理器的 CloseAll 一致：杀进程、回收、清空记录，之后仍可重新开始新
// 任务；可安全重复调用（第二次是 no-op）。
func (r Runtime) Close() error {
	var errs []error
	if r.jobs != nil {
		if err := r.jobs.CloseAll(); err != nil {
			errs = append(errs, fmt.Errorf("full: close background jobs: %w", err))
		}
	}
	if r.terminals != nil {
		if err := r.terminals.CloseAll(); err != nil {
			errs = append(errs, fmt.Errorf("full: close terminal sessions: %w", err))
		}
	}
	return errors.Join(errs...)
}
