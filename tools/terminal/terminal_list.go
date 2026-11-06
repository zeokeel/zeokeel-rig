package terminal

import (
	"context"
	"fmt"
	"sort"
	"strings"

	v1 "github.com/zeokeel/zeokeel-rig/contracts/v1"
)

// ToolTerminalList is the model-facing name of the session listing tool.
const ToolTerminalList = "terminal_list"

// sessionSnapshot is one read-only view of a registered terminal session.
type sessionSnapshot struct {
	ID        string
	Transport Transport
	Cwd       string
	Running   bool
	Exited    bool
	ExitCode  int
	ExitErr   error
}

// TerminalListTool returns the terminal_list tool bound to manager: it lists the
// manager's registered sessions (running or exited) with their id, transport,
// working directory, running flag and exit code.
//
// The legacy terminal surface predates the terminal_* tools and had a single
// listing tool; hosts can register it alone or through ToolsWithList.
func NewTerminalListTool(manager *Manager) v1.Tool {
	if manager == nil {
		return nil
	}
	return manager.terminalListTool()
}

// TerminalListTool is the method form of NewTerminalListTool.
func (m *Manager) TerminalListTool() v1.Tool { return NewTerminalListTool(m) }

// ToolsWithList returns the five terminal tools plus terminal_list. Hosts that
// expose the legacy tool use this instead of Tools().
func (m *Manager) ToolsWithList() []v1.Tool {
	if m == nil {
		return nil
	}
	return append(m.Tools(), m.terminalListTool())
}

// ToolsWithList is the package-level form of (*Manager).ToolsWithList.
func ToolsWithList(manager *Manager) []v1.Tool { return manager.ToolsWithList() }

// terminalListTool builds the terminal_list tool.
func (m *Manager) terminalListTool() v1.Tool {
	return &terminalTool{
		name: ToolTerminalList,
		description: "List the terminal sessions of this manager with session_id, transport (pty|pipe), " +
			"working directory, whether the shell is still running, and its exit code once it exited.",
		parameters: objectSchema(map[string]any{}),
		run: func(ctx context.Context, call v1.ToolCall) (v1.ToolResult, error) {
			if err := ctx.Err(); err != nil {
				return v1.ToolResult{}, err
			}
			sessions := m.sessionSnapshots()
			if len(sessions) == 0 {
				return textResult("no terminal sessions"), nil
			}
			blocks := make([]string, 0, len(sessions))
			for _, session := range sessions {
				blocks = append(blocks, formatSessionSnapshot(session))
			}
			return textResult(strings.Join(blocks, "\n\n")), nil
		},
	}
}

// sessionSnapshots returns the registered sessions in id order.
func (m *Manager) sessionSnapshots() []sessionSnapshot {
	m.mu.Lock()
	sessions := make([]*session, 0, len(m.sessions))
	for _, current := range m.sessions {
		sessions = append(sessions, current)
	}
	m.mu.Unlock()

	snapshots := make([]sessionSnapshot, 0, len(sessions))
	for _, current := range sessions {
		current.mu.Lock()
		snapshot := sessionSnapshot{
			ID:        current.id,
			Transport: current.transport,
			Cwd:       current.cwd,
			Exited:    current.exited,
			ExitCode:  current.exitCode,
			ExitErr:   current.exitErr,
		}
		snapshot.Running = !current.exited && !current.closed
		current.mu.Unlock()
		snapshots = append(snapshots, snapshot)
	}
	sort.Slice(snapshots, func(i, j int) bool { return snapshots[i].ID < snapshots[j].ID })
	return snapshots
}

// formatSessionSnapshot renders one session block.
func formatSessionSnapshot(session sessionSnapshot) string {
	exit := "-"
	if session.Exited {
		exit = fmt.Sprintf("%d", session.ExitCode)
		if session.ExitErr != nil {
			exit += " (" + session.ExitErr.Error() + ")"
		}
	}
	return strings.Join([]string{
		"session_id: " + session.ID,
		"transport: " + session.Transport.String(),
		"cwd: " + session.Cwd,
		fmt.Sprintf("running: %t", session.Running),
		"exit: " + exit,
	}, "\n")
}
