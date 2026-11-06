package terminal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	v1 "github.com/zeokeel/zeokeel-rig/contracts/v1"
	"github.com/zeokeel/zeokeel-rig/sandbox"
)

// terminalTool is the common v1.Tool implementation of the terminal tool set.
type terminalTool struct {
	name        string
	description string
	parameters  map[string]any
	run         func(ctx context.Context, call v1.ToolCall) (v1.ToolResult, error)
}

func (t *terminalTool) Name() string           { return t.name }
func (t *terminalTool) Description() string    { return t.description }
func (t *terminalTool) Schema() map[string]any { return t.parameters }
func (t *terminalTool) Invoke(ctx context.Context, call v1.ToolCall) (v1.ToolResult, error) {
	return t.run(ctx, call)
}

type openArgs struct {
	Cwd   string `json:"cwd"`
	Shell string `json:"shell"`
}

func (m *Manager) openTool() v1.Tool {
	return &terminalTool{
		name: ToolTerminalOpen,
		description: "Open a persistent shell session and return its session id. On unix the default transport is " +
			"a real PTY: the shell is interactive, so prompts, line editing, Ctrl-C and full-screen programs work. " +
			"If no PTY is available (or on Windows in v0.5) a marker-based pipe session is used instead and the " +
			"result says so. Close it with terminal_close when done. cwd is the initial working directory; shell " +
			"overrides /bin/sh (e.g. /bin/bash).",
		parameters: objectSchema(map[string]any{
			"cwd":   stringProp("Initial working directory (default: the current directory; under a sandbox policy it must be inside an allowed root)."),
			"shell": stringProp("Shell executable to run (default /bin/sh). It is started without -c and reads commands from stdin."),
		}),
		run: func(ctx context.Context, call v1.ToolCall) (v1.ToolResult, error) {
			var args openArgs
			if err := decodeArgs(call, &args); err != nil {
				return v1.ToolResult{}, err
			}
			if err := ctx.Err(); err != nil {
				return v1.ToolResult{}, err
			}
			id, transport, warning, err := m.open(args.Cwd, args.Shell)
			if err != nil {
				return errorResultErr(err), nil
			}
			lines := []string{
				"session_id: " + id,
				"transport: " + transport.String(),
				"use terminal_send/terminal_read/terminal_signal/terminal_close",
			}
			if transport == TransportPTY {
				lines = append(lines, "real PTY: interactive programs and Ctrl-C (signal INT) are supported")
			} else {
				lines = append(lines, "pipe transport: marker-based completion, no PTY")
			}
			if warning != "" {
				lines = append(lines, warning)
			}
			return textResult(strings.Join(lines, "\n")), nil
		},
	}
}

type sendArgs struct {
	SessionID string `json:"session_id"`
	Input     string `json:"input"`
	TimeoutMS int    `json:"timeout_ms"`
}

func (m *Manager) sendTool() v1.Tool {
	return &terminalTool{
		name: ToolTerminalSend,
		description: "Run one command line in a terminal session and return its merged stdout+stderr, capped. " +
			"Completion depends on the session transport: a PTY session returns when the terminal has been quiet " +
			"for a short period (so a silent long-running command returns early with partial output; poll with " +
			"terminal_read and interrupt with terminal_signal), while the pipe transport waits for an internal " +
			"completion marker and reports a timeout as an error. Pass timeout_ms to bound the wait.",
		parameters: objectSchema(map[string]any{
			"session_id": stringProp("Session id returned by terminal_open."),
			"input":      stringProp("Shell command line to run (a trailing newline is optional)."),
			"timeout_ms": integerProp("Maximum time to wait for completion, in milliseconds (default 30000, max 600000)."),
		}, "session_id", "input"),
		run: func(ctx context.Context, call v1.ToolCall) (v1.ToolResult, error) {
			var args sendArgs
			if err := decodeArgs(call, &args); err != nil {
				return v1.ToolResult{}, err
			}
			if strings.TrimSpace(args.Input) == "" {
				return errorResult("terminal_send: input is required"), nil
			}
			if strings.ContainsRune(args.Input, 0) {
				return errorResult("terminal_send: input contains a NUL byte"), nil
			}
			if err := ctx.Err(); err != nil {
				return v1.ToolResult{}, err
			}
			current, err := m.lookup(args.SessionID)
			if err != nil {
				return errorResultErr(err), nil
			}
			if err := m.checkCommand(args.Input, current.cwd); err != nil {
				if errors.Is(err, sandbox.ErrDenied) {
					return errorResultErr(err), nil
				}
				return errorResult("terminal_send: %v", err), nil
			}
			timeout := m.defaultTimeout
			if args.TimeoutMS > 0 {
				timeout = time.Duration(args.TimeoutMS) * time.Millisecond
			}
			if timeout > MaxTimeout {
				timeout = MaxTimeout
			}
			output, timedOut, sendErr := current.send(ctx, args.Input, timeout)
			switch {
			case sendErr != nil:
				if ctxErr := ctx.Err(); ctxErr != nil {
					return v1.ToolResult{}, ctxErr
				}
				return errorResultErr(sendErr), nil
			case timedOut:
				return v1.ToolResult{Content: output, IsError: true}, nil
			default:
				return textResult(output), nil
			}
		},
	}
}

type readArgs struct {
	SessionID string `json:"session_id"`
	WaitMS    int    `json:"wait_ms"`
}

func (m *Manager) readTool() v1.Tool {
	return &terminalTool{
		name: ToolTerminalRead,
		description: "Read new output from a terminal session since the last read or send. " +
			"With no new output it returns an empty result unless wait_ms is set, in which case it waits up to " +
			"that long for output (useful for polling a long-running command).",
		parameters: objectSchema(map[string]any{
			"session_id": stringProp("Session id returned by terminal_open."),
			"wait_ms":    integerProp("How long to wait for new output, in milliseconds (default 0 = return immediately)."),
		}, "session_id"),
		run: func(ctx context.Context, call v1.ToolCall) (v1.ToolResult, error) {
			var args readArgs
			if err := decodeArgs(call, &args); err != nil {
				return v1.ToolResult{}, err
			}
			if err := ctx.Err(); err != nil {
				return v1.ToolResult{}, err
			}
			current, err := m.lookup(args.SessionID)
			if err != nil {
				return errorResultErr(err), nil
			}
			wait := time.Duration(args.WaitMS) * time.Millisecond
			if wait > MaxTimeout {
				wait = MaxTimeout
			}
			output, readErr := current.read(ctx, wait)
			if readErr != nil {
				if ctxErr := ctx.Err(); ctxErr != nil {
					return v1.ToolResult{}, ctxErr
				}
				return errorResultErr(readErr), nil
			}
			if strings.TrimSpace(output) == "" {
				return textResult(""), nil
			}
			return textResult(output), nil
		},
	}
}

type signalArgs struct {
	SessionID string          `json:"session_id"`
	Signal    json.RawMessage `json:"signal"`
}

func (m *Manager) signalTool() v1.Tool {
	return &terminalTool{
		name: ToolTerminalSignal,
		description: "Send a signal to a terminal session (Ctrl-C support). In PTY mode INT is delivered as the " +
			"Ctrl-C character (0x03) to the terminal's foreground process group; other signals go to the shell's " +
			"process group. signal accepts a name (INT/CTRL-C/SIGINT, TERM/SIGTERM, KILL/SIGKILL, HUP/QUIT) or a " +
			"number; the default is KILL.",
		parameters: objectSchema(map[string]any{
			"session_id": stringProp("Session id returned by terminal_open."),
			"signal": map[string]any{
				"description": "Signal name (INT/TERM/KILL/HUP/QUIT) or POSIX signal number (1-64). Default KILL.",
				"oneOf": []any{
					map[string]any{"type": "string", "enum": []string{
						"INT", "CTRL-C", "SIGINT", "TERM", "SIGTERM", "KILL", "SIGKILL", "HUP", "QUIT",
					}},
					map[string]any{"type": "integer", "minimum": 1, "maximum": 64},
				},
			},
		}, "session_id"),
		run: func(ctx context.Context, call v1.ToolCall) (v1.ToolResult, error) {
			var args signalArgs
			if err := decodeArgs(call, &args); err != nil {
				return v1.ToolResult{}, err
			}
			spec, err := parseSignalArgument(args.Signal)
			if err != nil {
				return errorResult("terminal_signal: %v", err), nil
			}
			if err := ctx.Err(); err != nil {
				return v1.ToolResult{}, err
			}
			current, lookupErr := m.lookup(args.SessionID)
			if lookupErr != nil {
				return errorResultErr(lookupErr), nil
			}
			if err := current.signal(spec); err != nil {
				return errorResult("terminal_signal: %v", err), nil
			}
			label := spec.name
			if label == "" {
				label = fmt.Sprintf("%d", spec.number)
			}
			return textResult(fmt.Sprintf("signal %s sent to terminal session %s", label, args.SessionID)), nil
		},
	}
}

type closeArgs struct {
	SessionID string `json:"session_id"`
}

func (m *Manager) closeTool() v1.Tool {
	return &terminalTool{
		name: ToolTerminalClose,
		description: "Close a terminal session: kill its process group, reap the shell and release the buffers. " +
			"Use it when a session is no longer needed or after a timeout desynchronized it.",
		parameters: objectSchema(map[string]any{
			"session_id": stringProp("Session id returned by terminal_open."),
		}, "session_id"),
		run: func(ctx context.Context, call v1.ToolCall) (v1.ToolResult, error) {
			var args closeArgs
			if err := decodeArgs(call, &args); err != nil {
				return v1.ToolResult{}, err
			}
			if err := ctx.Err(); err != nil {
				return v1.ToolResult{}, err
			}
			current, err := m.lookup(args.SessionID)
			if err != nil {
				return errorResultErr(err), nil
			}
			if err := current.close(); err != nil {
				return errorResult("terminal_close: %v", err), nil
			}
			current.mu.Lock()
			code := current.exitCode
			exited := current.exited
			current.mu.Unlock()
			m.remove(args.SessionID)
			if exited {
				return textResult(fmt.Sprintf("terminal session %s closed (shell exit code %d)", args.SessionID, code)), nil
			}
			return textResult(fmt.Sprintf("terminal session %s closed", args.SessionID)), nil
		},
	}
}

// --- shared helpers --------------------------------------------------------

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

func errorResultErr(err error) v1.ToolResult {
	return v1.ToolResult{Content: err.Error(), IsError: true}
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
