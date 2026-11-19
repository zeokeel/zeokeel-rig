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
	"github.com/zeokeel/zeokeel-rig/internal/procattr"
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
// BackgroundJobs 挂接可选后台任务接口：设置后，run_in_background 会
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
	// Runner 是可选的 OS 级受限执行器。为 nil（默认）时行为完全不变：策略
	// 校验后的 argv 直接启动。非 nil 且存在策略时，spawn 前会调用
	// Runner.Confine 并只启动它返回的 argv；Confine 失败时 fail closed（返回
	// 工具错误，绝不启动未受限进程）。
	Runner sandbox.Runner
	// Escalator 是逐命令提权审批通道（见 escalation.go）。nil 表示不支持提权：
	// 带 sandbox_permissions 的调用会以"composition 不支持"的兼容错误被拒绝。
	Escalator Escalator
	// GrantTool 表示组合里存在 request_sandbox_access（用于沙箱拒绝的处置建议）。
	GrantTool bool
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
		Runner:                 resolved.Runner,
		Escalator:              resolved.Escalator,
		GrantTool:              resolved.GrantTool,
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
		Runner:                 resolved.Runner,
		Escalator:              resolved.Escalator,
		GrantTool:              resolved.GrantTool,
	})
}

// NewPwshWithConfig 返回使用显式配置的 pwsh 工具。当需要注入二进制（测试、
// 便携式安装）或同时使用策略模式与自定义二进制时，应使用该构造函数。
func NewPwshWithConfig(config PwshConfig) rig.Tool {
	tool := &pwshTool{
		binary:               strings.TrimSpace(config.Binary),
		policy:               config.Policy,
		background:           config.BackgroundJobs,
		runner:               config.Runner,
		allowRunInBackground: !config.DisableRunInBackground,
		promote:              !config.DisablePromotion,
		escalator:            config.Escalator,
		grantTool:            config.GrantTool,
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
	runner               sandbox.Runner // nil 表示不做 OS 级包装
	allowRunInBackground bool
	promote              bool
	escalator            Escalator // nil 表示不支持逐命令提权
	grantTool            bool      // 组合里是否有 request_sandbox_access
}

// escalationOptions 把 pwshTool 的字段投影成 Options（提权与处置建议共用）。
func (t *pwshTool) escalationOptions() Options {
	return Options{Escalator: t.escalator, Runner: t.runner, GrantTool: t.grantTool}
}

// pwshArgs 是 pwsh 的参数对象（与 bash 同形）。
type pwshArgs struct {
	Command     string `json:"command"`
	Description string `json:"description"`
	TimeoutMS   int    `json:"timeout_ms"`
	// TimeoutMSCamel 是超时覆盖参数拼写；两者同时出现时遗留的
	// timeout_ms 优先。
	TimeoutMSCamel  int    `json:"timeoutMs"`
	Workdir         string `json:"workdir"`
	RunInBackground bool   `json:"run_in_background"`
	// SandboxPermissions 与 Justification 兼容而接受，但会被
	// 拒绝：Rig 没有按调用升级沙箱的执行器。
	SandboxPermissions string `json:"sandbox_permissions"`
	Justification      string `json:"justification"`
}

var _ rig.Tool = (*pwshTool)(nil)

// Name 实现 rig.Tool。
func (t *pwshTool) Name() string { return ToolPwsh }

// optionsForDescription 把 pwshTool 的字段投影成 Options，供描述/模式说明使用
// （pwsh 的字段是历史遗留的平铺结构，见 newPwsh）。
func (t *pwshTool) optionsForDescription() Options {
	return Options{
		Runner:         t.runner,
		Escalator:      t.escalator,
		GrantTool:      t.grantTool,
		BackgroundJobs: t.background,
	}
}

// Description 实现 rig.Tool。边界句随组合动态化（审计 RG-05）：未挂接
// runner 时保留「not an OS sandbox」原句；挂接后改写为准确的 OS 沙箱边界句。
func (t *pwshTool) Description() string {
	boundary := "This tool is not an OS sandbox: the approval gate is."
	if t.runner != nil {
		boundary = runnerBoundaryDescription
	}
	description := "Run a PowerShell script with <binary> -NoProfile -NonInteractive -Command. " +
		bufferingHint +
		"stdout and stderr are merged and capped; the exit code is appended; " +
		"non-zero exits and timeouts are reported as tool errors. workdir must be " +
		"inside the tool root (or the sandbox policy roots). In policy mode the binary " +
		"must be allow-listed and unquoted operators (;, |, &&, $(...), redirects) are " +
		"rejected unless the policy opts in. " + boundary
	if t.backgroundEnabled() {
		description += " Long scripts can run in the background (run_in_background returns a job id immediately; " +
			"collect output with job_output, stop with job_kill); a foreground command that reaches its timeout is " +
			"moved to a background job instead of being killed."
	}
	// 同上：提权不可用时这句提示必须消失（指向必然失败的参数比不说更糟）。
	return description + escalationHint(t.optionsForDescription(), t.policy)
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
	escalation := escalationAvailable(t.optionsForDescription(), t.policy)
	properties["sandbox_permissions"] = map[string]any{
		"type":        "string",
		"description": sandboxPermissionsDescription(escalation),
	}
	properties["justification"] = toolutil.StringProp(justificationDescription(escalation))
	if t.backgroundEnabled() && t.allowRunInBackground {
		properties["run_in_background"] = toolutil.BooleanProp("Run in the background and return a job id immediately (collect with job_output, stop with job_kill). No timeout applies.")
	}
	return toolutil.ObjectSchema(properties, "command")
}

// pwshCommandScript 返回真正传给 `-Command` 的脚本文本。
//
// Windows 上会前置 `procattr.PwshUTF8Prologue`（强制 UTF-8 输出、把控制台代码页
// 切成 65001、并原样保留退出码）：中文 Windows 的默认活动代码页是 936，不前置时
// 子进程写出的是 GBK 字节，我们按 UTF-8 解码就是乱码。其它平台原样返回——Unix 上
// 的 pwsh 默认已经是 UTF-8，注入语句只会污染用户的变量与 `$LASTEXITCODE`。
//
// 注意：策略检查（policy.CheckShellCommand / CheckCommand）仍然看**用户原文**，
// 不看这段前缀——前缀是我们自己的常量，不该参与允许/拒绝规则匹配。
func pwshCommandScript(script string) string {
	if runtime.GOOS != "windows" {
		return script
	}
	return procattr.WrapPwshScript(script)
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
	// 审批人看到的是**用户原文**（不带我们的编码前置语句）：Argv 是审批文案与
	// 事件 payload 的一部分，混入实现细节会让"到底要跑什么"变模糊。
	approvalArgv := []string{binary, "-NoProfile", "-NonInteractive", "-Command", args.Command}
	// 与 bash 一致：逐命令提权只在人工批准后跳过沙箱包装；策略与危险命令判定不变。
	escalation, escErr := resolveEscalation(ctx, t.escalationOptions(), escalationInput{
		tool:               "pwsh",
		command:            args.Command,
		sandboxPermissions: args.SandboxPermissions,
		justification:      args.Justification,
		argv:               approvalArgv,
		workdir:            workdir,
		policy:             t.policy,
		windows:            true,
	})
	if escErr != nil {
		return toolutil.ErrorResult(escErr), nil
	}
	// 真正执行用的 argv：Windows 上带 UTF-8 编码前置语句。
	argv := []string{binary, "-NoProfile", "-NonInteractive", "-Command", pwshCommandScript(args.Command)}
	confined := sandbox.ConfinedCommand{Argv: argv}
	if !escalation.Escalated {
		confined, err = confineCommand(t.runner, t.policy, argv)
		if err != nil {
			return toolutil.ErrorResultf("pwsh: sandbox runner cannot confine the command: %v", err), nil
		}
	}

	if args.RunInBackground {
		result, runErr := t.runInBackground(args, confined, workdir, outputLimit)
		return applyEscalationNote(result, escalation), runErr
	}
	if t.backgroundEnabled() && t.promote {
		if result, handled, runErr := t.runPromotedForeground(ctx, args, confined, workdir, effectiveTimeout, outputLimit); handled {
			return applyEscalationNote(result, escalation), runErr
		}
	}
	result, runErr := t.runForeground(ctx, confined, workdir, effectiveTimeout, outputLimit)
	return applyEscalationNote(result, escalation), runErr
}

// runInBackground 把脚本作为受管后台任务启动。Confine 的拒绝方言随
// ProcessSpec 交给管理器，由其失败结果视图渲染归因行。
func (t *pwshTool) runInBackground(args pwshArgs, confined sandbox.ConfinedCommand, workdir string, outputLimit int) (rig.ToolResult, error) {
	job, err := t.background.StartProcess(background.ProcessSpec{
		Kind:             string(background.KindPwsh),
		Label:            args.Command,
		Command:          args.Command,
		Argv:             confined.Argv,
		Dir:              workdir,
		OutputLimitBytes: outputLimit,
		PolicyChecked:    true,
		DenialSignatures: confined.DenialSignatures,
		Enforcement:      confined.Enforcement,
	})
	if err != nil {
		return toolutil.ErrorResultf("pwsh: background jobs unavailable: %v", err), nil
	}
	return toolutil.TextResult("started background job " + job.ID()), nil
}

// runPromotedForeground 把前台脚本登记为任务，最多等待 timeout；若仍在
// 运行，则把它作为后台任务交给模型。
func (t *pwshTool) runPromotedForeground(ctx context.Context, args pwshArgs, confined sandbox.ConfinedCommand, workdir string, timeout time.Duration, outputLimit int) (rig.ToolResult, bool, error) {
	job, err := t.background.StartProcess(background.ProcessSpec{
		Kind:             string(background.KindPwsh),
		Label:            args.Command,
		Command:          args.Command,
		Argv:             confined.Argv,
		Dir:              workdir,
		OutputLimitBytes: outputLimit,
		PolicyChecked:    true,
		DenialSignatures: confined.DenialSignatures,
		Enforcement:      confined.Enforcement,
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
		content := renderShellOutput(output, view.ExitCode)
		// 已结束且不是被模型杀死的命令：输出命中拒绝方言时追加归因行（不判断
		// 退出码：CLI 可能把落盘失败包成自己的文案后仍退 0）。
		if view.Status != background.StatusKilled {
			content += sandboxDenialSuffix(content, confined, t.policy)
		}
		return toolutil.TextResult(content), true, nil
	}
	output, _ := job.Read()
	return toolutil.TextResult(renderPromoted(output, timeout, job.ID())), true, nil
}

// runForeground 是截止时间终止的前台路径。
func (t *pwshTool) runForeground(ctx context.Context, confined sandbox.ConfinedCommand, workdir string, effectiveTimeout time.Duration, outputLimit int) (rig.ToolResult, error) {
	runCtx, cancel := context.WithTimeout(ctx, effectiveTimeout)
	defer cancel()
	argv := confined.Argv
	output := &truncatingBuffer{limit: outputLimit}
	command := exec.CommandContext(runCtx, argv[0], argv[1:]...)
	procattr.HideWindow(command)
	// 与 bash 前台路径一致：独立进程组 + 取消时杀整个组，避免命令行派生的
	// 孙进程在工具返回超时后继续运行（审计 P1）。
	configureForegroundProcessGroup(command)
	command.Cancel = func() error { return killForegroundProcessGroup(command) }
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
	if errors.Is(runErr, exec.ErrWaitDelay) {
		// 直接子进程已退出但孙进程还攥着管道：收掉整个组。
		_ = killForegroundProcessGroup(command)
	}

	content := output.String()
	switch {
	case ctx.Err() != nil:
		// 调用方上下文被取消：中止运行，而不是把它误报为工具失败。
		return rig.ToolResult{}, ctx.Err()
	case runCtx.Err() == context.DeadlineExceeded:
		content += fmt.Sprintf("\n[pwsh timeout: command did not finish within %s]", effectiveTimeout)
		content += sandboxDenialSuffix(content, confined, t.policy)
		return rig.ToolResult{Content: content, IsError: true}, nil
	case runErr != nil:
		if exitErr, ok := errors.AsType[*exec.ExitError](runErr); ok {
			// 线协议语义：非零退出码是供模型解读的结果，而不是工具错误。上面
			// 的超时仍然是错误。失败输出匹配拒绝方言时追加沙箱归因行（RG-03）。
			code := exitErr.ExitCode()
			content += fmt.Sprintf("\n[exit code: %d]", code)
			content += sandboxDenialSuffixForExit(content, code, confined, t.policy)
			return toolutil.TextResult(content), nil
		}
		// 启动失败（缺少二进制、权限不足等）属于配置问题，而不是退出码。
		content += fmt.Sprintf("\n[pwsh: cannot start %q: %v]", argv[0], runErr)
		content += sandboxDenialSuffix(content, confined, t.policy)
		return rig.ToolResult{Content: content, IsError: true}, nil
	default:
		// 退出 0 也要归因：CLI 可能把落盘失败包成自己的错误文案后仍以 0
		// 退出。未命中方言时后缀为空串，成功路径保持逐字节不变。
		content += sandboxDenialSuffixForExit(content, 0, confined, t.policy)
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
