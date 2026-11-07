package terminal

import (
	"slices"
	"time"
)

// Transport 选择终端会话与 shell 通信的方式。
type Transport int

const (
	// TransportAuto 在支持它的平台上使用真实 PTY，PTY 初始化失败时回退到
	// pipe 传输（并给出一次性警告）。
	TransportAuto Transport = iota
	// TransportPTY 强制使用真实伪终端。若 PTY 无法打开，Manager 会回退到
	// pipe 传输并报告原因。
	TransportPTY
	// TransportPipe 强制使用基于标记的 pipe 传输（v0.4 行为）。它可在所有
	// 平台工作，是确定性、非交互自动化的正确选择。
	TransportPipe
)

// String 实现 fmt.Stringer。
func (t Transport) String() string {
	switch t {
	case TransportPTY:
		return "pty"
	case TransportPipe:
		return "pipe"
	default:
		return "auto"
	}
}

// PTY 默认值。
const (
	// DefaultQuietPeriod 是 PTY 模式下 terminal_send 在没有新输出时等待多久
	// 才判定命令结束。
	DefaultQuietPeriod = 150 * time.Millisecond
	// DefaultPTYCols 与 DefaultPTYRows 是初始 PTY 窗口大小。
	DefaultPTYCols = 80
	DefaultPTYRows = 24
)

// WithTransport 选择会话传输方式。默认是 TransportAuto：Unix 上为真实
// PTY，其他平台为 pipe。
func WithTransport(transport Transport) Option {
	return func(manager *Manager) { manager.transport = transport }
}

// WithPTY 是 WithTransport 的布尔简写：true 强制 PTY，false 强制 pipe
// 传输。
func WithPTY(enabled bool) Option {
	transport := TransportPipe
	if enabled {
		transport = TransportPTY
	}
	return func(manager *Manager) { manager.transport = transport }
}

// WithWindowSize 设置初始 PTY 窗口大小（非正数保留 80x24 默认值）。它对
// pipe 传输没有影响。
func WithWindowSize(cols, rows int) Option {
	return func(manager *Manager) {
		if cols > 0 {
			manager.ptyCols = cols
		}
		if rows > 0 {
			manager.ptyRows = rows
		}
	}
}

// WithQuietPeriod 设置 PTY 模式下 terminal_send 在宣告命令结束前等待多久
// 没有新输出（非正数保留 DefaultQuietPeriod）。
func WithQuietPeriod(quiet time.Duration) Option {
	return func(manager *Manager) {
		if quiet > 0 {
			manager.quietPeriod = quiet
		}
	}
}

// WithStripANSI 控制是否清洗 PTY 输出：移除 ANSI 转义序列并把 CRLF
// 归一化为 LF。默认 true；pipe 传输从不改动输出。
func WithStripANSI(enabled bool) Option {
	return func(manager *Manager) { manager.stripANSI = enabled }
}

// withDefaultEnv 在 name 尚未存在时追加 name=value。
func withDefaultEnv(env []string, name, value string) []string {
	prefix := name + "="
	for _, entry := range env {
		if len(entry) >= len(prefix) && entry[:len(prefix)] == prefix {
			return env
		}
	}
	return append(slices.Clone(env), prefix+value)
}
