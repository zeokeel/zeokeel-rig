package kit_test

// 个人客户端默认行为的守门用例。
//
// 这三条对应第一轮全项目审计（docs/audit/PERSONAL-CLIENT-AUDIT-1.md）的 P0：
//   - ApprovalAuto 曾经是 gate.AllowAll()，`rm -rf /` 会直接执行；
//   - ApprovalAsk 曾经是 gate.AllowTools(4 个只读工具)，写/执行静默拒绝、从不询问；
//   - 花费计量包存在但没接线（模型花了钱，UI 一无所知）。

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig/costmeter"
	"github.com/zeokeel/zeokeel-rig/kit"
	"github.com/zeokeel/zeokeel-rig/session/store"
)

// countingAsker 记录被问了几次，并返回固定答复。
type countingAsker struct {
	asked   int
	answer  string
	queries []string
}

func (c *countingAsker) Ask(ctx context.Context, question string, options []string) (string, error) {
	c.asked++
	c.queries = append(c.queries, question)
	if c.answer != "" {
		return c.answer, nil
	}
	return "allow", nil
}

// TestProductDefaultAutoStillDeniesDangerousCommands 钉住 auto ≠ AllowAll。
func TestProductDefaultAutoStillDeniesDangerousCommands(t *testing.T) {
	root := t.TempDir()
	provider := &scriptProvider{scripts: [][]kit.Chunk{
		toolCallScript(kit.ToolCall{ID: "c1", Name: "bash", Arguments: `{"command":"rm -rf /"}`}),
		textScript("stopped"),
	}}
	a := mustNew(t, kit.Config{
		Model:    "m",
		Provider: provider,
		Root:     root,
		Approval: "auto",
	})
	if _, _, err := a.RunText(context.Background(), "s", "go", nil); err != nil {
		t.Fatalf("RunText: %v", err)
	}
	msg, ok := findToolMessage(provider.request(1).Messages, "bash")
	if !ok {
		t.Fatalf("no bash tool message in %+v", provider.request(1).Messages)
	}
	if !strings.Contains(msg.Text(), "denied") {
		t.Fatalf("auto 模式放行了危险命令: %q", msg.Text())
	}
	if !strings.Contains(msg.Text(), "rm.recursive-root") && !strings.Contains(msg.Text(), "危险命令") {
		t.Fatalf("拒绝理由没有指出规则: %q", msg.Text())
	}
}

// TestProductDefaultAskCallsTheHuman 钉住 ask 模式真的会问人。
func TestProductDefaultAskCallsTheHuman(t *testing.T) {
	root := t.TempDir()
	asker := &countingAsker{answer: "allow"}
	provider := &scriptProvider{scripts: [][]kit.Chunk{
		toolCallScript(kit.ToolCall{ID: "c1", Name: "write_file", Arguments: `{"path":"a.txt","content":"hi\n"}`}),
		textScript("done"),
	}}
	a := mustNew(t, kit.Config{
		Model:    "m",
		Provider: provider,
		Root:     root,
		Approval: "ask",
		Asker:    asker,
	})
	if _, _, err := a.RunText(context.Background(), "s", "write", nil); err != nil {
		t.Fatalf("RunText: %v", err)
	}
	if asker.asked != 1 {
		t.Fatalf("审批人被告知 %d 次，want 1", asker.asked)
	}
	// 只读命令不应该打扰人。
	asker2 := &countingAsker{answer: "allow"}
	provider2 := &scriptProvider{scripts: [][]kit.Chunk{
		toolCallScript(kit.ToolCall{ID: "c1", Name: "bash", Arguments: `{"command":"ls"}`}),
		textScript("done"),
	}}
	b := mustNew(t, kit.Config{
		Model: "m", Provider: provider2, Root: root, Approval: "ask", Asker: asker2,
	})
	if _, _, err := b.RunText(context.Background(), "s", "list", nil); err != nil {
		t.Fatalf("RunText: %v", err)
	}
	if asker2.asked != 0 {
		t.Fatalf("只读命令也打扰了审批人 %d 次: %v", asker2.asked, asker2.queries)
	}
}

// TestProductDefaultShellRunsInWorkspaceWrite 钉住"能跑命令"这个最基本的产品能力。
func TestProductDefaultShellRunsInWorkspaceWrite(t *testing.T) {
	root := t.TempDir()
	provider := &scriptProvider{scripts: [][]kit.Chunk{
		toolCallScript(kit.ToolCall{ID: "c1", Name: "bash", Arguments: `{"command":"echo hello-from-shell"}`}),
		textScript("done"),
	}}
	a := mustNew(t, kit.Config{Model: "m", Provider: provider, Root: root})
	if _, _, err := a.RunText(context.Background(), "s", "run", nil); err != nil {
		t.Fatalf("RunText: %v", err)
	}
	msg, ok := findToolMessage(provider.request(1).Messages, "bash")
	if !ok {
		t.Fatalf("no bash tool message")
	}
	if !strings.Contains(msg.Text(), "hello-from-shell") {
		t.Fatalf("只读命令没能执行: %q", msg.Text())
	}

	// 显式关闭 shell 后应当拒绝（而不是静默变成别的东西）。
	off := &scriptProvider{scripts: [][]kit.Chunk{
		toolCallScript(kit.ToolCall{ID: "c1", Name: "bash", Arguments: `{"command":"echo nope"}`}),
		textScript("done"),
	}}
	b := mustNew(t, kit.Config{Model: "m", Provider: off, Root: root, DisableShellAccess: true})
	if _, _, err := b.RunText(context.Background(), "s", "run", nil); err != nil {
		t.Fatalf("RunText: %v", err)
	}
	denied, ok := findToolMessage(off.request(1).Messages, "bash")
	if !ok {
		t.Fatalf("no bash tool message")
	}
	if strings.Contains(denied.Text(), "nope") {
		t.Fatalf("DisableShellAccess 没生效: %q", denied.Text())
	}
}

// TestProductDefaultMeterAndSessionStore 钉住花费计量与会话列表入口。
func TestProductDefaultMeterAndSessionStore(t *testing.T) {
	dir := t.TempDir()
	st, err := store.NewDir(dir)
	if err != nil {
		t.Fatalf("NewDir: %v", err)
	}
	meter := kitNewMeter()
	provider := &usageProvider{}

	a := mustNew(t, kit.Config{
		Model:        "gpt-4o",
		Provider:     provider,
		Root:         t.TempDir(),
		SessionStore: st,
		Meter:        meter,
	})
	if a.SessionStore() != st {
		t.Fatalf("SessionStore() 没有返回宿主传入的实例")
	}
	if _, _, err := a.RunText(context.Background(), "s1", "hi", nil); err != nil {
		t.Fatalf("RunText: %v", err)
	}
	if provider.calls == 0 {
		t.Fatalf("provider 没有被调用")
	}
	sum := meter.Session()
	if sum.Requests == 0 || sum.PromptTokens == 0 {
		t.Fatalf("Meter 没收到用量: %+v", sum)
	}
	if sum.CostUSD <= 0 {
		t.Fatalf("CostUSD = %v, want > 0（gpt-4o 是有价模型）", sum.CostUSD)
	}
	// 会话落盘且能被列表看到（UI 侧栏的最小路径）。
	metas, err := st.List(context.Background(), store.Filter{Limit: 10})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(metas) != 1 || metas[0].ID != "s1" {
		t.Fatalf("List = %+v, want one session s1", metas)
	}
	if _, err := os.Stat(filepath.Join(dir, "s1.ndjson")); err != nil {
		t.Fatalf("会话文件不存在: %v", err)
	}
}

func kitNewMeter() *costmeter.Meter { return costmeter.New() }

// usageProvider 每轮都报一次用量（模拟真实 provider 的 Usage），然后收尾。
type usageProvider struct{ calls int }

func (u *usageProvider) Name() string { return "usage" }

func (u *usageProvider) Stream(ctx context.Context, req kit.Request) (kit.Stream, error) {
	u.calls++
	return &chunkStream{chunks: []kit.Chunk{
		{Usage: &kit.Usage{PromptTokens: 1000, CompletionTokens: 500, TotalTokens: 1500, CachedTokens: 200}},
		{FinishReason: "stop"},
	}}, nil
}

var _ = fmt.Sprintf
