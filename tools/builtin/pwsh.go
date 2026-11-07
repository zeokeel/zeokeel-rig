package builtin

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/sandbox"
	"github.com/zeokeel/zeokeel-rig/tools/internal/background"
	"github.com/zeokeel/zeokeel-rig/tools/internal/toolutil"
)

// ToolPwsh 是 PowerShell 工具面向模型的名称。
const ToolPwsh = "pwsh"

// PwshConfig 配置 pwsh 工具。
//
// 它有两种模式，与 bash 对应：
//
//   - 策略模式（Policy != nil）：命令允许列表、环境允许列表、超时上限、输出
//     上限与根目录均来自沙箱策略。使用 NewPwshWithPolicy 让二进制自动探测，
//     或使用 NewPwshWithConfig 注入二进制。
//   - 遗留模式（Policy == nil）：Root 是工具根，进程环境被继承，只施加
//     root/workdir 约束。
//
// Binary 覆盖可执行文件：为空时按 PATH 依次自动探测 pwsh 与 powershell
// （Windows 上优先 pwsh.exe/powershell.exe）。测试通过 Binary 注入假脚本。
//
// BackgroundJobs 挂接可选的 DSH 后台任务接口：设置后，run_in_background 会
// 立即返回任务 id，达到超时的前台命令会被提升为后台任务。
// DisableRunInBackground 与 DisablePromotion 可分别关闭这两种行为。
type PwshConfig struct {
	// Binary 是 PowerShell 可执行文件（路径或 PATH 查找名）。为空表示自动
	// 探测。
	Binary string
	// Root 是遗留模式下的工具根（为空表示进程工作目录）。策略模式下被
	// 忽略。
	Root string
	// Timeout 是每次调用的默认超时；为零表示使用策略超时（策略模式）或
	// DefaultShellTimeout。
	Timeout time.Duration
	// 非 nil 的 Policy 会启用沙箱策略强制。
	Policy *sandbox.Policy
	// BackgroundJobs 挂接宿主的后台任务管理器（可选）。
	BackgroundJobs BackgroundJobs
	// DisableRunInBackground 移除并拒绝 run_in_background。
	DisableRunInBackground bool
	// DisablePromotion 让超时的前台命令继续走截止时间终止路径，而不是提升
	// 为后台任务。
	DisablePromotion bool
}

// NewPwshWithPolicy 返回由 policy 治理的 pwsh 工具，使用自动探测的
// PowerShell 二进制。nil policy 表示 fail-closed 的默认策略（sandbox.New），
// 与 NewWithPolicy 完全一致。它是 NewShell 策略模式的 PowerShell 对应
// 版本。
func NewPwshWithPolicy(policy *sandbox.Policy) rig.Tool {
	if policy == nil {
		policy = sandbox.New()
	}
	return NewPwshWithConfig(PwshConfig{Policy: policy})
}

// NewPwshWithPolicyOptions 是带后台执行选项的 NewPwshWithPolicy：
// builtin.WithBackgroundJobs(manager) 可启用 run_in_background 与超时提升，
// 无需手工构造 PwshConfig。
func NewPwshWithPolicyOptions(policy *sandbox.Policy, opts ...Option) rig.Tool {
	if policy == nil {
		policy = sandbox.New()
	}
	resolved := resolveOptions(opts)
	return NewPwshWithConfig(PwshConfig{
		Policy:                 policy,
		BackgroundJobs:         resolved.BackgroundJobs,
		DisableRunInBackground: !resolved.EnableRunInBackground,
		DisablePromotion:       !resolved.PromoteOnTimeout,
	})
}

// NewPwsh 返回遗留（root + timeout）pwsh 工具，与 NewShell 对齐：命令在
// root 中运行，进程环境被继承，且没有命令允许列表。
func NewPwsh(root string, timeout time.Duration) rig.Tool {
	return NewPwshWithConfig(PwshConfig{Root: root, Timeout: timeout})
}

// NewPwshWithOptions 是带后台执行选项的 NewPwsh。
func NewPwshWithOptions(root string, timeout time.Duration, opts ...Option) rig.Tool {
	resolved := resolveOptions(opts)
	return NewPwshWithConfig(PwshConfig{
		Root:                   root,
		Timeout:                timeout,
		BackgroundJobs:         resolved.BackgroundJobs,
		DisableRunInBackground: !resolved.EnableRunInBackground,
		DisablePromotion:       !resolved.PromoteOnTimeout,
	})
}

// NewPwshWithConfig 返回使用显式配置的 pwsh 工具。当需要注入二进制（测试、
// 便携式安装）或同时使用策略模式与自定义二进制时，应使用该构造函数。
func NewPwshWithConfig(config PwshConfig) rig.Tool {
	tool := &pwshTool{
		binary:               strings.TrimSpace(config.Binary),
		policy:               config.Policy,
		background:           config.BackgroundJobs,
		allowRunInBackground: !config.DisableRunInBackground,
		promote:              !config.DisablePromotion,
	}
	if tool.policy != nil {
		tool.root = newPolicyRoot(tool.policy)
		tool.timeout = config.Timeout
		if tool.timeout <= 0 {
			tool.timeout = tool.policy.Timeout()
		}
	} else {
		tool.root = newFSRoot(config.Root)
		tool.timeout = config.Timeout
	}
	if tool.timeout <= 0 {
		tool.timeout = DefaultShellTimeout
	}
	if tool.timeout > MaxShellTimeout {
		tool.timeout = MaxShellTimeout
	}
	return tool
}

// pwshTool 是 pwsh 的 rig.Tool 实现。
type pwshTool struct {
	binary  string // 显式二进制；为空表示每次调用时自动探测
	root    fsRoot
	policy  *sandbox.Policy // 遗留模式下为 nil
	timeout time.Duration

	background           BackgroundJobs // nil 表示仅前台
	allowRunInBackground bool
	promote              bool
}

// pwshArgs 是 pwsh 的参数对象（与 bash 同形）。
type pwshArgs struct {
	Command     string `json:"command"`
	Description string `json:"description"`
	TimeoutMS   int    `json:"timeout_ms"`
	// TimeoutMSCamel 是超时覆盖参数的 DSH 拼写；两者同时出现时遗留的
	// timeout_ms 优先。
	TimeoutMSCamel  int    `json:"timeoutMs"`
	Workdir         string `json:"workdir"`
	RunInBackground bool   `json:"run_in_background"`
	// SandboxPermissions 与 Justification 为 DSH 线协议兼容而接受，但会被
	// 拒绝：Rig 没有按调用升级沙箱的执行器。
	SandboxPermissions string `json:"sandbox_permissions"`
	Justification      string `json:"justification"`
}

var _ rig.Tool = (*pwshTool)(nil)

// Name 实现 rig.Tool。
func (t *pwshTool) Name() string { return ToolPwsh }

// Description 实现 rig.Tool。
func (t *pwshTool) Description() string {
	description := "Run a PowerShell script with <binary> -NoProfile -NonInteractive -Command. " +
		"stdout and stderr are merged and capped; the exit code is appended; " +
		"non-zero exits and timeouts are reported as tool errors. workdir must be " +
		"inside the tool root (or the sandbox policy roots). In policy mode the binary " +
		"must be allow-listed and unquoted operators (;, |, &&, $(...), redirects) are " +
		"rejected unless the policy opts in. This tool is not an OS sandbox: the approval gate is."
	if t.backgroundEnabled() {
		description += " Long scripts can run in the background (run_in_background returns a job id immediately; " +
			"collect output with job_output, stop with job_kill); a foreground command that reaches its timeout is " +
			"moved to a background job instead of being killed."
	}
	return description
}

// backgroundEnabled 报告工具能否启动后台任务。
func (t *pwshTool) backgroundEnabled() bool { return t.background != nil }

// Schema 实现 rig.Tool。
func (t *pwshTool) Schema() map[string]any {
	properties := map[string]any{
		"command":     toolutil.StringProp("PowerShell script to run with -NoProfile -NonInteractive -Command."),
		"description": toolutil.StringProp("Optional active-voice description of the script for the UI, 5-10 words. Provide it before command when the wire format allows."),
		"timeout_ms":  toolutil.IntegerProp("Timeout in milliseconds (default 30000, maximum 600000)."),
		"timeoutMs":   toolutil.IntegerProp("DSH alias for timeout_ms."),
		"workdir":     toolutil.StringProp("Working directory relative to the tool root / policy roots (default: the root)."),
	}
	if t.backgroundEnabled() && t.allowRunInBackground {
		properties["run_in_background"] = toolutil.BooleanProp("Run in the background and return a job id immediately (collect with job_output, stop with job_kill). No timeout applies.")
	}
	return toolutil.ObjectSchema(properties, "command")
}

// Invoke 实现 rig.Tool。
func (t *pwshTool) Invoke(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
	var args pwshArgs
	if err := toolutil.DecodeArgs(call, &args); err != nil {
		return rig.ToolResult{}, err
	}
	if strings.TrimSpace(args.Command) == "" {
		return toolutil.ErrorResultf("pwsh: command is required"), nil
	}
	if args.TimeoutMS <= 0 {
		args.TimeoutMS = args.TimeoutMSCamel
	}
	if err := checkEscalationArgs("pwsh", args.SandboxPermissions, args.Justification); err != nil {
		return toolutil.ErrorResult(err), nil
	}
	if args.RunInBackground && !t.backgroundEnabled() {
		return toolutil.ErrorResultf("pwsh: background jobs unavailable: no background job manager is configured; run the command in the foreground"), nil
	}
	if args.RunInBackground && !t.allowRunInBackground {
		return toolutil.ErrorResultf("pwsh: run_in_background is disabled for this tool configuration"), nil
	}

	binary, err := resolvePwshBinary(t.binary)
	if err != nil {
		return toolutil.ErrorResultf("pwsh: %v", err), nil
	}

	// 策略模式：只读策略禁用 shell；命令（包括 PowerShell 参数与脚本文本）
	// 必须通过允许/拒绝检查，且 workdir 必须位于策略根内。
	if t.policy != nil {
		if t.policy.ReadOnly() {
			return toolutil.ErrorResultf("pwsh: sandbox policy is read-only; shell commands are disabled"), nil
		}
		if err := t.policy.CheckShellCommand(args.Command); err != nil {
			return toolutil.ErrorResult(err), nil
		}
		argv := []string{filepath.Base(binary), "-NoProfile", "-NonInteractive", "-Command", args.Command}
		if err := t.policy.CheckCommand(argv, args.Workdir); err != nil {
			return toolutil.ErrorResult(err), nil
		}
	}

	effectiveTimeout := t.timeout
	if args.TimeoutMS > 0 {
		effectiveTimeout = time.Duration(args.TimeoutMS) * time.Millisecond
	}
	if t.policy != nil && effectiveTimeout > t.policy.Timeout() {
		effectiveTimeout = t.policy.Timeout()
	}
	effectiveTimeout = min(effectiveTimeout, MaxShellTimeout)

	var workdir string
	if t.policy != nil {
		workdir, err = resolvePolicyWorkdir(t.root, args.Workdir)
	} else {
		workdir, err = resolveWorkdir(t.root, args.Workdir)
	}
	if err != nil {
		return toolutil.ErrorResult(err), nil
	}

	outputLimit := MaxShellOutputBytes
	if t.policy != nil {
		outputLimit = t.policy.MaxOutputBytes()
	}
	argv := []string{binary, "-NoProfile", "-NonInteractive", "-Command", args.Command}

	if args.RunInBackground {
		return t.runInBackground(args, argv, workdir, outputLimit)
	}
	if t.backgroundEnabled() && t.promote {
		if result, handled, runErr := t.runPromotedForeground(ctx, args, argv, workdir, effectiveTimeout, outputLimit); handled {
			return result, runErr
		}
	}
	return t.runForeground(ctx, args, binary, workdir, effectiveTimeout, outputLimit)
}

// runInBackground 把脚本作为受管后台任务启动。
func (t *pwshTool) runInBackground(args pwshArgs, argv []string, workdir string, outputLimit int) (rig.ToolResult, error) {
	job, err := t.background.StartProcess(background.ProcessSpec{
		Kind:             string(background.KindPwsh),
		Label:            args.Command,
		Command:          args.Command,
		Argv:             argv,
		Dir:              workdir,
		OutputLimitBytes: outputLimit,
		PolicyChecked:    true,
	})
	if err != nil {
		return toolutil.ErrorResultf("pwsh: background jobs unavailable: %v", err), nil
	}
	return toolutil.TextResult("started background job " + job.ID()), nil
}

// runPromotedForeground 把前台脚本登记为任务，最多等待 timeout；若仍在
// 运行，则把它作为后台任务交给模型。
func (t *pwshTool) runPromotedForeground(ctx context.Context, args pwshArgs, argv []string, workdir string, timeout time.Duration, outputLimit int) (rig.ToolResult, bool, error) {
	job, err := t.background.StartProcess(background.ProcessSpec{
		Kind:             string(background.KindPwsh),
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
		job.Cancel("tool call aborted")
		_, _, _ = job.Wait(context.Background(), killWaitGrace)
		_ = job.Drop()
		return rig.ToolResult{}, true, waitErr
	}
	if settled {
		output, view := job.Read()
		_ = job.Drop()
		return toolutil.TextResult(renderShellOutput(output, view.ExitCode)), true, nil
	}
	output, _ := job.Read()
	return toolutil.TextResult(renderPromoted(output, timeout, job.ID())), true, nil
}

// runForeground 是截止时间终止的前台路径。
func (t *pwshTool) runForeground(ctx context.Context, args pwshArgs, binary, workdir string, effectiveTimeout time.Duration, outputLimit int) (rig.ToolResult, error) {
	runCtx, cancel := context.WithTimeout(ctx, effectiveTimeout)
	defer cancel()
	output := &truncatingBuffer{limit: outputLimit}
	command := exec.CommandContext(runCtx, binary, "-NoProfile", "-NonInteractive", "-Command", args.Command)
	if t.policy != nil {
		command.Env = t.policy.CommandEnv(os.Environ())
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
		content += fmt.Sprintf("\n[pwsh timeout: command did not finish within %s]", effectiveTimeout)
		return rig.ToolResult{Content: content, IsError: true}, nil
	case runErr != nil:
		if exitErr, ok := errors.AsType[*exec.ExitError](runErr); ok {
			// DSH 语义：非零退出码是供模型解读的结果，而不是工具错误。上面
			// 的超时仍然是错误。
			content += fmt.Sprintf("\n[exit code: %d]", exitErr.ExitCode())
			return toolutil.TextResult(content), nil
		}
		// 启动失败（缺少二进制、权限不足等）属于配置问题，而不是退出码。
		content += fmt.Sprintf("\n[pwsh: cannot start %q: %v]", binary, runErr)
		return rig.ToolResult{Content: content, IsError: true}, nil
	default:
		return toolutil.TextResult(content), nil
	}
}

// resolvePwshBinary 返回注入的二进制，或在 PATH 上探测 pwsh/powershell。
// 错误信息对模型可读，并告知操作者如何修复。
func resolvePwshBinary(explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	names := pwshBinaryNames()
	for _, name := range names {
		if path, err := exec.LookPath(name); err == nil {
			return path, nil
		}
	}
	return "", fmt.Errorf(
		"PowerShell not found: looked for %s on PATH; install PowerShell or set PwshConfig.Binary",
		strings.Join(names, ", "))
}

// pwshBinaryNames 按优先级返回要探测的可执行文件名。
func pwshBinaryNames() []string {
	if runtime.GOOS == "windows" {
		return []string{"pwsh.exe", "powershell.exe", "pwsh", "powershell"}
	}
	return []string{"pwsh", "powershell"}
}
