package jobs

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/sandbox"
	"github.com/zeokeel/zeokeel-rig/tools/internal/background"
	"github.com/zeokeel/zeokeel-rig/tools/internal/toolutil"
)

// 默认值与上限。它们被导出，便于宿主与测试在不重复数字的前提下断言文档化
// 行为。
const (
	// DefaultBufferBytes 是每个任务的输出环容量。
	DefaultBufferBytes = 1 << 20
	// DefaultMaxOutputBytes 限制单次工具响应。
	DefaultMaxOutputBytes = 32 << 10
	// DefaultWaitTimeout 在调用未传 timeout_ms 时限制 job_output(wait) 与
	// job_wait 的等待时长。
	DefaultWaitTimeout = 30 * time.Second
	// MaxWaitTimeout 限制单次调用的等待 timeout_ms。
	MaxWaitTimeout = 10 * time.Minute
	// DefaultMaxJobs 限制每个管理器保留的任务表大小。
	DefaultMaxJobs = 64
	// DefaultTailLines 是 job_wait 返回的结尾行数。
	DefaultTailLines = 100
	// killGrace 是 job_kill/CloseAll 等待被杀死进程结束的时长。
	killGrace = 2 * time.Second
	// waitDelay 在进程退出后不久关闭继承的 stdio 管道。
	waitDelay = time.Second
	// operationDrainGrace 限制任务结束等待操作输出源交付最后一段输出、随后
	// 关闭输出环的时长。
	operationDrainGrace = 2 * time.Second
)

// 工具名称。它们是稳定的、面向模型的标识符。
const (
	ToolJobStart  = "job_start"
	ToolJobOutput = "job_output"
	ToolJobKill   = "job_kill"
	ToolJobList   = "job_list"
	ToolJobWait   = "job_wait"
)

// errJobFinished 在向已结束的任务发送信号/终止请求时返回。
var errJobFinished = errors.New("jobs: job has already finished")

// IsFinishedError 报告 err 是否只表示任务已经结束。此时工具调用方仍会返回
// 最终状态。
func IsFinishedError(err error) bool { return err != nil && errors.Is(err, errJobFinished) }

// Option 配置一个 Manager。
type Option func(*Manager)

// WithPolicy 为此管理器的每个任务应用沙箱策略。
func WithPolicy(policy *sandbox.Policy) Option {
	return func(manager *Manager) { manager.policy = policy }
}

// WithBufferBytes 设置每个任务的输出环容量（非正数保留
// DefaultBufferBytes）。
func WithBufferBytes(capacity int) Option {
	return func(manager *Manager) {
		if capacity > 0 {
			manager.bufferBytes = capacity
		}
	}
}

// WithMaxOutputBytes 限制单次工具响应（非正数保留 DefaultMaxOutputBytes）。
func WithMaxOutputBytes(maxBytes int) Option {
	return func(manager *Manager) {
		if maxBytes > 0 {
			manager.maxOutputBytes = maxBytes
		}
	}
}

// WithMaxJobs 限制保留的任务表大小（非正数保留 DefaultMaxJobs）。
func WithMaxJobs(max int) Option {
	return func(manager *Manager) {
		if max > 0 {
			manager.maxJobs = max
		}
	}
}

// WithWaitTimeout 设置 job_output(wait)/job_wait 的默认超时（非正数保留
// DefaultWaitTimeout）。
func WithWaitTimeout(timeout time.Duration) Option {
	return func(manager *Manager) {
		if timeout > 0 {
			manager.waitTimeout = timeout
		}
	}
}

// WithNoticeHandler 注册完成通知回调。当任务在没有等待者的情况下结束时，
// 回调会在自己的 goroutine 上运行。注册回调是可选的——PendingNotices 无需
// 回调也能工作——不想要通知的宿主可以两者都不使用。
func WithNoticeHandler(handler func(Notice)) Option {
	return func(manager *Manager) { manager.noticeHandler = handler }
}

// Notice 为只导入 tools/jobs 的宿主重新导出后台完成通知类型。
type Notice = background.Notice

// Manager 管理后台任务。它可安全并发使用。
type Manager struct {
	mu sync.Mutex

	jobs map[string]*job
	// policy 是可选的；nil 表示没有沙箱策略（此时宿主的审批 gate 是唯一
	// 边界）。
	policy *sandbox.Policy

	bufferBytes    int
	maxOutputBytes int
	maxJobs        int
	waitTimeout    time.Duration

	notices       []background.Notice
	noticeHandler func(background.Notice)
}

// NewManager 使用包默认值创建一个空的任务管理器。
func NewManager(opts ...Option) *Manager {
	manager := &Manager{
		jobs:           make(map[string]*job),
		bufferBytes:    DefaultBufferBytes,
		maxOutputBytes: DefaultMaxOutputBytes,
		maxJobs:        DefaultMaxJobs,
		waitTimeout:    DefaultWaitTimeout,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(manager)
		}
	}
	return manager
}

// Tools 返回绑定到 manager 的五个任务工具（面向组装代码的
// (*Manager).Tools 便捷包装）。
func Tools(manager *Manager) []rig.Tool {
	if manager == nil {
		return nil
	}
	return manager.Tools()
}

// Tools 返回绑定到本管理器的五个任务工具。
func (m *Manager) Tools() []rig.Tool {
	return []rig.Tool{
		m.startTool(),
		m.outputTool(),
		m.killTool(),
		m.listTool(),
		m.waitTool(),
	}
}

// Len 报告保留的任务数量（运行中与已结束）。
func (m *Manager) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.jobs)
}

// PendingNotices 返回并清空已排队的完成通知。使用轮询（而非
// WithNoticeHandler）的宿主在每个 agent 步骤后调用它，把已结束的后台任务
// 转成会话内消息。
func (m *Manager) PendingNotices() []background.Notice {
	m.mu.Lock()
	defer m.mu.Unlock()
	notices := m.notices
	m.notices = nil
	return notices
}

// CloseAll 杀死所有运行中的任务，等待它们被回收并清空任务表。它是宿主
// 关闭钩子，可安全重复调用。
func (m *Manager) CloseAll() error {
	m.mu.Lock()
	current := make([]*job, 0, len(m.jobs))
	for id, running := range m.jobs {
		current = append(current, running)
		delete(m.jobs, id)
	}
	m.notices = nil
	m.mu.Unlock()
	var errs []error
	for _, running := range current {
		running.mu.Lock()
		running.teardown = true
		exited := running.exited
		running.mu.Unlock()
		if !exited {
			if err := running.terminate("teardown"); err != nil && !IsFinishedError(err) {
				errs = append(errs, err)
			}
		}
	}
	for _, running := range current {
		select {
		case <-running.done:
		case <-time.After(killGrace):
			errs = append(errs, fmt.Errorf("jobs: job %s did not exit after kill", running.id))
		}
	}
	return errors.Join(errs...)
}

// StartProcess 实现 background.Manager：把一个命令作为受管后台任务启动，
// 并立即返回其句柄。
func (m *Manager) StartProcess(spec background.ProcessSpec) (background.Job, error) {
	if strings.TrimSpace(spec.Kind) == "" {
		return nil, errors.New("jobs: background job kind is required")
	}
	if len(spec.Argv) == 0 {
		return nil, errors.New("jobs: background job argv is required")
	}
	current, err := m.startExec(processRequest{
		kind:          spec.Kind,
		label:         spec.Label,
		command:       spec.Command,
		argv:          spec.Argv,
		cwd:           spec.Dir,
		env:           spec.Env,
		timeout:       spec.Timeout,
		outputLimit:   spec.OutputLimitBytes,
		policyChecked: spec.PolicyChecked,
	})
	if err != nil {
		return nil, err
	}
	return current, nil
}

// StartOperation 实现 background.Manager：把一个生产者拥有的操作作为受管
// 后台任务启动，并立即返回其句柄。
func (m *Manager) StartOperation(spec background.OperationSpec) (background.Job, error) {
	if strings.TrimSpace(spec.Kind) == "" {
		return nil, errors.New("jobs: background job kind is required")
	}
	if spec.Run == nil {
		return nil, errors.New("jobs: operation run function is required")
	}
	if len(spec.Sources) == 0 {
		return nil, errors.New("jobs: operation output source is required")
	}
	m.mu.Lock()
	if len(m.jobs) >= m.maxJobs {
		limit := m.maxJobs
		m.mu.Unlock()
		return nil, fmt.Errorf("jobs: too many retained jobs (limit %d); list and let the host clean up", limit)
	}
	m.mu.Unlock()

	operation, err := spec.Run()
	if err != nil {
		return nil, fmt.Errorf("jobs: start operation %q: %w", spec.Kind, err)
	}
	if operation == nil || operation.Done() == nil {
		return nil, errors.New("jobs: operation returned no done channel")
	}
	current := &job{
		manager:         m,
		id:              newJobIDForKind(spec.Kind),
		kind:            spec.Kind,
		label:           spec.Label,
		command:         spec.Label,
		started:         time.Now(),
		buffer:          newOutputBuffer(m.bufferBytes),
		done:            make(chan struct{}),
		exitCode:        -1,
		outputLimit:     spec.OutputLimitBytes,
		cancelOperation: operation.Cancel,
		operationDone:   operation.Done(),
	}
	current.pumpCtx, current.pumpCancel = context.WithCancel(context.Background())
	m.mu.Lock()
	m.jobs[current.id] = current
	m.mu.Unlock()
	go current.pumpOperation(spec.Sources, operation)
	return current, nil
}

// start 为 job_start 工具启动一个后台 shell 进程。
func (m *Manager) start(command, cwd string, timeout time.Duration) (*job, error) {
	if runtime.GOOS == "windows" {
		return nil, errors.New("jobs: background shell jobs require a unix /bin/sh (Unix-only in this release); use the pwsh tool on Windows")
	}
	return m.startExec(processRequest{
		kind:     string(background.KindBash),
		label:    command,
		command:  command,
		argv:     []string{"/bin/sh", "-c", command},
		cwd:      cwd,
		timeout:  timeout,
		idPrefix: "job-",
	})
}

// processRequest 是要启动的一个进程任务（遗留 job_start 或生产者的后台
// 运行）。
type processRequest struct {
	kind          string
	label         string
	command       string
	argv          []string
	cwd           string
	env           []string
	timeout       time.Duration
	outputLimit   int
	policyChecked bool
	idPrefix      string
}

// startExec 启动一个由管理器拥有的进程任务。
func (m *Manager) startExec(request processRequest) (*job, error) {
	command := strings.TrimSpace(request.command)
	if command == "" {
		command = strings.TrimSpace(request.label)
	}
	if command == "" {
		return nil, errors.New("jobs: command is required")
	}
	if strings.ContainsRune(command, 0) {
		return nil, errors.New("jobs: command contains a NUL byte")
	}
	for _, arg := range request.argv {
		if strings.ContainsRune(arg, 0) {
			return nil, errors.New("jobs: command argument contains a NUL byte")
		}
	}
	if m.policy != nil && m.policy.ReadOnly() {
		return nil, errors.New("jobs: sandbox policy is read-only; background jobs are disabled")
	}
	m.mu.Lock()
	if len(m.jobs) >= m.maxJobs {
		limit := m.maxJobs
		m.mu.Unlock()
		return nil, fmt.Errorf("jobs: too many retained jobs (limit %d); list and let the host clean up", limit)
	}
	m.mu.Unlock()

	workdir, err := m.resolveCwd(request.cwd)
	if err != nil {
		return nil, err
	}
	timeout := request.timeout
	if m.policy != nil {
		if !request.policyChecked && request.kind == string(background.KindBash) {
			// 用策略的 POSIX 解析器校验生产者的命令文本；pwsh 保留生产者侧
			// 的检查，因为 PowerShell 方言不是 POSIX。
			argv, parseErr := m.policy.ParseShellCommand(command)
			if parseErr != nil {
				return nil, parseErr
			}
			if err := m.policy.CheckCommand(argv, workdir); err != nil {
				return nil, err
			}
		}
		if timeout > m.policy.Timeout() {
			timeout = m.policy.Timeout()
		}
	}

	cmd := exec.Command(request.argv[0], request.argv[1:]...)
	configureProcessGroup(cmd)
	cmd.Dir = workdir
	cmd.WaitDelay = waitDelay
	switch {
	case m.policy != nil:
		cmd.Env = m.policy.CommandEnv(os.Environ())
	case request.env != nil:
		cmd.Env = request.env
	}
	output := newOutputBuffer(m.bufferBytes)
	cmd.Stdout = output
	cmd.Stderr = output
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("jobs: start %q: %w", command, err)
	}
	prefix := request.idPrefix
	if prefix == "" {
		prefix = request.kind + "-"
	}
	current := &job{
		manager:     m,
		id:          newJobIDWithPrefix(prefix),
		kind:        request.kind,
		label:       request.label,
		command:     command,
		cmd:         cmd,
		started:     time.Now(),
		buffer:      output,
		done:        make(chan struct{}),
		exitCode:    -1,
		outputLimit: request.outputLimit,
	}
	go current.waitLoop(timeout)
	m.mu.Lock()
	m.jobs[current.id] = current
	m.mu.Unlock()
	return current, nil
}

// job 是一个后台进程或操作及其保留的输出。
type job struct {
	manager *Manager
	id      string
	kind    string
	label   string
	command string
	cmd     *exec.Cmd
	started time.Time
	buffer  *outputBuffer
	done    chan struct{}

	// 仅操作型任务使用
	cancelOperation func(reason string)
	operationDone   <-chan background.Outcome
	pumpCtx         context.Context
	pumpCancel      context.CancelFunc

	outputLimit int

	doneOnce sync.Once
	mu       sync.Mutex
	exited   bool
	exitCode int
	exitErr  error
	ended    time.Time
	timedOut bool
	// killRequested 在模型（job_kill/terminal_signal）或宿主（CloseAll）
	// 请求停止任务时置位；它把任务标记为 stopping 并抑制其完成通知。
	killRequested bool
	killReason    string
	status        background.Status
	detail        string
	consumed      int64
	waiters       int
	teardown      bool
	dropped       bool
}

// waitLoop 回收进程，并执行任务超时。
func (j *job) waitLoop(timeout time.Duration) {
	waitDone := make(chan error, 1)
	go func() { waitDone <- j.cmd.Wait() }()
	if timeout <= 0 {
		j.finishProcess(<-waitDone)
		return
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case err := <-waitDone:
		j.finishProcess(err)
	case <-timer.C:
		j.mu.Lock()
		if !j.exited {
			j.timedOut = true
		}
		j.mu.Unlock()
		_ = signalProcessGroup(j.cmd, signalSpec{name: "KILL"})
		j.finishProcess(<-waitDone)
	}
}

// pumpOperation 把每个输出源泵入输出环，监视生产者的结果并结束任务。
func (j *job) pumpOperation(sources []background.Source, operation background.Operation) {
	var wg sync.WaitGroup
	var errMu sync.Mutex
	var sourceErr error
	for _, source := range sources {
		if source == nil {
			continue
		}
		wg.Add(1)
		go func(source background.Source) {
			defer wg.Done()
			for {
				text, final, err := source.Read(j.pumpCtx)
				if text != "" {
					_, _ = j.buffer.Write([]byte(text))
				}
				if err != nil {
					errMu.Lock()
					if sourceErr == nil {
						sourceErr = err
					}
					errMu.Unlock()
					return
				}
				if final {
					return
				}
			}
		}(source)
	}

	outcome := <-j.operationDone
	// 通知泵交付最后一段缓冲文本后停止，并限制任务结束等待行为异常的源
	// 的时长，超时后关闭输出环。
	if j.pumpCancel != nil {
		j.pumpCancel()
	}
	drained := make(chan struct{})
	go func() {
		wg.Wait()
		close(drained)
	}()
	select {
	case <-drained:
	case <-time.After(operationDrainGrace):
	}
	errMu.Lock()
	readErr := sourceErr
	errMu.Unlock()
	if readErr != nil && outcome.Status == background.StatusCompleted {
		outcome = background.Outcome{Status: background.StatusFailed, Detail: readErr.Error()}
	}
	j.finishOperation(outcome)
}

// finishProcess 记录进程任务的退出状态并结束它。
func (j *job) finishProcess(err error) {
	code := -1
	if err == nil {
		code = 0
	} else {
		if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
			code = exitErr.ExitCode()
		}
	}
	j.mu.Lock()
	if j.exited {
		j.mu.Unlock()
		return
	}
	j.exited = true
	j.exitCode = code
	j.exitErr = err
	j.ended = time.Now()
	status, detail := background.StatusCompleted, fmt.Sprintf("exit code: %d", code)
	switch {
	case j.timedOut:
		status, detail = background.StatusFailed, "timed out"
	case j.killRequested:
		status, detail = background.StatusKilled, killedDetail(j.killReason)
	}
	j.status = status
	j.detail = detail
	awaited := j.waiters > 0
	j.mu.Unlock()
	j.doneOnce.Do(func() { close(j.done) })
	j.manager.publishNotice(j, awaited)
}

// finishOperation 记录操作任务的终态结果。
func (j *job) finishOperation(outcome background.Outcome) {
	status := outcome.Status
	switch status {
	case background.StatusCompleted, background.StatusKilled, background.StatusFailed:
	default:
		status = background.StatusFailed
	}
	detail := strings.TrimSpace(outcome.Detail)
	j.mu.Lock()
	if j.exited {
		j.mu.Unlock()
		return
	}
	j.exited = true
	j.ended = time.Now()
	j.status = status
	if detail != "" {
		j.detail = detail
	} else {
		j.detail = string(status)
	}
	if status == background.StatusKilled {
		j.killRequested = true
		if j.killReason != "" {
			j.detail = killedDetail(j.killReason)
		}
	}
	awaited := j.waiters > 0
	j.mu.Unlock()
	j.doneOnce.Do(func() { close(j.done) })
	j.manager.publishNotice(j, awaited)
}

// killedDetail 渲染被取消任务的终态说明。
func killedDetail(reason string) string {
	reason = strings.TrimSpace(reason)
	if reason == "" || reason == "teardown" {
		return "killed"
	}
	return "killed: " + reason
}

// publishNotice 排队（并可选投递）一条完成通知，除非该结束事件已被等待、
// 被模型杀死、被丢弃或属于关闭流程。
func (m *Manager) publishNotice(current *job, awaited bool) {
	current.mu.Lock()
	if !current.exited || awaited || current.killRequested || current.teardown || current.dropped {
		current.mu.Unlock()
		return
	}
	notice := background.Notice{
		JobID:  current.id,
		Kind:   current.kind,
		Label:  current.label,
		Status: current.status,
		Detail: current.detail,
	}
	notice.Text = noticeText(notice)
	current.mu.Unlock()

	m.mu.Lock()
	m.notices = append(m.notices, notice)
	handler := m.noticeHandler
	m.mu.Unlock()
	if handler != nil {
		go handler(notice)
	}
}

// noticeText 渲染一条 DSH 形态的完成通知。
func noticeText(notice background.Notice) string {
	status := "[status: " + string(notice.Status)
	if notice.Detail != "" {
		status += ", " + notice.Detail
	}
	status += "]"
	label := notice.Label
	if label == "" {
		label = notice.JobID
	}
	return fmt.Sprintf("background job %s (%s: %s) finished %s. Read its output with job_output.",
		notice.JobID, notice.Kind, label, status)
}

// terminate 请求取消任务（进程任务：杀死整个进程组；操作任务：把原因转发
// 给生产者）并把它标记为 stopping。
func (j *job) terminate(reason string) error {
	j.mu.Lock()
	if j.exited {
		j.mu.Unlock()
		return errJobFinished
	}
	j.killRequested = true
	if j.killReason == "" {
		j.killReason = reason
	}
	cancel := j.cancelOperation
	cmd := j.cmd
	j.mu.Unlock()
	if cancel != nil {
		cancel(reason)
		return nil
	}
	return signalProcessGroup(cmd, signalSpec{name: "KILL"})
}

// signal 向任务发送一个信号：进程任务发送真实信号，操作任务转发生产者取消
// （其取消含义是工具级中断，例如向终端的前台进程组发送 Ctrl-C）。
func (j *job) signal(spec signalSpec) error {
	j.mu.Lock()
	if j.exited {
		j.mu.Unlock()
		return errJobFinished
	}
	cancel := j.cancelOperation
	j.mu.Unlock()
	if cancel != nil {
		reason := "signal " + spec.label()
		j.mu.Lock()
		j.killRequested = true
		if j.killReason == "" {
			j.killReason = reason
		}
		j.mu.Unlock()
		cancel(reason)
		return nil
	}
	if err := signalProcessGroup(j.cmd, spec); err != nil {
		return err
	}
	j.mu.Lock()
	if !j.exited {
		j.killRequested = true
		if j.killReason == "" {
			j.killReason = "signal " + spec.label()
		}
	}
	j.mu.Unlock()
	return nil
}

// jobStatus 是任务的一份不可变快照。
type jobStatus struct {
	state    string
	status   background.Status
	detail   string
	exitCode int
	duration time.Duration
}

// snapshot 返回当前状态。
func (j *job) snapshot() jobStatus {
	j.mu.Lock()
	defer j.mu.Unlock()
	duration := time.Since(j.started)
	if j.exited {
		duration = j.ended.Sub(j.started)
	}
	state := "running"
	status := background.StatusRunning
	switch {
	case !j.exited:
		if j.killRequested {
			status = background.StatusStopping
		}
	case j.timedOut:
		state = "timed_out"
		status = background.StatusFailed
	case j.status == background.StatusKilled:
		state = "killed"
		status = background.StatusKilled
	case j.status == background.StatusFailed:
		state = "failed"
		status = background.StatusFailed
	default:
		state = "exited"
		status = background.StatusCompleted
	}
	exitCode := -1
	if j.exited {
		exitCode = j.exitCode
	}
	detail := j.detail
	if detail == "" && !j.exited {
		detail = "running"
	}
	return jobStatus{state: state, status: status, detail: detail, exitCode: exitCode, duration: duration}
}

// dshStatus 渲染带说明的 DSH 风格状态词。
func (s jobStatus) dshStatus() string {
	if s.detail == "" {
		return string(s.status)
	}
	return fmt.Sprintf("%s (%s)", s.status, s.detail)
}

// newOutput 返回自上一次新输出读取以来产生的输出，并推进游标。
func (j *job) newOutput() string {
	j.mu.Lock()
	from := j.consumed
	j.mu.Unlock()
	text, _ := j.buffer.Slice(from)
	j.mu.Lock()
	j.consumed = j.buffer.Total()
	j.mu.Unlock()
	return j.capOutput(text)
}

// outputFrom 返回从绝对偏移开始的输出。
func (j *job) outputFrom(offset int64) string {
	text, _ := j.buffer.Slice(offset)
	return j.capOutput(text)
}

// tailOutput 返回结尾若干行并排空新输出游标。
func (j *job) tailOutput() string {
	text := j.buffer.LastLines(DefaultTailLines)
	j.mu.Lock()
	j.consumed = j.buffer.Total()
	j.mu.Unlock()
	return j.capOutput(text)
}

// capOutput 应用每个任务面向模型的输出上限。
func (j *job) capOutput(text string) string {
	limit := j.modelOutputLimit()
	return toolutil.TruncateBytes(text, limit, fmt.Sprintf("…\n[truncated: output exceeded %d bytes]\n", limit))
}

// modelOutputLimit 是单次响应上限：管理器上限，被生产者的上限收紧，策略
// 更严格时再被策略收紧。
func (j *job) modelOutputLimit() int {
	limit := j.manager.outputLimit()
	if j.outputLimit > 0 && j.outputLimit < limit {
		limit = j.outputLimit
	}
	return limit
}

// view 返回该任务在后台契约中的投影。
func (j *job) view() background.JobView {
	snapshot := j.snapshot()
	j.mu.Lock()
	started := j.started
	ended := j.ended
	j.mu.Unlock()
	view := background.JobView{
		ID:       j.id,
		Kind:     j.kind,
		Label:    j.label,
		Status:   snapshot.status,
		Detail:   snapshot.detail,
		ExitCode: snapshot.exitCode,
		Started:  started,
	}
	if !ended.IsZero() {
		view.Finished = ended
	}
	return view
}

// format 渲染状态头与输出。
func (j *job) format(output string) string {
	status := j.snapshot()
	var builder strings.Builder
	fmt.Fprintf(&builder, "job_id: %s\n", j.id)
	fmt.Fprintf(&builder, "kind: %s\n", j.kind)
	if j.label != "" && j.label != j.command {
		fmt.Fprintf(&builder, "label: %s\n", j.label)
	}
	fmt.Fprintf(&builder, "command: %s\n", j.command)
	fmt.Fprintf(&builder, "state: %s\n", status.state)
	fmt.Fprintf(&builder, "status: %s\n", status.dshStatus())
	fmt.Fprintf(&builder, "exit_code: %d\n", status.exitCode)
	fmt.Fprintf(&builder, "duration_ms: %d\n", status.duration.Milliseconds())
	if output == "" {
		builder.WriteString("output: (none)\n")
	} else {
		builder.WriteString("output:\n")
		builder.WriteString(output)
		if !strings.HasSuffix(output, "\n") {
			builder.WriteString("\n")
		}
	}
	return builder.String()
}

// ID 实现 background.Job。
func (j *job) ID() string { return j.id }

// isFinished 报告任务是否已经结束。
func (j *job) isFinished() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.exited
}

// Wait 实现 background.Job：阻塞到任务结束或超时；它把本次等待登记为
// “已等待”，因此结束事件不会产生通知。
func (j *job) Wait(ctx context.Context, timeout time.Duration) (background.JobView, bool, error) {
	j.mu.Lock()
	j.waiters++
	j.mu.Unlock()
	defer func() {
		j.mu.Lock()
		j.waiters--
		j.mu.Unlock()
	}()
	settled, err := j.waitFor(ctx, timeout)
	return j.view(), settled, err
}

// Read 实现 background.Job：返回自上一次消费性读取以来产生的输出，以及
// 一份新的投影。
func (j *job) Read() (string, background.JobView) {
	return j.newOutput(), j.view()
}

// Cancel 实现 background.Job。
func (j *job) Cancel(reason string) { _ = j.terminate(reason) }

// Drop 实现 background.Job：移除调用方自行收集的已结束任务，并丢弃它
// 待处理的完成通知。
func (j *job) Drop() error {
	j.mu.Lock()
	if !j.exited {
		j.mu.Unlock()
		return errors.New("jobs: cannot drop a running job")
	}
	j.dropped = true
	j.mu.Unlock()
	j.manager.removeJob(j.id)
	return nil
}

// removeJob 注销一个任务并丢弃其待处理通知。
func (m *Manager) removeJob(id string) {
	m.mu.Lock()
	delete(m.jobs, id)
	notices := m.notices[:0]
	for _, notice := range m.notices {
		if notice.JobID != id {
			notices = append(notices, notice)
		}
	}
	m.notices = notices
	m.mu.Unlock()
}

// waitFor 阻塞到任务结束或超时；ctx 取消会中止等待但不影响任务。
// timeout <= 0 表示仅受 context 约束的等待。
func (j *job) waitFor(ctx context.Context, timeout time.Duration) (bool, error) {
	if timeout <= 0 {
		select {
		case <-j.done:
			return true, nil
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-j.done:
		return true, nil
	case <-ctx.Done():
		return false, ctx.Err()
	case <-timer.C:
		return false, nil
	}
}

// waitJob 等待单个任务，并把本次等待计为“已等待”（面向模型的等待工具
// 使用它，因此它们收集到的结束事件不会产生通知）。
func (m *Manager) waitJob(ctx context.Context, current *job, timeout time.Duration) (bool, error) {
	current.mu.Lock()
	current.waiters++
	current.mu.Unlock()
	defer func() {
		current.mu.Lock()
		current.waiters--
		current.mu.Unlock()
	}()
	return current.waitFor(ctx, timeout)
}

// wait 是遗留 job_wait/job_kill 工具使用的管理器级等待。
func (m *Manager) wait(ctx context.Context, current *job, timeout time.Duration) (bool, error) {
	return m.waitJob(ctx, current, timeout)
}

// outputLimit 是单次响应上限：管理器上限，策略更严格时再被策略收紧。
func (m *Manager) outputLimit() int {
	limit := m.maxOutputBytes
	if m.policy != nil && m.policy.MaxOutputBytes() < limit {
		limit = m.policy.MaxOutputBytes()
	}
	return limit
}

// lookup 查找一个保留的任务。
func (m *Manager) lookup(id string) (*job, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, errors.New("jobs: job_id is required")
	}
	m.mu.Lock()
	current, ok := m.jobs[id]
	m.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("jobs: unknown job %q", id)
	}
	return current, nil
}

// jobsSnapshot 返回按启动时间排序的保留任务。
func (m *Manager) jobsSnapshot() []*job {
	m.mu.Lock()
	out := make([]*job, 0, len(m.jobs))
	for _, current := range m.jobs {
		out = append(out, current)
	}
	m.mu.Unlock()
	slices.SortFunc(out, func(a, b *job) int {
		return cmp.Or(a.started.Compare(b.started), strings.Compare(a.id, b.id))
	})
	return out
}

// resolveCwd 校验任务工作目录。有策略时默认值是主根，且目录必须位于某个
// 根内；没有策略时默认值是进程工作目录。
func (m *Manager) resolveCwd(cwd string) (string, error) {
	if m.policy != nil {
		requested := strings.TrimSpace(cwd)
		if requested == "" {
			requested = "."
		}
		abs, err := m.policy.ResolvePath(requested)
		if err != nil {
			return "", fmt.Errorf("jobs: cwd: %w", err)
		}
		info, statErr := os.Stat(abs)
		if statErr != nil {
			return "", fmt.Errorf("jobs: cwd: %w", statErr)
		}
		if !info.IsDir() {
			return "", fmt.Errorf("jobs: cwd %q is not a directory", cwd)
		}
		return abs, nil
	}
	dir := strings.TrimSpace(cwd)
	if dir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("jobs: cwd: %w", err)
		}
		dir = wd
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("jobs: cwd: %w", err)
	}
	info, statErr := os.Stat(abs)
	if statErr != nil {
		return "", fmt.Errorf("jobs: cwd: %w", statErr)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("jobs: cwd %q is not a directory", cwd)
	}
	return abs, nil
}

// newJobIDForKind 返回以生产者种类为前缀的任务 id。
func newJobIDForKind(kind string) string { return newJobIDWithPrefix(kind + "-") }

// newJobIDWithPrefix 返回前缀加六个随机字节的十六进制编码。
func newJobIDWithPrefix(prefix string) string { return prefix + randomHex(6) }

// randomHex 返回 n 个随机字节的十六进制编码。
func randomHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return hex.EncodeToString([]byte(strconv.FormatInt(time.Now().UnixNano(), 10)))
	}
	return hex.EncodeToString(buf)
}
