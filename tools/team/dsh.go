package team

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/tools/internal/toolutil"
)

// 为对齐 DeepSeek Harness 能力而新增的 DSH 工具名。
const (
	ToolSpawnTeammate = "spawn_teammate"
	ToolListAgents    = "list_agents"
	ToolWaitAgent     = "wait_agent"
	ToolSubagent      = "subagent"
	ToolSubagentFork  = "subagent_fork"
)

// DSHTools 返回 DSH 兼容的异步委派与团队控制工具：spawn_teammate、list_agents、
// wait_agent、subagent 与 subagent_fork。它们与 team_* 工具使用同一个 Manager，并通过
// rig.NoticeSource 暴露完成通知（agent 会在下一步把它们作为 user 消息追加）。
func DSHTools(manager *Manager) []rig.Tool {
	if manager == nil {
		return nil
	}
	return []rig.Tool{
		manager.dshSpawnTeammateTool(),
		manager.dshListAgentsTool(),
		manager.dshWaitAgentTool(),
		manager.dshSubagentTool(false),
		manager.dshSubagentTool(true),
	}
}

func (m *Manager) dshSpawnTeammateTool() rig.Tool {
	return &teamTool{
		name: ToolSpawnTeammate,
		description: "Create a named teammate with an initial task. The teammate starts working in the background; " +
			"use list_agents to see availability, send_message to steer it, wait_agent for the next team change, " +
			"and team_task_* for the shared task board. Only a live Lead should create teammates.",
		parameters: toolutil.ObjectSchema(map[string]any{
			"name":        toolutil.StringProp("Unique display name for the teammate."),
			"description": toolutil.StringProp("Short role description for the teammate (for logs/UI)."),
			"prompt":      toolutil.StringProp("Initial task for the teammate (self-contained)."),
			"task":        toolutil.StringProp("Legacy alias of prompt."),
			"model":       toolutil.StringProp("Optional model override; omitted inherits the parent model."),
		}, "name", "prompt"),
		notices: m.PendingNotices,
		run: func(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
			var args struct {
				Name        string `json:"name"`
				Description string `json:"description"`
				Prompt      string `json:"prompt"`
				Task        string `json:"task"`
				Model       string `json:"model"`
			}
			if err := toolutil.DecodeArgs(call, &args); err != nil {
				return rig.ToolResult{}, err
			}
			prompt := firstNonEmpty(args.Prompt, args.Task)
			if prompt == "" {
				return toolutil.ErrorResultf("spawn_teammate: prompt (or task) is required"), nil
			}
			member, err := m.Spawn(ctx, rig.SubagentSpec{
				Name:   args.Name,
				Prompt: prompt,
				Model:  args.Model,
			})
			if err != nil {
				return failure(ctx, ToolSpawnTeammate, err), nil
			}
			m.watchCompletion(member.ID)
			return toolutil.TextResult(fmt.Sprintf(
				"spawned teammate %s (target %s); it is running in the background", member.Name, member.ID)), nil
		},
	}
}

func (m *Manager) dshListAgentsTool() rig.Tool {
	return &teamTool{
		name:        ToolListAgents,
		description: "List every team member with its target (id), name, availability and last result status.",
		parameters:  toolutil.ObjectSchema(map[string]any{}),
		notices:     m.PendingNotices,
		run: func(ctx context.Context, _ rig.ToolCall) (rig.ToolResult, error) {
			if err := ctx.Err(); err != nil {
				return rig.ToolResult{}, err
			}
			members := m.Members()
			if len(members) == 0 {
				return toolutil.TextResult("no agents"), nil
			}
			var builder strings.Builder
			for _, member := range members {
				builder.WriteString(fmt.Sprintf("target: %s\tname: %s\tstatus: %s", member.ID, member.Name, member.Status))
				if member.Last != nil {
					builder.WriteString(fmt.Sprintf("\tlast: %s", strings.TrimSpace(member.Last.Output)))
				}
				builder.WriteString("\n")
			}
			return toolutil.TextResult(strings.TrimRight(builder.String(), "\n")), nil
		},
	}
}

func (m *Manager) dshWaitAgentTool() rig.Tool {
	return &teamTool{
		name: ToolWaitAgent,
		description: "Wait for the next team change (a member run finishing or an availability change) and report " +
			"which agent changed. Returns noProgress immediately when no member is running or provisioning.",
		parameters: toolutil.ObjectSchema(map[string]any{
			"timeout_ms": toolutil.IntegerProp("Wait budget in milliseconds (default 30000, maximum 600000)."),
		}),
		notices: m.PendingNotices,
		run: func(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
			var args struct {
				TimeoutMS int `json:"timeout_ms"`
			}
			if err := toolutil.DecodeArgs(call, &args); err != nil {
				return rig.ToolResult{}, err
			}
			timeout := time.Duration(args.TimeoutMS) * time.Millisecond
			if timeout <= 0 {
				timeout = 30 * time.Second
			}
			timeout = min(timeout, 10*time.Minute)
			changed, ok := m.waitNextChange(ctx, timeout)
			if !ok {
				return toolutil.TextResult("noProgress"), nil
			}
			return toolutil.TextResult(fmt.Sprintf("agent %s (%s) status %s", changed.ID, changed.Name, changed.Status)), nil
		},
	}
}

// waitNextChange 轮询成员快照，直到某个成员的状态或最近结果发生变化。没有活跃成员时
// 返回 false（不会有进展）。
func (m *Manager) waitNextChange(ctx context.Context, timeout time.Duration) (Member, bool) {
	previous := m.Members()
	active := false
	for _, member := range previous {
		if member.Status == StatusActive {
			active = true
			break
		}
	}
	if !active {
		return Member{}, false
	}
	deadline := time.Now().Add(timeout)
	for {
		select {
		case <-ctx.Done():
			return Member{}, false
		case <-time.After(15 * time.Millisecond):
		}
		current := m.Members()
		for index := range current {
			if index >= len(previous) {
				return current[index], true
			}
			before, after := previous[index], current[index]
			if before.Status != after.Status {
				return after, true
			}
			beforeOutput, afterOutput := "", ""
			if before.Last != nil {
				beforeOutput = before.Last.Output
			}
			if after.Last != nil {
				afterOutput = after.Last.Output
			}
			if beforeOutput != afterOutput {
				return after, true
			}
		}
		if time.Now().After(deadline) {
			return Member{}, false
		}
	}
}

func (m *Manager) dshSubagentTool(fork bool) rig.Tool {
	name := ToolSubagent
	if fork {
		name = ToolSubagentFork
	}
	description := "Start a child agent immediately and return its target id; it runs independently and reports its " +
		"final answer as a background notification. Use list_agents/send_message/wait_agent for control."
	if fork {
		description = "Start a forked child agent (inherits a copy of this conversation's completed history) " +
			"immediately and return its target id; it runs independently and reports its answer as a background notification."
	}
	return &teamTool{
		name:        name,
		description: description,
		parameters: toolutil.ObjectSchema(map[string]any{
			"description": toolutil.StringProp("Short active-voice description shown by the UI."),
			"prompt":      toolutil.StringProp("Complete task for the child; it cannot see this conversation unless this is subagent_fork."),
			"name":        toolutil.StringProp("Optional child display name."),
			"model":       toolutil.StringProp("Optional model override; omitted inherits the parent model."),
			"max_iterations": map[string]any{
				"type":        "integer",
				"description": "Optional cap on the child's model<->tool rounds (0 = runner default).",
			},
		}, "description", "prompt"),
		notices: m.PendingNotices,
		run: func(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
			var args struct {
				Description   string `json:"description"`
				Prompt        string `json:"prompt"`
				Name          string `json:"name"`
				Model         string `json:"model"`
				MaxIterations int    `json:"max_iterations"`
			}
			if err := toolutil.DecodeArgs(call, &args); err != nil {
				return rig.ToolResult{}, err
			}
			spec := rig.SubagentSpec{
				Name:          args.Name,
				Prompt:        args.Prompt,
				Model:         args.Model,
				MaxIterations: args.MaxIterations,
				Fork:          fork,
			}
			if strings.TrimSpace(spec.Name) == "" {
				spec.Name = strings.TrimSpace(args.Description)
			}
			if strings.TrimSpace(spec.Name) == "" {
				spec.Name = "subagent"
			}
			member, err := m.Spawn(ctx, spec)
			if err != nil {
				return failure(ctx, name, err), nil
			}
			m.watchCompletion(member.ID)
			return toolutil.TextResult(fmt.Sprintf("started subagent %s", member.ID)), nil
		},
	}
}
