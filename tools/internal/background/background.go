package background

import (
	"context"
	"time"
)

// Kind 是生产者定义的任务种类。该种类同时作为任务 id 前缀
// （`bash-1a2b3c`、`pty-send-4d5e6f`），并由 job_list 展示。
type Kind string

// Rig 生产者使用的任务种类。
const (
	KindBash    Kind = "bash"
	KindPwsh    Kind = "pwsh"
	KindPTYSend Kind = "pty-send"
)

// Status 是一个任务与 DSH 兼容的生命周期状态：running、可选的
// stopping，然后恰好一个终态。
type Status string

// 任务生命周期状态。
const (
	StatusRunning   Status = "running"
	StatusStopping  Status = "stopping"
	StatusCompleted Status = "completed"
	StatusKilled    Status = "killed"
	StatusFailed    Status = "failed"
)

// Settled 报告状态是否为终态。
func (s Status) Settled() bool {
	switch s {
	case StatusCompleted, StatusKilled, StatusFailed:
		return true
	default:
		return false
	}
}

// JobView 是受管后台任务的一份不可变投影。它从不携带输出；读取输出需
// 单独消费任务的输出环。
type JobView struct {
	// ID 是管理器签发的任务 id（`<kind>-<hex>`）。
	ID string
	// Kind 是任务注册时使用的生产者种类。
	Kind string
	// Label 是生产者提供的一行标签（命令文本）。
	Label string
	// Status 是生命周期状态。
	Status Status
	// Detail 是任务结束后的终态原因（`exit code: 3`），或在任务存活期间
	// 描述它正在做什么。
	Detail string
	// ExitCode 是顶层进程退出码（运行中或生产者没有进程时为 -1）。
	ExitCode int
	// Started 是登记时间。
	Started time.Time
	// Finished 是结束时间（运行中为零值）。
	Finished time.Time
}

// Job 是生产者对某个已启动后台任务的句柄。
type Job interface {
	// ID 返回管理器签发的任务 id。
	ID() string
	// Wait 阻塞到任务结束或 timeout 到期。timeout <= 0 表示“没有截止时间”
	// （仅受 ctx 约束）。它报告任务是否已结束；超时或被取消的等待不会影响
	// 任务，任务继续运行。存活的等待会被登记为“已等待”，因此它收集到的结束
	// 事件不会再产生完成通知。
	Wait(ctx context.Context, timeout time.Duration) (JobView, bool, error)
	// Read 返回自上一次消费性 Read 以来产生的输出，以及一份新的投影。它
	// 从不阻塞。
	Read() (string, JobView)
	// Cancel 请求终止但不等待。原因会被记录并转发给生产者；被取消的任务
	// 会抑制其完成通知，因为调用方已经知道结果。
	Cancel(reason string)
	// Drop 移除一个调用方自行收集的已结束任务（例如前台命令），使模型
	// 永远看不到它的 id，并丢弃待处理的完成通知。对存活任务调用 Drop 会
	// 返回错误。
	Drop() error
}

// Manager 是宿主挂接到 shell 与 terminal 工具的后台任务接口。
// *jobs.Manager 实现了它。
type Manager interface {
	// StartProcess 把一个命令作为受管后台任务启动，并立即返回其句柄。
	StartProcess(spec ProcessSpec) (Job, error)
	// StartOperation 把一个不透明的生产者操作（终端发送）作为受管后台
	// 任务启动，并立即返回其句柄。
	StartOperation(spec OperationSpec) (Job, error)
}

// ProcessSpec 描述一个进程型后台任务。
type ProcessSpec struct {
	// Kind 是生产者种类（KindBash、KindPwsh）。
	Kind string
	// Label 是面向模型的一行标签（命令文本）。
	Label string
	// Command 是原始命令文本。它用于展示，并且当管理器持有沙箱策略而
	// PolicyChecked 为 false 时，也用于策略的 shell 语法校验。
	Command string
	// Argv 是完整的进程参数向量（argv[0] 是二进制）。
	Argv []string
	// Dir 是工作目录；为空表示使用管理器的默认值。
	Dir string
	// Env 是进程环境；nil 表示使用管理器的默认值。存在沙箱策略时，管理器
	// 总是改为应用策略的环境允许列表。
	Env []string
	// Timeout 为正数时，在该时长后杀死任务。后台任务传零；被提升的前台
	// 任务继续运行。
	Timeout time.Duration
	// OutputLimitBytes 限制单次面向模型的输出读取（0 表示管理器默认值）。
	OutputLimitBytes int
	// PolicyChecked 表示生产者已针对管理器持有的同一沙箱策略校验过
	// Command 与 Argv，因此管理器只重新应用环境、工作目录与只读检查。
	PolicyChecked bool
}

// OperationSpec 描述一个以后台任务形式暴露的不透明生产者操作（持久终端
// 发送）。
type OperationSpec struct {
	// Kind 是生产者种类（KindPTYSend）。
	Kind string
	// Label 是面向模型的一行标签。
	Label string
	// OutputLimitBytes 限制单次面向模型的输出读取（0 表示管理器默认值）。
	OutputLimitBytes int
	// Sources 是管理器泵入任务受限输出环的拉取源。任务结束前，管理器会
	// 再把每个源排空一次。
	Sources []Source
	// Run 同步启动操作。返回错误时不会登记任何内容。Run 在准入后只被调用
	// 一次。
	Run func() (Operation, error)
}

// Source 是管理器泵入任务输出环的一个增量输出源。
type Source interface {
	// Read 阻塞到有文本可用、源结束或 ctx 被取消。final 报告不会再有任何
	// 文本到达（最后一次读取时 text 仍可能非空）。返回错误会以 failed 结束
	// 任务。
	//
	// 操作结束后管理器会取消 ctx；取消时 Read 必须先返回它已持有的文本，再
	// 报告 final=true，从而保证结束事件不会丢失生产者的最后一段输出。
	Read(ctx context.Context) (text string, final bool, err error)
}

// Operation 是已启动操作任务的生产者侧。
type Operation interface {
	// Cancel 请求终止；reason 是调用方给出的 job_kill 原因（没有时为空
	// 字符串）。它必须幂等且不得阻塞。
	Cancel(reason string)
	// Done 在操作真正停止（资源已释放）后结束。必须恰好交付一个 Outcome；
	// 管理器只读取一次。
	Done() <-chan Outcome
}

// Outcome 是生产者为一个操作任务报告的终态结果。
type Outcome struct {
	// Status 必须是 StatusCompleted、StatusKilled 或 StatusFailed。
	Status Status
	// Detail 是由 job_output/job_list 渲染的终态原因
	// （`wait: session_exit`、`signal: SIGINT`）。
	Detail string
}

// Notice 是管理器为所属 agent 排队的一条完成通知。宿主（agent 循环）注入
// 或据此唤醒；jobs 工具本身从不消费这些通知。
type Notice struct {
	// JobID 是已结束任务的 id。
	JobID string
	// Kind 是生产者种类。
	Kind string
	// Label 是生产者提供的标签。
	Label string
	// Status 是 DSH 终态。
	Status Status
	// Detail 是终态原因。
	Detail string
	// Text 是可直接注入的面向模型通知。
	Text string
}
