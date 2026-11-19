package kit_test

// DSH 四档权限预设的验收测试（表驱动）。
//
// 矩阵覆盖：Mode、Approval（通过 Presets() 表与门禁行为两处断言）、
// PermissionPreset()、以及一次真实工具调用得到的门禁决策；auto 档覆盖
// reviewer 放行 / 拒绝后问人 / 拒绝且无人可问 / 审查失败回退四种路径；
// 另外覆盖 custom 推导、未知预设错误、审批"默认不超时"与 AskTimeout 上界。

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/gate"
	"github.com/zeokeel/zeokeel-rig/kit"
	"github.com/zeokeel/zeokeel-rig/sandbox"
)

// presetProbeTool 是一次调用被执行过的证据：name 固定为写类工具（不在只读免问
// 名单里），Invoke 只记一笔并回 "executed"。
func presetProbeTool() *fakeTool {
	return &fakeTool{name: "write_file", fn: func(context.Context, kit.ToolCall) (kit.ToolResult, error) {
		return kit.ToolResult{Content: "executed"}, nil
	}}
}

// asleepAsker 是"人过一会儿才回"的审批通道：证明 kit 默认会等，不设 10 分钟上限。
type asleepAsker struct {
	delay  time.Duration
	answer string
	asked  int
}

func (a *asleepAsker) Ask(ctx context.Context, _ string, _ []string) (string, error) {
	a.asked++
	select {
	case <-time.After(a.delay):
	case <-ctx.Done():
		return "", ctx.Err()
	}
	if a.answer == "" {
		return "allow", nil
	}
	return a.answer, nil
}

// allowReviewer 是放行一切的自动预审器，并记录被审查的调用。
type allowReviewer struct{ requests []gate.ToolReviewRequest }

func (r *allowReviewer) ReviewTool(_ context.Context, request gate.ToolReviewRequest) (gate.ToolReviewDecision, error) {
	r.requests = append(r.requests, request)
	return gate.ToolReviewDecision{Allow: true}, nil
}

// denyReviewer 拒绝一切，并带上可读理由。
type denyReviewer struct{ requests int }

func (r *denyReviewer) ReviewTool(context.Context, gate.ToolReviewRequest) (gate.ToolReviewDecision, error) {
	r.requests++
	return gate.ToolReviewDecision{Allow: false, Reason: "touching the repo config is not allowed"}, nil
}

// failingReviewer 模拟审查失败（超时/模型出错）。
type failingReviewer struct{}

func (failingReviewer) ReviewTool(context.Context, gate.ToolReviewRequest) (gate.ToolReviewDecision, error) {
	return gate.ToolReviewDecision{}, errors.New("reviewer timed out")
}

// TestPermissionPresetMatrix 是四档的主矩阵：Mode 与 PermissionPreset() 从 agent 读，
// Approval 与 Presets() 表逐项对齐后再用"一次真实工具调用"验证门禁决策。
func TestPermissionPresetMatrix(t *testing.T) {
	presets := kit.Presets()
	wantOrder := []string{
		kit.PermissionPresetReadOnly,
		kit.PermissionPresetWorkspaceWrite,
		kit.PermissionPresetDangerFullAccess,
		kit.PermissionPresetAuto,
	}
	if len(presets) != len(wantOrder) {
		t.Fatalf("Presets() 返回 %d 档，want %d: %+v", len(presets), len(wantOrder), presets)
	}
	wantApproval := map[string]kit.ApprovalMode{
		kit.PermissionPresetReadOnly:         kit.ApprovalAsk,
		kit.PermissionPresetWorkspaceWrite:   kit.ApprovalAsk,
		kit.PermissionPresetDangerFullAccess: kit.ApprovalAuto,
		kit.PermissionPresetAuto:             kit.ApprovalAsk,
	}
	for index, preset := range presets {
		if preset.Name != wantOrder[index] {
			t.Fatalf("Presets()[%d] = %q，want %q（顺序固定）", index, preset.Name, wantOrder[index])
		}
		if preset.Approval != wantApproval[preset.Name] {
			t.Fatalf("%s 的 Approval = %q，want %q", preset.Name, preset.Approval, wantApproval[preset.Name])
		}
		if preset.Description == "" {
			t.Fatalf("%s 缺少 Description（设置界面要显示）", preset.Name)
		}
	}

	cases := []struct {
		preset         string
		wantMode       sandbox.Mode
		noAskerDenied  bool // 没有 Asker 时是否被拒（danger-full-access 不问人）
		wantGateAllows bool // 有会批准的 Asker 时是否执行
	}{
		{preset: kit.PermissionPresetReadOnly, wantMode: sandbox.ModeReadOnly, noAskerDenied: true, wantGateAllows: true},
		{preset: kit.PermissionPresetWorkspaceWrite, wantMode: sandbox.ModeWorkspaceWrite, noAskerDenied: true, wantGateAllows: true},
		{preset: kit.PermissionPresetDangerFullAccess, wantMode: sandbox.ModeDangerFullAccess, noAskerDenied: false, wantGateAllows: true},
		{preset: kit.PermissionPresetAuto, wantMode: sandbox.ModeDangerFullAccess, noAskerDenied: false, wantGateAllows: true},
	}
	for _, tc := range cases {
		t.Run(tc.preset+"/no-asker", func(t *testing.T) {
			tool := presetProbeTool()
			cfg := kit.Config{
				Model:            "m",
				Root:             t.TempDir(),
				Tools:            []kit.Tool{tool},
				PermissionPreset: tc.preset,
			}
			if tc.preset == kit.PermissionPresetAuto {
				cfg.ToolReviewer = &allowReviewer{}
			}
			agent := mustNew(t, cfg)
			if got := agent.Policy().Mode(); got != tc.wantMode {
				t.Fatalf("Mode = %q，want %q", got, tc.wantMode)
			}
			if got := agent.PermissionPreset(); got != tc.preset {
				t.Fatalf("PermissionPreset() = %q，want %q", got, tc.preset)
			}
			text, err := runOneTool(t, cfg, "write_file", `{"path":"x","content":"y"}`)
			if err != nil {
				t.Fatalf("Run error: %v", err)
			}
			executed := probeExecutions(tool) == 1
			if executed == tc.noAskerDenied {
				t.Fatalf("执行=%v，want 执行=%v（tool message=%q）", executed, !tc.noAskerDenied, text)
			}
			if tc.noAskerDenied && !strings.Contains(text, "审批通道") {
				t.Fatalf("拒绝理由不可读（应说明没有审批通道）：%q", text)
			}
		})

		t.Run(tc.preset+"/asker-approves", func(t *testing.T) {
			tool := presetProbeTool()
			asker := &asleepAsker{answer: "allow"}
			cfg := kit.Config{
				Model:            "m",
				Root:             t.TempDir(),
				Tools:            []kit.Tool{tool},
				PermissionPreset: tc.preset,
				Asker:            asker,
			}
			if tc.preset == kit.PermissionPresetAuto {
				cfg.ToolReviewer = &allowReviewer{}
			}
			if _, err := runOneTool(t, cfg, "write_file", `{"path":"x","content":"y"}`); err != nil {
				t.Fatalf("Run error: %v", err)
			}
			if !tc.wantGateAllows {
				t.Fatalf("该档本不该执行")
			}
			if probeExecutions(tool) != 1 {
				t.Fatalf("工具没有执行（gate 决策不是 allow）")
			}
		})
	}
}

// TestPermissionPresetAutoReviewPaths 覆盖 auto 的四种审查路径：放行、拒绝后问人、
// 拒绝且无人可问、审查失败回退。
func TestPermissionPresetAutoReviewPaths(t *testing.T) {
	t.Run("reviewer-allow", func(t *testing.T) {
		tool := presetProbeTool()
		reviewer := &allowReviewer{}
		asker := &asleepAsker{answer: "allow"}
		cfg := kit.Config{
			Model: "m", Root: t.TempDir(), Tools: []kit.Tool{tool},
			PermissionPreset: kit.PermissionPresetAuto,
			ToolReviewer:     reviewer,
			Asker:            asker,
		}
		if _, err := runOneTool(t, cfg, "write_file", `{"path":"x"}`); err != nil {
			t.Fatalf("Run error: %v", err)
		}
		if probeExecutions(tool) != 1 {
			t.Fatalf("reviewer 放行后工具没有执行")
		}
		if asker.asked != 0 {
			t.Fatalf("reviewer 放行仍打扰了人 %d 次", asker.asked)
		}
		if len(reviewer.requests) != 1 || reviewer.requests[0].Tool != "write_file" {
			t.Fatalf("reviewer 没有收到正确的请求: %+v", reviewer.requests)
		}
	})

	t.Run("reviewer-deny-asks-human", func(t *testing.T) {
		tool := presetProbeTool()
		reviewer := &denyReviewer{}
		asker := &asleepAsker{answer: "allow"}
		cfg := kit.Config{
			Model: "m", Root: t.TempDir(), Tools: []kit.Tool{tool},
			PermissionPreset: kit.PermissionPresetAuto,
			ToolReviewer:     reviewer,
			Asker:            asker,
		}
		text, err := runOneTool(t, cfg, "write_file", `{"path":"x"}`)
		if err != nil {
			t.Fatalf("Run error: %v", err)
		}
		if reviewer.requests != 1 {
			t.Fatalf("reviewer 调用 %d 次，want 1", reviewer.requests)
		}
		if asker.asked != 1 {
			t.Fatalf("reviewer 拒绝后应回退问人一次，实际 %d 次（tool message=%q）", asker.asked, text)
		}
		if probeExecutions(tool) != 1 {
			t.Fatalf("人批准后工具没有执行（tool message=%q）", text)
		}
	})

	t.Run("reviewer-deny-no-asker", func(t *testing.T) {
		tool := presetProbeTool()
		cfg := kit.Config{
			Model: "m", Root: t.TempDir(), Tools: []kit.Tool{tool},
			PermissionPreset: kit.PermissionPresetAuto,
			ToolReviewer:     &denyReviewer{},
		}
		text, err := runOneTool(t, cfg, "write_file", `{"path":"x"}`)
		if err != nil {
			t.Fatalf("Run error: %v", err)
		}
		if probeExecutions(tool) != 0 {
			t.Fatalf("无人可问时不应执行")
		}
		if !strings.Contains(text, "审批通道") {
			t.Fatalf("拒绝理由不可读：%q", text)
		}
	})

	t.Run("reviewer-error-falls-back-to-ask", func(t *testing.T) {
		tool := presetProbeTool()
		asker := &asleepAsker{answer: "allow"}
		cfg := kit.Config{
			Model: "m", Root: t.TempDir(), Tools: []kit.Tool{tool},
			PermissionPreset: kit.PermissionPresetAuto,
			ToolReviewer:     failingReviewer{},
			Asker:            asker,
		}
		if _, err := runOneTool(t, cfg, "write_file", `{"path":"x"}`); err != nil {
			t.Fatalf("Run error: %v", err)
		}
		if asker.asked != 1 || probeExecutions(tool) != 1 {
			t.Fatalf("审查失败应回退问人并执行，asked=%d executed=%d", asker.asked, probeExecutions(tool))
		}
	})

	t.Run("auto-without-reviewer-degrades-to-ask", func(t *testing.T) {
		agent := mustNew(t, kit.Config{
			Model: "m", Root: t.TempDir(), Approval: "",
			PermissionPreset: kit.PermissionPresetAuto,
		})
		if got := agent.PermissionPreset(); got != kit.PermissionPresetAuto {
			t.Fatalf("PermissionPreset() = %q，want auto（选择明确，只是降级）", got)
		}
		if got := agent.Policy().Mode(); got != sandbox.ModeDangerFullAccess {
			t.Fatalf("Mode = %q，want danger-full-access", got)
		}
		report := agent.Preflight()
		joined := strings.Join(report.Warnings, " ")
		if !strings.Contains(joined, "ToolReviewer") {
			t.Fatalf("Preflight 没有说明 auto 缺 ToolReviewer 的降级：%v", report.Warnings)
		}
	})
}

// TestPermissionPresetCustomDerivation 覆盖"显式字段优先 → custom"的两条规则。
func TestPermissionPresetCustomDerivation(t *testing.T) {
	t.Run("explicit-mode", func(t *testing.T) {
		agent := mustNew(t, kit.Config{
			Model:            "m",
			Root:             t.TempDir(),
			PermissionPreset: kit.PermissionPresetReadOnly,
			Mode:             sandbox.ModeWorkspaceWrite, // 显式设置优先
		})
		if got := agent.PermissionPreset(); got != kit.PermissionPresetCustom {
			t.Fatalf("PermissionPreset() = %q，want custom", got)
		}
		if got := agent.Policy().Mode(); got != sandbox.ModeWorkspaceWrite {
			t.Fatalf("Mode = %q，want 宿主显式设置的 workspace-write（预设不得覆盖）", got)
		}
	})

	t.Run("explicit-gate", func(t *testing.T) {
		tool := presetProbeTool()
		agent := mustNew(t, kit.Config{
			Model:            "m",
			Root:             t.TempDir(),
			Tools:            []kit.Tool{tool},
			PermissionPreset: kit.PermissionPresetReadOnly,
			Gate:             gate.AllowAll(), // 宿主自己的门禁
		})
		if got := agent.PermissionPreset(); got != kit.PermissionPresetCustom {
			t.Fatalf("PermissionPreset() = %q，want custom（Gate 由宿主提供）", got)
		}
		// 宿主门禁必须原样生效：不经过预设的 read-only + ask。
		if _, err := runOneTool(t, kit.Config{
			Model: "m", Root: t.TempDir(), Tools: []kit.Tool{tool},
			PermissionPreset: kit.PermissionPresetReadOnly,
			Gate:             gate.AllowAll(),
		}, "write_file", `{"path":"x"}`); err != nil {
			t.Fatalf("Run error: %v", err)
		}
		if probeExecutions(tool) != 1 {
			t.Fatalf("宿主显式 Gate 没有生效")
		}
	})

	t.Run("default-knobs-match-workspace-write", func(t *testing.T) {
		agent := mustNew(t, kit.Config{Model: "m", Root: t.TempDir()})
		if got := agent.PermissionPreset(); got != kit.PermissionPresetWorkspaceWrite {
			t.Fatalf("零配置 PermissionPreset() = %q，want workspace-write（反查表）", got)
		}
	})
}

// TestPermissionPresetUnknownIsAnError：未知预设必须在构造期报可读错误。
func TestPermissionPresetUnknownIsAnError(t *testing.T) {
	_, err := kit.New(kit.Config{
		Model:            "m",
		Root:             t.TempDir(),
		Provider:         &scriptProvider{},
		PermissionPreset: "read-only-ish",
	})
	if err == nil {
		t.Fatal("未知预设没有报错")
	}
	for _, want := range []string{"unknown permission preset", "read-only-ish", "danger-full-access"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("错误缺少 %q：%v", want, err)
		}
	}
}

// TestAskWaits:默认（AskTimeout=0）审批不超时——Asker 200ms 后才回应，决策仍是 allow。
func TestAskWaits(t *testing.T) {
	tool := presetProbeTool()
	asker := &asleepAsker{delay: 200 * time.Millisecond, answer: "allow"}
	start := time.Now()
	text, err := runOneTool(t, kit.Config{
		Model: "m", Root: t.TempDir(), Tools: []kit.Tool{tool}, Asker: asker,
	}, "write_file", `{"path":"x"}`)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	if asker.asked != 1 || probeExecutions(tool) != 1 {
		t.Fatalf("审批没有被等待：asked=%d executed=%d", asker.asked, probeExecutions(tool))
	}
	if errors.Is(err, gate.ErrApprovalExpired) {
		t.Fatalf("默认配置下不应出现审批超时: %v", err)
	}
	if elapsed < 200*time.Millisecond {
		t.Fatalf("elapsed = %s，说明没有真的等审批", elapsed)
	}
	if !strings.Contains(text, "executed") {
		t.Fatalf("工具结果 = %q", text)
	}
}

// TestAskTimeoutExpires:只有显式 AskTimeout>0 时才会出现 ErrApprovalExpired。
func TestAskTimeoutExpires(t *testing.T) {
	tool := presetProbeTool()
	asker := &asleepAsker{delay: 2 * time.Second, answer: "allow"}
	_, err := runOneTool(t, kit.Config{
		Model: "m", Root: t.TempDir(), Tools: []kit.Tool{tool},
		Asker: asker, AskTimeout: 20 * time.Millisecond,
	}, "write_file", `{"path":"x"}`)
	if err == nil {
		t.Fatal("AskTimeout=20ms 且 Asker 不回应时应当报错")
	}
	if !errors.Is(err, gate.ErrApprovalExpired) {
		t.Fatalf("错误没有包装 ErrApprovalExpired: %v", err)
	}
	if probeExecutions(tool) != 0 {
		t.Fatalf("审批超时后工具不应执行")
	}
}

// TestNoDefaultProtectionPaths：DSH 对齐后 kit 不再追加任何默认保护路径，
// 三个废弃字段也不再有行为。
func TestNoDefaultProtectionPaths(t *testing.T) {
	root := t.TempDir()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(writeFile(t, root, ".git/config", "[core]\n"))
	must(writeFile(t, root, ".aws/credentials", "[default]\n"))
	must(writeFile(t, root, ".codex/config.toml", "x\n"))
	home := secludeHome(t)
	must(writeFile(t, home, ".ssh/id_ed25519", "key\n"))

	// 不再用 Policy.ProtectedPaths() 断言（该 API 已 Deprecated，返回值恒空）；
	// 改为断言**没有根内只读子路径**——那是 DSH 对齐后唯一还生效的"局部只读"机制。
	for _, cfg := range []kit.Config{
		{Model: "m", Root: root},
		{Model: "m", Root: root, DisableProtection: false},
		{Model: "m", Root: root, DisableProtection: true},
		{Model: "m", Root: root, ProtectPaths: true},
		{Model: "m", Root: root, ProtectWorkspaceMetadata: true},
	} {
		cfg.Provider = &scriptProvider{}
		agent := mustNew(t, cfg)
		for _, writable := range agent.Policy().WritableRoots() {
			if len(writable.ReadOnlySubpaths) != 0 {
				t.Fatalf("kit 仍然登记了 .git 保护（只读子路径 %v）", writable.ReadOnlySubpaths)
			}
		}
	}
}

// probeExecutions 在持锁下读调用次数（与 fakeTool.Invoke 的写入互斥，便于 -race）。
func probeExecutions(tool *fakeTool) int {
	tool.mu.Lock()
	defer tool.mu.Unlock()
	return len(tool.calls)
}

// writeFile 在 root 下创建相对路径文件（自动建父目录）。
func writeFile(t *testing.T, root, relative, content string) error {
	t.Helper()
	path := filepath.Join(root, relative)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0o644)
}

// capturePresenter 记录 present 真的递交了哪些文件。
type capturePresenter struct {
	calls int
	files []rig.PresentedFile
}

func (p *capturePresenter) PresentFiles(_ context.Context, files []rig.PresentedFile) error {
	p.calls++
	p.files = append(p.files, files...)
	return nil
}

// TestPresenterIsActuallyWired（审计 P0-3）：Config.Presenter 非 nil 时，present
// 工具必须把文件真的交给宿主实现——只断言"工具名注册了/active"抓不到漏传
// full.AllOptions.Presenter 的缺陷。
func TestPresenterIsActuallyWired(t *testing.T) {
	root := t.TempDir()
	deliverable := filepath.Join(root, "report.txt")
	if err := os.WriteFile(deliverable, []byte("ok\n"), 0o644); err != nil {
		t.Fatalf("write deliverable: %v", err)
	}
	presenter := &capturePresenter{}
	provider := &scriptProvider{scripts: [][]kit.Chunk{
		toolCallScript(kit.ToolCall{ID: "p1", Name: "present",
			Arguments: `{"files":[{"path":` + quoteJSON(deliverable) + `,"description":"audit"}]}`}),
		textScript("done"),
	}}
	agent := mustNew(t, kit.Config{
		Model: "m", Root: root, Provider: provider,
		Presenter: presenter, Approval: kit.ApprovalAuto,
	})
	if _, _, err := agent.RunText(context.Background(), "s", "deliver", nil); err != nil {
		t.Fatalf("RunText: %v", err)
	}
	if presenter.calls != 1 {
		t.Fatalf("宿主 PresentFiles 被调用 %d 次，want 1（present 没有真的接线）", presenter.calls)
	}
	if len(presenter.files) != 1 || presenter.files[0].Path != deliverable {
		t.Fatalf("递交的文件 = %+v，want %q", presenter.files, deliverable)
	}

	// 对照：没有 Presenter 时 present 被体检摘掉（不是留一个必然报错的工具）。
	without := mustNew(t, kit.Config{Model: "m", Root: root, Provider: &scriptProvider{}})
	if contains(without.RegisteredTools(), "present") {
		t.Fatalf("没有 Presenter 时 present 不该注册: %v", without.RegisteredTools())
	}
}
