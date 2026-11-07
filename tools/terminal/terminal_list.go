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

// ToolTerminalList 是会话列表工具面向模型的名称。
const ToolTerminalList = "terminal_list"

// TerminalListTool 返回绑定到 manager 的 terminal_list 工具：它列出管理器
// 已注册的会话（运行中或已结束），包含 id、传输方式、工作目录、运行标志与
// 退出码。
//
// 遗留 terminal 接口早于 terminal_* 工具，只有一个列表工具；宿主可以单独
// 注册它，或通过 ToolsWithList 获得全部工具。
func NewTerminalListTool(manager *Manager) rig.Tool {
	if manager == nil {
		return nil
	}
	return manager.terminalListTool()
}

// TerminalListTool 是 NewTerminalListTool 的方法形式。
func (m *Manager) TerminalListTool() rig.Tool { return NewTerminalListTool(m) }

// ToolsWithList 返回五个 terminal 工具再加 terminal_list。暴露遗留工具的
// 宿主应使用它而不是 Tools()。
func (m *Manager) ToolsWithList() []rig.Tool {
	if m == nil {
		return nil
	}
	return append(m.Tools(), m.terminalListTool())
}

// ToolsWithList 是 (*Manager).ToolsWithList 的包级形式。
func ToolsWithList(manager *Manager) []rig.Tool { return manager.ToolsWithList() }

// terminalListTool 构建 terminal_list 工具。
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

// sessionSnapshots 按 id 顺序返回已注册会话的快照（含 owner）。
func (m *Manager) sessionSnapshots() []SessionInfo {
	m.mu.Lock()
	sessions := make([]*session, 0, len(m.sessions))
	for _, current := range m.sessions {
		sessions = append(sessions, current)
	}
	m.mu.Unlock()

	snapshots := make([]SessionInfo, 0, len(sessions))
	for _, current := range sessions {
		snapshots = append(snapshots, current.info())
	}
	slices.SortFunc(snapshots, func(a, b SessionInfo) int { return strings.Compare(a.ID, b.ID) })
	return snapshots
}

// formatSessionSnapshot 渲染一个会话块。
func formatSessionSnapshot(session SessionInfo) string {
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
	if session.Owner != "" {
		lines = append(lines, "owner: "+session.Owner)
	}
	return strings.Join(append(lines,
		"transport: "+session.Transport,
		"cwd: "+session.Cwd,
		fmt.Sprintf("running: %t", session.Running),
		"exit: "+exit,
	), "\n")
}
