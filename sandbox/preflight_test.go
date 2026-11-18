package sandbox_test

// Preflight 的验收标准只有一条：**用户读一遍就知道自己现在安不安全、能做什么、
// 被挡住了点哪里**。所以这里既断言状态字段（Enforced/Backend/Network），也断言
// 人话里必须出现/不许出现的关键词（例如"不能联网"必须出现在网络关闭时）。

import (
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig/sandbox"
)

// fakeReporter 是一个自报"完整强制"的 runner。
type fakeReporter struct {
	name        string
	enforcement sandbox.Enforcement
	limits      string
	coverage    string
}

func (r *fakeReporter) Confine(argv []string, policy *sandbox.Policy) (sandbox.ConfinedCommand, error) {
	return sandbox.ConfinedCommand{Argv: argv, Enforcement: r.enforcement}, nil
}

func (r *fakeReporter) RunnerName() string                     { return r.name }
func (r *fakeReporter) RunnerEnforcement() sandbox.Enforcement { return r.enforcement }
func (r *fakeReporter) RunnerLimits() string                   { return r.limits }
func (r *fakeReporter) NetworkCoverage() string                { return r.coverage }

func TestPreflightIsolatedFullCoverage(t *testing.T) {
	root := t.TempDir()
	policy := sandbox.New(sandbox.WithRoots(root), sandbox.WithMode(sandbox.ModeWorkspaceWrite))
	runner := &fakeReporter{name: "bwrap", enforcement: sandbox.EnforcementFull, coverage: "full"}

	report := sandbox.Preflight(policy, runner, true)
	if !report.Enforced {
		t.Fatalf("Enforced = false, want true")
	}
	if report.Backend != "bubblewrap" {
		t.Fatalf("Backend = %q, want bubblewrap", report.Backend)
	}
	if report.Network != sandbox.NetworkOff {
		t.Fatalf("Network = %q, want off", report.Network)
	}
	joined := strings.Join(report.Summaries, " ")
	if !strings.Contains(joined, "已隔离") || !strings.Contains(joined, root) {
		t.Fatalf("Summaries 没有说清「已隔离 + 只改工作区」：%q", joined)
	}
	if !strings.Contains(joined, "不能联网") {
		t.Fatalf("网络关闭时必须明说「不能联网」：%q", joined)
	}
	if len(report.NextSteps) == 0 || !strings.Contains(strings.Join(report.NextSteps, " "), "允许这一次") {
		t.Fatalf("NextSteps 没告诉用户被挡住时点哪里：%v", report.NextSteps)
	}
	warnings := strings.Join(report.Warnings, " ")
	if !strings.Contains(warnings, "这个后端不管读") {
		t.Fatalf("完整覆盖时应直说「这个后端不管读」：%q", warnings)
	}
	if strings.Contains(warnings, "通常") {
		t.Fatalf("不许出现模糊词「通常」：%q", warnings)
	}
}

func TestPreflightPartialCoverageExplainsWhatIsNotCovered(t *testing.T) {
	root := t.TempDir()
	pol := sandbox.New(sandbox.WithRoots(root), sandbox.WithMode(sandbox.ModeWorkspaceWrite))
	runner := &fakeReporter{
		name:        "windows-acl",
		enforcement: sandbox.EnforcementPartial,
		limits:      "reads and network are not governed",
		coverage:    "none",
	}
	report := sandbox.Preflight(pol, runner, true)
	if !report.Enforced {
		t.Fatal("partial 仍然是隔离，Enforced 应为 true")
	}
	if report.Backend != "Windows ACL" {
		t.Fatalf("Backend = %q", report.Backend)
	}
	if report.Network != sandbox.NetworkNotGoverned {
		t.Fatalf("Network = %q, want not-governed", report.Network)
	}
	joined := strings.Join(report.Summaries, " ")
	if !strings.Contains(joined, "部分隔离") {
		t.Fatalf("Summaries 应说明「部分隔离」：%q", joined)
	}
	warnings := strings.Join(report.Warnings, " ")
	if strings.Contains(warnings, "读") {
		t.Fatalf("partial 的摘要已经说了「读取没有限制」，Warnings 不该再重复一遍：%q", warnings)
	}
	if strings.Contains(warnings, "通常") {
		t.Fatalf("Warnings 里不许出现「通常」这类模糊词：%q", warnings)
	}
}

func TestPreflightDangerFullAccessSaysUnisolated(t *testing.T) {
	root := t.TempDir()
	pol := sandbox.New(sandbox.WithRoots(root), sandbox.WithMode(sandbox.ModeDangerFullAccess))
	runner := &fakeReporter{name: "bwrap", enforcement: sandbox.EnforcementFull, coverage: "full"}
	report := sandbox.Preflight(pol, runner, true)
	if report.Enforced {
		t.Fatal("danger-full-access 下 runner 被跳过，Enforced 必须为 false")
	}
	joined := strings.Join(report.Summaries, " ")
	if !strings.Contains(joined, "未隔离") {
		t.Fatalf("Summaries 应说未隔离：%q", joined)
	}
	if !strings.Contains(joined, "危险命令仍会被拦") {
		t.Fatalf("必须说明「危险命令仍会被拦，但不等于安全」：%q", joined)
	}
}

func TestPreflightNoRunnerSaysWhy(t *testing.T) {
	root := t.TempDir()
	pol := sandbox.New(sandbox.WithRoots(root), sandbox.WithMode(sandbox.ModeWorkspaceWrite))
	report := sandbox.Preflight(pol, nil, false, "bubblewrap 未安装")
	if report.Enforced {
		t.Fatal("没有 runner 时 Enforced 必须为 false")
	}
	if report.Backend != "" {
		t.Fatalf("Backend = %q, want empty", report.Backend)
	}
	if report.Network != sandbox.NetworkNotGoverned {
		t.Fatalf("Network = %q, want not-governed", report.Network)
	}
	joined := strings.Join(report.Summaries, " ")
	if !strings.Contains(joined, "未隔离") || !strings.Contains(joined, "系统沙箱") {
		t.Fatalf("Summaries 应说明「没有可用的系统沙箱」：%q", joined)
	}
	all := joined + " " + strings.Join(report.Warnings, " ")
	if !strings.Contains(all, "发行版包名通常是 bubblewrap") {
		t.Fatalf("无后端时应给出可执行的指引（包名通常叫什么）：%q", all)
	}
	if !strings.Contains(all, "bubblewrap 未安装") {
		t.Fatalf("应该带上具体原因：%q", all)
	}
	if steps := strings.Join(report.NextSteps, " "); !strings.Contains(steps, "不需要改配置") && !strings.Contains(steps, "不需要改任何配置") {
		t.Fatalf("NextSteps 应告诉用户怎么恢复隔离：%v", report.NextSteps)
	}
}

// TestPreflightNoAskerDoesNotPromiseApproval 断言"没有人可问"时不承诺「点允许这一次」。
func TestPreflightNoAskerDoesNotPromiseApproval(t *testing.T) {
	root := t.TempDir()
	pol := sandbox.New(sandbox.WithRoots(root), sandbox.WithMode(sandbox.ModeWorkspaceWrite))
	runner := &fakeReporter{name: "landlock", enforcement: sandbox.EnforcementPartial, coverage: "full"}
	report := sandbox.Preflight(pol, runner, false)
	steps := strings.Join(report.NextSteps, " ")
	if strings.Contains(steps, "允许这一次") {
		t.Fatalf("没有审批通道时不该承诺「点允许这一次」：%q", steps)
	}
	if !strings.Contains(steps, "审批") {
		t.Fatalf("应该说明要在设置里接入审批：%q", steps)
	}
}

// TestPreflightReportText 断言 UI 用的多行文本把三段都渲染出来。
func TestPreflightReportText(t *testing.T) {
	root := t.TempDir()
	pol := sandbox.New(sandbox.WithRoots(root), sandbox.WithMode(sandbox.ModeWorkspaceWrite))
	report := sandbox.Preflight(pol, nil, true, "sandbox-exec 不可用")
	text := report.Text()
	for _, want := range []string{"未隔离", "需要知道", "下一步"} {
		if !strings.Contains(text, want) {
			t.Fatalf("Text() 缺少 %q：\n%s", want, text)
		}
	}
}

// TestPreflightNeverLeaksJargon 是"不用理解也能懂"的硬约束：给用户看的句子
// 里不许出现内部术语。术语只允许出现在 Backend/Enforcement 这类结构化字段里。
func TestPreflightNeverLeaksJargon(t *testing.T) {
	root := t.TempDir()
	// 面向用户的句子（Summaries/Warnings/NextSteps）里不许出现内部术语。
	// 例外只有两类：
	//   - 后端名（"bubblewrap"/"Windows ACL"）只允许出现在 rigdoctor 的"检测到的后端"
	//     那一行，以及"Linux 上装一个系统沙箱（发行版包名通常是 bubblewrap）"这种
	//     可执行指引里——所以这里禁的是内部 id "bwrap"，不是包名；
	//   - json 字段名（enforcement 等）不出现在人话里。
	banned := []string{
		"osrunner", "Enforcement", "enforcement",
		"workspace-write", "read-only", "danger-full-access",
		"ModeWorkspaceWrite", "ModeDangerFullAccess",
		"bwrap", "seatbelt_", "landlock_",
	}
	cases := []struct {
		name   string
		policy *sandbox.Policy
		runner sandbox.Runner
		asker  bool
	}{
		{"isolated", sandbox.New(sandbox.WithRoots(root)), &fakeReporter{name: "bwrap", enforcement: sandbox.EnforcementFull, coverage: "full"}, true},
		{"partial", sandbox.New(sandbox.WithRoots(root)), &fakeReporter{name: "windows-acl", enforcement: sandbox.EnforcementPartial, coverage: "none", limits: "network not governed"}, true},
		{"no-runner", sandbox.New(sandbox.WithRoots(root)), nil, false},
		{"danger", sandbox.New(sandbox.WithRoots(root), sandbox.WithMode(sandbox.ModeDangerFullAccess)), nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			report := sandbox.Preflight(tc.policy, tc.runner, tc.asker, "sandbox-exec 不可用")
			text := strings.Join(report.Summaries, " ") + " " +
				strings.Join(report.Warnings, " ") + " " + strings.Join(report.NextSteps, " ")
			for _, word := range banned {
				if strings.Contains(text, word) {
					t.Fatalf("面向用户的句子里出现了内部术语 %q：\n%s", word, text)
				}
			}
		})
	}
}
