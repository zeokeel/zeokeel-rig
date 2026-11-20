package sandboxgrant

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/sandbox"
	"github.com/zeokeel/zeokeel-rig/tools/internal/toolutil"
)

// ToolName 是工具对模型宣告的名字。
const ToolName = "request_sandbox_access"

// reason 参数的字符数范围（按 Unicode 字符计，不是字节）。
const (
	minReasonChars = 8
	maxReasonChars = 500
)

// grantArgs 是 request_sandbox_access 的参数对象。
//
// 刻意只有两个字段：授权是会话级、路径作用域的，因此没有 scope/mode/once 这类
// 逐次提权旋钮。
type grantArgs struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// grantResult 是成功（含"本来就可写"）时回填给模型的 JSON 形状。
//
// 字段顺序即序列化顺序：granted、path、alreadyWritable、budgetLeft、note。
type grantResult struct {
	Granted bool   `json:"granted"`
	Path    string `json:"path"`
	// AlreadyWritable 只在无需授权（路径已可写或模式已全开）时出现，提示模型不要
	// 重复申请。
	AlreadyWritable bool   `json:"alreadyWritable,omitempty"`
	BudgetLeft      int    `json:"budgetLeft"`
	Note            string `json:"note"`
}

// noteGranted 是新增授权时回填给模型的说明（逐字稳定）。
const noteGranted = "the path is now writable for this session; retry the failed command once and do not request it again"

// noteAlreadyWritable 是"本来就可写"时回填给模型的说明（逐字稳定）。
const noteAlreadyWritable = "the path is already writable for this session and no grant was used; retry the failed command once and do not request it again"

// Option 配置 New 返回的工具。
type Option func(*tool)

// tool 实现 rig.Tool。
type tool struct {
	policy   *sandbox.Policy
	observer func(rig.ToolCall, sandbox.Grant, error)
}

var _ rig.Tool = (*tool)(nil)

// New 返回 request_sandbox_access 工具。policy 为 nil 时工具仍可用，但每次调用
// 都会以 IsError 结果说明"没有沙箱策略，无法授予"——这比悄悄成功更安全，也让
// 装配错误在模型侧可见。
//
// 工具可安全并发调用：Policy 的授权集合本身并发安全，工具自身无状态。
func New(policy *sandbox.Policy, opts ...Option) rig.Tool {
	t := &tool{policy: policy}
	for _, opt := range opts {
		if opt != nil {
			opt(t)
		}
	}
	return t
}

// WithObserver 注册审计钩子：每次调用结束后都会被调用一次，成功与拒绝都包括，
// 包括 nil 策略、参数校验失败与重复申请。
//
// 回调收到的是调用方与结果本身：grant 在成功时是 sandbox.Policy 批准的授权
// （已规范化的 Path 与 At 时间戳）；在未成功时是"被拒的请求"（Path/Reason 为
// 参数里的值，At 为零）。err 与工具内部的判断逐字一致：新增授权为 nil，
// 已可写为包装了 sandbox.ErrGrantRedundant 的错误（工具对模型仍然报告
// granted:true），其余为拒绝原因。fn 为 nil 时等价于不注册。
//
// 回调在 Invoke 的 goroutine 上同步执行，必须自行保证并发安全且不能阻塞太久。
func WithObserver(fn func(rig.ToolCall, sandbox.Grant, error)) Option {
	return func(t *tool) { t.observer = fn }
}

// Name 实现 rig.Tool。
func (t *tool) Name() string { return ToolName }

// Description 实现 rig.Tool。文案对模型逐字稳定，且不含任何厂商/目录名。
func (t *tool) Description() string {
	return "Request write access to one specific directory for this session. " +
		"The sandbox only lets tools write inside the workspace and the platform temporary directories. " +
		"When a tool must keep state at a fixed location (its own credentials or cache directory) and the write was denied, use this tool to ask for access to that directory. " +
		"First try to redirect the tool instead (a config-dir option, XDG_* environment variables, or HOME), so the writable surface is not widened at all. " +
		"Request exactly one directory per call — the narrowest directory the tool actually uses — and give a concrete reason. " +
		"Approval is a human decision: this tool only submits the request, so it can be denied. " +
		"If a request is denied, do not ask again for the same path; report the denial and continue without that write."
}

// Schema 实现 rig.Tool。
func (t *tool) Schema() map[string]any {
	reasonProp := toolutil.StringProp("Why this tool must write to that directory (8-500 characters). " +
		"Shown to the approver and recorded in the audit log.")
	reasonProp["minLength"] = minReasonChars
	reasonProp["maxLength"] = maxReasonChars
	return toolutil.ObjectSchema(map[string]any{
		"path": toolutil.StringProp("Absolute path of the directory to make writable. " +
			"Give the specific directory the tool writes to, not a parent directory; it may not exist yet, but its parent must."),
		"reason": reasonProp,
	}, "path", "reason")
}

// Invoke 实现 rig.Tool。
func (t *tool) Invoke(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
	if err := ctx.Err(); err != nil {
		return rig.ToolResult{}, err
	}
	args, err := decodeArgs(call)
	requested := sandbox.Grant{Path: strings.TrimSpace(args.Path), Reason: strings.TrimSpace(args.Reason)}
	if err != nil {
		return t.refuse(call, requested, err), nil
	}
	path, reason := requested.Path, requested.Reason
	if path == "" {
		return t.refuse(call, requested, errors.New(`path is required: give the absolute directory path the tool must write to`)), nil
	}
	if n := utf8.RuneCountInString(reason); n < minReasonChars {
		return t.refuse(call, requested, fmt.Errorf(
			"reason is too short (%d characters): describe in %d-%d characters why the tool must write to this directory",
			n, minReasonChars, maxReasonChars)), nil
	} else if n > maxReasonChars {
		return t.refuse(call, requested, fmt.Errorf(
			"reason is too long (%d characters): keep it within %d characters", n, maxReasonChars)), nil
	}
	if t.policy == nil {
		return t.refuse(call, requested, errors.New(
			"no sandbox policy is attached, so no writable path can be granted; report this to the user instead of retrying")), nil
	}

	grant, grantErr := t.policy.AddWritableRoot(path, reason)
	if grantErr == nil {
		t.notify(call, grant, nil)
		return jsonResult(grantResult{
			Granted:    true,
			Path:       grant.Path,
			BudgetLeft: t.budgetLeft(),
			Note:       noteGranted,
		})
	}
	if errors.Is(grantErr, sandbox.ErrGrantRedundant) {
		// 不是失败：路径本来就可写（或在工作区内、或模式已全开）。让模型停止重复申请。
		t.notify(call, requested, grantErr)
		return jsonResult(grantResult{
			Granted:         true,
			Path:            path,
			AlreadyWritable: true,
			BudgetLeft:      t.budgetLeft(),
			Note:            noteAlreadyWritable,
		})
	}
	return t.refuse(call, requested, fmt.Errorf("%v; %s", grantErr, t.refusalAdvice(grantErr))), nil
}

// refuse 通知观察者并构造一次 IsError 结果。
func (t *tool) refuse(call rig.ToolCall, requested sandbox.Grant, err error) rig.ToolResult {
	t.notify(call, requested, err)
	return toolutil.ErrorResultf("%s: %v", ToolName, err)
}

// notify 调用观察者（nil 时安全跳过）。
func (t *tool) notify(call rig.ToolCall, grant sandbox.Grant, err error) {
	if t.observer == nil {
		return
	}
	t.observer(call, grant, err)
}

// budgetLeft 返回本次授权后仍可申请的路径数。
func (t *tool) budgetLeft() int {
	if t.policy == nil {
		return 0
	}
	left := t.policy.GrantBudget() - len(t.policy.Grants())
	if left < 0 {
		left = 0
	}
	return left
}

// refusalAdvice 把策略层的拒绝哨兵翻译成模型可执行的一句话：哪条规则拒绝了它、
// 以及应当怎么办。原始错误文本由调用方保留在前面，这里只补"下一步"。
func (t *tool) refusalAdvice(err error) string {
	switch {
	case errors.Is(err, sandbox.ErrGrantBudgetExceeded):
		return fmt.Sprintf("the session's writable-root grant budget of %d is exhausted, so do not request more paths; ask the user to widen the sandbox configuration (or switch permission mode) instead",
			t.policy.GrantBudget())
	case errors.Is(err, sandbox.ErrGrantSensitivePath):
		return "system directories can never be made writable; request the narrowest non-system directory the tool actually uses"
	case errors.Is(err, sandbox.ErrGrantTooBroad):
		return "the path is too broad (it contains the workspace root, or it is the home directory itself); request the specific subdirectory the tool uses"
	case errors.Is(err, sandbox.ErrGrantNotAbsolute):
		return "the path must be an absolute directory path with no \"..\" segments; if it does not exist yet, its parent must already exist and be a directory"
	default:
		return "the request was refused and no path was granted; check that the path is an existing directory that the tool really uses, or ask the user to adjust the sandbox configuration"
	}
}

// decodeArgs 解码参数并拒绝未知字段。Schema 已经声明 additionalProperties=false；
// 这里在实现层再严格一次，避免绕过 schema 的调用方（例如直接构造 ToolCall 的宿主）
// 悄悄塞进 scope/mode 之类不存在的旋钮。
func decodeArgs(call rig.ToolCall) (grantArgs, error) {
	var args grantArgs
	if err := toolutil.DecodeArgs(call, &args); err != nil {
		return args, err
	}
	raw := strings.TrimSpace(call.Arguments)
	if raw == "" {
		return args, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &fields); err != nil {
		// 上面的 DecodeArgs 已经验证过 JSON 合法性；这里只可能是类型不符（数组/
		// 标量），按"参数必须是对象"报错。
		return args, errors.New(`arguments must be a JSON object with "path" and "reason"`)
	}
	unknown := make([]string, 0, len(fields))
	for name := range fields {
		if name != "path" && name != "reason" {
			unknown = append(unknown, name)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return args, fmt.Errorf(`unknown argument(s) %s: this tool accepts only "path" and "reason"; a grant is session-wide, so there is no per-call scope or mode`,
			strings.Join(unknown, ", "))
	}
	return args, nil
}

// jsonResult 序列化成功结果。形状固定，编码不会失败；真失败也回造成工具错误
// 而不是静默成功。
func jsonResult(result grantResult) (rig.ToolResult, error) {
	payload, err := json.Marshal(result)
	if err != nil {
		return toolutil.ErrorResultf("%s: encode result: %v", ToolName, err), nil
	}
	return toolutil.TextResult(string(payload)), nil
}
