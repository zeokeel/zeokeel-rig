package kit_test

// 沙箱的"零思考装配"验收：**四种组合都不许让 New 失败**，且 Preflight 必须如实
// 说出现在到底有没有系统级围栏。用户视角里"装不上沙箱"不该等于"应用打不开"。

import (
	"context"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig/kit"
	"github.com/zeokeel/zeokeel-rig/sandbox"
)

// preflightRunner 是自报能力的假 runner（不执行任何东西）。
type preflightRunner struct {
	name        string
	enforcement sandbox.Enforcement
	coverage    string
	limits      string
}

func (r *preflightRunner) Confine(argv []string, policy *sandbox.Policy) (sandbox.ConfinedCommand, error) {
	return sandbox.ConfinedCommand{Argv: argv, Enforcement: r.enforcement}, nil
}

func (r *preflightRunner) RunnerName() string                     { return r.name }
func (r *preflightRunner) RunnerEnforcement() sandbox.Enforcement { return r.enforcement }
func (r *preflightRunner) RunnerLimits() string                   { return r.limits }
func (r *preflightRunner) NetworkCoverage() string                { return r.coverage }

// newPreflightAgent 用零配置 provider + 指定沙箱配置装配一个 agent。
func newPreflightAgent(t *testing.T, cfg kit.Config) *kit.Agent {
	t.Helper()
	cfg.Model = "m"
	cfg.Provider = &scriptProvider{scripts: [][]kit.Chunk{textScript("ok")}}
	if cfg.Root == "" {
		cfg.Root = t.TempDir()
	}
	agent, err := kit.New(cfg)
	if err != nil {
		t.Fatalf("kit.New 失败（沙箱环境问题不该让应用打不开）：%v", err)
	}
	return agent
}

// TestPreflightFourSandboxConfigurationsDoNotFail 是任务 B.4 的四态验收。
func TestPreflightFourSandboxConfigurationsDoNotFail(t *testing.T) {
	t.Run("有 runner（nil = 尽力而为）", func(t *testing.T) {
		agent := newPreflightAgent(t, kit.Config{
			SandboxRunner: &preflightRunner{name: "bwrap", enforcement: sandbox.EnforcementFull, coverage: "full"},
		})
		report := agent.Preflight()
		if !report.Enforced {
			t.Fatalf("Enforced = false，有 runner 时应为 true：%+v", report)
		}
		if report.Backend != "bubblewrap" {
			t.Fatalf("Backend = %q, want bubblewrap", report.Backend)
		}
		if len(report.Summaries) == 0 || !strings.Contains(report.Summaries[0], "已隔离") {
			t.Fatalf("Summaries = %v，应第一句就说已隔离", report.Summaries)
		}
	})

	t.Run("无 runner（nil = 尽力而为，退回应用层）", func(t *testing.T) {
		agent := newPreflightAgent(t, kit.Config{})
		report := agent.Preflight()
		if report.Enforced {
			t.Fatalf("Enforced = true，没有 runner 时应为 false：%+v", report)
		}
		if report.Backend != "" {
			t.Fatalf("Backend = %q，没有 runner 时应为空", report.Backend)
		}
		if len(report.Summaries) == 0 || !strings.Contains(report.Summaries[0], "未隔离") {
			t.Fatalf("Summaries = %v，应第一句就说未隔离", report.Summaries)
		}
	})

	t.Run("显式 false", func(t *testing.T) {
		agent := newPreflightAgent(t, kit.Config{Sandbox: kit.Bool(false)})
		report := agent.Preflight()
		if report.Enforced {
			t.Fatalf("显式关掉沙箱后 Enforced 应为 false：%+v", report)
		}
		if len(report.Summaries) == 0 || !strings.Contains(report.Summaries[0], "未隔离") {
			t.Fatalf("Summaries = %v", report.Summaries)
		}
	})

	t.Run("显式 true 但 runner 不可用", func(t *testing.T) {
		agent := newPreflightAgent(t, kit.Config{Sandbox: kit.Bool(true)})
		report := agent.Preflight()
		if report.Enforced {
			t.Fatalf("没有可用后端时 Enforced 应为 false：%+v", report)
		}
		// 期望与事实的落差必须说在最前面，而不是让人以为"没报错就是隔离了"。
		if len(report.Summaries) == 0 || !strings.Contains(report.Summaries[0], "你要求了系统沙箱") {
			t.Fatalf("Summaries = %v，应说明「要求了但不可用」", report.Summaries)
		}
	})
}

// TestPreflightCarriesUnavailableReason 断言宿主填的"为什么没有沙箱"会出现在 UI 文案里：
// kit 不探测平台，这句话只能由宿主（例如 osrunner.New 失败时）提供。
func TestPreflightCarriesUnavailableReason(t *testing.T) {
	agent := newPreflightAgent(t, kit.Config{SandboxUnavailableReason: "bubblewrap 未安装"})
	report := agent.Preflight()
	joined := strings.Join(report.Summaries, " ") + " " + strings.Join(report.Warnings, " ")
	if !strings.Contains(joined, "bubblewrap 未安装") {
		t.Fatalf("宿主提供的原因没有出现在预检里：%q", joined)
	}
}

// TestPreflightExplicitFalseWithRunnerIsAConfigError 是唯一允许报错的情形：
// 配置自相矛盾（一个说不要、一个注入了），不是环境问题。
func TestPreflightExplicitFalseWithRunnerIsAConfigError(t *testing.T) {
	_, err := kit.New(kit.Config{
		Model:         "m",
		Provider:      &scriptProvider{scripts: [][]kit.Chunk{textScript("ok")}},
		Root:          t.TempDir(),
		Sandbox:       kit.Bool(false),
		SandboxRunner: &preflightRunner{name: "bwrap", enforcement: sandbox.EnforcementFull},
	})
	if err == nil {
		t.Fatal("Sandbox=false 同时又注入 SandboxRunner 应当报配置错误")
	}
	if !strings.Contains(err.Error(), "Sandbox=false") {
		t.Fatalf("错误文案应点名冲突字段：%v", err)
	}
}

// TestPreflightSaysWhereToClickWhenBlocked 断言"被挡住点哪里"在同一份口径里。
func TestPreflightSaysWhereToClickWhenBlocked(t *testing.T) {
	// 没有 Asker：不许承诺"点允许这一次"。
	agent := newPreflightAgent(t, kit.Config{
		SandboxRunner: &preflightRunner{name: "landlock", enforcement: sandbox.EnforcementPartial, coverage: "full"},
	})
	steps := strings.Join(agent.Preflight().NextSteps, " ")
	if strings.Contains(steps, "允许这一次") {
		t.Fatalf("没有审批通道时不该承诺点允许：%q", steps)
	}
	if !strings.Contains(steps, "审批") {
		t.Fatalf("应说明去设置里接入审批：%q", steps)
	}
}

// TestPreflightMatchesDenialVocabulary 断言 UI 与模型看到的是同一套话术：
// Preflight 的下一步与工具拒绝时的建议都指向"允许这一次 / 可写目录"。
func TestPreflightMatchesDenialVocabulary(t *testing.T) {
	agent := newPreflightAgent(t, kit.Config{
		SandboxRunner: &preflightRunner{name: "bwrap", enforcement: sandbox.EnforcementFull, coverage: "full"},
		Asker: askerFunc(func(context.Context, string, []string) (string, error) {
			return "allow", nil
		}),
	})
	report := agent.Preflight()
	if !report.Enforced {
		t.Fatalf("应处于隔离状态：%+v", report)
	}
	steps := strings.Join(report.NextSteps, " ")
	for _, want := range []string{"允许这一次", "可写目录"} {
		if !strings.Contains(steps, want) {
			t.Fatalf("NextSteps 缺少 %q：%q", want, steps)
		}
	}
}
