package terminal

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
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
	// DefaultTimeout 限制一次 terminal_send 的等待。
	DefaultTimeout = 30 * time.Second
	// MaxTimeout 限制单次调用的 timeout_ms。
	MaxTimeout = 10 * time.Minute
	// DefaultMaxOutputBytes 限制单次工具响应。
	DefaultMaxOutputBytes = 32 << 10
	// DefaultBufferBytes 是每个会话的输出环容量。
	DefaultBufferBytes = 1 << 20
	// MaxSessions 限制每个管理器同时打开的会话数。
	MaxSessions = 64
	// DefaultReadCount 是省略 count 时 terminal_read 的页大小。
	DefaultReadCount = 500
	// MaxReadCount 限制单次 terminal_read 的页大小。
	MaxReadCount = 2000
	// markerPrefix 是 pipe 传输完成标记的前缀；标记每次发送都唯一，因此
	// 命令输出绝不会与它冲突。
	markerPrefix = "__RIG_DONE_"
)

// 工具名称。它们是稳定的、面向模型的标识符。
const (
	ToolTerminalOpen   = "terminal_open"
	ToolTerminalSend   = "terminal_send"
	ToolTerminalRead   = "terminal_read"
	ToolTerminalSignal = "terminal_signal"
	ToolTerminalClose  = "terminal_close"
)

// Option 配置一个 Manager。
type Option func(*Manager)

// WithPolicy 让每个会话都在沙箱策略下运行：read-only 策略拒绝打开终端，
// terminal_send 用 CheckCommand 校验第一条命令，shell 只继承 CommandEnv。
func WithPolicy(policy *sandbox.Policy) Option {
	return func(manager *Manager) { manager.policy = policy }
}

// WithBackgroundJobs 挂接宿主的后台任务管理器，从而启用
// terminal_send(run_in_background: true)：进行中的发送会登记为任务，其输出
// 可用 job_output 收集，或用 job_kill 停止（后者会向终端的前台进程组发送
// SIGINT）。
func WithBackgroundJobs(manager background.Manager) Option {
	return func(managerOptions *Manager) { managerOptions.background = manager }
}

// WithOwner 设置管理器的默认 owner 标签。此后打开的会话会记录该 owner；
// 宿主可用 Sessions/terminal_list 查看，并用 CloseAllOwned 只清理某个 owner
// 的会话。空字符串表示“无 owner”（也是默认值）。
func WithOwner(owner string) Option {
	return func(manager *Manager) { manager.owner = strings.TrimSpace(owner) }
}

// WithTimeout 设置 terminal_send 的默认等待时长（非正数保留
// DefaultTimeout）。
func WithTimeout(timeout time.Duration) Option {
	return func(manager *Manager) {
		if timeout > 0 {
			manager.defaultTimeout = timeout
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

// WithBufferBytes 设置每个会话的输出环容量（非正数保留
// DefaultBufferBytes）。
func WithBufferBytes(capacity int) Option {
	return func(manager *Manager) {
		if capacity > 0 {
			manager.bufferBytes = capacity
		}
	}
}

// WithMaxSessions 限制同时打开的会话数（非正数保留 MaxSessions）。
func WithMaxSessions(max int) Option {
	return func(manager *Manager) {
		if max > 0 {
			manager.maxSessions = max
		}
	}
}

// Manager 管理终端会话。它可安全并发使用，每个方法都可由多个 goroutine
// 同时调用。
type Manager struct {
	mu sync.Mutex

	sessions map[string]*session
	policy   *sandbox.Policy

	transport  Transport
	background background.Manager
	owner      string

	defaultTimeout time.Duration
	maxOutputBytes int
	bufferBytes    int
	maxSessions    int
	quietPeriod    time.Duration
	stripANSI      bool
	ptyCols        int
	ptyRows        int
	ptyWarned      bool
}

// NewManager 使用包默认值创建一个空的会话管理器：Unix 上使用 PTY 传输
// （可回退 pipe）、80x24 窗口、150ms 静默期、启用 ANSI 清洗。
func NewManager(opts ...Option) *Manager {
	manager := &Manager{
		sessions:       make(map[string]*session),
		transport:      TransportAuto,
		defaultTimeout: DefaultTimeout,
		maxOutputBytes: DefaultMaxOutputBytes,
		bufferBytes:    DefaultBufferBytes,
		maxSessions:    MaxSessions,
		quietPeriod:    DefaultQuietPeriod,
		stripANSI:      true,
		ptyCols:        DefaultPTYCols,
		ptyRows:        DefaultPTYRows,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(manager)
		}
	}
	return manager
}

// Tools 返回绑定到 manager 的五个终端工具（面向组装代码的
// (*Manager).Tools 便捷包装）。
func Tools(manager *Manager) []rig.Tool {
	if manager == nil {
		return nil
	}
	return manager.Tools()
}

// Tools 返回绑定到本管理器的五个终端工具。这些工具共享管理器的会话；
// 应与其他 Rig 工具一起注册。
func (m *Manager) Tools() []rig.Tool {
	return []rig.Tool{
		m.openTool(),
		m.sendTool(),
		m.readTool(),
		m.signalTool(),
		m.closeTool(),
	}
}

// Len 报告已注册会话的数量（运行中或已结束）。
func (m *Manager) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sessions)
}

// OpenOptions 是宿主显式打开一个终端会话的请求。工具路径
// （terminal_open）不需要 owner；宿主可按会话指定 owner，以便之后用
// CloseAllOwned 做作用域清理。
type OpenOptions struct {
	// Type 是后端类型："" / "shell"（默认）、"pty" 或 "pipe"。
	Type string
	// Name 是可选的、owner 本地的显示名。
	Name string
	// Cwd 是初始工作目录（空表示当前目录；沙箱策略下必须在允许根内）。
	Cwd string
	// Shell 覆盖 shell 可执行文件（空表示 /bin/sh）。
	Shell string
	// Owner 是会话的 owner 标签；空表示使用管理器的默认 owner
	// （WithOwner）。
	Owner string
}

// SessionInfo 是一个已注册终端会话的只读快照。Owner 是会话打开时记录的
// owner 标签；未设置 manager/tool owner 时为空字符串。
type SessionInfo struct {
	ID        string
	Name      string
	Owner     string
	Transport string
	Cwd       string
	Running   bool
	Exited    bool
	ExitCode  int
	ExitErr   error
}

// Owner 返回管理器的默认 owner 标签（可能为空）。
func (m *Manager) Owner() string {
	if m == nil {
		return ""
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.owner
}

// Open 使用显式选项打开一个持久终端会话（宿主 API）。它等价于
// terminal_open，但允许指定每个会话的 Owner；返回已登记会话的快照。
func (m *Manager) Open(opts OpenOptions) (SessionInfo, error) {
	if m == nil {
		return SessionInfo{}, errors.New("terminal: nil manager")
	}
	id, _, _, err := m.open(opts.Cwd, opts.Shell, opts.Name, opts.Type, opts.Owner)
	if err != nil {
		return SessionInfo{}, err
	}
	current, err := m.lookup(id)
	if err != nil {
		return SessionInfo{}, err
	}
	return current.info(), nil
}

// Sessions 按会话 id 顺序返回已注册会话（运行中或已结束）的只读快照，
// 包含 owner 标签。
func (m *Manager) Sessions() []SessionInfo {
	if m == nil {
		return nil
	}
	return m.sessionSnapshots()
}

// CloseAll 关闭并回收所有会话（不区分 owner）。它是宿主关闭钩子，可安全
// 多次调用，之后仍可重新打开会话。
func (m *Manager) CloseAll() error {
	m.mu.Lock()
	sessions := make([]*session, 0, len(m.sessions))
	for id, current := range m.sessions {
		sessions = append(sessions, current)
		delete(m.sessions, id)
	}
	m.mu.Unlock()
	_, err := closeSessions(sessions)
	return err
}

// CloseAllOwned 只关闭并回收 owner 标签精确匹配的会话，返回关闭的会话数
// 与聚合错误。owner 为空字符串时匹配未设置 owner 的会话。它是 CloseAll
// 的作用域版本，供一个管理器（或一组共享宿主资源）按 agent/owner 清理时
// 使用；不匹配的会话保持运行。
func (m *Manager) CloseAllOwned(owner string) (int, error) {
	if m == nil {
		return 0, nil
	}
	owner = strings.TrimSpace(owner)
	m.mu.Lock()
	sessions := make([]*session, 0)
	for id, current := range m.sessions {
		if current.owner != owner {
			continue
		}
		sessions = append(sessions, current)
		delete(m.sessions, id)
	}
	m.mu.Unlock()
	return closeSessions(sessions)
}

// closeSessions 关闭一组已从表中移除的会话，返回关闭数量与聚合错误。
func closeSessions(sessions []*session) (int, error) {
	var errs []error
	for _, current := range sessions {
		if err := current.close(); err != nil {
			errs = append(errs, err)
		}
	}
	return len(sessions), errors.Join(errs...)
}

// session 是一个持久 shell（PTY 或 pipe 传输）。
type session struct {
	manager    *Manager
	id         string
	name       string
	owner      string
	transport  Transport
	cmd        *exec.Cmd
	stdin      io.WriteCloser // pipe stdin 写入器或 PTY 主端
	reader     io.Reader      // pipe 读取器或 PTY 主端
	pipeWriter *io.PipeWriter // 仅 pipe 传输
	ptyMaster  *os.File       // 仅 PTY 传输
	filter     *ansiFilter    // 仅 PTY；禁用清洗时为 nil
	cwd        string         // 用于沙箱校验的初始工作目录

	sendMu sync.Mutex // 串行化命令写入 / 完成等待

	mu                sync.Mutex
	notify            chan struct{}
	buffer            *outputBuffer
	consumed          int64
	currentMarker     string
	commandDone       bool
	postMarkerNewline bool
	desynced          bool
	exited            bool
	exitCode          int
	exitErr           error
	closed            bool

	done     chan struct{}
	doneOnce sync.Once
}

// info 返回该会话的只读快照（含 owner 标签）。
func (s *session) info() SessionInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	info := SessionInfo{
		ID:        s.id,
		Name:      s.name,
		Owner:     s.owner,
		Transport: s.transport.String(),
		Cwd:       s.cwd,
		Exited:    s.exited,
		ExitCode:  s.exitCode,
		ExitErr:   s.exitErr,
	}
	info.Running = !s.exited && !s.closed
	return info
}

// ptyStart 是 startPTYCommand 背后的变量，便于测试强制 PTY 失败并覆盖
// pipe 回退路径（生产代码从不重新赋值）。
var ptyStart = startPTYCommand

// open 启动一个持久 shell 并注册它。它返回会话 id、实际使用的传输，以及
// 请求的 PTY 不可用而回退到 pipe 时的警告（每个管理器只报告一次）。
func (m *Manager) open(cwd, shell, name, sessionType, sessionOwner string) (string, Transport, string, error) {
	if runtime.GOOS == "windows" {
		return "", TransportPipe, "", errors.New("terminal: interactive terminal sessions require a unix PTY; Windows ConPTY support is not implemented in this release")
	}
	if m.policy != nil && m.policy.ReadOnly() {
		return "", TransportPipe, "", errors.New("terminal: sandbox policy is read-only; terminals are disabled")
	}
	if len(m.sessionsSlice()) >= m.maxSessions {
		return "", TransportPipe, "", fmt.Errorf("terminal: too many open sessions (limit %d)", m.maxSessions)
	}

	name = strings.TrimSpace(name)
	sessionOwner = strings.TrimSpace(sessionOwner)
	if sessionOwner == "" {
		sessionOwner = m.owner
	}
	if name != "" {
		if err := m.checkName(name); err != nil {
			return "", TransportPipe, "", err
		}
	}
	requestedTransport := m.transport
	if requested := strings.ToLower(strings.TrimSpace(sessionType)); requested != "" {
		switch requested {
		case "shell", "auto":
			requestedTransport = m.transport
		case "pty":
			requestedTransport = TransportPTY
		case "pipe":
			requestedTransport = TransportPipe
		default:
			return "", TransportPipe, "", fmt.Errorf("terminal: unknown terminal type %q (available: shell, pty, pipe)", sessionType)
		}
	}
	shellPath := strings.TrimSpace(shell)
	if shellPath == "" {
		shellPath = "/bin/sh"
	}
	if !filepath.IsAbs(shellPath) {
		resolved, err := exec.LookPath(shellPath)
		if err != nil {
			return "", TransportPipe, "", fmt.Errorf("terminal: shell %q: %w", shellPath, err)
		}
		shellPath = resolved
	}
	workdir, err := m.resolveCwd(cwd)
	if err != nil {
		return "", TransportPipe, "", err
	}
	if m.policy != nil {
		// 允许列表限制会话可以启动哪个 shell 二进制。检查裸基名：策略按
		// 设计拒绝路径形式。
		if err := m.policy.CheckCommand([]string{filepath.Base(shellPath)}, workdir); err != nil {
			return "", TransportPipe, "", fmt.Errorf("terminal: shell %q: %w", shellPath, err)
		}
	}

	usePTY := false
	switch requestedTransport {
	case TransportPipe:
		usePTY = false
	case TransportPTY:
		usePTY = true
	default:
		usePTY = ptySupported()
	}

	var warning string
	if usePTY {
		master, command, ptyErr := ptyStart(shellPath, workdir, m.shellEnv(), m.ptyCols, m.ptyRows)
		if ptyErr != nil {
			warning = m.notePTYFallback(ptyErr)
		} else {
			current := &session{
				manager:   m,
				id:        newID("term-"),
				name:      name,
				owner:     sessionOwner,
				transport: TransportPTY,
				cmd:       command,
				stdin:     master,
				reader:    master,
				ptyMaster: master,
				cwd:       workdir,
				notify:    make(chan struct{}),
				buffer:    newOutputBuffer(m.bufferBytes),
				done:      make(chan struct{}),
			}
			if m.stripANSI {
				current.filter = &ansiFilter{}
			}
			go current.readLoop(master)
			go current.waitLoop()
			if err := m.register(current); err != nil {
				_ = current.close()
				return "", TransportPTY, warning, err
			}
			return current.id, TransportPTY, warning, nil
		}
	}

	current, err := m.openPipeSession(shellPath, workdir, name, sessionOwner)
	if err != nil {
		return "", TransportPipe, warning, err
	}
	if err := m.register(current); err != nil {
		_ = current.close()
		return "", TransportPipe, warning, err
	}
	return current.id, TransportPipe, warning, nil
}

// openPipeSession 启动确定性的、基于标记的 pipe 传输。
func (m *Manager) openPipeSession(shellPath, workdir, name, sessionOwner string) (*session, error) {
	command := exec.Command(shellPath)
	configureProcessGroup(command)
	command.Dir = workdir
	if m.policy != nil {
		command.Env = m.policy.CommandEnv(os.Environ())
	}
	stdin, err := command.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("terminal: shell stdin: %w", err)
	}
	reader, writer := io.Pipe()
	command.Stdout = writer
	command.Stderr = writer
	if err := command.Start(); err != nil {
		_ = stdin.Close()
		_ = writer.Close()
		_ = reader.Close()
		return nil, fmt.Errorf("terminal: start shell %q: %w", shellPath, err)
	}
	current := &session{
		manager:    m,
		id:         newID("term-"),
		name:       name,
		owner:      sessionOwner,
		transport:  TransportPipe,
		cmd:        command,
		stdin:      stdin,
		reader:     reader,
		pipeWriter: writer,
		cwd:        workdir,
		notify:     make(chan struct{}),
		buffer:     newOutputBuffer(m.bufferBytes),
		done:       make(chan struct{}),
	}
	go current.readLoop(reader)
	go current.waitLoop()
	return current, nil
}

// shellEnv 是 shell 进程（PTY 模式）的环境。
func (m *Manager) shellEnv() []string {
	if m.policy != nil {
		return m.policy.CommandEnv(os.Environ())
	}
	return os.Environ()
}

// notePTYFallback 记录 PTY 传输失败，并返回一次性警告文本（首次报告之后
// 为空）。
func (m *Manager) notePTYFallback(err error) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ptyWarned {
		return ""
	}
	m.ptyWarned = true
	return fmt.Sprintf("warning: PTY transport unavailable (%v); using pipe transport", err)
}

// register 把一个已启动会话加入管理器。
func (m *Manager) register(current *session) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.sessions) >= m.maxSessions {
		return fmt.Errorf("terminal: too many open sessions (limit %d)", m.maxSessions)
	}
	if _, exists := m.sessions[current.id]; exists {
		return errors.New("terminal: session id collision")
	}
	m.sessions[current.id] = current
	return nil
}

// sessionsSlice 对已注册会话做快照（用于容量检查）。
func (m *Manager) sessionsSlice() []*session {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*session, 0, len(m.sessions))
	for _, current := range m.sessions {
		out = append(out, current)
	}
	return out
}

// lookup 返回一个已注册会话。
func (m *Manager) lookup(id string) (*session, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, errors.New("terminal: session_id is required")
	}
	m.mu.Lock()
	current, ok := m.sessions[id]
	m.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("terminal: unknown session %q", id)
	}
	return current, nil
}

// remove 注销一个会话。
func (m *Manager) remove(id string) {
	m.mu.Lock()
	delete(m.sessions, id)
	m.mu.Unlock()
}

// send 写入一段输入并等待传输的完成规则，返回自发送开始以来产生的输出。
// submit 会追加 Enter（以及 pipe 标记）；submit=false 写入原始文本，适合
// 控制字符或不完整的 REPL 输入。
func (s *session) send(ctx context.Context, input string, submit bool, timeout time.Duration) (output string, timedOut bool, err error) {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	start, err := s.startSend(input, submit)
	if err != nil {
		return "", false, err
	}
	return s.waitSend(ctx, start, submit, timeout)
}

// startSend 校验会话状态并写入一段输入。它返回本次发送输出开始的绝对
// 缓冲偏移。
func (s *session) startSend(input string, submit bool) (int64, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return 0, errors.New("terminal: session is closed")
	}
	if s.desynced {
		s.mu.Unlock()
		return 0, errors.New("terminal: session is desynchronized after a timeout; close and reopen the terminal")
	}
	if s.exited {
		code := s.exitCode
		s.mu.Unlock()
		return 0, fmt.Errorf("terminal: shell has exited (exit code %d); close and reopen the terminal", code)
	}
	start := s.buffer.Total()
	if s.transport == TransportPipe && submit {
		marker := markerPrefix + randomHex(12) + "__"
		s.currentMarker = marker
		s.commandDone = false
		s.postMarkerNewline = false
		s.mu.Unlock()
		payload := input
		if !strings.HasSuffix(payload, "\n") {
			payload += "\n"
		}
		payload += "printf '" + marker + "\\n'\n"
		if _, writeErr := io.WriteString(s.stdin, payload); writeErr != nil {
			s.markExited(-1, writeErr)
			return 0, fmt.Errorf("terminal: write to shell: %w", writeErr)
		}
		return start, nil
	}
	s.mu.Unlock()
	line := input
	if submit && !strings.HasSuffix(line, "\n") {
		line += "\n"
	}
	if _, err := io.WriteString(s.stdin, line); err != nil {
		s.markExited(-1, err)
		return 0, fmt.Errorf("terminal: write to pty: %w", err)
	}
	return start, nil
}

// waitSend 等待传输的完成规则并返回自 start 以来产生的输出。
// timeout <= 0 表示没有截止时间（后台发送）。
func (s *session) waitSend(ctx context.Context, start int64, submit bool, timeout time.Duration) (string, bool, error) {
	if s.transport == TransportPipe && submit {
		return s.waitSendPipe(ctx, start, timeout)
	}
	return s.waitSendPTY(ctx, start, submit, timeout)
}

// sendTimeoutDeadline 返回一次发送的等待截止时间（零表示没有）。
func sendTimeoutDeadline(timeout time.Duration) time.Time {
	if timeout <= 0 {
		return time.Time{}
	}
	return time.Now().Add(timeout)
}

// waitSendPipe 等待完成标记（pipe 传输）。
func (s *session) waitSendPipe(ctx context.Context, start int64, timeout time.Duration) (output string, timedOut bool, err error) {
	deadline := sendTimeoutDeadline(timeout)
	done, waitErr := s.waitFor(ctx, deadline, func() bool { return s.commandDone || s.exited })
	if waitErr != nil {
		return "", false, waitErr
	}
	if done {
		output = s.takeOutput(start)
		s.mu.Lock()
		exited := s.exited
		code := s.exitCode
		s.mu.Unlock()
		if exited && !strings.Contains(output, "shell exited") {
			output += fmt.Sprintf("\n[terminal: shell exited with code %d]", code)
		}
		return output, false, nil
	}

	// 超时：标记可能稍后才到（命令可能仍在运行），也可能永远不到（消费了
	// stdin 的命令把它吞掉了）。无论哪种情况，命令与输出的配对都不再可信，
	// 因此会话被标记为失同步，必须关闭后重开。
	s.mu.Lock()
	if !s.commandDone && !s.exited {
		s.desynced = true
	}
	s.mu.Unlock()
	output = s.takeOutput(start)
	notice := fmt.Sprintf("\n[timeout: command did not finish within %s]", timeout)
	notice += "\n[terminal: session desynchronized after the timeout; run terminal_close and terminal_open again]"
	return output + notice, true, nil
}

// waitSendPTY 等待终端静默达到配置的静默期（或超时、或 shell 退出）。
func (s *session) waitSendPTY(ctx context.Context, start int64, submit bool, timeout time.Duration) (string, bool, error) {
	quiet := s.manager.quietPeriod
	if quiet <= 0 {
		quiet = DefaultQuietPeriod
	}
	deadline := sendTimeoutDeadline(timeout)
	lastTotal := start
	lastData := time.Now()
	exited := false

	for {
		if !deadline.IsZero() && !time.Now().Before(deadline) {
			break
		}
		s.mu.Lock()
		closed := s.closed
		exited = s.exited
		s.mu.Unlock()
		if closed {
			return "", false, errors.New("terminal: session is closed")
		}
		if exited {
			break
		}
		next := lastData.Add(quiet)
		if !deadline.IsZero() && deadline.Before(next) {
			next = deadline
		}
		ready, waitErr := s.waitFor(ctx, next, func() bool {
			return s.buffer.Total() > lastTotal || s.exited
		})
		if waitErr != nil {
			return "", false, waitErr
		}
		if total := s.buffer.Total(); total > lastTotal {
			lastTotal = total
			lastData = time.Now()
			if ready {
				continue
			}
		}
		if !ready {
			// 静默期或整体截止时间已到。
			if !deadline.IsZero() && !time.Now().Before(deadline) {
				break
			}
			if time.Since(lastData) >= quiet {
				break
			}
		}
	}

	output := s.takeOutput(start)
	s.mu.Lock()
	exited = s.exited
	code := s.exitCode
	s.mu.Unlock()
	if exited {
		output += fmt.Sprintf("\n[terminal: shell exited with code %d]", code)
		return output, false, nil
	}
	if !deadline.IsZero() && !time.Now().Before(deadline) {
		output += fmt.Sprintf("\n[timeout: no quiet period within %s; the command may still be running]", timeout)
		return output, true, nil
	}
	return output, false, nil
}

// --- 后台发送（terminal_send run_in_background） --------------------------

// startBackgroundSend 把一次发送作为受管后台任务启动，并返回任务 id。发送
// 在结束前占用会话唯一的发送名额；在此之前第二次发送会失败。
func (m *Manager) startBackgroundSend(current *session, input string, submit bool) (string, error) {
	if m.background == nil {
		return "", errors.New("terminal: background sends require a background job manager")
	}
	label := current.id + ": " + input
	if len(label) > 120 {
		label = label[:120] + "…"
	}
	var operation *sendOperation
	source := &sendSource{bind: func() *sendOperation { return operation }}
	job, err := m.background.StartOperation(background.OperationSpec{
		Kind:             string(background.KindPTYSend),
		Label:            label,
		OutputLimitBytes: m.outputLimit(),
		Sources:          []background.Source{source},
		Run: func() (background.Operation, error) {
			started, startErr := current.beginBackgroundSend(input, submit)
			if startErr != nil {
				return nil, startErr
			}
			operation = started
			return started, nil
		},
	})
	if err != nil {
		return "", err
	}
	return job.ID(), nil
}

// beginBackgroundSend 预留会话唯一的发送名额，并在后台启动发送。
func (s *session) beginBackgroundSend(input string, submit bool) (*sendOperation, error) {
	if !s.sendMu.TryLock() {
		return nil, errors.New("terminal: another send is already active on this session")
	}
	start, err := s.startSend(input, submit)
	if err != nil {
		s.sendMu.Unlock()
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	operation := &sendOperation{
		session:  s,
		start:    start,
		cursor:   start,
		ctx:      ctx,
		cancel:   cancel,
		done:     make(chan background.Outcome, 1),
		finished: make(chan struct{}),
	}
	go func() {
		defer s.sendMu.Unlock()
		_, timedOut, sendErr := s.waitSend(ctx, start, submit, 0)
		operation.settle(s.backgroundOutcome(ctx, timedOut, sendErr))
	}()
	return operation, nil
}

// backgroundOutcome 把一次已结束的发送映射为 DSH 任务结果词汇。
func (s *session) backgroundOutcome(ctx context.Context, timedOut bool, sendErr error) background.Outcome {
	s.mu.Lock()
	exited, code := s.exited, s.exitCode
	s.mu.Unlock()
	switch {
	case sendErr != nil && ctx.Err() != nil:
		return background.Outcome{Status: background.StatusKilled, Detail: "signal: SIGINT"}
	case sendErr != nil:
		return background.Outcome{Status: background.StatusFailed, Detail: sendErr.Error()}
	case timedOut:
		return background.Outcome{Status: background.StatusFailed, Detail: "timed out"}
	case exited:
		return background.Outcome{Status: background.StatusCompleted, Detail: fmt.Sprintf("session exited: %d", code)}
	default:
		return background.Outcome{Status: background.StatusCompleted, Detail: "wait: inferred_idle"}
	}
}

// sendOperation 是一次进行中的后台发送。
type sendOperation struct {
	session *session
	start   int64
	ctx     context.Context
	cancel  context.CancelFunc
	done    chan background.Outcome
	// finished 在结果入队后关闭，使输出源被唤醒。
	finished chan struct{}

	mu           sync.Mutex
	cursor       int64
	settled      bool
	canceled     bool
	cancelReason string
}

// Cancel 实现 background.Operation：向终端的前台进程组发送 SIGINT（PTY
// 上即 Ctrl-C）并中止等待。
func (op *sendOperation) Cancel(reason string) {
	op.mu.Lock()
	if op.canceled {
		op.mu.Unlock()
		return
	}
	op.canceled = true
	op.cancelReason = reason
	op.mu.Unlock()
	_ = op.session.interrupt()
	op.cancel()
}

// Done 实现 background.Operation。
func (op *sendOperation) Done() <-chan background.Outcome { return op.done }

// settle 记录终态结果、将其入队并唤醒输出源。
func (op *sendOperation) settle(outcome background.Outcome) {
	op.mu.Lock()
	if op.settled {
		op.mu.Unlock()
		return
	}
	op.settled = true
	reason := op.cancelReason
	op.mu.Unlock()
	if outcome.Status == background.StatusKilled && strings.TrimSpace(reason) != "" {
		outcome.Detail = "signal: SIGINT (" + reason + ")"
	}
	op.done <- outcome
	close(op.finished)
}

// takeBuffered 返回自上次交付以来产生的输出，并推进会话的消费游标，使
// terminal_read 不会重复返回。
func (op *sendOperation) takeBuffered() (string, bool) {
	op.mu.Lock()
	from := op.cursor
	op.mu.Unlock()
	total := op.session.buffer.Total()
	if total <= from {
		return "", false
	}
	text, _ := op.session.buffer.Slice(from)
	op.mu.Lock()
	op.cursor = total
	op.mu.Unlock()
	op.session.mu.Lock()
	op.session.consumed = total
	op.session.mu.Unlock()
	return text, true
}

// sendSource 把一次进行中的后台发送适配到任务管理器的拉取源契约。它惰性
// 绑定，因为操作是在任务准入之后、于任务的启动函数内部启动的。
type sendSource struct {
	bind func() *sendOperation
}

// Read 实现 background.Source。
func (src *sendSource) Read(ctx context.Context) (string, bool, error) {
	operation := src.bind()
	if operation == nil {
		return "", true, nil
	}
	for {
		if text, ok := operation.takeBuffered(); ok {
			return text, false, nil
		}
		if text, ok := operation.drain(); ok {
			return text, false, nil
		}
		if operation.isFinished() {
			return "", true, nil
		}
		notify := operation.session.notifyChan()
		select {
		case <-notify:
		case <-operation.finished:
		case <-ctx.Done():
			if text, ok := operation.drain(); ok {
				return text, true, nil
			}
			return "", true, nil
		}
	}
}

// drain 返回上次 takeBuffered 之后到达的输出，并报告操作是否已结束。
func (op *sendOperation) drain() (string, bool) {
	op.mu.Lock()
	from := op.cursor
	op.mu.Unlock()
	total := op.session.buffer.Total()
	if total <= from {
		return "", false
	}
	text, _ := op.session.buffer.Slice(from)
	op.mu.Lock()
	op.cursor = total
	op.mu.Unlock()
	op.session.mu.Lock()
	op.session.consumed = total
	op.session.mu.Unlock()
	return text, true
}

// isFinished 报告操作是否已结束。
func (op *sendOperation) isFinished() bool {
	op.mu.Lock()
	defer op.mu.Unlock()
	return op.settled
}

// interrupt 向终端的前台进程组发送 SIGINT：PTY 上发送 Ctrl-C（0x03），
// 否则向 shell 的进程组发送真实的 SIGINT。
func (s *session) interrupt() error {
	return s.signal(signalSpec{name: "INT"})
}

// notifyChan 返回会话当前的唤醒通道。
func (s *session) notifyChan() chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.notify
}

// --- 分页读取保留输出 ------------------------------------------------------

// readPage 返回一页按最新相对位置定位的保留行：offset 是页的最新相对行
// 偏移（0 = 最新一页），count 是最大行数。
func (s *session) readPage(offset, count int) (text string, total, lineBegin, lineEnd int, truncated bool) {
	data, dropped := s.buffer.Snapshot()
	lines := splitRetainedLines(data)
	total = len(lines)
	if offset < 0 {
		offset = 0
	}
	if count <= 0 {
		count = DefaultReadCount
	}
	count = min(count, MaxReadCount)
	if offset >= total {
		return "", total, offset, offset, dropped
	}
	end := total - offset
	start := end - count
	if start < 0 {
		start = 0
	}
	text = strings.Join(lines[start:end], "\n")
	return text, total, offset, offset + (end - start), dropped || start > 0
}

// splitRetainedLines 把保留输出切成行，并去掉末尾换行产生的空元素。
func splitRetainedLines(data string) []string {
	if data == "" {
		return nil
	}
	lines := strings.Split(data, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// checkName 拒绝重复的存活 owner-local 会话名。
func (m *Manager) checkName(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, current := range m.sessions {
		current.mu.Lock()
		existing := current.name
		closed := current.closed
		current.mu.Unlock()
		if !closed && existing == name {
			return fmt.Errorf("terminal: session name %q is already in use", name)
		}
	}
	return nil
}

// read 返回尚未被之前的读取/发送消费的输出，可选地最多等待 wait 以获取
// 新输出。
func (s *session) read(ctx context.Context, wait time.Duration) (string, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return "", errors.New("terminal: session is closed")
	}
	after := s.consumed
	s.mu.Unlock()

	deadline := time.Now()
	if wait > 0 {
		deadline = time.Now().Add(wait)
	}
	if _, err := s.waitFor(ctx, deadline, func() bool {
		return s.buffer.Total() > after || s.exited
	}); err != nil {
		return "", err
	}
	return s.takeOutput(after), nil
}

// signal 向会话发送一个信号。PTY 上，中断以 Ctrl-C（0x03）形式交付，使
// 终端的前台进程组收到 SIGINT；其他信号发送到 shell 的进程组。
func (s *session) signal(spec signalSpec) error {
	s.mu.Lock()
	exited := s.exited
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return errors.New("terminal: session is closed")
	}
	if exited {
		return errors.New("terminal: shell has exited")
	}
	if s.transport == TransportPTY && isInterruptSignal(spec) {
		if _, err := s.stdin.Write([]byte{0x03}); err != nil {
			return fmt.Errorf("terminal: write Ctrl-C: %w", err)
		}
		return nil
	}
	return signalProcessGroup(s.cmd, spec)
}

// isInterruptSignal 报告 spec 是否表示 SIGINT（或工具暴露的 Ctrl-C
// 简写）。
func isInterruptSignal(spec signalSpec) bool {
	if spec.name == "INT" {
		return true
	}
	return spec.name == "" && spec.number == 2
}

// close 杀死进程组、关闭终端并回收 shell。它是幂等的。
func (s *session) close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	exited := s.exited
	s.mu.Unlock()

	_ = s.stdin.Close()
	if !exited {
		_ = signalProcessGroup(s.cmd, signalSpec{name: "KILL"})
	}
	select {
	case <-s.done:
	case <-time.After(2 * time.Second):
	}
	if s.pipeWriter != nil {
		_ = s.pipeWriter.Close()
	}
	if s.ptyMaster != nil {
		_ = s.ptyMaster.Close()
	}
	return nil
}

// takeOutput 返回从绝对偏移 from 开始的输出，对其施加输出上限，并推进
// 已消费游标。
func (s *session) takeOutput(from int64) string {
	text, _ := s.buffer.Slice(from)
	s.mu.Lock()
	s.consumed = s.buffer.Total()
	s.mu.Unlock()
	return toolutil.TruncateBytes(text, s.manager.outputLimit(), fmt.Sprintf("…\n[truncated: output exceeded %d bytes]\n", s.manager.outputLimit()))
}

// outputLimit 是单次响应上限：管理器上限，策略更严格时再被策略收紧。
func (m *Manager) outputLimit() int {
	limit := m.maxOutputBytes
	if m.policy != nil && m.policy.MaxOutputBytes() < limit {
		limit = m.policy.MaxOutputBytes()
	}
	return limit
}

// waitFor 等待到 ready 为真（在持有会话锁的情况下调用）或截止时间到期。
// 超时返回 false, nil，取消则返回 context 错误。
func (s *session) waitFor(ctx context.Context, deadline time.Time, ready func() bool) (bool, error) {
	for {
		s.mu.Lock()
		if ready() {
			s.mu.Unlock()
			return true, nil
		}
		if !deadline.IsZero() && !time.Now().Before(deadline) {
			s.mu.Unlock()
			return false, nil
		}
		notify := s.notify
		var wait time.Duration
		if !deadline.IsZero() {
			wait = time.Until(deadline)
		}
		s.mu.Unlock()

		var timer *time.Timer
		var timerC <-chan time.Time
		if !deadline.IsZero() {
			if wait < 0 {
				wait = 0
			}
			timer = time.NewTimer(wait)
			timerC = timer.C
		}
		select {
		case <-ctx.Done():
			if timer != nil {
				timer.Stop()
			}
			return false, ctx.Err()
		case <-notify:
			if timer != nil {
				timer.Stop()
			}
		case <-timerC:
		}
	}
}

// wakeLocked 唤醒所有等待者。调用方持有 s.mu。
func (s *session) wakeLocked() {
	close(s.notify)
	s.notify = make(chan struct{})
}

// appendOutput 把字节追加到会话缓冲；当标记行的尾部换行在后续块中到达
// 时将其剥离（pipe 传输）。
func (s *session) appendOutput(data []byte) {
	if len(data) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.postMarkerNewline {
		switch {
		case data[0] == '\n':
			data = data[1:]
			s.postMarkerNewline = false
		case len(data) >= 2 && data[0] == '\r' && data[1] == '\n':
			data = data[2:]
			s.postMarkerNewline = false
		case len(data) == 1 && data[0] == '\r':
			data = nil
			s.postMarkerNewline = false
		default:
			s.postMarkerNewline = false
		}
	}
	if len(data) > 0 {
		_, _ = s.buffer.Write(data)
	}
	s.wakeLocked()
}

// readLoop 读取 shell 输出（pipe 或 PTY 主端），清洗/处理它并追加到输出
// 缓冲。
func (s *session) readLoop(reader io.Reader) {
	var pending []byte
	chunk := make([]byte, 32*1024)
	for {
		n, readErr := reader.Read(chunk)
		if n > 0 {
			if s.transport == TransportPTY {
				data := chunk[:n]
				if s.filter != nil {
					data = s.filter.Feed(data)
				}
				if len(data) > 0 {
					s.appendOutput(data)
				}
			} else {
				pending = s.consumeOutput(chunk[:n], pending)
			}
		}
		if readErr != nil {
			break
		}
	}
	if len(pending) > 0 {
		s.appendOutput(pending)
	}
	// pipe/pty 到达 EOF（shell 已退出或其终端已关闭）。退出状态由
	// waitLoop 拥有，它持有真实退出码；唤醒等待者，使阻塞中的 send/read
	// 在 markExited 执行后立即观察到它。
	s.mu.Lock()
	s.wakeLocked()
	s.mu.Unlock()
}

// consumeOutput 在当前输出块（加上挂起的部分标记字节）中扫描当前的完成
// 标记（仅 pipe 传输）。
func (s *session) consumeOutput(chunk, pending []byte) []byte {
	data := make([]byte, 0, len(pending)+len(chunk))
	data = append(data, pending...)
	data = append(data, chunk...)
	for {
		s.mu.Lock()
		marker := s.currentMarker
		s.mu.Unlock()
		if marker == "" {
			s.appendOutput(data)
			return nil
		}
		markerBytes := []byte(marker)
		if before, rest, found := bytes.Cut(data, markerBytes); found {
			s.appendOutput(before)
			s.mu.Lock()
			s.commandDone = true
			s.currentMarker = ""
			s.postMarkerNewline = true
			s.wakeLocked()
			s.mu.Unlock()
			if len(rest) == 0 {
				return nil
			}
			data = rest
			continue
		}
		keep := len(markerBytes) - 1
		if keep < 0 {
			keep = 0
		}
		if len(data) > keep {
			flush := len(data) - keep
			s.appendOutput(data[:flush])
			return slices.Clone(data[flush:])
		}
		return slices.Clone(data)
	}
}

// waitLoop 回收 shell 进程并记录其退出状态。
func (s *session) waitLoop() {
	waitErr := s.cmd.Wait()
	code := -1
	if waitErr == nil {
		code = 0
	} else {
		if exitErr, ok := errors.AsType[*exec.ExitError](waitErr); ok {
			code = exitErr.ExitCode()
		}
	}
	if s.pipeWriter != nil {
		_ = s.pipeWriter.Close() // 为 pipe 读端产生 EOF
	}
	s.markExited(code, waitErr)
}

// markExited 记录退出状态并唤醒等待者。它是幂等的。
func (s *session) markExited(code int, err error) {
	s.mu.Lock()
	if !s.exited {
		s.exited = true
		s.exitCode = code
		s.exitErr = err
		s.commandDone = true
		s.currentMarker = ""
		s.wakeLocked()
	}
	s.mu.Unlock()
	s.doneOnce.Do(func() { close(s.done) })
}

// resolveCwd 校验终端的工作目录。有策略时目录必须位于某个根内（并以
// 符号链接解析后的形式返回）；没有策略时只需存在即可。
func (m *Manager) resolveCwd(cwd string) (string, error) {
	if m.policy != nil {
		requested := strings.TrimSpace(cwd)
		if requested == "" {
			requested = "."
		}
		abs, err := m.policy.ResolvePath(requested)
		if err != nil {
			return "", fmt.Errorf("terminal: cwd: %w", err)
		}
		info, statErr := os.Stat(abs)
		if statErr != nil {
			return "", fmt.Errorf("terminal: cwd: %w", statErr)
		}
		if !info.IsDir() {
			return "", fmt.Errorf("terminal: cwd %q is not a directory", cwd)
		}
		return abs, nil
	}
	dir := strings.TrimSpace(cwd)
	if dir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("terminal: cwd: %w", err)
		}
		dir = wd
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("terminal: cwd: %w", err)
	}
	info, statErr := os.Stat(abs)
	if statErr != nil {
		return "", fmt.Errorf("terminal: cwd: %w", statErr)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("terminal: cwd %q is not a directory", cwd)
	}
	return abs, nil
}

// checkCommand 依据沙箱命令策略校验一行输入：该行不得包含 shell 操作符
// 或替换，且其第一条命令必须被允许，cwd 位于策略根内。因此交互式 shell
// 是逐行受限的；配置策略时，shell 内建命令（cd、export 等）需要显式加入
// 允许列表。
func (m *Manager) checkCommand(input, cwd string) error {
	if m.policy == nil {
		return nil
	}
	argv, err := m.policy.ParseShellCommand(input)
	if err != nil {
		return err
	}
	if len(argv) == 0 {
		return nil
	}
	return m.policy.CheckCommand(argv, cwd)
}

// newID 返回带给定前缀的短随机标识符。
func newID(prefix string) string { return prefix + randomHex(6) }

// randomHex 返回 n 个随机字节的十六进制编码。
func randomHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return hex.EncodeToString([]byte(time.Now().Format("150405.000000")))
	}
	return hex.EncodeToString(buf)
}
