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

// jobTool 是任务工具集的通用 rig.Tool 实现。
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
		description: "Start a background shell command (platform shell: `/bin/sh -c` on Unix, PowerShell on Windows) and return a job id immediately. " +
			"In policy mode the first command must be allow-listed and unquoted shell operators are rejected. " +
			"Merged stdout+stderr is captured in a bounded ring buffer (default 1 MiB); poll it with " +
			"job_output, block with job_wait, or stop it with job_kill. The process dies with the manager " +
			"(Manager.CloseAll) and is killed automatically after timeout_ms when one is given.",
		parameters: toolutil.ObjectSchema(map[string]any{
			"command":    toolutil.StringProp("Shell command to run with the platform shell (/bin/sh -c; PowerShell on Windows)."),
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
			// state 通常为 running；命令极快结束时直接是 exited，始终与文本
			// 里的状态头一致。
			return withData(current.format("", nil), current.baseData()), nil
		},
	}
}

type outputArgs struct {
	JobID     string `json:"job_id"`
	Tail      int    `json:"tail"`
	Offset    *int64 `json:"offset"`
	Wait      bool   `json:"wait"`
	TimeoutMS int    `json:"timeout_ms"`
	Reader    string `json:"reader"`
}

func (m *Manager) outputTool() rig.Tool {
	return &jobTool{
		name: ToolJobOutput,
		description: "Read output from a background job. With no arguments it returns output produced since the " +
			"previous job_output/job_wait call; tail=N returns the last N lines instead; offset=N returns output " +
			"from an absolute byte offset. wait=true blocks until the job finishes or timeout_ms expires (a timed-out " +
			"wait leaves the job running). The response always includes state, exit code and duration. " +
			"Pass reader=<name> to keep an independent cursor per consumer: the same reader name continues where " +
			"that reader stopped, while a new reader name starts from the beginning; omit it to share the default " +
			"cursor (each byte is still delivered at most once, but interleaved readers split the output). " +
			"There is no per-reader or per-job output cap of our own: output is only cut when the host sets a byte " +
			"budget or the sandbox policy does, and then the response carries output_truncated: true plus " +
			"next_offset — call again with offset=next_offset to read the rest.",
		parameters: toolutil.ObjectSchema(map[string]any{
			"job_id":     toolutil.StringProp("Job id returned by the tool that started the background work."),
			"tail":       toolutil.IntegerProp("Return only the last N lines (0 or omitted = new output since the last read)."),
			"offset":     toolutil.IntegerProp("Return output from this absolute byte offset (mutually exclusive with tail)."),
			"wait":       toolutil.BooleanProp("Block until the job finishes or the timeout expires; a timed-out wait leaves the job running. Defaults to false."),
			"timeout_ms": toolutil.IntegerProp("Max wait in milliseconds with wait=true (default 30000, max 600000)."),
			"reader": toolutil.StringProp("Optional reader name. The same reader name always continues from " +
				"that reader's own cursor, so concurrent consumers each see the complete output instead of " +
				"racing for one shared cursor. Omit it to use the process-wide default cursor (each byte is " +
				"still delivered at most once, but interleaved consumers will split the output)."),
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
			if args.Wait {
				timeout := m.waitTimeout
				if args.TimeoutMS > 0 {
					timeout = time.Duration(args.TimeoutMS) * time.Millisecond
				}
				timeout = min(timeout, MaxWaitTimeout)
				if _, waitErr := m.waitJob(ctx, current, timeout); waitErr != nil {
					return rig.ToolResult{}, waitErr
				}
			}
			var output string
			var cursor *outputCursor
			switch {
			case args.Tail > 0:
				output = current.buffer.LastLines(args.Tail)
				current.resetCursorFor(args.Reader)
			case args.Offset != nil:
				output, cursor = current.outputFromChunk(*args.Offset)
			default:
				output, cursor = current.newOutputChunkFor(args.Reader)
			}
			return withData(current.format(output, cursor), current.outputData(cursor)), nil
		},
	}
}

type killArgs struct {
	JobID  string          `json:"job_id"`
	Signal json.RawMessage `json:"signal"`
	Reason string          `json:"reason"`
}

func (m *Manager) killTool() rig.Tool {
	return &jobTool{
		name: ToolJobKill,
		description: "Request cancellation of a running background job and return its final state plus output " +
			"tail. A running job is stopped (SIGKILL to its process group by default; a terminal background send " +
			"receives its interrupt instead) and settles as killed once its work actually stops. reason is a short " +
			"optional note recorded in the job's terminal detail and forwarded to the producer. Killing an " +
			"already-finished job just returns its state.",
		parameters: toolutil.ObjectSchema(map[string]any{
			"job_id": toolutil.StringProp("Job id returned by the tool that started the background work."),
			"reason": toolutil.StringProp("Optional short cancellation reason, recorded in the job detail and forwarded to the job."),
			"signal": map[string]any{
				"description": "Legacy signal override: name (INT/TERM/KILL/HUP/QUIT) or POSIX signal number (1-64). Default KILL.",
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
			if err := ctx.Err(); err != nil {
				return rig.ToolResult{}, err
			}
			current, err := m.lookup(args.JobID)
			if err != nil {
				return toolutil.ErrorResult(err), nil
			}
			outcome := "cancellation-requested"
			if current.isFinished() {
				outcome = "already-finished"
			} else if strings.TrimSpace(string(args.Signal)) == "" {
				// 线协议形态的调用：使用模型提供的原因取消。
				if killErr := current.terminate(strings.TrimSpace(args.Reason)); killErr != nil {
					if !IsFinishedError(killErr) {
						return toolutil.ErrorResultf("job_kill: %v", killErr), nil
					}
					outcome = "already-finished"
				}
			} else {
				spec, parseErr := parseSignalArgument(args.Signal)
				if parseErr != nil {
					return toolutil.ErrorResultf("job_kill: %v", parseErr), nil
				}
				if signalErr := current.signal(spec); signalErr != nil && !IsFinishedError(signalErr) {
					return toolutil.ErrorResultf("job_kill: %v", signalErr), nil
				}
				if reason := strings.TrimSpace(args.Reason); reason != "" {
					current.mu.Lock()
					current.killReason = reason
					current.mu.Unlock()
				}
			}
			// 给进程一点时间被回收，使返回的状态尽可能为终态。
			if _, waitErr := m.wait(ctx, current, killGrace); waitErr != nil {
				return rig.ToolResult{}, waitErr
			}
			data := current.baseData()
			data["outcome"] = outcome
			return withData("outcome: "+outcome+"\n"+current.format(current.tailOutput(), nil), data), nil
		},
	}
}

func (m *Manager) listTool() rig.Tool {
	return &jobTool{
		name: ToolJobList,
		description: "List the background jobs retained by this manager with their id, kind, state and status, " +
			"exit code, duration and command. Finished jobs stay listed until the host closes the manager.",
		parameters: toolutil.ObjectSchema(map[string]any{}),
		run: func(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
			if err := ctx.Err(); err != nil {
				return rig.ToolResult{}, err
			}
			current := m.jobsSnapshot()
			if len(current) == 0 {
				return withData("no jobs", map[string]any{"jobs": []map[string]any{}}), nil
			}
			var builder strings.Builder
			fmt.Fprintf(&builder, "jobs: %d\n", len(current))
			items := make([]map[string]any, 0, len(current))
			for _, running := range current {
				status := running.snapshot()
				command := running.command
				if len(command) > 120 {
					command = command[:120] + "…"
				}
				owner := ""
				if running.owner != "" {
					owner = " owner=" + running.owner
				}
				fmt.Fprintf(&builder, "- %s [%s]%s state=%s status=%s exit=%d duration_ms=%d command=%s\n",
					running.id, running.kind, owner, status.state, status.dshStatus(),
					status.exitCode, status.duration.Milliseconds(), command)
				items = append(items, running.listData())
			}
			return withData(builder.String(), map[string]any{"jobs": items}), nil
		},
	}
}

type waitArgs struct {
	JobID     string `json:"job_id"`
	TimeoutMS int    `json:"timeout_ms"`
	Reader    string `json:"reader"`
}

func (m *Manager) waitTool() rig.Tool {
	return &jobTool{
		name: ToolJobWait,
		description: "Wait for a background job to finish (up to timeout_ms) and return its final state plus the " +
			"last output lines. When the timeout expires the current state is returned and the job keeps running.",
		parameters: toolutil.ObjectSchema(map[string]any{
			"job_id":     toolutil.StringProp("Job id returned by job_start or a background tool."),
			"timeout_ms": toolutil.IntegerProp("How long to wait, in milliseconds (default 30000, max 600000)."),
			"reader": toolutil.StringProp("Optional reader name; the returned tail drains that reader's cursor " +
				"only (omit it to drain the shared default cursor)."),
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
			timeout = min(timeout, MaxWaitTimeout)
			if _, waitErr := m.wait(ctx, current, timeout); waitErr != nil {
				return rig.ToolResult{}, waitErr
			}
			return withData(current.format(current.tailOutputFor(args.Reader), nil), current.baseData()), nil
		},
	}
}

// --- 共享辅助函数 ----------------------------------------------------------

// withData 把模型可见文本与宿主结构化结果（rig.ToolResult.Data）配成一次工具
// 结果。文本一个字都不改：Data 只经 EventToolResult 交给宿主，不进模型消息。
func withData(content string, data map[string]any) rig.ToolResult {
	result := toolutil.TextResult(content)
	result.Data = data
	return result
}

// baseData 是任务类工具共用的宿主结构化结果：id、状态词与退出码，与响应文本的
// 状态头逐字一致。
func (j *job) baseData() map[string]any {
	status := j.snapshot()
	return map[string]any{
		"id":        j.id,
		"state":     status.state,
		"exit_code": status.exitCode,
	}
}

// outputData 给 job_output 的结果加上分页字段。cursor 为 nil（tail 读取）时文本
// 不渲染 offset 字段，这里把位置报成缓冲末尾：tail 已把该读者推进到末尾，没有
// 尾巴可续读，所以 output_truncated 恒为 false。
func (j *job) outputData(cursor *outputCursor) map[string]any {
	data := j.baseData()
	if cursor != nil {
		data["output_offset"] = cursor.From
		data["next_offset"] = cursor.Next
		data["output_truncated"] = cursor.More
		return data
	}
	end := j.buffer.Total()
	data["output_offset"] = end
	data["next_offset"] = end
	data["output_truncated"] = false
	return data
}

// listData 是 job_list 里每个任务一项的宿主结构化结果。
func (j *job) listData() map[string]any {
	data := j.baseData()
	data["kind"] = j.kind
	data["command"] = j.command
	data["output_bytes"] = int(j.buffer.Total())
	data["started"] = j.started
	j.mu.Lock()
	ended := j.ended
	j.mu.Unlock()
	if !ended.IsZero() {
		data["finished"] = ended
	}
	return data
}
