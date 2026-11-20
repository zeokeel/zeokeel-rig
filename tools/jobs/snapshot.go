package jobs

import "time"

// JobSnapshot 是一个后台任务的宿主可见状态（模型看到的是文本，这里给 UI/自动化）。
//
// 它等价于 job_list/job_output 渲染的状态头，但**不经过人读文本**：宿主画任务
// 列表、状态栏或做自动化判定时读这里，不要去解析 "state: exited" 这类文本（正文
// 里出现 `state:` 就会歧义）。字段语义与内部 background.JobView 对齐：State 是
// job_list 展示的状态词，ExitCode 在任务存活期间为 -1，Finished 零值表示还在跑。
type JobSnapshot struct {
	// ID 是管理器签发的任务 id（`<kind>-<hex>`）。
	ID string
	// Command 是启动任务时的命令文本（操作型任务为空）。
	Command string
	// State 是生命周期状态：running、exited、killed、failed，或超时被杀的
	// timed_out（与 job_list 的 state= 字段逐字一致）。
	State string
	// ExitCode 是顶层进程退出码（运行中为 -1）。
	ExitCode int
	// Started 是任务登记时间。
	Started time.Time
	// Finished 是任务结束时间（运行中为零值）。
	Finished time.Time
	// OutputBytes 是任务已产出的输出字节总数（环形缓冲丢弃旧字节后仍然单调
	// 增长，可用于展示"产出了多少"）。
	OutputBytes int
}

// Snapshot 返回所有保留任务的宿主视图，按 Started 升序排列，同一时刻按 ID
// 稳定排序。返回的切片是拷贝，宿主可以随意持有。nil Manager 返回 nil。
func (m *Manager) Snapshot() []JobSnapshot {
	if m == nil {
		return nil
	}
	current := m.jobsSnapshot()
	out := make([]JobSnapshot, 0, len(current))
	for _, running := range current {
		status := running.snapshot()
		running.mu.Lock()
		ended := running.ended
		running.mu.Unlock()
		entry := JobSnapshot{
			ID:          running.id,
			Command:     running.command,
			State:       status.state,
			ExitCode:    status.exitCode,
			Started:     running.started,
			OutputBytes: int(running.buffer.Total()),
		}
		if !ended.IsZero() {
			entry.Finished = ended
		}
		out = append(out, entry)
	}
	return out
}
