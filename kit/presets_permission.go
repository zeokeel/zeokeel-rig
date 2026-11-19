package kit

// DSH 对齐的**权限预设**：把"沙箱模式 × 审批策略"这两个旋钮捆绑成四个产品级档位。
//
// DSH（DeepSeek Harness）的 `permission-presets` 用一张"名称 → {sandbox, approval}"
// 的表表达同一件事，并把"不匹配任何预设"的状态叫 `custom`。kit 这里给出等价的四个
// 固定档位（顺序固定，见 Presets）：
//
//	read-only          只读 + ask
//	workspace-write    工作区可写 + ask（kit 的历史默认）
//	danger-full-access 全权 + auto（不问人、全部放行）
//	auto               全权 + ask + ToolReviewer 自动预审
//
// 与 DSH 的一处映射说明：DSH 的 `danger-full-access` 配 `approval=never`（"不问
// 人"），而 kit 的 `ApprovalNever` 是"拒绝所有调用"。为了让"全权"这一档真的可用，
// 这里用 kit 的 `ApprovalAuto`（= `PersonalAuto()` = `AllowAll()`：不问人、全部
// 放行）——这就是"ApprovalAuto 与 danger-full-access 预设等价"的含义。默认门禁
// 不做命令风险分类；要硬拦危险命令，宿主显式叠加 `gate.CommandRiskGate`。
//
// 语义要点：
//   - 预设只在宿主**没有**显式设置 `Gate`/`Mode`/`Approval`（以及没有传 `Policy`）时
//     生效；一旦有显式设置，实际档位退化为 `custom`，kit 不静默覆盖任何字段。
//   - `auto` 需要 `Config.ToolReviewer`；未配置时退化为纯 ask（注释与 Preflight 会说明）。

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/gate"
	"github.com/zeokeel/zeokeel-rig/sandbox"
)

// 权限预设名（与 Config.PermissionPreset 的取值一一对应）。
const (
	// PermissionPresetReadOnly 是"只读 + ask"档：不改文件、不执行可变命令。
	PermissionPresetReadOnly = "read-only"
	// PermissionPresetWorkspaceWrite 是"工作区可写 + ask"档（kit 的历史默认值）。
	PermissionPresetWorkspaceWrite = "workspace-write"
	// PermissionPresetDangerFullAccess 是"全权 + 不问人"档：移除工作区围栏，
	// 审批环节也不拦任何调用（默认不做命令风险分类）。
	PermissionPresetDangerFullAccess = "danger-full-access"
	// PermissionPresetAuto 是"全权 + ask + 自动预审"档：先用 ToolReviewer 审一次，
	// 拿不准再问人。
	PermissionPresetAuto = "auto"
	// PermissionPresetCustom 表示"当前旋钮值不匹配任何预设"（DSH 的 custom：
	// 可以显示，但不能被选中）。它不是 Config.PermissionPreset 的合法取值。
	PermissionPresetCustom = "custom"
)

// PermissionPreset 是一个权限预设：一个名字、一句人话说明，以及它写入的两个旋钮。
//
// Mode 对应 Config.Mode，Approval 对应 Config.Approval（决定内置门禁的组合）。
// Description 面向设置界面，可直接展示。
type PermissionPreset struct {
	Name        string
	Description string
	Mode        sandbox.Mode
	Approval    ApprovalMode
}

// Presets 返回内置的四个权限预设，顺序固定为
// read-only → workspace-write → danger-full-access → auto（设置界面的展示顺序）。
//
// 返回新切片，调用方可以自由修改。`custom` 不在表里：它是推导出来的状态，
// 不能作为选择目标。
func Presets() []PermissionPreset {
	return []PermissionPreset{
		{
			Name:        PermissionPresetReadOnly,
			Description: "只读：agent 不能改文件、不能执行命令；只读工具免问，其余调用会问你",
			Mode:        sandbox.ModeReadOnly,
			Approval:    ApprovalAsk,
		},
		{
			Name:        PermissionPresetWorkspaceWrite,
			Description: "只让它改工作区：只读工具免问，其余（写文件、bash/pwsh，含危险命令）都问你；审批默认不超时",
			Mode:        sandbox.ModeWorkspaceWrite,
			Approval:    ApprovalAsk,
		},
		{
			Name:        PermissionPresetDangerFullAccess,
			Description: "全权、不问人：可以改任何文件、执行任何命令，审批环节不再拦任何调用",
			Mode:        sandbox.ModeDangerFullAccess,
			Approval:    ApprovalAuto,
		},
		{
			Name:        PermissionPresetAuto,
			Description: "自动预审：全权，但每次调用先由审查模型判断，拒绝或审查失败时再问你",
			Mode:        sandbox.ModeDangerFullAccess,
			Approval:    ApprovalAsk,
		},
	}
}

// PermissionPresetNames 返回四个预设名（顺序与 Presets 一致），便于错误文案与 UI。
func PermissionPresetNames() []string {
	presets := Presets()
	names := make([]string, len(presets))
	for index, preset := range presets {
		names[index] = preset.Name
	}
	return names
}

// lookupPermissionPreset 按名字查预设（大小写与首尾空白不敏感）。
func lookupPermissionPreset(name string) (PermissionPreset, bool) {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return PermissionPreset{}, false
	}
	for _, preset := range Presets() {
		if preset.Name == name {
			return preset, true
		}
	}
	return PermissionPreset{}, false
}

// presetResolution 记录一次"配置里选了什么档"的解析结果。
type presetResolution struct {
	// selected 是规范化的选择名（"" = 宿主没选预设）。
	selected string
	// applied 表示预设真的写入了 Mode/Approval（没有被显式字段覆盖）。
	applied bool
}

// applyPermissionPreset 把 Config.PermissionPreset 落地到 Mode/Approval。
//
// 规则（与 Config.PermissionPreset 的文档一致）：只有宿主**没有**显式设置
// Gate/Mode/Approval（也没有传显式 Policy）时才写入；否则一个字段都不改、只记一条
// warning —— 显式设置优先，实际档位是 custom，绝不静默覆盖。
//
// 必须在本函数之后再跑 applyProfile：预设比"配置档"更具体，档只填剩下的空字段。
func applyPermissionPreset(cfg *Config) (presetResolution, error) {
	raw := strings.TrimSpace(cfg.PermissionPreset)
	if raw == "" {
		return presetResolution{}, nil
	}
	preset, ok := lookupPermissionPreset(raw)
	if !ok {
		return presetResolution{}, fmt.Errorf("kit: unknown permission preset %q (want %s)",
			cfg.PermissionPreset, strings.Join(PermissionPresetNames(), ", "))
	}
	var overridden []string
	// 只有"显式值与该预设不同"才算覆盖：显式写着相同的值不构成冲突，不惩罚这种
	// 冗余写法。
	if mode := strings.TrimSpace(string(cfg.Mode)); mode != "" && sandbox.Mode(mode) != preset.Mode {
		overridden = append(overridden, "Mode")
	}
	if approval := strings.TrimSpace(string(cfg.Approval)); approval != "" && ApprovalMode(approval) != preset.Approval {
		overridden = append(overridden, "Approval")
	}
	if cfg.Gate != nil {
		overridden = append(overridden, "Gate")
	}
	// Policy 非 nil 时 Mode/AllowNetwork/CommandAllowlist 都归策略所有（见 Config.Policy
	// 的文档）；此时再写 Mode 只会造成"预设说全权、策略却是别的东西"的错觉。
	if cfg.Policy != nil {
		overridden = append(overridden, "Policy")
	}
	if len(overridden) > 0 {
		slog.Warn("kit: PermissionPreset 没有生效：显式设置的字段优先，实际档位为 custom",
			"preset", preset.Name, "overridden", strings.Join(overridden, "、"))
		return presetResolution{selected: preset.Name}, nil
	}
	cfg.Mode = preset.Mode
	cfg.Approval = preset.Approval
	return presetResolution{selected: preset.Name, applied: true}, nil
}

// derivePermissionPreset 推导"当前生效的档位名"。推不出来返回 custom。
//
// 规则（顺序不能变）：
//  1. 宿主自己提供了 Gate：门禁不归 kit 组合，推不出档位 → custom；
//  2. 宿主选了预设且没有被显式字段覆盖 → 就是那个预设（此时即使 auto 缺少
//     ToolReviewer 也仍然报 auto：选择是明确的，降级行为由 Preflight 说明）；
//  3. 否则按 (Mode, Approval) 反查预设表；auto 额外要求 ToolReviewer 非 nil，
//     因为"没有审查者的全权 + ask"与 auto 不是一回事（它是自定义组合）；
//  4. 都不匹配 → custom。
func derivePermissionPreset(cfg Config, resolution presetResolution, mode sandbox.Mode, approval ApprovalMode) string {
	if cfg.Gate != nil {
		return PermissionPresetCustom
	}
	if resolution.selected != "" {
		// 选了预设但被显式字段覆盖：档位是 custom —— 即使剩下的旋钮恰好又匹配
		// 另一档，也不能把用户的选择改写成别的名字。
		if resolution.applied {
			return resolution.selected
		}
		return PermissionPresetCustom
	}
	for _, preset := range Presets() {
		if preset.Mode != mode || preset.Approval != approval {
			continue
		}
		if preset.Name == PermissionPresetAuto && cfg.ToolReviewer == nil {
			continue
		}
		return preset.Name
	}
	return PermissionPresetCustom
}

// autoReviewGate 按 DSH 的 auto 语义组合门禁：先让 reviewer 自动预审，reviewer
// 放行即执行；reviewer **明确拒绝**或审查失败（超时/出错/参数非法）都回退到人工
// ask。reviewer 为 nil 时直接返回 ask（"未配置审查者时按 ask 运行"）。
//
// 为什么不直接用 gate.Review(reviewer, gate.WithReviewFallback(ask)) 一句：
// gate 的 WithReviewFallback **只在审查失败时**回退，审查者明确拒绝会直接返回
// DecisionDeny（见 gate.WithReviewFallback 的文档）。DSH 的 auto 要在拒绝时把决定
// 交回给人，所以这里补一层。若 gate 将来提供"拒绝也回退"的选项（例如
// WithReviewFallbackOnDeny），本包装可以整体删掉换成那个选项。
func autoReviewGate(reviewer gate.ToolReviewer, ask rig.Gate) rig.Gate {
	if reviewer == nil || ask == nil {
		return ask
	}
	return &reviewAskGate{
		review: gate.Review(reviewer, gate.WithReviewFallback(ask)),
		ask:    ask,
	}
}

// reviewAskGate 在 gate.ReviewGate 之上补"拒绝也回退到人工"的语义。
type reviewAskGate struct {
	review *gate.ReviewGate
	ask    rig.Gate
}

var _ rig.Gate = (*reviewAskGate)(nil)

// Approve 实现 rig.Gate：reviewer 放行 → 放行；reviewer 拒绝 → 交给 ask 决定
// （ask 是 PersonalAsk 组合，approval=never 时它本身就是终局拒绝）。
func (g *reviewAskGate) Approve(ctx context.Context, tool rig.Tool, call rig.ToolCall) (rig.Decision, error) {
	decision, err := g.review.Approve(ctx, tool, call)
	if err != nil {
		return "", err
	}
	if decision == rig.DecisionDeny {
		return g.ask.Approve(ctx, tool, call)
	}
	return decision, nil
}

// ExplainDenial 保留拒绝理由通道：ReviewGate 会优先取回退门禁（也就是 ask）的
// 理由，其次才是审查者记录的拒绝原因，正是模型最需要的两句话。
func (g *reviewAskGate) ExplainDenial(tool rig.Tool, call rig.ToolCall) string {
	return g.review.ExplainDenial(tool, call)
}
