package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/tools/internal/toolutil"
)

// jobTool is the common rig.Tool implementation of the job tool set.
type jobTool struct {
	name        string
	description string
	parameters  map[string]any
	run         func(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error)
}

func (t *jobTool) Name() string           { return t.name }
func (t *jobTool) Description() string    { return t.description }
func (t *jobTool) Schema() map[string]any { return t.parameters }
func (t *jobTool) Invoke(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
	return t.run(ctx, call)
}

type startArgs struct {
	Command   string `json:"command"`
	Cwd       string `json:"cwd"`
	TimeoutMS int    `json:"timeout_ms"`
}

func (m *Manager) startTool() rig.Tool {
	return &jobTool{
		name: ToolJobStart,
		description: "Start a background shell command (`/bin/sh -c`) and return a job id immediately. " +
			"In policy mode the first command must be allow-listed and unquoted shell operators are rejected. " +
			"Merged stdout+stderr is captured in a bounded ring buffer (default 1 MiB); poll it with " +
			"job_output, block with job_wait, or stop it with job_kill. The process dies with the manager " +
			"(Manager.CloseAll) and is killed automatically after timeout_ms when one is given.",
		parameters: toolutil.ObjectSchema(map[string]any{
			"command":    toolutil.StringProp("Shell command to run with /bin/sh -c."),
			"cwd":        toolutil.StringProp("Working directory (default: the current directory; under a sandbox policy it must be inside an allowed root)."),
			"timeout_ms": toolutil.IntegerProp("Kill the job after this many milliseconds (default: no timeout)."),
		}, "command"),
		run: func(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
			var args startArgs
			if err := toolutil.DecodeArgs(call, &args); err != nil {
				return rig.ToolResult{}, err
			}
			if err := ctx.Err(); err != nil {
				return rig.ToolResult{}, err
			}
			timeout := time.Duration(0)
			if args.TimeoutMS > 0 {
				timeout = time.Duration(args.TimeoutMS) * time.Millisecond
			}
			current, err := m.start(args.Command, args.Cwd, timeout)
			if err != nil {
				return toolutil.ErrorResult(err), nil
			}
			return toolutil.TextResult(current.format("")), nil
		},
	}
}

type outputArgs struct {
	JobID  string `json:"job_id"`
	Tail   int    `json:"tail"`
	Offset *int64 `json:"offset"`
}

func (m *Manager) outputTool() rig.Tool {
	return &jobTool{
		name: ToolJobOutput,
		description: "Read output from a background job. With no arguments it returns output produced since the " +
			"previous job_output/job_wait call; tail=N returns the last N lines instead; offset=N returns output " +
			"from an absolute byte offset. The response always includes state, exit code and duration.",
		parameters: toolutil.ObjectSchema(map[string]any{
			"job_id": toolutil.StringProp("Job id returned by job_start."),
			"tail":   toolutil.IntegerProp("Return only the last N lines (0 or omitted = new output since the last read)."),
			"offset": toolutil.IntegerProp("Return output from this absolute byte offset (mutually exclusive with tail)."),
		}, "job_id"),
		run: func(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
			var args outputArgs
			if err := toolutil.DecodeArgs(call, &args); err != nil {
				return rig.ToolResult{}, err
			}
			if err := ctx.Err(); err != nil {
				return rig.ToolResult{}, err
			}
			current, err := m.lookup(args.JobID)
			if err != nil {
				return toolutil.ErrorResult(err), nil
			}
			var output string
			switch {
			case args.Tail > 0:
				output = current.buffer.LastLines(args.Tail)
			case args.Offset != nil:
				output = current.outputFrom(*args.Offset)
			default:
				output = current.newOutput()
			}
			return toolutil.TextResult(current.format(output)), nil
		},
	}
}

type killArgs struct {
	JobID  string          `json:"job_id"`
	Signal json.RawMessage `json:"signal"`
}

func (m *Manager) killTool() rig.Tool {
	return &jobTool{
		name: ToolJobKill,
		description: "Send a signal to a background job's process group and return its final state plus output " +
			"tail. signal accepts a name (INT, TERM, KILL, HUP, QUIT) or a number; the default is KILL. " +
			"Killing an already-finished job just returns its state.",
		parameters: toolutil.ObjectSchema(map[string]any{
			"job_id": toolutil.StringProp("Job id returned by job_start."),
			"signal": map[string]any{
				"description": "Signal name (INT/TERM/KILL/HUP/QUIT) or POSIX signal number (1-64). Default KILL.",
				"oneOf": []any{
					map[string]any{"type": "string", "enum": []string{"INT", "TERM", "KILL", "HUP", "QUIT"}},
					map[string]any{"type": "integer", "minimum": 1, "maximum": 64},
				},
			},
		}, "job_id"),
		run: func(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
			var args killArgs
			if err := toolutil.DecodeArgs(call, &args); err != nil {
				return rig.ToolResult{}, err
			}
			spec, err := parseSignalArgument(args.Signal)
			if err != nil {
				return toolutil.ErrorResultf("job_kill: %v", err), nil
			}
			if err := ctx.Err(); err != nil {
				return rig.ToolResult{}, err
			}
			current, err := m.lookup(args.JobID)
			if err != nil {
				return toolutil.ErrorResult(err), nil
			}
			if signalErr := current.signal(spec); signalErr != nil && !isFinishedError(signalErr) {
				return toolutil.ErrorResultf("job_kill: %v", signalErr), nil
			}
			// Give the process a moment to be reaped so the returned state is
			// final whenever possible.
			finished, waitErr := m.wait(ctx, current, killGrace)
			if waitErr != nil {
				return rig.ToolResult{}, waitErr
			}
			_ = finished
			return toolutil.TextResult(current.format(current.tailOutput())), nil
		},
	}
}

func (m *Manager) listTool() rig.Tool {
	return &jobTool{
		name: ToolJobList,
		description: "List the background jobs retained by this manager with their state, exit code, duration and " +
			"command. Finished jobs stay listed until the host closes the manager.",
		parameters: toolutil.ObjectSchema(map[string]any{}),
		run: func(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
			if err := ctx.Err(); err != nil {
				return rig.ToolResult{}, err
			}
			current := m.jobsSnapshot()
			if len(current) == 0 {
				return toolutil.TextResult("no jobs"), nil
			}
			var builder strings.Builder
			fmt.Fprintf(&builder, "jobs: %d\n", len(current))
			for _, running := range current {
				status := running.snapshot()
				command := running.command
				if len(command) > 120 {
					command = command[:120] + "…"
				}
				fmt.Fprintf(&builder, "- %s state=%s exit=%d duration_ms=%d command=%s\n",
					running.id, status.state, status.exitCode, status.duration.Milliseconds(), command)
			}
			return toolutil.TextResult(builder.String()), nil
		},
	}
}

type waitArgs struct {
	JobID     string `json:"job_id"`
	TimeoutMS int    `json:"timeout_ms"`
}

func (m *Manager) waitTool() rig.Tool {
	return &jobTool{
		name: ToolJobWait,
		description: "Wait for a background job to finish (up to timeout_ms) and return its final state plus the " +
			"last output lines. When the timeout expires the current state is returned and the job keeps running.",
		parameters: toolutil.ObjectSchema(map[string]any{
			"job_id":     toolutil.StringProp("Job id returned by job_start."),
			"timeout_ms": toolutil.IntegerProp("How long to wait, in milliseconds (default 30000, max 600000)."),
		}, "job_id"),
		run: func(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
			var args waitArgs
			if err := toolutil.DecodeArgs(call, &args); err != nil {
				return rig.ToolResult{}, err
			}
			if err := ctx.Err(); err != nil {
				return rig.ToolResult{}, err
			}
			current, err := m.lookup(args.JobID)
			if err != nil {
				return toolutil.ErrorResult(err), nil
			}
			timeout := m.waitTimeout
			if args.TimeoutMS > 0 {
				timeout = time.Duration(args.TimeoutMS) * time.Millisecond
			}
			if timeout > MaxWaitTimeout {
				timeout = MaxWaitTimeout
			}
			if _, waitErr := m.wait(ctx, current, timeout); waitErr != nil {
				return rig.ToolResult{}, waitErr
			}
			return toolutil.TextResult(current.format(current.tailOutput())), nil
		},
	}
}

// --- shared helpers --------------------------------------------------------

// isFinishedError reports whether a job_kill signal error only means the job had
// already exited (the tool still reports the final state in that case).
func isFinishedError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "already finished")
}
