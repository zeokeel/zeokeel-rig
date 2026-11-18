package builtin_test

// "提权存在"必须让模型在看描述时就看得见——但**只在它真的可用时**。
//
// 背景：逐命令提权（sandbox_permissions:"require_escalated" + justification）早就
// 实现了，可描述里没提，于是模型遇到"沙箱跑不了"时只能去猜环境变量或改写法。
// 反过来，对没有提权通道的组合写"你可以提权"同样有害：那是指向一个必然失败的
// 参数。所以这两件事必须同时成立：可用时明说，不可用时一个字都不留。

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/sandbox"
	"github.com/zeokeel/zeokeel-rig/tools/builtin"
)

// unavailableEscalator 模拟"接了通道但通道自报不可用"（kit 在没 Asker、或
// approval=auto/never 时正是这种形态：对象非 nil，但 Available()==false）。
type unavailableEscalator struct{}

func (unavailableEscalator) Escalate(context.Context, builtin.EscalationRequest) (bool, error) {
	return false, nil
}

func (unavailableEscalator) Available() bool { return false }

// legacyEscalator 没有 Available()：自定义实现按"可用"处理。
type legacyEscalator struct{}

func (legacyEscalator) Escalate(context.Context, builtin.EscalationRequest) (bool, error) {
	return false, nil
}

const (
	paramSandbox    = `sandbox_permissions:"require_escalated"`
	paramJustifyLow = "justification"
)

// bashDescription 构造一份 bash 描述。
func bashDescription(t *testing.T, policy *sandbox.Policy, opts ...builtin.Option) string {
	t.Helper()
	tools := policyToolMap(t, builtin.NewWithPolicyOptions(policy, opts...))
	tool := tools[builtin.ToolBash]
	if tool == nil {
		t.Fatal("bash tool missing")
	}
	return tool.Description()
}

// pwshDescription 构造一份 pwsh 描述。
func pwshDescription(t *testing.T, policy *sandbox.Policy, opts ...builtin.Option) string {
	t.Helper()
	tool := builtin.NewPwshWithPolicyOptions(policy, opts...)
	return tool.Description()
}

// requireEscalationHint 断言描述里带完整的三条信息。
func requireEscalationHint(t *testing.T, description string) {
	t.Helper()
	for _, want := range []string{paramSandbox, paramJustifyLow, "human-approved", "no bypass"} {
		if !strings.Contains(description, want) {
			t.Fatalf("description 缺少 %q：\n%s", want, description)
		}
	}
}

// forbidEscalationHint 断言描述里完全不提提权（不能指向必然失败的参数）。
func forbidEscalationHint(t *testing.T, description string) {
	t.Helper()
	for _, banned := range []string{"sandbox_permissions", "require_escalated", paramJustifyLow} {
		if strings.Contains(description, banned) {
			t.Fatalf("提权不可用时描述里仍然出现 %q：\n%s", banned, description)
		}
	}
}

func TestBashDescriptionMentionsEscalationOnlyWhenAvailable(t *testing.T) {
	policy := sandbox.New(sandbox.WithRoots(t.TempDir()), sandbox.WithAnyCommandAllowed(true))

	available := bashDescription(t, policy, builtin.WithEscalator(&escalationRecorder{}), builtin.WithRunner(&fakeRunner{}))
	requireEscalationHint(t, available)

	// 没有提权通道。
	none := bashDescription(t, policy, builtin.WithRunner(&fakeRunner{}))
	forbidEscalationHint(t, none)

	// 接了通道但通道自报不可用（没 Asker / auto 档）。
	unavailable := bashDescription(t, policy, builtin.WithEscalator(unavailableEscalator{}), builtin.WithRunner(&fakeRunner{}))
	forbidEscalationHint(t, unavailable)

	// 有提权通道但没有 OS runner：没有沙箱就没有"跳出沙箱"。
	noRunner := bashDescription(t, policy, builtin.WithEscalator(&escalationRecorder{}))
	forbidEscalationHint(t, noRunner)

	// danger-full-access：命令本来就无围栏。
	fullAccess := bashDescription(t, sandbox.New(sandbox.WithRoots(t.TempDir()), sandbox.WithMode(sandbox.ModeDangerFullAccess)),
		builtin.WithEscalator(&escalationRecorder{}), builtin.WithRunner(&fakeRunner{}))
	forbidEscalationHint(t, fullAccess)

	// 同一份 description 不应对两种组合说同样的话。
	if available == none {
		t.Fatal("有/无提权通道时 bash 描述完全相同——模型无法区分")
	}
	// 没有 Available() 的自定义通道按可用处理（它自己会拒绝）。
	legacy := bashDescription(t, policy, builtin.WithEscalator(legacyEscalator{}), builtin.WithRunner(&fakeRunner{}))
	requireEscalationHint(t, legacy)
}

func TestPwshDescriptionMentionsEscalationOnlyWhenAvailable(t *testing.T) {
	policy := sandbox.New(sandbox.WithRoots(t.TempDir()), sandbox.WithAnyCommandAllowed(true))

	available := pwshDescription(t, policy, builtin.WithEscalator(&escalationRecorder{}), builtin.WithRunner(&fakeRunner{}))
	requireEscalationHint(t, available)

	none := pwshDescription(t, policy, builtin.WithRunner(&fakeRunner{}))
	forbidEscalationHint(t, none)

	unavailable := pwshDescription(t, policy, builtin.WithEscalator(unavailableEscalator{}), builtin.WithRunner(&fakeRunner{}))
	forbidEscalationHint(t, unavailable)

	if available == none {
		t.Fatal("有/无提权通道时 pwsh 描述完全相同——模型无法区分")
	}
}

// TestEscalationHintBudget 钉住常驻成本：那句提示连同前导空格不超过 130 字节。
func TestEscalationHintBudget(t *testing.T) {
	policy := sandbox.New(sandbox.WithRoots(t.TempDir()), sandbox.WithAnyCommandAllowed(true))
	withHint := bashDescription(t, policy, builtin.WithEscalator(&escalationRecorder{}), builtin.WithRunner(&fakeRunner{}))
	withoutHint := bashDescription(t, policy, builtin.WithRunner(&fakeRunner{}))
	delta := len(withHint) - len(withoutHint)
	if delta <= 0 {
		t.Fatalf("有提权通道时描述反而没有变长：%d", delta)
	}
	if delta > 130 {
		t.Fatalf("提权提示占 %d 字节，超过 130 字节的常驻预算", delta)
	}
	t.Logf("bash 描述增量 = %d 字节（上限 130）", delta)
}

// TestEscalationSchemaHasNoEnumLadder 断言 schema 里没有 enum 阶梯：支持的取值
// 由运行期校验（resolveEscalation）决定，写死在 schema 里等于把规则复制两遍。
func TestEscalationSchemaHasNoEnumLadder(t *testing.T) {
	policy := sandbox.New(sandbox.WithRoots(t.TempDir()), sandbox.WithAnyCommandAllowed(true))
	tools := []rig.Tool{
		policyToolMap(t, builtin.NewWithPolicyOptions(policy,
			builtin.WithEscalator(&escalationRecorder{}), builtin.WithRunner(&fakeRunner{})))[builtin.ToolBash],
		builtin.NewPwshWithPolicyOptions(policy, builtin.WithEscalator(&escalationRecorder{}), builtin.WithRunner(&fakeRunner{})),
	}
	for _, tool := range tools {
		properties, ok := tool.Schema()["properties"].(map[string]any)
		if !ok {
			t.Fatalf("%s: schema 没有 properties", tool.Name())
		}
		field, ok := properties["sandbox_permissions"].(map[string]any)
		if !ok {
			t.Fatalf("%s: schema 没有 sandbox_permissions", tool.Name())
		}
		if _, exists := field["enum"]; exists {
			t.Fatalf("%s: sandbox_permissions 里出现了 enum 阶梯：%v（取值应由运行期校验）", tool.Name(), field["enum"])
		}
		if field["type"] != "string" {
			t.Fatalf("%s: sandbox_permissions.type = %v, want string", tool.Name(), field["type"])
		}
		if description, _ := field["description"].(string); description == "" {
			t.Fatalf("%s: sandbox_permissions 没有 description", tool.Name())
		}
	}
}

// TestEscalationParamNamesMatchDenialHints 把"两处文案的参数名"钉在一起：
// 工具描述里叫 sandbox_permissions / justification，沙箱拒绝的处置建议里也必须
// 叫同样的名字，且不可用时两边都不出现。
func TestEscalationParamNamesMatchDenialHints(t *testing.T) {
	policy := sandbox.New(sandbox.WithRoots(t.TempDir()), sandbox.WithAnyCommandAllowed(true))
	tool := policyToolMap(t, builtin.NewWithPolicyOptions(policy,
		builtin.WithEscalator(&escalationRecorder{}), builtin.WithRunner(&fakeRunner{})))[builtin.ToolBash]
	description := tool.Description()

	denial := sandbox.DetectDenial("echo x: Permission denied", []string{"permission denied"}, policy, sandbox.EnforcementFull)
	if denial == nil {
		t.Fatal("denial = nil")
	}
	denial.Options = sandbox.Options{HasEscalation: true}
	steps := strings.Join(denial.NextSteps(), " ")
	for _, name := range []string{"sandbox_permissions", "require_escalated", "justification"} {
		if !strings.Contains(steps, name) {
			t.Fatalf("HasEscalation=true 的 NextSteps 缺少 %q：%s", name, steps)
		}
		if !strings.Contains(description, name) {
			t.Fatalf("工具描述缺少 %q：%s", name, description)
		}
	}

	disabled := sandbox.DetectDenial("echo x: Permission denied", []string{"permission denied"}, policy, sandbox.EnforcementFull)
	disabled.Options = sandbox.Options{}
	for _, name := range []string{"sandbox_permissions", "require_escalated", "justification"} {
		if strings.Contains(strings.Join(disabled.NextSteps(), " "), name) {
			t.Fatalf("HasEscalation=false 的 NextSteps 不该出现 %q", name)
		}
	}

	// 常驻开关：两处都用到同一个常量值，改名只改一处会立刻红。
	if !strings.Contains(description, builtin.EscalationRequireEscalated) {
		t.Fatalf("描述里没有 EscalationRequireEscalated 常量值")
	}
	_ = time.Second
}
