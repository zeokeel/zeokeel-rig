package kit_test

// 个人客户端默认行为的守门用例。
//
// 这三条是个人客户端默认行为的 P0 回归用例：
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
// TestProductDefaultAutoNeverAsks（DSH 对齐）：auto 档不问人、也不在门禁层硬拒，
// 危险命令与普通调用一样直接交给工具——围栏由 Mode 负责，规则表不再参与。
// 为了不真的执行 `rm -rf /`，这里用假 bash 工具，只验证门禁决策。
func TestProductDefaultAutoNeverAsks(t *testing.T) {
	tool := &fakeTool{name: "bash", fn: func(context.Context, kit.ToolCall) (kit.ToolResult, error) {
		return kit.ToolResult{Content: "executed"}, nil
	}}
	asker := &countingAsker{answer: "allow"}
	provider := &scriptProvider{scripts: [][]kit.Chunk{
		toolCallScript(kit.ToolCall{ID: "c1", Name: "bash", Arguments: `{"command":"rm -rf /"}`}),
		textScript("stopped"),
	}}
	a := mustNew(t, kit.Config{
		Model:    "m",
		Provider: provider,
		Root:     t.TempDir(),
		Approval: "auto",
		Tools:    []kit.Tool{tool},
		Asker:    asker,
	})
	if _, _, err := a.RunText(context.Background(), "s", "go", nil); err != nil {
		t.Fatalf("RunText: %v", err)
	}
	if asker.asked != 0 {
		t.Fatalf("auto 档不该问人，实际问了 %d 次", asker.asked)
	}
	if len(tool.calls) != 1 {
		t.Fatalf("auto 档不该在门禁层硬拒，executed=%d", len(tool.calls))
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
	// DSH 对齐后免问只按**工具名**：bash/pwsh 永远不在只读免问名单里，所以 `ls`
	// 这类只读命令也会问人（命令文本不再参与判定）。
	if asker2.asked != 1 {
		t.Fatalf("bash 调用应当被问一次（免问只按工具名），实际 %d 次: %v", asker2.asked, asker2.queries)
	}
}

// TestProductDefaultShellRunsInWorkspaceWrite 钉住"能跑命令"这个最基本的产品能力。
func TestProductDefaultShellRunsInWorkspaceWrite(t *testing.T) {
	root := t.TempDir()
	provider := &scriptProvider{scripts: [][]kit.Chunk{
		toolCallScript(kit.ToolCall{ID: "c1", Name: "bash", Arguments: `{"command":"echo hello-from-shell"}`}),
		textScript("done"),
	}}
	// bash 不在只读免问名单里：要跑命令就得有人批准（这里用会批准的 Asker）。
	a := mustNew(t, kit.Config{Model: "m", Provider: provider, Root: root,
		Asker: &countingAsker{answer: "allow"}})
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
	b := mustNew(t, kit.Config{Model: "m", Provider: off, Root: root, DisableShellAccess: true,
		Asker: &countingAsker{answer: "allow"}})
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
	// 有人批准也不该能跑：拒绝来自策略层（shell 被关），不是来自审批。
	if !strings.Contains(denied.Text(), "disabled") && !strings.Contains(denied.Text(), "allowlist") &&
		!strings.Contains(denied.Text(), "拒绝") {
		t.Fatalf("拒绝应来自策略层（shell 已禁用）：%q", denied.Text())
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
	// 能力断言（不是类型断言）：宿主拿回的这个句柄必须能做"最近会话侧栏"要的事
	// ——列会话、按 id 读会话。2026-11 钉子复核（名字级契约的钉子用例）：原来这里是
	// `a.SessionStore() != st`，即"必须原样返回宿主传的那个 *store.Dir 指针"。
	// 那条断言守的是**某个具体实现**：`SessionStore` 一旦接口化、或 kit 改成返回
	// 一个同底层适配器，它就会红，而用户可见行为一点没变。
	// 真正要守的是两件事，都在下面：
	//   1. 拿回来的句柄具备会话列表/读取能力（`store.Store` 的那两个方法）；
	//   2. 宿主**自己**手里的句柄能看到 agent 写进去的会话（同一个后端，不是副本）。
	// 只要这两条成立，返回指针还是适配器都是实现自由。
	// 编译期能力断言：返回值必须满足"会话列表/读取"契约。`SessionStore` 接口化
	// 之后这一行不用改，而"必须返回 *store.Dir 指针"会被它拒掉——正是要的。
	var listable store.Store = a.SessionStore()
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
	// 两个句柄看到同一份会话：这是"同一实例"语义的**可观测形式**。
	viaAgent, err := listable.List(context.Background(), store.Filter{Limit: 10})
	if err != nil {
		t.Fatalf("SessionStore().List: %v", err)
	}
	if len(viaAgent) != len(metas) || viaAgent[0].ID != metas[0].ID {
		t.Fatalf("两个句柄看到的会话不一致：agent=%+v 宿主=%+v", viaAgent, metas)
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
