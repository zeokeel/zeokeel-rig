package gate

// 本文件提供"命令级"审批门禁：在工具粒度之外，再按 bash/pwsh 的 command 参数
// 内容决定要不要打扰人。
//
// 背景：gate.SandboxReadOnlyTools 只能按工具名放行，一旦放行 bash，其中 `ls`
// 与 `rm -rf /` 无差别。CommandRiskGate 用 sandbox.ClassifyShellCommand 对命令
// 文本做风险分级，于是可以获得"只读自动放行、危险直接拒绝、其余问人"的行为：
//
//	g := gate.AllowThenAsk(
//		gate.SandboxReadOnlyTools,                    // 文件读取类工具
//		gate.Ask(host.Asker),                         // 其余：人工审批
//	)
//	g = gate.CommandRiskGate(g)                       // bash/pwsh 再细到命令粒度
//
// 对 bash/pwsh 的调用：
//
//	RiskReadOnly  → DecisionAllow（不打扰人）
//	RiskDangerous → DecisionDeny（拒绝；agent 会把它作为工具错误回传给模型）
//	RiskUnknown / RiskMutating → 委托 inner（通常是人工审批）
//
// 参数取自 call.Arguments 的 JSON 字段 command（bash 与 pwsh 都用这个字段；
// run_in_background、timeout_ms 等其它字段被忽略）。参数不是合法 JSON、缺少
// command 字段或该字段为空时一律委托 inner——判定不了就不要替人做决定
// （fail-safe），inner 为 nil 时返回配置错误而不是放行（fail-closed）。
//
// # 拒绝原因的可见性
//
// rig.Gate 的契约只有 Decision，没有"拒绝理由"通道：agent 固定把拒绝渲染成
// "tool call denied by approval gate: <tool>"，门禁无法往模型侧附带文案（见
// 该契约的文档）。因此命中的规则 id 与中文理由通过两处暴露给宿主：
//
//   - WithCommandRiskObserver 注册的观察者（供审计日志与审批 UI 展示）；
//   - AssessToolCommand，宿主可以在自己的 AskPrompt 里把规则渲染进提问文案。
//
// # 边界
//
// 与所有进程内门禁一样，它不是安全边界：base64 解码后执行、编译后运行、动态
// 拼装的命令一律无法识别（会被判成 RiskUnknown 而转人工审批）。不可信模型仍需
// OS 沙箱（sandbox/osrunner）或容器。

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/sandbox"
)

// defaultCommandToolNames 是默认被判定命令参数的工具名。bash 与 pwsh 的
// command 字段语义相同，因此共用一套判定。
var defaultCommandToolNames = []string{"bash", "pwsh"}

// commandRiskConfig 是 CommandRiskGateWithOptions 的配置。
type commandRiskConfig struct {
	toolNames []string
	observer  func(tool rig.Tool, call rig.ToolCall, assessment sandbox.CommandAssessment)
}

// CommandRiskOption 配置命令级风险门禁。
type CommandRiskOption func(*commandRiskConfig)

// WithCommandRiskTools 覆盖被判定命令参数的工具名（默认 "bash"、"pwsh"）。
// 传入的名字会去空白并去重；全部为空时回退到默认名单。
func WithCommandRiskTools(names ...string) CommandRiskOption {
	copied := append([]string(nil), names...)
	return func(cfg *commandRiskConfig) {
		cfg.toolNames = normalizeToolNames(copied)
		if len(cfg.toolNames) == 0 {
			cfg.toolNames = append([]string(nil), defaultCommandToolNames...)
		}
	}
}

// WithCommandRiskObserver 注册观察者：每次成功判定一条命令后调用一次（在调用
// Approve 的 goroutine 上串行执行，必须足够快且不得回调同一个门禁）。宿主用它
// 把命中的规则 id 与中文理由写进审计日志或审批 UI——这是规则信息唯一的对外通道，
// 因为 rig.Gate 无法把理由附带给模型。fn 为 nil 时不做任何事。
func WithCommandRiskObserver(fn func(tool rig.Tool, call rig.ToolCall, assessment sandbox.CommandAssessment)) CommandRiskOption {
	return func(cfg *commandRiskConfig) { cfg.observer = fn }
}

// CommandRiskGate 返回按命令内容判定风险的门禁。
//
//	toolNames 为空（不传）时默认 {"bash", "pwsh"}。
//
// 决策规则见文件头。inner 为 nil 时：只读命令仍然放行、危险命令仍然拒绝，其余
// 情况（需要审批但没有 inner）返回配置错误（fail closed，绝不放行）。
func CommandRiskGate(inner rig.Gate, toolNames ...string) rig.Gate {
	opts := make([]CommandRiskOption, 0, 1)
	if len(toolNames) > 0 {
		opts = append(opts, WithCommandRiskTools(toolNames...))
	}
	return CommandRiskGateWithOptions(inner, opts...)
}

// CommandRiskGateWithOptions 是 CommandRiskGate 的可选配置版本：除了工具名，
// 还能注册观察者（见 WithCommandRiskObserver）。
func CommandRiskGateWithOptions(inner rig.Gate, opts ...CommandRiskOption) rig.Gate {
	cfg := commandRiskConfig{toolNames: append([]string(nil), defaultCommandToolNames...)}
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	commandTools := make(map[string]struct{}, len(cfg.toolNames))
	for _, name := range cfg.toolNames {
		commandTools[name] = struct{}{}
	}
	return funcGate{fn: func(ctx context.Context, tool rig.Tool, call rig.ToolCall) (rig.Decision, error) {
		name := toolCallName(tool, call)
		if _, ok := commandTools[name]; !ok {
			// 不是命令类工具：本门禁不参与判定。
			if inner == nil {
				return "", fmt.Errorf("gate: tool %q is not a command tool and no inner gate is configured", name)
			}
			return inner.Approve(ctx, tool, call)
		}
		assessment, ok := AssessToolCommand(tool, call)
		if !ok {
			// 判定不了（无参数/坏 JSON/空命令）：交给 inner 决定。
			if inner == nil {
				return "", fmt.Errorf("gate: cannot classify the command of tool %q and no inner gate is configured", name)
			}
			return inner.Approve(ctx, tool, call)
		}
		if cfg.observer != nil {
			cfg.observer(tool, call, assessment)
		}
		switch assessment.Risk {
		case sandbox.RiskReadOnly:
			return rig.DecisionAllow, nil
		case sandbox.RiskDangerous:
			return rig.DecisionDeny, nil
		}
		if inner == nil {
			return "", fmt.Errorf("gate: command for tool %q needs approval (%s: %s) but no inner gate is configured",
				name, assessment.Detail, assessment.Reason)
		}
		return inner.Approve(ctx, tool, call)
	}}
}

// AssessToolCommand 解析一次工具调用的 command 参数并做风险判定。
//
// ok=false 表示这条调用没有可判定的命令（参数为空、不是合法 JSON、缺少 command
// 字段或该字段为空白）——调用方应把它当作"判定失败"处理，而不是"只读"。
//
// 它不检查工具名：CommandRiskGate 自己按工具名过滤；宿主在构造审批文案时可以直接
// 调用它，把 assessment.Detail/Reason 展示给人。
func AssessToolCommand(tool rig.Tool, call rig.ToolCall) (sandbox.CommandAssessment, bool) {
	command, ok := commandArgument(call.Arguments)
	if !ok {
		return sandbox.CommandAssessment{}, false
	}
	return sandbox.ClassifyShellCommandOnPlatform(command, isWindowsShellTool(toolCallName(tool, call))), true
}

// isWindowsShellTool 报告该工具名是否代表 PowerShell/cmd 方言。判定方言影响
// Windows 专属危险规则（format、diskpart、rd /s /q、reg delete、Remove-Item …）：
// 同一段文本在 bash 工具里只是未知程序，在 pwsh 工具里必须是危险命令。
func isWindowsShellTool(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "pwsh", "powershell", "powershell.exe", "pwsh.exe", "cmd", "cmd.exe":
		return true
	}
	return false
}

// commandArgument 从工具调用参数里取出 command 字段。
func commandArgument(arguments string) (string, bool) {
	trimmed := strings.TrimSpace(arguments)
	if trimmed == "" || trimmed == "null" {
		return "", false
	}
	var args struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal([]byte(trimmed), &args); err != nil {
		return "", false
	}
	command := strings.TrimSpace(args.Command)
	if command == "" {
		return "", false
	}
	return command, true
}

// normalizeToolNames 去掉空白与重复的工具名。
func normalizeToolNames(names []string) []string {
	out := make([]string, 0, len(names))
	seen := make(map[string]struct{}, len(names))
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if _, exists := seen[name]; exists {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	return out
}
