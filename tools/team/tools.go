package team

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	v1 "github.com/zeokeel/zeokeel-rig/contracts/v1"
)

// Model-facing team tool names. They are stable identifiers.
const (
	ToolSpawn     = "team_spawn"
	ToolList      = "team_list"
	ToolSend      = "team_send"
	ToolWait      = "team_wait"
	ToolLast      = "team_last"
	ToolClose     = "team_close"
	ToolInterrupt = "team_interrupt"
)

// maxOutputChars bounds one member output rendered to the model.
const maxOutputChars = 32 << 10

// ToolNames returns the model-facing names of the team tools, in the order
// Tools returns them. Hosts use it to build the child tool filter before a
// Manager exists (children must not receive orchestration tools).
func ToolNames() []string {
	return []string{ToolSpawn, ToolList, ToolSend, ToolWait, ToolLast, ToolClose, ToolInterrupt}
}

// Tools returns the orchestration tools of this manager, ready for
// tools.NewRegistry:
//
//	registry := tools.NewRegistry(append(builtinTools, manager.Tools()...)...)
func (m *Manager) Tools() []v1.Tool {
	return []v1.Tool{
		m.spawnTool(),
		m.listTool(),
		m.sendTool(),
		m.waitTool(),
		m.lastTool(),
		m.closeTool(),
		m.interruptTool(),
	}
}

// teamTool is the common v1.Tool implementation of the team tool set.
type teamTool struct {
	name        string
	description string
	parameters  map[string]any
	run         func(ctx context.Context, call v1.ToolCall) (v1.ToolResult, error)
}

func (t *teamTool) Name() string           { return t.name }
func (t *teamTool) Description() string    { return t.description }
func (t *teamTool) Schema() map[string]any { return t.parameters }
func (t *teamTool) Invoke(ctx context.Context, call v1.ToolCall) (v1.ToolResult, error) {
	return t.run(ctx, call)
}

// --- team_spawn -------------------------------------------------------------

type spawnArgs struct {
	Name         string `json:"name"`
	Prompt       string `json:"prompt"`
	Model        string `json:"model"`
	SystemPrompt string `json:"system_prompt"`
}

func (m *Manager) spawnTool() v1.Tool {
	return &teamTool{
		name: ToolSpawn,
		description: "Spawn a named subagent (team member) with an initial prompt and return its member id immediately. " +
			"The first run executes in the background: use team_wait to read its result, team_send to continue the " +
			"conversation, team_list to see the team, and team_close/team_interrupt to stop it. " +
			"Live members are limited (8 by default) and names may be reused only for distinct members; " +
			"use ids when names are ambiguous.",
		parameters: objectSchema(map[string]any{
			"name":          stringProp("Unique-ish display name for the member (used with team_send and friends; duplicates make name lookups ambiguous)."),
			"prompt":        stringProp("First user message for the subagent; its first run starts in the background."),
			"model":         stringProp("Model override for the subagent (omitted = inherit the parent's model)."),
			"system_prompt": stringProp("System prompt override for the subagent (omitted = inherit the parent's)."),
		}, "name", "prompt"),
		run: func(ctx context.Context, call v1.ToolCall) (v1.ToolResult, error) {
			var args spawnArgs
			if err := decodeArgs(call, &args); err != nil {
				return v1.ToolResult{}, err
			}
			if err := ctx.Err(); err != nil {
				return v1.ToolResult{}, err
			}
			member, err := m.Spawn(ctx, v1.SubagentSpec{
				Name:         args.Name,
				Prompt:       args.Prompt,
				Model:        args.Model,
				SystemPrompt: args.SystemPrompt,
			})
			if err != nil {
				return failure(ctx, ToolSpawn, err), nil
			}
			return textResult(fmt.Sprintf(
				"spawned team member %s (%s); its first run is in the background — use team_wait to read the result",
				member.Name, member.ID)), nil
		},
	}
}

// --- team_list --------------------------------------------------------------

func (m *Manager) listTool() v1.Tool {
	return &teamTool{
		name: ToolList,
		description: "List the live team members with their id, name, status and uptime. Interrupted members stay listed " +
			"until team_close removes them. Use the ids shown here for team_send/team_wait/team_close when names are ambiguous.",
		parameters: objectSchema(map[string]any{}),
		run: func(ctx context.Context, call v1.ToolCall) (v1.ToolResult, error) {
			if err := ctx.Err(); err != nil {
				return v1.ToolResult{}, err
			}
			members := m.Members()
			if len(members) == 0 {
				return textResult("no team members"), nil
			}
			now := time.Now()
			lines := make([]string, 0, len(members))
			for _, member := range members {
				lines = append(lines, fmt.Sprintf("%s\t%s\t%s\t%s",
					member.ID, member.Name, member.Status, formatUptime(now.Sub(member.Created))))
			}
			return textResult(strings.Join(lines, "\n")), nil
		},
	}
}

// --- team_send --------------------------------------------------------------

type sendArgs struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Message string `json:"message"`
}

func (m *Manager) sendTool() v1.Tool {
	return &teamTool{
		name: ToolSend,
		description: "Send a follow-up user message to one team member and wait for its reply. Runs on one member are " +
			"serialized. Identify the member by id or by a unique name (if two members share a name, the call reports " +
			"the ambiguity and the id is required). The reply is the member's final assistant text for that run.",
		parameters: objectSchema(map[string]any{
			"id":      stringProp("Member id shown by team_list or team_spawn."),
			"name":    stringProp("Member name, when it is unique among live members."),
			"message": stringProp("Follow-up user message for the member."),
		}, "message"),
		run: func(ctx context.Context, call v1.ToolCall) (v1.ToolResult, error) {
			var args sendArgs
			if err := decodeArgs(call, &args); err != nil {
				return v1.ToolResult{}, err
			}
			if err := ctx.Err(); err != nil {
				return v1.ToolResult{}, err
			}
			ref, err := referenceArg(args.ID, args.Name)
			if err != nil {
				return errorResult("team_send: %v", err), nil
			}
			result, err := m.Send(ctx, ref, args.Message)
			if err != nil {
				return failure(ctx, ToolSend, err), nil
			}
			return textResult(formatRunResult(result, "replied")), nil
		},
	}
}

// --- team_wait --------------------------------------------------------------

type waitArgs struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	TimeoutMS int    `json:"timeout_ms"`
}

func (m *Manager) waitTool() v1.Tool {
	return &teamTool{
		name: ToolWait,
		description: "Wait for a team member's current (or first) run and return its result. Calling it after the run " +
			"finished returns the same result. The default timeout is 30s (max 10m); on timeout the member keeps " +
			"running and the tool reports that no result arrived yet.",
		parameters: objectSchema(map[string]any{
			"id":         stringProp("Member id shown by team_list or team_spawn."),
			"name":       stringProp("Member name, when it is unique among live members."),
			"timeout_ms": integerProp("How long to wait, in milliseconds (default 30000, max 600000)."),
		}),
		run: func(ctx context.Context, call v1.ToolCall) (v1.ToolResult, error) {
			var args waitArgs
			if err := decodeArgs(call, &args); err != nil {
				return v1.ToolResult{}, err
			}
			if err := ctx.Err(); err != nil {
				return v1.ToolResult{}, err
			}
			ref, err := referenceArg(args.ID, args.Name)
			if err != nil {
				return errorResult("team_wait: %v", err), nil
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
			result, err := m.Wait(waitCtx, ref)
			if err != nil {
				if ctx.Err() != nil {
					return v1.ToolResult{}, ctx.Err()
				}
				if errors.Is(err, context.DeadlineExceeded) {
					return errorResult("team_wait: no result after %s; the member is still running — call team_wait again or use team_last",
						timeout), nil
				}
				return failure(ctx, ToolWait, err), nil
			}
			return textResult(formatRunResult(result, "finished")), nil
		},
	}
}

// --- team_last --------------------------------------------------------------

func (m *Manager) lastTool() v1.Tool {
	return &teamTool{
		name: ToolLast,
		description: "Return the newest completed run result of a team member without waiting. Use it after team_wait " +
			"timed out or after team_send to re-read the last reply.",
		parameters: objectSchema(map[string]any{
			"id":   stringProp("Member id shown by team_list or team_spawn."),
			"name": stringProp("Member name, when it is unique among live members."),
		}),
		run: func(ctx context.Context, call v1.ToolCall) (v1.ToolResult, error) {
			var args struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			}
			if err := decodeArgs(call, &args); err != nil {
				return v1.ToolResult{}, err
			}
			if err := ctx.Err(); err != nil {
				return v1.ToolResult{}, err
			}
			ref, err := referenceArg(args.ID, args.Name)
			if err != nil {
				return errorResult("team_last: %v", err), nil
			}
			result, ok, err := m.Last(ref)
			if err != nil {
				return failure(ctx, ToolLast, err), nil
			}
			if !ok {
				return textResult("no completed run yet for " + ref), nil
			}
			return textResult(formatRunResult(result, "last finished")), nil
		},
	}
}

// --- team_close -------------------------------------------------------------

type closeArgs struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func (m *Manager) closeTool() v1.Tool {
	return &teamTool{
		name: ToolClose,
		description: "Stop and remove a team member. The member no longer appears in team_list and its name becomes " +
			"available to team_spawn. Idempotency is at the subagent level; closing an already removed member reports " +
			"an unknown-member error.",
		parameters: objectSchema(map[string]any{
			"id":   stringProp("Member id shown by team_list or team_spawn."),
			"name": stringProp("Member name, when it is unique among live members."),
		}),
		run: func(ctx context.Context, call v1.ToolCall) (v1.ToolResult, error) {
			var args closeArgs
			if err := decodeArgs(call, &args); err != nil {
				return v1.ToolResult{}, err
			}
			if err := ctx.Err(); err != nil {
				return v1.ToolResult{}, err
			}
			ref, err := referenceArg(args.ID, args.Name)
			if err != nil {
				return errorResult("team_close: %v", err), nil
			}
			if err := m.Close(ref); err != nil {
				return failure(ctx, ToolClose, err), nil
			}
			return textResult("closed team member " + ref), nil
		},
	}
}

// --- team_interrupt ---------------------------------------------------------

func (m *Manager) interruptTool() v1.Tool {
	return &teamTool{
		name: ToolInterrupt,
		description: "Stop a team member's current run but keep the member listed as interrupted. Use it to cancel work " +
			"without losing the member record; use team_close to remove it. Interrupting an already interrupted member " +
			"is a no-op.",
		parameters: objectSchema(map[string]any{
			"id":   stringProp("Member id shown by team_list or team_spawn."),
			"name": stringProp("Member name, when it is unique among live members."),
		}),
		run: func(ctx context.Context, call v1.ToolCall) (v1.ToolResult, error) {
			var args closeArgs
			if err := decodeArgs(call, &args); err != nil {
				return v1.ToolResult{}, err
			}
			if err := ctx.Err(); err != nil {
				return v1.ToolResult{}, err
			}
			ref, err := referenceArg(args.ID, args.Name)
			if err != nil {
				return errorResult("team_interrupt: %v", err), nil
			}
			if err := m.Interrupt(ref); err != nil {
				return failure(ctx, ToolInterrupt, err), nil
			}
			return textResult("interrupted team member " + ref + "; it stays listed until team_close"), nil
		},
	}
}

// --- shared helpers ---------------------------------------------------------

// referenceArg picks the member reference from the id/name arguments.
func referenceArg(id, name string) (string, error) {
	id = strings.TrimSpace(id)
	name = strings.TrimSpace(name)
	switch {
	case id != "" && name != "":
		return "", errors.New("pass either id or name, not both")
	case id != "":
		return id, nil
	case name != "":
		return name, nil
	default:
		return "", errors.New("id or name is required (team_list shows both)")
	}
}

// failure renders a manager error for the model. A cancelled caller context is
// an error for the agent loop; manager errors are readable tool results.
func failure(ctx context.Context, tool string, err error) v1.ToolResult {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return errorResult("%s: %v", tool, ctxErr)
	}
	return errorResult("%s: %v", tool, err)
}

// formatRunResult renders one subagent result: its output plus non-zero usage
// and iteration counters.
func formatRunResult(result v1.SubagentResult, verb string) string {
	name := strings.TrimSpace(result.Name)
	if name == "" {
		name = "member"
	}
	var builder strings.Builder
	if output := strings.TrimSpace(result.Output); output != "" {
		fmt.Fprintf(&builder, "%s %s:\n%s", name, verb, truncateOutput(output))
	} else {
		fmt.Fprintf(&builder, "%s %s with no output", name, verb)
	}
	if result.Iterations > 0 || result.Usage.TotalTokens > 0 {
		fmt.Fprintf(&builder, "\n[iterations: %d, tokens: prompt=%d completion=%d total=%d]",
			result.Iterations, result.Usage.PromptTokens, result.Usage.CompletionTokens, result.Usage.TotalTokens)
	}
	return builder.String()
}

// formatUptime renders a member's age compactly ("0s", "42s", "3m10s").
func formatUptime(age time.Duration) string {
	if age < 0 {
		age = 0
	}
	return age.Round(time.Second).String()
}

// truncateOutput caps one member output at maxOutputChars (UTF-8 safe).
func truncateOutput(output string) string {
	if len(output) <= maxOutputChars {
		return output
	}
	cut := maxOutputChars
	for cut > 0 && !utf8.RuneStart(output[cut]) {
		cut--
	}
	return output[:cut] + "\n[truncated: member output exceeded 32 KiB]"
}

func decodeArgs(call v1.ToolCall, target any) error {
	raw := strings.TrimSpace(call.Arguments)
	if raw == "" {
		raw = "{}"
	}
	if err := json.Unmarshal([]byte(raw), target); err != nil {
		return fmt.Errorf("invalid arguments JSON: %w", err)
	}
	return nil
}

func textResult(content string) v1.ToolResult { return v1.ToolResult{Content: content} }

func errorResult(format string, args ...any) v1.ToolResult {
	return v1.ToolResult{Content: fmt.Sprintf(format, args...), IsError: true}
}

func objectSchema(properties map[string]any, required ...string) map[string]any {
	schema := map[string]any{
		"type":                 "object",
		"properties":           properties,
		"additionalProperties": false,
	}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

func stringProp(description string) map[string]any {
	return map[string]any{"type": "string", "description": description}
}

func integerProp(description string) map[string]any {
	return map[string]any{"type": "integer", "description": description}
}
