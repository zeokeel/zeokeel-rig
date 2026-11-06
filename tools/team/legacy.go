package team

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/zeokeel/zeokeel-rig"
)

// Legacy model-facing tool names from the pre-rename Rig orchestration surface.
// They are thin adapters over the same Manager operations as the team_* tools.
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

// LegacyToolNames returns the legacy tool names in the order LegacyTools
// returns them. Hosts add these to the child tool filter together with
// ToolNames.
func LegacyToolNames() []string {
	return []string{
		ToolSpawnTeam, ToolListTeams, ToolTeamStatus, ToolCloseTeam,
		ToolWaitTeam, ToolSendMessage, ToolInterruptAgent, ToolListSubagentModels,
	}
}

// LegacyTools returns the pre-rename orchestration tools adapted to this
// manager: spawn_team, list_teams, team_status, close_team, wait_team,
// send_message, interrupt_agent and list_subagent_models.
//
// The tools reuse the same Manager operations and formatting as the team_*
// set; only the names and a few legacy argument aliases differ:
//
//	spawn_team     name + prompt (task is accepted as a prompt alias)
//	send_message   target + message (target = member id or unique name)
//	wait_team      target + timeout_ms
//	close_team     target
//	interrupt_agent target
//
// models is what list_subagent_models reports (the host's configured model
// overrides); an empty list means the tool explains that subagents inherit the
// parent model.
func LegacyTools(manager *Manager, models []string) []rig.Tool {
	if manager == nil {
		return nil
	}
	modelsCopy := append([]string(nil), models...)
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

// LegacyTools is the nil-safe receiver form used by manager-centric code.
func (m *Manager) LegacyTools(models []string) []rig.Tool { return LegacyTools(m, models) }

// --- legacy argument helpers ------------------------------------------------

// legacyRef returns the member reference from legacy target/id/name aliases.
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
		parameters: objectSchema(map[string]any{
			"name":          stringProp("Display name for the member."),
			"prompt":        stringProp("First user message for the subagent (legacy calls may use task instead)."),
			"task":          stringProp("Legacy alias of prompt."),
			"model":         stringProp("Model override for the subagent (omitted = inherit the parent's model)."),
			"system_prompt": stringProp("System prompt override for the subagent (omitted = inherit the parent's)."),
		}, "name"),
		run: func(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
			var args legacySpawnArgs
			if err := decodeArgs(call, &args); err != nil {
				return rig.ToolResult{}, err
			}
			if err := ctx.Err(); err != nil {
				return rig.ToolResult{}, err
			}
			prompt := firstNonEmpty(args.Prompt, args.Task)
			if prompt == "" {
				return errorResult("spawn_team: prompt (or task) is required"), nil
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
			return textResult(fmt.Sprintf(
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
		parameters:  objectSchema(map[string]any{}),
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
		parameters: objectSchema(map[string]any{
			"target": stringProp("Member id or unique name (omit to list every member)."),
			"id":     stringProp("Legacy alias: member id."),
			"name":   stringProp("Legacy alias: member name."),
		}),
		run: func(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
			var args legacyStatusArgs
			if err := decodeArgs(call, &args); err != nil {
				return rig.ToolResult{}, err
			}
			if err := ctx.Err(); err != nil {
				return rig.ToolResult{}, err
			}
			reference := firstNonEmpty(args.Target, args.ID, args.Name)
			if reference == "" {
				members := m.Members()
				if len(members) == 0 {
					return textResult("no team members"), nil
				}
				blocks := make([]string, 0, len(members))
				for _, member := range members {
					blocks = append(blocks, formatMemberStatus(member, true))
				}
				return textResult(strings.Join(blocks, "\n\n")), nil
			}
			current, err := m.resolve(reference)
			if err != nil {
				return failure(ctx, ToolTeamStatus, err), nil
			}
			return textResult(formatMemberStatus(current.snapshot(), true)), nil
		},
	}
}

// formatMemberStatus renders one member status block.
func formatMemberStatus(member Member, withDetails bool) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "id: %s\nname: %s\nstatus: %s", member.ID, member.Name, member.Status)
	if withDetails {
		fmt.Fprintf(&builder, "\ncreated: %s", member.Created.Format(time.RFC3339))
		if member.Last == nil {
			builder.WriteString("\nlast: (no completed run yet)")
		} else {
			fmt.Fprintf(&builder, "\nlast: %s", truncateOutput(strings.TrimSpace(member.Last.Output)))
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
		parameters: objectSchema(map[string]any{
			"target": stringProp("Member id or unique name."),
			"id":     stringProp("Legacy alias: member id."),
			"name":   stringProp("Legacy alias: member name."),
		}),
		run: func(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
			var args legacyTargetArgs
			if err := decodeArgs(call, &args); err != nil {
				return rig.ToolResult{}, err
			}
			if err := ctx.Err(); err != nil {
				return rig.ToolResult{}, err
			}
			reference, err := legacyRef(args.Target, args.ID, args.Name)
			if err != nil {
				return errorResult("close_team: %v", err), nil
			}
			if err := m.Close(reference); err != nil {
				return failure(ctx, ToolCloseTeam, err), nil
			}
			return textResult("closed team member " + reference), nil
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
		parameters: objectSchema(map[string]any{
			"target":     stringProp("Member id or unique name."),
			"id":         stringProp("Legacy alias: member id."),
			"name":       stringProp("Legacy alias: member name."),
			"timeout_ms": integerProp("How long to wait, in milliseconds (default 30000, max 600000)."),
		}),
		run: func(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
			var args legacyWaitArgs
			if err := decodeArgs(call, &args); err != nil {
				return rig.ToolResult{}, err
			}
			if err := ctx.Err(); err != nil {
				return rig.ToolResult{}, err
			}
			reference, err := legacyRef(args.Target, args.ID, args.Name)
			if err != nil {
				return errorResult("wait_team: %v", err), nil
			}
			timeout := DefaultWaitTimeout
			if args.TimeoutMS > 0 {
				timeout = time.Duration(args.TimeoutMS) * time.Millisecond
			}
			if timeout > MaxWaitTimeout {
				timeout = MaxWaitTimeout
			}
			waitCtx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			result, err := m.Wait(waitCtx, reference)
			if err != nil {
				if ctx.Err() != nil {
					return rig.ToolResult{}, ctx.Err()
				}
				if errors.Is(err, context.DeadlineExceeded) {
					return errorResult("wait_team: no result after %s; the member is still running — call wait_team again", timeout), nil
				}
				return failure(ctx, ToolWaitTeam, err), nil
			}
			return textResult(formatRunResult(result, "finished")), nil
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
		description: "Legacy alias of team_send: send a follow-up message to a team member (target = id or unique name) " +
			"and wait for its reply.",
		parameters: objectSchema(map[string]any{
			"target":  stringProp("Member id or unique name."),
			"message": stringProp("Follow-up user message for the member."),
			"id":      stringProp("Legacy alias: member id."),
			"name":    stringProp("Legacy alias: member name."),
		}, "message"),
		run: func(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
			var args legacySendArgs
			if err := decodeArgs(call, &args); err != nil {
				return rig.ToolResult{}, err
			}
			if err := ctx.Err(); err != nil {
				return rig.ToolResult{}, err
			}
			reference, err := legacyRef(args.Target, args.ID, args.Name)
			if err != nil {
				return errorResult("send_message: %v", err), nil
			}
			if strings.TrimSpace(args.Message) == "" {
				return errorResult("send_message: message is required"), nil
			}
			result, err := m.Send(ctx, reference, args.Message)
			if err != nil {
				return failure(ctx, ToolSendMessage, err), nil
			}
			return textResult(formatRunResult(result, "replied")), nil
		},
	}
}

// --- interrupt_agent --------------------------------------------------------

func (m *Manager) legacyInterruptTool() rig.Tool {
	return &teamTool{
		name:        ToolInterruptAgent,
		description: "Legacy alias of team_interrupt: stop a member's current run but keep the member listed.",
		parameters: objectSchema(map[string]any{
			"target": stringProp("Member id or unique name."),
			"id":     stringProp("Legacy alias: member id."),
			"name":   stringProp("Legacy alias: member name."),
		}),
		run: func(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
			var args legacyTargetArgs
			if err := decodeArgs(call, &args); err != nil {
				return rig.ToolResult{}, err
			}
			if err := ctx.Err(); err != nil {
				return rig.ToolResult{}, err
			}
			reference, err := legacyRef(args.Target, args.ID, args.Name)
			if err != nil {
				return errorResult("interrupt_agent: %v", err), nil
			}
			if err := m.Interrupt(reference); err != nil {
				return failure(ctx, ToolInterruptAgent, err), nil
			}
			return textResult("interrupted team member " + reference + "; it stays listed until close_team"), nil
		},
	}
}

// --- list_subagent_models ---------------------------------------------------

// legacyModelsTool reports the host's configured subagent model overrides.
func legacyModelsTool(models []string) rig.Tool {
	modelsCopy := append([]string(nil), models...)
	return &teamTool{
		name:        ToolListSubagentModels,
		description: "List the model names the host allows subagents to use. When empty, subagents inherit the parent's model.",
		parameters:  objectSchema(map[string]any{}),
		run: func(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
			if err := ctx.Err(); err != nil {
				return rig.ToolResult{}, err
			}
			if len(modelsCopy) == 0 {
				return textResult("no explicit subagent models configured; subagents inherit the parent's model"), nil
			}
			lines := make([]string, 0, len(modelsCopy)+1)
			lines = append(lines, "available subagent models:")
			for _, model := range modelsCopy {
				lines = append(lines, "- "+model)
			}
			return textResult(strings.Join(lines, "\n")), nil
		},
	}
}
