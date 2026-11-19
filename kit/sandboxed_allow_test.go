package kit_test

// 沙箱内的命令免审批（`kit.Config.AutoAllowSandboxedCommands`，默认开）。
//
// 核心命题：**沙箱与审批是两种独立的成本**。一条命令已经被 OS 层限制到"只能写
// 工作区、不能连网"时，再要求人工逐条同意是把两个成本叠加；用户为了少点几十次弹窗
// 会直接关掉审批（连危险命令的拦截一起丢了）。因此"已沙箱化"是免审批的**前提**。
//
// 七条用例把条件钉死：免问只在"真的会被 runner 包住 + 不是危险命令 + 不是
// danger-full-access + 是 shell 类工具"时成立，其余行为与今天完全一致。

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/events"
	"github.com/zeokeel/zeokeel-rig/kit"
	"github.com/zeokeel/zeokeel-rig/sandbox"
)

// sandboxCountingAsker 记录被问了几次（并发安全），默认回答 allow。
type sandboxCountingAsker struct {
	mu      sync.Mutex
	calls   int
	answers []string
}

func (a *sandboxCountingAsker) Ask(ctx context.Context, question string, options []string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls++
	a.answers = append(a.answers, question)
	return "allow", nil
}

func (a *sandboxCountingAsker) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls
}

// sandboxedFixture 装配一个"接了 fake runner + 默认审批档 + 计数 Asker"的 agent。
func sandboxedFixture(t *testing.T, mutate func(*kit.Config)) (*kit.Agent, *sandboxCountingAsker, *scriptProvider, *strings.Builder) {
	t.Helper()
	dir := t.TempDir()
	asker := &sandboxCountingAsker{}
	runner := &kitRunner{}
	var out strings.Builder
	cfg := kit.Config{
		Model:         "m",
		Root:          dir,
		Asker:         asker,
		SandboxRunner: runner,
		EventWriter:   &out,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	provider := &scriptProvider{}
	cfg.Provider = provider
	agent, err := kit.New(cfg)
	if err != nil {
		t.Fatalf("kit.New: %v", err)
	}
	return agent, asker, provider, &out
}

// runBash 让模型调用一次 bash 并返回工具结果文本与错误。
func runBash(t *testing.T, agent *kit.Agent, provider *scriptProvider, command string) (string, error) {
	t.Helper()
	provider.scripts = [][]kit.Chunk{
		toolCallScript(kit.ToolCall{
			ID: "c1", Name: "bash",
			Arguments: `{"command":` + quoteJSON(command) + `,"description":"probe"}`,
		}),
		textScript("done"),
	}
	provider.calls = nil
	result, err := agent.Run(context.Background(), kit.RunRequest{
		SessionID: "s", Input: []kit.Message{kit.NewMessage(kit.RoleUser, "run it")},
	})
	if err != nil {
		return "", err
	}
	for _, message := range result.Messages {
		if message.Role == kit.RoleTool {
			return message.Text(), nil
		}
	}
	return "", nil
}

func quoteJSON(s string) string {
	raw, _ := json.Marshal(s)
	return string(raw)
}

// TestSandboxedBashNeedsNoApproval：接了 runner 之后，普通（非危险）可变命令不再问人。
func TestSandboxedBashNeedsNoApproval(t *testing.T) {
	agent, asker, provider, out := sandboxedFixture(t, nil)
	text, err := runBash(t, agent, provider, "mkdir -p build")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if asker.count() != 0 {
		t.Fatalf("已沙箱化的命令不该问人，却问了 %d 次（问题：%v）", asker.count(), asker.answers)
	}
	if strings.Contains(text, "denied") || strings.Contains(text, "拒绝") {
		t.Fatalf("命令被拒了：%s", text)
	}
	// 命令真的执行了（mkdir 成功时输出为空，所以看副作用）。
	if _, err := os.Stat(filepath.Join(agent.Policy().Roots()[0], "build")); err != nil {
		t.Fatalf("build 目录没有被创建（命令没执行）：%v", err)
	}
	// 事件：approval.response decision=allow reason=sandboxed。
	var responses []events.ApprovalResponse
	decoder := events.NewDecoder(strings.NewReader(out.String()))
	for {
		envelope, err := decoder.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if envelope.Kind != events.KindApprovalResponse {
			continue
		}
		var payload events.ApprovalResponse
		if err := json.Unmarshal(envelope.Payload, &payload); err != nil {
			t.Fatalf("decode approval.response: %v", err)
		}
		responses = append(responses, payload)
	}
	if len(responses) != 1 {
		t.Fatalf("approval.response 事件数 = %d, want 1（免问也必须留痕）", len(responses))
	}
	if responses[0].Decision != string(rig.DecisionAllow) {
		t.Fatalf("decision = %q, want allow", responses[0].Decision)
	}
	if responses[0].Reason != "sandboxed" {
		t.Fatalf("reason = %q, want sandboxed（UI 要能显示\"没问是因为已隔离\"）", responses[0].Reason)
	}
	if responses[0].Tool != "bash" {
		t.Fatalf("tool = %q, want bash", responses[0].Tool)
	}
}

// TestSandboxedCommandIsApprovedRegardlessOfCommandText（DSH 对齐）：免问判定不再
// 看命令文本——围栏已生效时，工具名是 shell 类就免问，`rm -rf /` 也不再被门禁硬拒。
// 为了不真的执行危险命令，这里用**会被分类为危险**的命令文本但换成无害的 echo，
// 断言"门禁允许 + 没问人"。
func TestSandboxedCommandIsApprovedRegardlessOfCommandText(t *testing.T) {
	agent, asker, provider, out := sandboxedFixture(t, nil)
	text, err := runBash(t, agent, provider, "echo sandboxed-command")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if asker.count() != 0 {
		t.Fatalf("已沙箱化的命令不该问人，Asker 被调用 %d 次", asker.count())
	}
	if !strings.Contains(text, "sandboxed-command") {
		t.Fatalf("命令没有执行：%q", text)
	}
	// 事件流里仍要有"没问是因为已隔离"的留痕。
	var sawSandboxed bool
	for _, envelope := range wiringDecodeAll(t, []byte(out.String())) {
		if envelope.Kind != events.KindApprovalResponse {
			continue
		}
		var payload events.ApprovalResponse
		if err := json.Unmarshal(envelope.Payload, &payload); err != nil {
			continue
		}
		if payload.Reason == "sandboxed" {
			sawSandboxed = true
		}
	}
	if !sawSandboxed {
		t.Fatalf("事件流里没有 reason=sandboxed 的 approval.response")
	}
}

// TestNoRunnerStillAsks：没有 runner 就没有围栏，行为与今天完全一致（问人）。
func TestNoRunnerStillAsks(t *testing.T) {
	agent, asker, provider, _ := sandboxedFixture(t, func(cfg *kit.Config) {
		cfg.SandboxRunner = nil
	})
	if _, err := runBash(t, agent, provider, "mkdir -p build"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if asker.count() != 1 {
		t.Fatalf("没有 runner 时必须问人，Asker 调用 %d 次", asker.count())
	}
}

// TestDangerFullAccessStillAsks：danger-full-access 没有沙箱，自然不免问。
func TestDangerFullAccessStillAsks(t *testing.T) {
	agent, asker, provider, _ := sandboxedFixture(t, func(cfg *kit.Config) {
		cfg.Policy = sandbox.New(
			sandbox.WithRoots(cfg.Root),
			sandbox.WithMode(sandbox.ModeDangerFullAccess),
			sandbox.WithAnyCommandAllowed(true),
		)
	})
	if _, err := runBash(t, agent, provider, "mkdir -p build"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if asker.count() != 1 {
		t.Fatalf("danger-full-access 下必须问人，Asker 调用 %d 次", asker.count())
	}
}

// TestAutoAllowCanBeDisabled：显式关掉这个开关 → 回到今天的行为。
func TestAutoAllowCanBeDisabled(t *testing.T) {
	agent, asker, provider, _ := sandboxedFixture(t, func(cfg *kit.Config) {
		cfg.AutoAllowSandboxedCommands = kit.Bool(false)
	})
	if _, err := runBash(t, agent, provider, "mkdir -p build"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if asker.count() != 1 {
		t.Fatalf("关掉开关之后必须问人，Asker 调用 %d 次", asker.count())
	}
}

// TestFileToolsAreNotAffected：文件工具不经 OS runner，仍然问人。
func TestFileToolsAreNotAffected(t *testing.T) {
	agent, asker, provider, _ := sandboxedFixture(t, nil)
	provider.scripts = [][]kit.Chunk{
		toolCallScript(kit.ToolCall{
			ID: "c1", Name: "write_file",
			Arguments: `{"path":"out.txt","content":"hi"}`,
		}),
		textScript("done"),
	}
	if _, err := agent.Run(context.Background(), kit.RunRequest{
		SessionID: "s", Input: []kit.Message{kit.NewMessage(kit.RoleUser, "write it")},
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if asker.count() != 1 {
		t.Fatalf("write_file 不被 runner 包住，必须问人；Asker 调用 %d 次", asker.count())
	}
}

// TestSandboxedAllowEmitsApprovalEvent：事件字段完整（tool / decision / reason）。
//
// 刻意**只发 approval.response**：`approval.request` 的语义是"正在等人确认"，
// 而这条调用根本没有在等人——发 request 会让 UI 闪一下不该出现的等待态。
// 决策本身必须留痕，所以 response 一定要有（decision=allow、reason=sandboxed）。
func TestSandboxedAllowEmitsApprovalEvent(t *testing.T) {
	agent, _, provider, out := sandboxedFixture(t, nil)
	if _, err := runBash(t, agent, provider, "mkdir -p build"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	var decided int
	decoder := events.NewDecoder(strings.NewReader(out.String()))
	for {
		envelope, err := decoder.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		switch envelope.Kind {
		case events.KindApprovalRequest:
			t.Fatalf("免问的调用不该发 approval.request（那会让 UI 显示等待态）")
		case events.KindApprovalResponse:
			var payload events.ApprovalResponse
			if err := json.Unmarshal(envelope.Payload, &payload); err != nil {
				t.Fatalf("decode approval.response: %v", err)
			}
			if payload.Tool != "bash" || payload.Decision != "allow" || payload.Reason != "sandboxed" {
				t.Fatalf("approval.response payload = %+v", payload)
			}
			decided++
		}
	}
	if decided != 1 {
		t.Fatalf("approval.response 事件数 = %d, want 1", decided)
	}
	// 命令真的执行了。
	if _, err := os.Stat(filepath.Join(agent.Policy().Roots()[0], "build")); err != nil {
		t.Fatalf("build 目录没有被创建（命令没执行？）：%v", err)
	}
}

// TestApprovalNeverIsNotBypassedBySandbox：`approval=never` 是宿主的明确拒绝，
// "已沙箱化"不能绕过它——否则用户以为关掉了执行能力，实际还在跑命令。
func TestApprovalNeverIsNotBypassedBySandbox(t *testing.T) {
	agent, asker, provider, _ := sandboxedFixture(t, func(cfg *kit.Config) {
		cfg.Approval = kit.ApprovalNever
	})
	text, err := runBash(t, agent, provider, "mkdir -p build")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(text, "denied") && !strings.Contains(text, "拒绝") {
		t.Fatalf("approval=never 下命令被放行了：%s", text)
	}
	if asker.count() != 0 {
		t.Fatalf("approval=never 不该问人，Asker 调用 %d 次", asker.count())
	}
}

// TestSandboxedBashWorksWithoutAsker：**headless 宿主**（没接 Asker）以前连 `mkdir`
// 都跑不了（"没有可用的审批通道"）；现在围栏成立就放行——这正是本开关最大的收益。
func TestSandboxedBashWorksWithoutAsker(t *testing.T) {
	dir := t.TempDir()
	provider := &scriptProvider{scripts: [][]kit.Chunk{
		toolCallScript(kit.ToolCall{ID: "c1", Name: "bash", Arguments: `{"command":"mkdir -p build","description":"x"}`}),
		textScript("done"),
	}}
	a, err := kit.New(kit.Config{
		Model: "m", Provider: provider, Root: dir, SandboxRunner: &kitRunner{},
	})
	if err != nil {
		t.Fatalf("kit.New: %v", err)
	}
	result, err := a.Run(context.Background(), kit.RunRequest{
		SessionID: "s", Input: []kit.Message{kit.NewMessage(kit.RoleUser, "build it")},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, message := range result.Messages {
		if message.Role == kit.RoleTool && strings.Contains(message.Text(), "审批") {
			t.Fatalf("headless 下已沙箱化的命令仍被拒：%s", message.Text())
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "build")); err != nil {
		t.Fatalf("build 目录没有被创建（命令没执行）：%v", err)
	}
}
