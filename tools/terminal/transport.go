package terminal

import (
	"slices"
	"time"
)

// Transport selects how a terminal session talks to its shell.
type Transport int

const (
	// TransportAuto uses a real PTY on platforms that support it and falls back
	// to the pipe transport (with a one-time warning) when PTY setup fails.
	TransportAuto Transport = iota
	// TransportPTY forces a real pseudo-terminal. If the PTY cannot be opened
	// the Manager falls back to the pipe transport and reports the reason.
	TransportPTY
	// TransportPipe forces the marker-based pipe transport (the v0.4 behavior).
	// It works on every platform and is the right choice for deterministic,
	// non-interactive automation.
	TransportPipe
)

// String implements fmt.Stringer.
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

// PTY defaults.
const (
	// DefaultQuietPeriod is how long terminal_send waits without new output
	// before deciding a command finished in PTY mode.
	DefaultQuietPeriod = 150 * time.Millisecond
	// DefaultPTYCols and DefaultPTYRows are the initial PTY window size.
	DefaultPTYCols = 80
	DefaultPTYRows = 24
)

// WithTransport selects the session transport. The default is TransportAuto:
// a real PTY on unix, pipe elsewhere.
func WithTransport(transport Transport) Option {
	return func(manager *Manager) { manager.transport = transport }
}

// WithPTY is the boolean shorthand of WithTransport: true forces PTY, false
// forces the pipe transport.
func WithPTY(enabled bool) Option {
	transport := TransportPipe
	if enabled {
		transport = TransportPTY
	}
	return func(manager *Manager) { manager.transport = transport }
}

// WithWindowSize sets the initial PTY window size (non-positive keeps the
// 80x24 default). It has no effect on the pipe transport.
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

// WithQuietPeriod sets how long PTY-mode terminal_send waits without new output
// before declaring the command finished (non-positive keeps
// DefaultQuietPeriod).
func WithQuietPeriod(quiet time.Duration) Option {
	return func(manager *Manager) {
		if quiet > 0 {
			manager.quietPeriod = quiet
		}
	}
}

// WithStripANSI controls whether PTY output is sanitized: ANSI escape sequences
// are removed and CRLF is normalized to LF. Default true; the pipe transport
// never alters output.
func WithStripANSI(enabled bool) Option {
	return func(manager *Manager) { manager.stripANSI = enabled }
}

// withDefaultEnv appends name=value when name is not already present.
func withDefaultEnv(env []string, name, value string) []string {
	prefix := name + "="
	for _, entry := range env {
		if len(entry) >= len(prefix) && entry[:len(prefix)] == prefix {
			return env
		}
	}
	return append(slices.Clone(env), prefix+value)
}
