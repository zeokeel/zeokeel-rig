package team

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/tools/internal/toolutil"
)

// 旧版（重命名前）对外编排工具名。它们是同一 Manager 操作之上的薄适配器，与 team_*
// 工具集等价。
const (
	ToolSpawnTeam          = "spawn_team"
	ToolListTeams          = "list_teams"
	ToolTeamStatus         = "team_status"
	ToolCloseTeam          = "close_team"
	ToolWaitTeam           = "wait_team"
	ToolSendMessage        = "send_message"
	ToolInterruptAgent     = "interrupt_agent"
	ToolListSubagentModels = "list_subagent_models"
)

// LegacyToolNames 按 LegacyTools 返回顺序给出旧版工具名。宿主在 Manager 创建之前构建
// 子代理工具过滤时会把它们加入排除列表。
func LegacyToolNames() []string {
	return []string{
		ToolSpawnTeam, ToolListTeams, ToolTeamStatus, ToolCloseTeam,
		ToolWaitTeam, ToolSendMessage, ToolInterruptAgent, ToolListSubagentModels,
	}
}

// LegacyTools 返回适配到该 Manager 的重命名前编排工具：spawn_team、list_teams、
// team_status、close_team、wait_team、send_message、interrupt_agent 与
// list_subagent_models。
//
// 这些工具复用与 team_* 相同的 Manager 操作与格式化，只有名称与少量旧版参数别名不同：
//
//	spawn_team      name + prompt（task 可作为 prompt 别名）
//	send_message    target + message（target = 成员 id 或唯一名称）
//	wait_team       target + timeout_ms
//	close_team      target
//	interrupt_agent target
//
// models 是 list_subagent_models 报告的内容（宿主配置的模型覆盖）；空列表表示该工具会说明
// 子代理继承父模型。
func LegacyTools(manager *Manager, models []string) []rig.Tool {
	if manager == nil {
		return nil
	}
	modelsCopy := slices.Clone(models)
	return []rig.Tool{
		manager.legacySpawnTool(),
		manager.legacyListTool(),
		manager.legacyStatusTool(),
		manager.legacyCloseTool(),
		manager.legacyWaitTool(),
		manager.legacySendTool(),
		manager.legacyInterruptTool(),
		legacyModelsTool(modelsCopy),
	}
}

// LegacyTools 是 nil 安全的接收者形式，供以 manager 为中心的代码使用。
func (m *Manager) LegacyTools(models []string) []rig.Tool { return LegacyTools(m, models) }

// --- 旧版参数辅助函数 -------------------------------------------------------

// legacyRef 从旧版 target/id/name 别名中解析成员引用。
func legacyRef(target, id, name string) (string, error) {
	if reference := strings.TrimSpace(target); reference != "" {
		return reference, nil
	}
	return referenceArg(id, name)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

// --- spawn_team -------------------------------------------------------------

type legacySpawnArgs struct {
	Name         string `json:"name"`
	Prompt       string `json:"prompt"`
	Task         string `json:"task"`
	Model        string `json:"model"`
	SystemPrompt string `json:"system_prompt"`
}

func (m *Manager) legacySpawnTool() rig.Tool {
	return &teamTool{
		name: ToolSpawnTeam,
		description: "Legacy alias of team_spawn: spawn a named subagent (team member) with a first prompt and return " +
			"its member id. prompt (or the legacy task alias) is the first user message. " +
			"Use list_teams/team_status to inspect members and send_message/wait_team to interact with them.",
		parameters: toolutil.ObjectSchema(map[string]any{
			"name":          toolutil.StringProp("Display name for the member."),
			"prompt":        toolutil.StringProp("First user message for the subagent (legacy calls may use task instead)."),
			"task":          toolutil.StringProp("Legacy alias of prompt."),
			"model":         toolutil.StringProp("Model override for the subagent (omitted = inherit the parent's model)."),
			"system_prompt": toolutil.StringProp("System prompt override for the subagent (omitted = inherit the parent's)."),
		}, "name"),
		run: func(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
			var args legacySpawnArgs
			if err := toolutil.DecodeArgs(call, &args); err != nil {
				return rig.ToolResult{}, err
			}
			if err := ctx.Err(); err != nil {
				return rig.ToolResult{}, err
			}
			prompt := firstNonEmpty(args.Prompt, args.Task)
			if prompt == "" {
				return toolutil.ErrorResultf("spawn_team: prompt (or task) is required"), nil
			}
			member, err := m.Spawn(ctx, rig.SubagentSpec{
				Name:         args.Name,
				Prompt:       prompt,
				Model:        args.Model,
				SystemPrompt: args.SystemPrompt,
			})
			if err != nil {
				return failure(ctx, ToolSpawnTeam, err), nil
			}
			return toolutil.TextResult(fmt.Sprintf(
				"spawned team member %s (%s); its first run is in the background — use wait_team to read the result",
				member.Name, member.ID)), nil
		},
	}
}

// --- list_teams -------------------------------------------------------------

func (m *Manager) legacyListTool() rig.Tool {
	return &teamTool{
		name:        ToolListTeams,
		description: "Legacy alias of team_list: list live team members with id, name, status and uptime.",
		parameters:  toolutil.ObjectSchema(map[string]any{}),
		run: func(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
			return m.listTool().Invoke(ctx, call)
		},
	}
}

// --- team_status ------------------------------------------------------------

type legacyStatusArgs struct {
	Target string `json:"target"`
	ID     string `json:"id"`
	Name   string `json:"name"`
}

func (m *Manager) legacyStatusTool() rig.Tool {
	return &teamTool{
		name: ToolTeamStatus,
		description: "Show member status details including the newest completed run output. With no target it lists every " +
			"live member; with a target (member id or unique name) it shows that member in detail.",
		parameters: toolutil.ObjectSchema(map[string]any{
			"target": toolutil.StringProp("Member id or unique name (omit to list every member)."),
			"id":     toolutil.StringProp("Legacy alias: member id."),
			"name":   toolutil.StringProp("Legacy alias: member name."),
		}),
		run: func(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
			var args legacyStatusArgs
			if err := toolutil.DecodeArgs(call, &args); err != nil {
				return rig.ToolResult{}, err
			}
			if err := ctx.Err(); err != nil {
				return rig.ToolResult{}, err
			}
			reference := firstNonEmpty(args.Target, args.ID, args.Name)
			if reference == "" {
				members := m.Members()
				if len(members) == 0 {
					return toolutil.TextResult("no team members"), nil
				}
				blocks := make([]string, 0, len(members))
				for _, member := range members {
					blocks = append(blocks, formatMemberStatus(member, true))
				}
				return toolutil.TextResult(strings.Join(blocks, "\n\n")), nil
			}
			current, err := m.resolve(reference)
			if err != nil {
				return failure(ctx, ToolTeamStatus, err), nil
			}
			return toolutil.TextResult(formatMemberStatus(current.snapshot(), true)), nil
		},
	}
}

// formatMemberStatus 渲染一个成员状态块。
func formatMemberStatus(member Member, withDetails bool) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "id: %s\nname: %s\nstatus: %s", member.ID, member.Name, member.Status)
	if withDetails {
		fmt.Fprintf(&builder, "\ncreated: %s", member.Created.Format(time.RFC3339))
		if member.Last == nil {
			builder.WriteString("\nlast: (no completed run yet)")
		} else {
			fmt.Fprintf(&builder, "\nlast: %s", toolutil.TruncateBytes(strings.TrimSpace(member.Last.Output), maxOutputChars, "\n[truncated: member output exceeded 32 KiB]"))
			if member.Last.Iterations > 0 || member.Last.Usage.TotalTokens > 0 {
				fmt.Fprintf(&builder, "\nusage: iterations=%d prompt=%d completion=%d total=%d",
					member.Last.Iterations, member.Last.Usage.PromptTokens,
					member.Last.Usage.CompletionTokens, member.Last.Usage.TotalTokens)
			}
		}
	}
	return builder.String()
}

// --- close_team -------------------------------------------------------------

type legacyTargetArgs struct {
	Target string `json:"target"`
	ID     string `json:"id"`
	Name   string `json:"name"`
}

func (m *Manager) legacyCloseTool() rig.Tool {
	return &teamTool{
		name:        ToolCloseTeam,
		description: "Legacy alias of team_close: stop and remove a team member by id or unique name.",
		parameters: toolutil.ObjectSchema(map[string]any{
			"target": toolutil.StringProp("Member id or unique name."),
			"id":     toolutil.StringProp("Legacy alias: member id."),
			"name":   toolutil.StringProp("Legacy alias: member name."),
		}),
		run: func(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
			var args legacyTargetArgs
			if err := toolutil.DecodeArgs(call, &args); err != nil {
				return rig.ToolResult{}, err
			}
			if err := ctx.Err(); err != nil {
				return rig.ToolResult{}, err
			}
			reference, err := legacyRef(args.Target, args.ID, args.Name)
			if err != nil {
				return toolutil.ErrorResultf("close_team: %v", err), nil
			}
			if err := m.Close(reference); err != nil {
				return failure(ctx, ToolCloseTeam, err), nil
			}
			return toolutil.TextResult("closed team member " + reference), nil
		},
	}
}

// --- wait_team --------------------------------------------------------------

type legacyWaitArgs struct {
	Target    string `json:"target"`
	ID        string `json:"id"`
	Name      string `json:"name"`
	TimeoutMS int    `json:"timeout_ms"`
}

func (m *Manager) legacyWaitTool() rig.Tool {
	return &teamTool{
		name:        ToolWaitTeam,
		description: "Legacy alias of team_wait: wait for a member's current/first run and return its result.",
		parameters: toolutil.ObjectSchema(map[string]any{
			"target":     toolutil.StringProp("Member id or unique name."),
			"id":         toolutil.StringProp("Legacy alias: member id."),
			"name":       toolutil.StringProp("Legacy alias: member name."),
			"timeout_ms": toolutil.IntegerProp("How long to wait, in milliseconds (default 30000, max 600000)."),
		}),
		run: func(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
			var args legacyWaitArgs
			if err := toolutil.DecodeArgs(call, &args); err != nil {
				return rig.ToolResult{}, err
			}
			if err := ctx.Err(); err != nil {
				return rig.ToolResult{}, err
			}
			reference, err := legacyRef(args.Target, args.ID, args.Name)
			if err != nil {
				return toolutil.ErrorResultf("wait_team: %v", err), nil
			}
			timeout := DefaultWaitTimeout
			if args.TimeoutMS > 0 {
				timeout = time.Duration(args.TimeoutMS) * time.Millisecond
			}
			timeout = min(timeout, MaxWaitTimeout)
			waitCtx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			result, err := m.Wait(waitCtx, reference)
			if err != nil {
				if ctx.Err() != nil {
					return rig.ToolResult{}, ctx.Err()
				}
				if errors.Is(err, context.DeadlineExceeded) {
					return toolutil.ErrorResultf("wait_team: no result after %s; the member is still running — call wait_team again", timeout), nil
				}
				return failure(ctx, ToolWaitTeam, err), nil
			}
			return toolutil.TextResult(formatRunResult(result, "finished")), nil
		},
	}
}

// --- send_message -----------------------------------------------------------

type legacySendArgs struct {
	Target  string `json:"target"`
	Message string `json:"message"`
	ID      string `json:"id"`
	Name    string `json:"name"`
}

func (m *Manager) legacySendTool() rig.Tool {
	return &teamTool{
		name: ToolSendMessage,
		description: "DSH-compatible send_message: hand a follow-up message to a team member (target = id or unique name) " +
			"and return immediately after acceptance; read the reply later with team_last/team_wait.",
		parameters: toolutil.ObjectSchema(map[string]any{
			"target":  toolutil.StringProp("Member id or unique name."),
			"message": toolutil.StringProp("Follow-up user message for the member."),
			"id":      toolutil.StringProp("Legacy alias: member id."),
			"name":    toolutil.StringProp("Legacy alias: member name."),
		}, "message"),
		run: func(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
			var args legacySendArgs
			if err := toolutil.DecodeArgs(call, &args); err != nil {
				return rig.ToolResult{}, err
			}
			if err := ctx.Err(); err != nil {
				return rig.ToolResult{}, err
			}
			reference, err := legacyRef(args.Target, args.ID, args.Name)
			if err != nil {
				return toolutil.ErrorResultf("send_message: %v", err), nil
			}
			if strings.TrimSpace(args.Message) == "" {
				return toolutil.ErrorResultf("send_message: message is required"), nil
			}
			if err := m.SendAsync(reference, args.Message); err != nil {
				return failure(ctx, ToolSendMessage, err), nil
			}
			return toolutil.TextResult(`{"sent":true}`), nil
		},
	}
}

// --- interrupt_agent --------------------------------------------------------

func (m *Manager) legacyInterruptTool() rig.Tool {
	return &teamTool{
		name:        ToolInterruptAgent,
		description: "Legacy alias of team_interrupt: stop a member's current run but keep the member listed.",
		parameters: toolutil.ObjectSchema(map[string]any{
			"target": toolutil.StringProp("Member id or unique name."),
			"id":     toolutil.StringProp("Legacy alias: member id."),
			"name":   toolutil.StringProp("Legacy alias: member name."),
		}),
		run: func(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
			var args legacyTargetArgs
			if err := toolutil.DecodeArgs(call, &args); err != nil {
				return rig.ToolResult{}, err
			}
			if err := ctx.Err(); err != nil {
				return rig.ToolResult{}, err
			}
			reference, err := legacyRef(args.Target, args.ID, args.Name)
			if err != nil {
				return toolutil.ErrorResultf("interrupt_agent: %v", err), nil
			}
			if err := m.Interrupt(reference); err != nil {
				return failure(ctx, ToolInterruptAgent, err), nil
			}
			return toolutil.TextResult("interrupted team member " + reference + "; it stays listed until close_team"), nil
		},
	}
}

// --- list_subagent_models ---------------------------------------------------

// legacyModelsTool 报告宿主配置的子代理模型覆盖。
func legacyModelsTool(models []string) rig.Tool {
	modelsCopy := slices.Clone(models)
	return &teamTool{
		name:        ToolListSubagentModels,
		description: "List the model names the host allows subagents to use. When empty, subagents inherit the parent's model.",
		parameters:  toolutil.ObjectSchema(map[string]any{}),
		run: func(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
			if err := ctx.Err(); err != nil {
				return rig.ToolResult{}, err
			}
			if len(modelsCopy) == 0 {
				return toolutil.TextResult("no explicit subagent models configured; subagents inherit the parent's model"), nil
			}
			lines := make([]string, 0, len(modelsCopy)+1)
			lines = append(lines, "available subagent models:")
			for _, model := range modelsCopy {
				lines = append(lines, "- "+model)
			}
			return toolutil.TextResult(strings.Join(lines, "\n")), nil
		},
	}
}
