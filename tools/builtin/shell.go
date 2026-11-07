package builtin

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/tools/internal/background"
	"github.com/zeokeel/zeokeel-rig/tools/internal/toolutil"
)

type bashArgs struct {
	Command     string `json:"command"`
	Description string `json:"description"`
	TimeoutMS   int    `json:"timeout_ms"`
	// TimeoutMSCamel 是超时覆盖参数的 DSH 拼写；两者同时出现时遗留的
	// timeout_ms 优先。
	TimeoutMSCamel  int    `json:"timeoutMs"`
	Workdir         string `json:"workdir"`
	RunInBackground bool   `json:"run_in_background"`
	// SandboxPermissions 与 Justification 为 DSH 线协议兼容而接受。Rig 没有
	// 按调用升级沙箱的执行器，因此设置这些字段的调用会以 DSH 的
	// "no sandboxing executor" 错误被拒绝。
	SandboxPermissions string `json:"sandbox_permissions"`
	Justification      string `json:"justification"`
}

// bashTool 是 bash 内置工具：/bin/sh -c、合并 stdout+stderr、限制输出，并带
// 每次调用超时（构造默认值可被 timeout_ms/timeoutMs 覆盖）。策略模式下会依据
// 根目录校验工作目录，命令必须通过允许/拒绝检查，且未加引号的 shell 控制操作符
// 会被策略拒绝（CheckShellCommand），从而保证被允许的命令无法再串联第二条
// 命令。审批 gate 仍是真正的边界（见包注释）。
//
// 挂接后台任务管理器（Options.BackgroundJobs）后，该工具还支持 DSH 后台执行：
// run_in_background 会立即返回任务 id，达到超时的前台命令会被提升为后台任务
// 而不是被杀死。
type bashTool struct {
	root           fsRoot
	defaultTimeout time.Duration
	options        Options
}

// newBash 构建 bash 工具。仅前台执行的构造函数（NewShell、NewWithPolicy）
// 中 options.BackgroundJobs 为 nil。
func newBash(root fsRoot, timeout time.Duration, options Options) rig.Tool {
	defaultTimeout := timeout
	if root.policy != nil && defaultTimeout <= 0 {
		defaultTimeout = root.policy.Timeout()
	}
	if defaultTimeout <= 0 {
		defaultTimeout = DefaultShellTimeout
	}
	defaultTimeout = min(defaultTimeout, MaxShellTimeout)
	return &bashTool{root: root, defaultTimeout: defaultTimeout, options: options}
}

var _ rig.Tool = (*bashTool)(nil)

// Name 实现 rig.Tool。
func (t *bashTool) Name() string { return ToolBash }

// Description 实现 rig.Tool。
func (t *bashTool) Description() string {
	description := "Run a bash command with /bin/sh -c. stdout and stderr are merged and capped at 32 KiB. " +
		"Each call runs in a fresh shell; pass workdir instead of using cd. " +
		"The exit code is appended; a non-zero exit or a timeout is reported as a tool error. " +
		"workdir must be inside the tool root. In policy mode the first command must be allow-listed and " +
		"unquoted shell operators (;, |, &&, $(...), redirects) are rejected unless the policy opts in. " +
		"This tool is not an OS sandbox: the approval gate is the boundary."
	if t.backgroundEnabled() {
		description += " Long commands can run in the background (run_in_background returns a job id immediately; " +
			"collect output with job_output, stop with job_kill); a foreground command that reaches its timeout is " +
			"moved to a background job instead of being killed."
	}
	return description
}

// backgroundEnabled 报告工具能否启动后台任务并提升超时的前台命令。
func (t *bashTool) backgroundEnabled() bool {
	return t.options.BackgroundJobs != nil
}

// Schema 实现 rig.Tool。
func (t *bashTool) Schema() map[string]any {
	properties := map[string]any{
		"command":     toolutil.StringProp("Shell command to run with /bin/sh -c."),
		"description": toolutil.StringProp("Optional active-voice description of the command for the UI, 5-10 words. Provide it before command when the wire format allows."),
		"timeout_ms":  toolutil.IntegerProp("Timeout in milliseconds (default 30000, maximum 600000)."),
		"timeoutMs":   toolutil.IntegerProp("DSH alias for timeout_ms."),
		"workdir":     toolutil.StringProp("Working directory relative to the tool root (default: the tool root)."),
	}
	if t.backgroundEnabled() && t.options.EnableRunInBackground {
		properties["run_in_background"] = toolutil.BooleanProp("Run in the background and return a job id immediately (collect with job_output, stop with job_kill). No timeout applies.")
	}
	return toolutil.ObjectSchema(properties, "command")
}

// Invoke 实现 rig.Tool。
func (t *bashTool) Invoke(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
	if runtime.GOOS == "windows" {
		return toolutil.ErrorResultf("bash: /bin/sh is not available on Windows; use the pwsh tool"), nil
	}
	var args bashArgs
	if err := toolutil.DecodeArgs(call, &args); err != nil {
		return rig.ToolResult{}, err
	}
	if strings.TrimSpace(args.Command) == "" {
		return toolutil.ErrorResultf("bash: command is required"), nil
	}
	if args.TimeoutMS <= 0 {
		args.TimeoutMS = args.TimeoutMSCamel
	}
	if err := checkEscalationArgs("bash", args.SandboxPermissions, args.Justification); err != nil {
		return toolutil.ErrorResult(err), nil
	}
	if args.RunInBackground && !t.backgroundEnabled() {
		return toolutil.ErrorResultf("bash: background jobs unavailable: no background job manager is configured; run the command in the foreground"), nil
	}
	if args.RunInBackground && !t.options.EnableRunInBackground {
		return toolutil.ErrorResultf("bash: run_in_background is disabled for this tool configuration"), nil
	}

	// 策略模式：只读策略禁用 shell；命令必须通过 shell 语法策略与命令
	// 允许/拒绝检查，且 workdir 必须位于根内。ParseShellCommand 与策略使用
	// 同一个解析器，因此校验结果与 argv[0] 在引号或续行上不会产生分歧。
	if t.root.policy != nil {
		if t.root.policy.ReadOnly() {
			return toolutil.ErrorResultf("bash: sandbox policy is read-only; shell commands are disabled"), nil
		}
		argv, parseErr := t.root.policy.ParseShellCommand(args.Command)
		if parseErr != nil {
			return toolutil.ErrorResult(parseErr), nil
		}
		if err := t.root.policy.CheckCommand(argv, args.Workdir); err != nil {
			return toolutil.ErrorResult(err), nil
		}
	}

	effectiveTimeout := t.defaultTimeout
	if args.TimeoutMS > 0 {
		effectiveTimeout = time.Duration(args.TimeoutMS) * time.Millisecond
	}
	if t.root.policy != nil && effectiveTimeout > t.root.policy.Timeout() {
		effectiveTimeout = t.root.policy.Timeout()
	}
	effectiveTimeout = min(effectiveTimeout, MaxShellTimeout)

	var (
		workdir string
		err     error
	)
	if t.root.policy != nil {
		workdir, err = resolvePolicyWorkdir(t.root, args.Workdir)
	} else {
		workdir, err = resolveWorkdir(t.root, args.Workdir)
	}
	if err != nil {
		return toolutil.ErrorResult(err), nil
	}

	outputLimit := MaxShellOutputBytes
	if t.root.policy != nil {
		outputLimit = t.root.policy.MaxOutputBytes()
	}
	argv := []string{"/bin/sh", "-c", args.Command}

	if args.RunInBackground {
		return t.runInBackground(args, argv, workdir, outputLimit)
	}
	if t.backgroundEnabled() && t.options.PromoteOnTimeout {
		if result, handled, runErr := t.runPromotedForeground(ctx, args, argv, workdir, effectiveTimeout, outputLimit); handled {
			return result, runErr
		}
		// 登记失败：回退到截止时间终止的前台路径。
	}
	return t.runForeground(ctx, args, workdir, effectiveTimeout, outputLimit)
}

// runInBackground 把命令作为受管后台任务启动，并立即返回其 id。后台任务
// 不受超时限制。
func (t *bashTool) runInBackground(args bashArgs, argv []string, workdir string, outputLimit int) (rig.ToolResult, error) {
	job, err := t.options.BackgroundJobs.StartProcess(background.ProcessSpec{
		Kind:             string(background.KindBash),
		Label:            args.Command,
		Command:          args.Command,
		Argv:             argv,
		Dir:              workdir,
		OutputLimitBytes: outputLimit,
		PolicyChecked:    true,
	})
	if err != nil {
		return toolutil.ErrorResultf("bash: background jobs unavailable: %v", err), nil
	}
	return toolutil.TextResult("started background job " + job.ID()), nil
}

// runPromotedForeground 把前台命令登记为任务，最多等待 timeout；若命令仍
// 在运行，则把它作为后台任务交给模型。仅当任务无法登记时 handled 才为 false
// （此时调用方回退到截止时间终止路径）。
func (t *bashTool) runPromotedForeground(ctx context.Context, args bashArgs, argv []string, workdir string, timeout time.Duration, outputLimit int) (rig.ToolResult, bool, error) {
	job, err := t.options.BackgroundJobs.StartProcess(background.ProcessSpec{
		Kind:             string(background.KindBash),
		Label:            args.Command,
		Command:          args.Command,
		Argv:             argv,
		Dir:              workdir,
		OutputLimitBytes: outputLimit,
		PolicyChecked:    true,
	})
	if err != nil {
		return rig.ToolResult{}, false, nil
	}
	_, settled, waitErr := job.Wait(ctx, timeout)
	if waitErr != nil {
		// 调用方取消：以本次调用为由停止命令，并把取消作为 Go error 上报，
		// 而不是面向模型的失败。
		job.Cancel("tool call aborted")
		_, _, _ = job.Wait(context.Background(), killWaitGrace)
		_ = job.Drop()
		return rig.ToolResult{}, true, waitErr
	}
	if settled {
		output, settledView := job.Read()
		_ = job.Drop()
		return toolutil.TextResult(renderShellOutput(output, settledView.ExitCode)), true, nil
	}
	output, _ := job.Read()
	return toolutil.TextResult(renderPromoted(output, timeout, job.ID())), true, nil
}

// killWaitGrace 限制被取消的前台调用在交还控制权之前等待其后台任务停止的
// 时长。
const killWaitGrace = 2 * time.Second

// renderShellOutput 渲染一次已结束的前台 shell 结果：先输出内容，再为非零
// 退出追加 DSH 退出标记。
func renderShellOutput(output string, exitCode int) string {
	var builder strings.Builder
	builder.WriteString(output)
	if output != "" && !strings.HasSuffix(output, "\n") {
		builder.WriteString("\n")
	}
	if exitCode > 0 {
		fmt.Fprintf(&builder, "[exit code: %d]", exitCode)
	}
	return builder.String()
}

// renderPromoted 渲染一次停止等待并已把命令交给后台任务的前台调用：到目前
// 为止的输出、交接标记与任务使用指引。
func renderPromoted(output string, timeout time.Duration, jobID string) string {
	var builder strings.Builder
	builder.WriteString(output)
	if output != "" && !strings.HasSuffix(output, "\n") {
		builder.WriteString("\n")
	}
	fmt.Fprintf(&builder, "[still running after %dms; moved to background job %s]\n", timeout.Milliseconds(), jobID)
	builder.WriteString("The command keeps running in the background. You will be notified when it finishes; " +
		"read newer output with job_output, stop it with job_kill.")
	return builder.String()
}

// runForeground 是后台执行不可用或被禁用时使用的截止时间终止前台路径。
func (t *bashTool) runForeground(ctx context.Context, args bashArgs, workdir string, effectiveTimeout time.Duration, outputLimit int) (rig.ToolResult, error) {
	runCtx, cancel := context.WithTimeout(ctx, effectiveTimeout)
	defer cancel()
	output := &truncatingBuffer{limit: outputLimit}
	command := exec.CommandContext(runCtx, "/bin/sh", "-c", args.Command)
	if t.root.policy != nil {
		command.Env = t.root.policy.CommandEnv(os.Environ())
	}
	// 超时的 shell 会被杀死，但子进程可能仍保持输出管道打开（例如
	// `sleep`）：WaitDelay 会关闭这些管道，使工具在截止时间返回，而不是
	// 等待子进程。
	command.WaitDelay = 500 * time.Millisecond
	command.Dir = workdir
	command.Stdout = output
	command.Stderr = output
	command.Stdin = nil
	runErr := command.Run()

	content := output.String()
	switch {
	case ctx.Err() != nil:
		// 调用方上下文被取消：中止运行，而不是把它误报为工具失败。
		return rig.ToolResult{}, ctx.Err()
	case runCtx.Err() == context.DeadlineExceeded:
		content += fmt.Sprintf("\n[timeout: command did not finish within %s]", effectiveTimeout)
		return rig.ToolResult{Content: content, IsError: true}, nil
	case runErr != nil:
		// DSH 语义：非零退出码是供模型解读的结果，而不是工具错误。上面
		// 的超时仍然是错误。
		content += fmt.Sprintf("\n[exit code: %d]", exitCode(runErr))
		return toolutil.TextResult(content), nil
	default:
		return toolutil.TextResult(content), nil
	}
}

// checkEscalationArgs 拒绝 DSH 沙箱升级参数。Rig 没有可按调用把当前模式放宽
// 的沙箱执行器，因此这些字段仅为线协议兼容而接受，并以 DSH 在未组合升级目标时
// 给出的同样指引 fail closed。
func checkEscalationArgs(tool, sandboxPermissions, justification string) error {
	if strings.TrimSpace(sandboxPermissions) != "" {
		return fmt.Errorf("%s: sandbox_permissions is not available in this composition (no sandboxing executor to escalate)", tool)
	}
	if strings.TrimSpace(justification) != "" {
		return fmt.Errorf("%s: justification requires sandbox_permissions, which is not available in this composition (no sandboxing executor to escalate)", tool)
	}
	return nil
}

// resolveWorkdir 依据 root 校验可选的 workdir，并返回其绝对文件系统路径。
func resolveWorkdir(root fsRoot, workdir string) (string, error) {
	if strings.TrimSpace(workdir) == "" {
		return root.dir, nil
	}
	fsys, err := root.open()
	if err != nil {
		return "", err
	}
	defer fsys.Close()
	relative, err := root.rel(workdir)
	if err != nil {
		return "", err
	}
	info, err := fsys.Stat(relative)
	if err != nil {
		return "", fmt.Errorf("bash: workdir: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("bash: workdir %q is not a directory", workdir)
	}
	return root.abs(relative), nil
}

// resolvePolicyWorkdir 通过沙箱策略校验可选的 workdir，并返回其真实绝对
// 路径。该目录必须存在且是某个策略根内的目录。
func resolvePolicyWorkdir(root fsRoot, workdir string) (string, error) {
	requested := strings.TrimSpace(workdir)
	if requested == "" {
		requested = "."
	}
	fsys, relative, abs, _, err := root.policy.OpenPath(requested)
	if err != nil {
		return "", fmt.Errorf("bash: workdir: %w", err)
	}
	defer fsys.Close()
	info, err := fsys.Stat(relative)
	if err != nil {
		return "", fmt.Errorf("bash: workdir: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("bash: workdir %q is not a directory", workdir)
	}
	return abs, nil
}

// exitCode 是失败命令的进程退出码（被杀死时为 -1）。
func exitCode(err error) int {
	if exitError, ok := errors.AsType[*exec.ExitError](err); ok {
		return exitError.ExitCode()
	}
	return -1
}

// truncatingBuffer 最多收集 limit 字节的命令输出。并发写入是安全的，因为
// 当 stdout 与 stderr 都不为 *os.File 时，exec 会在各自的 goroutine 上复制
// 它们。
type truncatingBuffer struct {
	mu        sync.Mutex
	limit     int
	buffer    bytes.Buffer
	truncated bool
}

func (b *truncatingBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	remaining := b.limit - b.buffer.Len()
	if remaining > 0 {
		if len(p) > remaining {
			b.buffer.Write(p[:remaining])
		} else {
			b.buffer.Write(p)
		}
	}
	if len(p) > remaining {
		b.truncated = true
	}
	return len(p), nil
}

// String 返回已捕获的输出及截断提示。
func (b *truncatingBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	content := b.buffer.String()
	if b.truncated {
		content += truncationNotice(fmt.Sprintf("output exceeded %d bytes", b.limit))
	}
	return content
}
