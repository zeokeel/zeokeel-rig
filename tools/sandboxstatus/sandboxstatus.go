package sandboxstatus

import (
	"context"
	"encoding/json"
	"os"
	"runtime"
	"strings"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/sandbox"
	"github.com/zeokeel/zeokeel-rig/tools/internal/toolutil"
)

// ToolSandboxStatus 是工具面向模型的稳定名称。
const ToolSandboxStatus = "sandbox_status"

// description 是模型可见的工具描述（稳定文案：不出现具体产品/厂商/工具目录名）。
const description = "Report the sandbox policy in effect for this session: mode, workspace roots, writable roots " +
	"(including runtime write grants), protected read-only paths, whether outbound network is allowed, and whether " +
	"a new writable directory can still be requested. Read-only and side-effect free. Use it after a write is denied " +
	"to decide whether to redirect a tool's own state into a writable root or to request one specific directory."

// guidance 是通用处置顺序（同样是稳定文案，只讲方法、不点名任何工具或目录）。
const guidance = "This is the sandbox policy in effect, not a permission prompt. When something cannot be written: " +
	"read the exact path from that message, then prefer redirecting the tool's own state into a writable root " +
	"(a config/state/cache directory flag, or an XDG_* / HOME environment override); only if the tool insists on one " +
	"fixed absolute path, request a writable root for exactly that directory and explain why the tool needs it; " +
	"never repeat the same failing command unchanged. Use the temporary directories when tempWritable reports them " +
	"as writable."

// Option 配置 sandbox_status 工具。
type Option func(*options)

// options 是 New 的可选配置。
type options struct {
	// runnerInfo 返回 runner 名称与其 enforcement；nil 表示宿主没有 runner，
	// 或不愿公开它。
	runnerInfo func() (name string, enforcement sandbox.Enforcement)
	// runtimeView 返回"实际生效"的运行时诊断（缓存根、网络覆盖度、被屏蔽的
	// socket）。provider 由宿主注入（它知道用哪个 runner / 哪个 profile），
	// 工具本身不去探测平台。
	runtimeView func() RuntimeView
}

// RuntimeView 是宿主报告的运行时诊断快照。
//
// 字段语义（读的时候请成对看）：
//   - ToolchainCaches：本次**已授权**的工具链缓存根。可以包含尚不存在的路径——
//     可写的语义是"可以创建"，它们在首次 `Confine` 时会以 0700 目录被补出来；
//   - MissingCaches：声明了但**本次未能授权**的项（创建失败、或该后端要求路径
//     已存在且仍不存在）。非空表示宿主需要介入（只读 HOME、ENOTDIR 等）；
//   - NetworkCoverage：runner 对 `AllowNetwork=false` 的真实覆盖度（full/none）；
//   - MaskedSockets：被 `/dev/null` 覆盖的宿主守护进程 socket（防御纵深）。
//
// 空切片一律渲染成 []（不是 null），避免模型读成"未知"。
type RuntimeView struct {
	ToolchainCaches    []string
	NetworkCoverage    string
	MaskedSockets      []string
	MissingCaches      []string
	PrivateTmpDisabled string
}

// WithRuntimeView 让工具报告运行时的实际生效情况。provider 每次调用求值。
func WithRuntimeView(provider func() RuntimeView) Option {
	return func(opts *options) { opts.runtimeView = provider }
}

// WithRunnerInfo 让工具报告 OS runner 的名称与 enforcement（例如
// "bwrap"/"full"）。provider 在每次调用时求值，因此可以反映 runner 的运行时
// 状态；返回空名称时省略 runner 字段。不设置时工具不报告 runner 信息。
func WithRunnerInfo(provider func() (name string, enforcement sandbox.Enforcement)) Option {
	return func(opts *options) { opts.runnerInfo = provider }
}

// tempWritable 报告两个平台临时目录开关。
type tempWritable struct {
	// SlashTmp 报告字面 /tmp 是否可写。
	SlashTmp bool `json:"slashTmp"`
	// TmpdirEnv 报告 $TMPDIR（os.TempDir()）是否可写。
	TmpdirEnv bool `json:"tmpdirEnv"`
}

// grantView 是一次已批准的运行期可写授权。
type grantView struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// runnerView 是挂接的 OS runner 的只读描述。
type runnerView struct {
	Name        string `json:"name"`
	Enforcement string `json:"enforcement"`
}

// status 是工具的 JSON 载荷。字段顺序即序列化顺序：两次调用逐字节一致（除
// policy 的运行时授权这类真实状态变化之外）。
type status struct {
	Mode                    string       `json:"mode"`
	WorkspaceRoots          []string     `json:"workspaceRoots"`
	WritableRoots           []string     `json:"writableRoots"`
	ConfiguredWritableRoots []string     `json:"configuredWritableRoots"`
	TempWritable            tempWritable `json:"tempWritable"`
	ProtectedPaths          []string     `json:"protectedPaths"`
	Network                 bool         `json:"network"`
	Grants                  []grantView  `json:"grants"`
	GrantBudget             int          `json:"grantBudget"`
	GrantBudgetLeft         int          `json:"grantBudgetLeft"`
	CanRequestWritablePath  bool         `json:"canRequestWritablePath"`
	Runner                  *runnerView  `json:"runner,omitempty"`
	ToolchainCaches         []string     `json:"toolchainCaches"`
	NetworkCoverage         string       `json:"networkCoverage"`
	MaskedSockets           []string     `json:"maskedSockets"`
	MissingToolchainCaches  []string     `json:"missingToolchainCaches"`
	PrivateTmpDisabled      string       `json:"privateTmpDisabled,omitempty"`
	Guidance                string       `json:"guidance"`
}

// statusTool 是 sandbox_status 的 rig.Tool 实现。
type statusTool struct {
	policy *sandbox.Policy
	opts   options
}

// New 返回绑定到 policy 的 sandbox_status 工具。它是只读工具：不修改策略、不
// 触发授权、不产生文件副作用。
//
// policy 为 nil 时工具仍然可调用，但每次调用返回 IsError=true 的可读错误结果
// （宿主没有配置策略时，汇报“什么都没有放开”只会误导模型），绝不 panic。
func New(policy *sandbox.Policy, opts ...Option) rig.Tool {
	tool := &statusTool{policy: policy}
	for _, opt := range opts {
		if opt != nil {
			opt(&tool.opts)
		}
	}
	return tool
}

// Name 实现 rig.Tool。
func (t *statusTool) Name() string { return ToolSandboxStatus }

// Description 实现 rig.Tool。
func (t *statusTool) Description() string { return description }

// Schema 实现 rig.Tool：本工具没有参数。
func (t *statusTool) Schema() map[string]any {
	return toolutil.ObjectSchema(map[string]any{})
}

// Invoke 实现 rig.Tool：序列化当前策略快照。
func (t *statusTool) Invoke(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
	if err := ctx.Err(); err != nil {
		return rig.ToolResult{}, err
	}
	if t.policy == nil {
		return toolutil.ErrorResultf("%s: no sandbox policy is configured; the host must pass a *sandbox.Policy", ToolSandboxStatus), nil
	}
	payload := t.snapshot()
	encoded, err := json.Marshal(payload)
	if err != nil {
		return toolutil.ErrorResultf("%s: %v", ToolSandboxStatus, err), nil
	}
	return toolutil.TextResult(string(encoded)), nil
}

// snapshot 读取策略（与可选 runner）的当前状态。
//
// 所有切片都渲染成 JSON 数组（空也是 []，不是 null），避免模型看到 null 时误判
// 为“未知”。授权按批准顺序输出；预算剩余 = 上限 - 已批准数。
func (t *statusTool) snapshot() status {
	policy := t.policy
	grants := policy.Grants()
	views := make([]grantView, 0, len(grants))
	for _, grant := range grants {
		views = append(views, grantView{Path: grant.Path, Reason: grant.Reason})
	}
	budget := policy.GrantBudget()
	left := budget - len(grants)
	if left < 0 {
		left = 0
	}
	payload := status{
		Mode:                    string(policy.Mode()),
		WorkspaceRoots:          nonNil(policy.Roots()),
		WritableRoots:           nonNil(writablePaths(policy)),
		ConfiguredWritableRoots: nonNil(policy.ConfiguredWritableRoots()),
		TempWritable: tempWritable{
			SlashTmp:  policy.IncludeSlashTmp(),
			TmpdirEnv: policy.IncludeTmpdirEnv(),
		},
		ProtectedPaths:         nonNil(policy.ProtectedPaths()),
		Network:                policy.NetworkAllowed(),
		Grants:                 views,
		GrantBudget:            budget,
		GrantBudgetLeft:        left,
		CanRequestWritablePath: canRequestWritablePath(policy, left),
		Guidance:               guidance,
	}
	// 运行时诊断：没有 provider 时也要渲染成 [] 与 "none"，避免模型把 null
	// 误读成"未知/不限制"。
	payload.ToolchainCaches = []string{}
	payload.NetworkCoverage = "none"
	payload.MaskedSockets = []string{}
	payload.MissingToolchainCaches = []string{}
	if t.opts.runtimeView != nil {
		view := t.opts.runtimeView()
		payload.ToolchainCaches = nonNil(view.ToolchainCaches)
		payload.MaskedSockets = nonNil(view.MaskedSockets)
		payload.MissingToolchainCaches = nonNil(view.MissingCaches)
		if coverage := strings.TrimSpace(view.NetworkCoverage); coverage != "" {
			payload.NetworkCoverage = coverage
		}
		payload.PrivateTmpDisabled = view.PrivateTmpDisabled
	}
	if t.opts.runnerInfo != nil {
		name, enforcement := t.opts.runnerInfo()
		if name = strings.TrimSpace(name); name != "" {
			payload.Runner = &runnerView{Name: name, Enforcement: string(enforcement)}
		}
	}
	return payload
}

// writablePaths 返回实际可写面：工作区根 + 追加可写根（静态配置与运行时授权）
// + 已启用的平台临时目录。
//
// 只读模式没有任何可写面（工具会拒绝写入），因此返回空列表而不是把临时目录
// 列进去——那会让模型以为还能写。
func writablePaths(policy *sandbox.Policy) []string {
	if policy.ReadOnly() {
		return nil
	}
	out := append([]string(nil), policy.Roots()...)
	out = append(out, policy.AdditionalWritableRoots()...)
	if policy.Mode() != sandbox.ModeDangerFullAccess {
		out = append(out, tempPaths(policy)...)
	}
	return dedupe(out)
}

// tempPaths 返回已启用的平台临时目录。Windows 没有字面 /tmp，因此 slashTmp
// 只在非 Windows 平台贡献一个路径。
func tempPaths(policy *sandbox.Policy) []string {
	var out []string
	if policy.IncludeSlashTmp() && runtime.GOOS != "windows" {
		out = append(out, "/tmp")
	}
	if policy.IncludeTmpdirEnv() {
		if dir := strings.TrimSpace(os.TempDir()); dir != "" {
			out = append(out, dir)
		}
	}
	return out
}

// canRequestWritablePath 报告现在是否还能申请新的可写目录：只有 workspace-write
// 且授权预算未用尽时才可能。read-only 模式按设计不放开写入；danger-full-access
// 已经无需申请。
func canRequestWritablePath(policy *sandbox.Policy, left int) bool {
	if left <= 0 {
		return false
	}
	return policy.Mode() == sandbox.ModeWorkspaceWrite
}

// nonNil 把 nil 切片规范化成空切片，使 JSON 渲染为 [] 而不是 null。
func nonNil(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}

// dedupe 保序去重（空串被丢弃）。
func dedupe(values []string) []string {
	out := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}
