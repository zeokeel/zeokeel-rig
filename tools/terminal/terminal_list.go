package terminal

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/tools/internal/toolutil"
)

// ToolTerminalList is the model-facing name of the session listing tool.
const ToolTerminalList = "terminal_list"

// sessionSnapshot is one read-only view of a registered terminal session.
type sessionSnapshot struct {
	ID        string
	Name      string
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
func NewTerminalListTool(manager *Manager) rig.Tool {
	if manager == nil {
		return nil
	}
	return manager.terminalListTool()
}

// TerminalListTool is the method form of NewTerminalListTool.
func (m *Manager) TerminalListTool() rig.Tool { return NewTerminalListTool(m) }

// ToolsWithList returns the five terminal tools plus terminal_list. Hosts that
// expose the legacy tool use this instead of Tools().
func (m *Manager) ToolsWithList() []rig.Tool {
	if m == nil {
		return nil
	}
	return append(m.Tools(), m.terminalListTool())
}

// ToolsWithList is the package-level form of (*Manager).ToolsWithList.
func ToolsWithList(manager *Manager) []rig.Tool { return manager.ToolsWithList() }

// terminalListTool builds the terminal_list tool.
func (m *Manager) terminalListTool() rig.Tool {
	return &terminalTool{
		name: ToolTerminalList,
		description: "List the terminal sessions of this manager with session_id, transport (pty|pipe), " +
			"working directory, whether the shell is still running, and its exit code once it exited.",
		parameters: toolutil.ObjectSchema(map[string]any{}),
		run: func(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
			if err := ctx.Err(); err != nil {
				return rig.ToolResult{}, err
			}
			sessions := m.sessionSnapshots()
			if len(sessions) == 0 {
				return toolutil.TextResult("no terminal sessions"), nil
			}
			blocks := make([]string, 0, len(sessions))
			for _, session := range sessions {
				blocks = append(blocks, formatSessionSnapshot(session))
			}
			return toolutil.TextResult(strings.Join(blocks, "\n\n")), nil
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
			Name:      current.name,
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
	slices.SortFunc(snapshots, func(a, b sessionSnapshot) int { return strings.Compare(a.ID, b.ID) })
	return snapshots
}

// formatSessionSnapshot renders one session block.
func formatSessionSnapshot(session sessionSnapshot) string {
	exit := "-"
	if session.Exited {
		exit = strconv.Itoa(session.ExitCode)
		if session.ExitErr != nil {
			exit += " (" + session.ExitErr.Error() + ")"
		}
	}
	lines := []string{"session_id: " + session.ID}
	if session.Name != "" {
		lines = append(lines, "name: "+session.Name)
	}
	return strings.Join(append(lines,
		"transport: "+session.Transport.String(),
		"cwd: "+session.Cwd,
		fmt.Sprintf("running: %t", session.Running),
		"exit: "+exit,
	), "\n")
}
