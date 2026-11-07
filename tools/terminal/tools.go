package terminal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/sandbox"
	"github.com/zeokeel/zeokeel-rig/tools/internal/toolutil"
)

// terminalTool is the common rig.Tool implementation of the terminal tool set.
type terminalTool struct {
	name        string
	description string
	parameters  map[string]any
	run         func(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error)
}

func (t *terminalTool) Name() string           { return t.name }
func (t *terminalTool) Description() string    { return t.description }
func (t *terminalTool) Schema() map[string]any { return t.parameters }
func (t *terminalTool) Invoke(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
	return t.run(ctx, call)
}

// sessionArg merges the DSH camelCase sessionId spelling with the legacy
// session_id spelling.
func sessionArg(sessionID, sessionIDCamel string) (string, error) {
	id := strings.TrimSpace(sessionID)
	if id == "" {
		id = strings.TrimSpace(sessionIDCamel)
	}
	if id == "" {
		return "", errors.New("terminal: sessionId is required")
	}
	return id, nil
}

type openArgs struct {
	// Type is the DSH backend type: "shell" (default), "pty" or "pipe".
	Type string `json:"type"`
	// Name is an optional owner-local display name.
	Name  string `json:"name"`
	Cwd   string `json:"cwd"`
	Shell string `json:"shell"`
}

func (m *Manager) openTool() rig.Tool {
	return &terminalTool{
		name: ToolTerminalOpen,
		description: "Open a persistent shell session and return its session id. On unix the default transport is " +
			"a real PTY: the shell is interactive, so prompts, line editing, Ctrl-C and full-screen programs work. " +
			"If no PTY is available (or on Windows in v0.5) a marker-based pipe session is used instead and the " +
			"result says so. Close it with terminal_close when done. type is the backend type (shell, pty or pipe); " +
			"name is an optional owner-local label such as main or gdb; cwd is the initial working directory; shell " +
			"overrides /bin/sh (e.g. /bin/bash).",
		parameters: toolutil.ObjectSchema(map[string]any{
			"type":  toolutil.StringProp("Terminal backend type: shell (default), pty or pipe."),
			"name":  toolutil.StringProp("Optional owner-local display name such as main or gdb."),
			"cwd":   toolutil.StringProp("Initial working directory (default: the current directory; under a sandbox policy it must be inside an allowed root)."),
			"shell": toolutil.StringProp("Shell executable to run (default /bin/sh). It is started without -c and reads commands from stdin."),
		}),
		run: func(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
			var args openArgs
			if err := toolutil.DecodeArgs(call, &args); err != nil {
				return rig.ToolResult{}, err
			}
			if err := ctx.Err(); err != nil {
				return rig.ToolResult{}, err
			}
			id, transport, warning, err := m.open(args.Cwd, args.Shell, args.Name, args.Type)
			if err != nil {
				return toolutil.ErrorResult(err), nil
			}
			current, lookupErr := m.lookup(id)
			name := ""
			if lookupErr == nil {
				current.mu.Lock()
				name = current.name
				current.mu.Unlock()
			}
			lines := []string{"session_id: " + id}
			if name != "" {
				lines = append(lines, "name: "+name)
			}
			lines = append(lines, "transport: "+transport.String())
			lines = append(lines, "use terminal_send/terminal_read/terminal_signal/terminal_close")
			if transport == TransportPTY {
				lines = append(lines, "real PTY: interactive programs and Ctrl-C (signal INT) are supported")
			} else {
				lines = append(lines, "pipe transport: marker-based completion, no PTY")
			}
			if warning != "" {
				lines = append(lines, warning)
			}
			return toolutil.TextResult(strings.Join(lines, "\n")), nil
		},
	}
}

type sendArgs struct {
	SessionID      string `json:"session_id"`
	SessionIDCamel string `json:"sessionId"`
	Input          string `json:"input"`
	// Text is the DSH spelling of Input; Text wins when both are present.
	Text string `json:"text"`
	// Submit submits Enter after text (default true). Set false for control
	// characters or incomplete REPL input.
	Submit          *bool `json:"submit"`
	TimeoutMS       int   `json:"timeout_ms"`
	RunInBackground bool  `json:"run_in_background"`
}

func (m *Manager) sendTool() rig.Tool {
	return &terminalTool{
		name: ToolTerminalSend,
		description: "Send text to a persistent terminal session. By default Enter is submitted and the call waits " +
			"for the terminal to be ready (PTY: a quiet period; pipe: an internal completion marker) and returns the " +
			"merged output, capped. submit=false writes raw text without Enter (control characters or incomplete REPL " +
			"input) and returns after output settles. run_in_background=true returns a job id instead of waiting; " +
			"collect it with job_output and stop it with job_kill (which delivers SIGINT to the terminal's foreground " +
			"process group). Pass timeout_ms to bound the foreground wait.",
		parameters: toolutil.ObjectSchema(map[string]any{
			"sessionId":         toolutil.StringProp("Session id returned by terminal_open (session_id is accepted too)."),
			"text":              toolutil.StringProp("UTF-8 text to write to the terminal (input is accepted too)."),
			"submit":            toolutil.BooleanProp("Submit Enter after text (default true). Set false for control characters or incomplete REPL input."),
			"timeout_ms":        toolutil.IntegerProp("Maximum time to wait for completion, in milliseconds (default 30000, max 600000)."),
			"run_in_background": toolutil.BooleanProp("Return a job id immediately; collect with job_output or stop with job_kill."),
		}, "sessionId", "text"),
		run: func(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
			var args sendArgs
			if err := toolutil.DecodeArgs(call, &args); err != nil {
				return rig.ToolResult{}, err
			}
			sessionID, err := sessionArg(args.SessionID, args.SessionIDCamel)
			if err != nil {
				return toolutil.ErrorResult(err), nil
			}
			input := args.Text
			if input == "" {
				input = args.Input
			}
			if strings.TrimSpace(input) == "" {
				return toolutil.ErrorResultf("terminal_send: text is required"), nil
			}
			if strings.ContainsRune(input, 0) {
				return toolutil.ErrorResultf("terminal_send: text contains a NUL byte"), nil
			}
			submit := true
			if args.Submit != nil {
				submit = *args.Submit
			}
			if err := ctx.Err(); err != nil {
				return rig.ToolResult{}, err
			}
			current, err := m.lookup(sessionID)
			if err != nil {
				return toolutil.ErrorResult(err), nil
			}
			if err := m.checkCommand(input, current.cwd); err != nil {
				if errors.Is(err, sandbox.ErrDenied) {
					return toolutil.ErrorResult(err), nil
				}
				return toolutil.ErrorResultf("terminal_send: %v", err), nil
			}
			if args.RunInBackground {
				if m.background == nil {
					return toolutil.ErrorResultf("terminal_send: background sends unavailable: no background job manager is configured"), nil
				}
				jobID, startErr := m.startBackgroundSend(current, input, submit)
				if startErr != nil {
					return toolutil.ErrorResultf("terminal_send: %v", startErr), nil
				}
				return toolutil.TextResult("started background job " + jobID), nil
			}
			timeout := m.defaultTimeout
			if args.TimeoutMS > 0 {
				timeout = time.Duration(args.TimeoutMS) * time.Millisecond
			}
			timeout = min(timeout, MaxTimeout)
			output, timedOut, sendErr := current.send(ctx, input, submit, timeout)
			switch {
			case sendErr != nil:
				if ctxErr := ctx.Err(); ctxErr != nil {
					return rig.ToolResult{}, ctxErr
				}
				return toolutil.ErrorResult(sendErr), nil
			case timedOut:
				return rig.ToolResult{Content: output, IsError: true}, nil
			default:
				return toolutil.TextResult(output), nil
			}
		},
	}
}

type readArgs struct {
	SessionID      string `json:"session_id"`
	SessionIDCamel string `json:"sessionId"`
	// Offset is the newest-relative line offset of the requested page (0 =
	// newest page). Supplying offset or count switches to paged reads of the
	// retained scrollback.
	Offset *int `json:"offset"`
	// Count is the requested page size (default 500, max 2000).
	Count *int `json:"count"`
	// WaitMS preserves the legacy "new output since the last read/send" wait.
	WaitMS int `json:"wait_ms"`
}

func (m *Manager) readTool() rig.Tool {
	return &terminalTool{
		name: ToolTerminalRead,
		description: "Read retained output from a persistent terminal without sending input. With offset/count it " +
			"returns one newest-relative page of retained scrollback (offset 0 = newest page; count default 500) plus " +
			"a [lines: begin-end of total] marker. Without offset/count it returns new output since the last read or " +
			"send; wait_ms waits up to that long for new output.",
		parameters: toolutil.ObjectSchema(map[string]any{
			"sessionId": toolutil.StringProp("Session id returned by terminal_open (session_id is accepted too)."),
			"offset":    toolutil.IntegerProp("Newest-relative line offset (default 0). Supplying offset or count switches to paged reads."),
			"count":     toolutil.IntegerProp("Requested line count (default 500, max 2000)."),
			"wait_ms":   toolutil.IntegerProp("Legacy: how long to wait for new output, in milliseconds (default 0 = return immediately)."),
		}, "sessionId"),
		run: func(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
			var args readArgs
			if err := toolutil.DecodeArgs(call, &args); err != nil {
				return rig.ToolResult{}, err
			}
			sessionID, err := sessionArg(args.SessionID, args.SessionIDCamel)
			if err != nil {
				return toolutil.ErrorResult(err), nil
			}
			if err := ctx.Err(); err != nil {
				return rig.ToolResult{}, err
			}
			current, err := m.lookup(sessionID)
			if err != nil {
				return toolutil.ErrorResult(err), nil
			}
			if args.Offset != nil || args.Count != nil {
				offset := 0
				if args.Offset != nil {
					offset = *args.Offset
				}
				if offset < 0 {
					return toolutil.ErrorResultf("terminal_read: offset must be a non-negative integer"), nil
				}
				count := 0
				if args.Count != nil {
					count = *args.Count
				}
				if args.Count != nil && count <= 0 {
					return toolutil.ErrorResultf("terminal_read: count must be a positive integer"), nil
				}
				text, total, lineBegin, lineEnd, truncated := current.readPage(offset, count)
				if text == "" {
					text = "(no retained output)"
				}
				markers := fmt.Sprintf("[lines: %d-%d of %d]", lineBegin, lineEnd, total)
				if truncated {
					markers += "\n[truncated: older output is no longer retained]"
				}
				return toolutil.TextResult(text + "\n" + markers), nil
			}
			wait := time.Duration(args.WaitMS) * time.Millisecond
			wait = min(wait, MaxTimeout)
			output, readErr := current.read(ctx, wait)
			if readErr != nil {
				if ctxErr := ctx.Err(); ctxErr != nil {
					return rig.ToolResult{}, ctxErr
				}
				return toolutil.ErrorResult(readErr), nil
			}
			if strings.TrimSpace(output) == "" {
				return toolutil.TextResult(""), nil
			}
			return toolutil.TextResult(output), nil
		},
	}
}

type signalArgs struct {
	SessionID      string          `json:"session_id"`
	SessionIDCamel string          `json:"sessionId"`
	Signal         json.RawMessage `json:"signal"`
}

func (m *Manager) signalTool() rig.Tool {
	return &terminalTool{
		name: ToolTerminalSignal,
		description: "Deliver one allowed signal to a terminal session's foreground process group. In PTY mode INT is " +
			"delivered as the Ctrl-C character (0x03) to the terminal's foreground process group; other signals go to " +
			"the shell's process group. signal accepts SIGINT/SIGTERM/SIGKILL/SIGTSTP/SIGHUP (and the legacy " +
			"INT/CTRL-C/TERM/KILL/HUP/QUIT spellings) or a POSIX signal number; the default is KILL. Prefer " +
			"terminal_close for shell-targeted SIGKILL.",
		parameters: toolutil.ObjectSchema(map[string]any{
			"sessionId": toolutil.StringProp("Session id returned by terminal_open (session_id is accepted too)."),
			"signal": map[string]any{
				"description": "Signal name or POSIX signal number (1-64). Default KILL.",
				"oneOf": []any{
					map[string]any{"type": "string", "enum": []string{
						"SIGINT", "SIGTERM", "SIGKILL", "SIGTSTP", "SIGHUP",
						"INT", "CTRL-C", "SIGINT", "TERM", "KILL", "HUP", "QUIT",
					}},
					map[string]any{"type": "integer", "minimum": 1, "maximum": 64},
				},
			},
		}, "sessionId"),
		run: func(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
			var args signalArgs
			if err := toolutil.DecodeArgs(call, &args); err != nil {
				return rig.ToolResult{}, err
			}
			sessionID, err := sessionArg(args.SessionID, args.SessionIDCamel)
			if err != nil {
				return toolutil.ErrorResult(err), nil
			}
			spec, err := parseSignalArgument(args.Signal)
			if err != nil {
				return toolutil.ErrorResultf("terminal_signal: %v", err), nil
			}
			if err := ctx.Err(); err != nil {
				return rig.ToolResult{}, err
			}
			current, lookupErr := m.lookup(sessionID)
			if lookupErr != nil {
				return toolutil.ErrorResult(lookupErr), nil
			}
			if err := current.signal(spec); err != nil {
				return toolutil.ErrorResultf("terminal_signal: %v", err), nil
			}
			return toolutil.TextResult(fmt.Sprintf("delivered %s to terminal session %s", spec.label(), sessionID)), nil
		},
	}
}

type closeArgs struct {
	SessionID      string `json:"session_id"`
	SessionIDCamel string `json:"sessionId"`
}

func (m *Manager) closeTool() rig.Tool {
	return &terminalTool{
		name: ToolTerminalClose,
		description: "Close a terminal session: kill its process group, reap the shell and release the buffers. " +
			"Use it when a session is no longer needed or after a timeout desynchronized it.",
		parameters: toolutil.ObjectSchema(map[string]any{
			"sessionId": toolutil.StringProp("Session id returned by terminal_open (session_id is accepted too)."),
		}, "sessionId"),
		run: func(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
			var args closeArgs
			if err := toolutil.DecodeArgs(call, &args); err != nil {
				return rig.ToolResult{}, err
			}
			sessionID, err := sessionArg(args.SessionID, args.SessionIDCamel)
			if err != nil {
				return toolutil.ErrorResult(err), nil
			}
			if err := ctx.Err(); err != nil {
				return rig.ToolResult{}, err
			}
			current, lookupErr := m.lookup(sessionID)
			if lookupErr != nil {
				return toolutil.ErrorResult(lookupErr), nil
			}
			if closeErr := current.close(); closeErr != nil {
				return toolutil.ErrorResultf("terminal_close: %v", closeErr), nil
			}
			current.mu.Lock()
			code := current.exitCode
			exited := current.exited
			current.mu.Unlock()
			m.remove(sessionID)
			if exited {
				return toolutil.TextResult(fmt.Sprintf("terminal session %s closed (shell exit code %d)", sessionID, code)), nil
			}
			return toolutil.TextResult(fmt.Sprintf("terminal session %s closed", sessionID)), nil
		},
	}
}

// --- shared helpers --------------------------------------------------------
