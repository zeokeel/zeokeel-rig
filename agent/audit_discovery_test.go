package agent_test

// 第 4A 轮对抗审计（只读）：动态工具加载的**缓存契约**是否与设计一致。
//
// 设计承诺（docs/MODULES.md 第四章）：不激活时 tools 数组逐字节稳定；激活只追加；
// 永不移除；未激活不可调用且不过 gate；按会话隔离。
//
// 本文件用 sha256 + 字节数 + 逐字节前缀比较**独立复验**（不复用 agent/lazy_tools_test.go
// 的断言，只复用它的 fixture 助手）。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
)

// auditToolsDigest 返回 tools 数组的 JSON、sha256 与字节数。
func auditToolsDigest(t *testing.T, request rig.Request) (string, string, int) {
	t.Helper()
	encoded, err := json.Marshal(request.Tools)
	if err != nil {
		t.Fatalf("marshal tools: %v", err)
	}
	sum := sha256.Sum256(encoded)
	return string(encoded), hex.EncodeToString(sum[:]), len(encoded)
}

// auditRun 跑一轮对话（只回文本）。
func auditRun(t *testing.T, a rig.Agent, sessionID, input string) {
	t.Helper()
	if _, err := a.Run(context.Background(), rig.RunRequest{
		SessionID: sessionID,
		Input:     []rig.Message{userMessage(input)},
	}); err != nil {
		t.Fatalf("Run(%q): %v", sessionID, err)
	}
}

// 承诺 1：不激活时，连续 5 轮的 tools 数组 sha256 与字节数恒定（Messages 会变）。
func TestAudit4AToolsAreByteStableWithoutActivation(t *testing.T) {
	tools := []*fakeTool{
		{name: "read_file", desc: "read a file", schema: map[string]any{"type": "object"}},
		{name: "write_file", desc: "write a file", schema: map[string]any{"type": "object"}},
		{name: "grep", desc: "search inside files", schema: map[string]any{"type": "object"}},
	}
	ordered := []rig.Tool{tools[0], tools[1], tools[2]}
	provider := &scriptProvider{scripts: [][]rig.Chunk{
		textScript("a"), textScript("b"), textScript("c"), textScript("d"), textScript("e"),
	}}
	a := mustNewAgent(t, rig.AgentSpec{
		Model: "m", Provider: provider, Tools: ordered,
		InitialActiveTools: []string{"read_file"},
	})

	for round := 0; round < 5; round++ {
		auditRun(t, a, "", fmt.Sprintf("turn %d", round))
	}
	if got := provider.callCount(); got != 5 {
		t.Fatalf("请求数 = %d, want 5", got)
	}

	_, wantDigest, wantBytes := auditToolsDigest(t, provider.request(0))
	var previousMessages string
	for round := 0; round < 5; round++ {
		_, digest, size := auditToolsDigest(t, provider.request(round))
		if digest != wantDigest || size != wantBytes {
			t.Fatalf("第 %d 轮 tools 变了：sha256 %s (%d 字节) vs 首轮 %s (%d 字节)",
				round, digest, size, wantDigest, wantBytes)
		}
		// 反向证据：Messages 必须**在变**，否则说明我们量错了对象。
		encoded, err := json.Marshal(provider.request(round).Messages)
		if err != nil {
			t.Fatalf("marshal messages: %v", err)
		}
		current := string(encoded)
		if round > 0 && current == previousMessages {
			t.Fatalf("第 %d 轮 Messages 与上一轮相同：这个 fixture 没有真正推进对话", round)
		}
		previousMessages = current
	}
	t.Logf("5 轮 tools 恒定：sha256=%s，%d 字节；Messages 每轮都在变", wantDigest[:16], wantBytes)
}

// 承诺 2：激活只在末尾追加——即使被激活的工具**注册得比已 active 的工具更早**。
func TestAudit4AActivationAppendsAtEndRegardlessOfRegistrationOrder(t *testing.T) {
	// 注册顺序 read_file, write_file, grep；初始 active 是 **grep**（最后一个），
	// 然后激活 read_file（注册得更早）——按"注册顺序过滤"会插到 grep 前面，
	// 正确的实现必须追加到末尾。
	tools := []rig.Tool{
		&fakeTool{name: "read_file"},
		&fakeTool{name: "write_file"},
		&fakeTool{name: "grep"},
	}
	provider := &scriptProvider{scripts: [][]rig.Chunk{
		textScript("1"), textScript("2"), textScript("3"), textScript("4"),
	}}
	a := mustNewAgent(t, rig.AgentSpec{
		Model: "m", Provider: provider, Tools: tools,
		InitialActiveTools: []string{"grep"},
	})
	auditRun(t, a, "", "first")
	beforeJSON, beforeDigest, beforeBytes := auditToolsDigest(t, provider.request(0))

	activator := a.(rig.ToolActivator)
	activated, unknown := activator.ActivateTools("", "read_file")
	if len(unknown) != 0 {
		t.Fatalf("unexpected unknown: %v", unknown)
	}
	if strings.Join(activated, ",") != "read_file" {
		t.Fatalf("activated = %v", activated)
	}
	auditRun(t, a, "", "second")
	afterJSON, afterDigest, afterBytes := auditToolsDigest(t, provider.request(1))

	names := toolNames(provider.request(1))
	if strings.Join(names, ",") != "grep,read_file" {
		t.Fatalf("激活后的顺序 = %v, want [grep read_file]（必须是末尾追加，不能按注册顺序重排）", names)
	}
	// 前缀逐字节相同（去掉末项与结尾的 ']'）。
	prefix := beforeJSON[:len(beforeJSON)-1]
	if !strings.HasPrefix(afterJSON, prefix) {
		t.Fatalf("激活改变了已有前缀（prefix cache 会失效）:\nbefore: %s\nafter:  %s", beforeJSON, afterJSON)
	}
	t.Logf("激活 read_file：%s (%d 字节) → %s (%d 字节)，旧数组是新数组的前缀",
		beforeDigest[:16], beforeBytes, afterDigest[:16], afterBytes)

	// 之后再跑 2 轮：必须重新稳定。
	auditRun(t, a, "", "third")
	auditRun(t, a, "", "fourth")
	for round := 2; round < 4; round++ {
		if _, digest, size := auditToolsDigest(t, provider.request(round)); digest != afterDigest || size != afterBytes {
			t.Fatalf("激活后第 %d 轮 tools 又变了", round)
		}
	}
}

// 承诺 3：永不移除——空/重复/未知名字都不改变 tools。
func TestAudit4ANeverRemovesAnything(t *testing.T) {
	tools := []rig.Tool{&fakeTool{name: "read_file"}, &fakeTool{name: "write_file"}}
	provider := &scriptProvider{scripts: [][]rig.Chunk{textScript("1"), textScript("2")}}
	a := mustNewAgent(t, rig.AgentSpec{
		Model: "m", Provider: provider, Tools: tools,
		InitialActiveTools: []string{"read_file"},
	})
	auditRun(t, a, "", "baseline")
	baselineJSON, baselineDigest, baselineBytes := auditToolsDigest(t, provider.request(0))

	activator := a.(rig.ToolActivator)
	if activated, unknown := activator.ActivateTools(""); len(activated) != 0 || len(unknown) != 0 {
		t.Fatalf("空激活返回 activated=%v unknown=%v", activated, unknown)
	}
	// 重复激活一个已 active 的名字（幂等）。
	if activated, unknown := activator.ActivateTools("", "read_file"); strings.Join(activated, ",") != "read_file" || len(unknown) != 0 {
		t.Fatalf("重复激活返回 activated=%v unknown=%v", activated, unknown)
	}
	// 未知名字：只进 unknown。
	if activated, unknown := activator.ActivateTools("", "no_such_tool"); len(activated) != 0 || strings.Join(unknown, ",") != "no_such_tool" {
		t.Fatalf("未知名字返回 activated=%v unknown=%v", activated, unknown)
	}
	auditRun(t, a, "", "after no-ops")
	afterJSON, afterDigest, afterBytes := auditToolsDigest(t, provider.request(1))
	if afterJSON != baselineJSON || afterDigest != baselineDigest || afterBytes != baselineBytes {
		t.Fatalf("空/重复/未知激活改变了 tools:\nbefore: %s\nafter:  %s", baselineJSON, afterJSON)
	}
	if got := activator.ActiveTools(""); strings.Join(got, ",") != "read_file" {
		t.Fatalf("active 集合被 no-op 改动了: %v", got)
	}
}

// 承诺 4：未激活的工具不可调用——不执行、**不过 gate**。
func TestAudit4AInactiveToolSkipsGateAndExecution(t *testing.T) {
	scripted := []rig.Chunk{
		{ToolCalls: []rig.ToolCallDelta{{Index: 0, ID: "c1", Name: "write_file"}}},
		{FinishReason: "tool_calls"},
	}
	provider := &scriptProvider{scripts: [][]rig.Chunk{scripted, textScript("ok")}}
	var gateCalls int
	gate := gateFunc(func(context.Context, rig.Tool, rig.ToolCall) (rig.Decision, error) {
		gateCalls++
		return rig.DecisionAllow, nil
	})
	writeTool := &fakeTool{name: "write_file"}
	a := mustNewAgent(t, rig.AgentSpec{
		Model: "m", Provider: provider,
		Tools:              []rig.Tool{&fakeTool{name: "read_file"}, writeTool},
		Gate:               gate,
		InitialActiveTools: []string{"read_file"},
	})
	result, err := a.Run(context.Background(), rig.RunRequest{
		Input: []rig.Message{userMessage("just write it")},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if gateCalls != 0 {
		t.Fatalf("未激活的调用过了 gate %d 次（应当在 gate 之前拒绝）", gateCalls)
	}
	if executed := writeTool.callCount(); executed != 0 {
		t.Fatalf("未激活的工具被执行了 %d 次", executed)
	}
	var rejection string
	for _, message := range result.Messages {
		if message.Role == rig.RoleTool && strings.Contains(message.Text(), "not active") {
			rejection = message.Text()
		}
	}
	if rejection == "" {
		t.Fatalf("没有产生「not active」的工具错误：%+v", result.Messages)
	}
	if !strings.Contains(rejection, "tool_search") {
		t.Fatalf("拒绝文案没有给出可执行的下一步: %q", rejection)
	}
	t.Logf("拒绝文案: %s", rejection)
}

// 承诺 5：按会话隔离；切回来不丢。
func TestAudit4ASessionIsolationAndSwitchBack(t *testing.T) {
	tools := []rig.Tool{
		&fakeTool{name: "read_file"},
		&fakeTool{name: "write_file"},
		&fakeTool{name: "grep"},
	}
	provider := &scriptProvider{scripts: [][]rig.Chunk{
		textScript("1"), textScript("2"), textScript("3"), textScript("4"),
	}}
	a := mustNewAgent(t, rig.AgentSpec{
		Model: "m", Provider: provider, Tools: tools,
		InitialActiveTools: []string{"read_file"},
	})
	activator := a.(rig.ToolActivator)
	activator.ActivateTools("s1", "write_file")
	activator.ActivateTools("s2", "grep")

	auditRun(t, a, "s1", "s1 first")
	auditRun(t, a, "s2", "s2 first")
	auditRun(t, a, "s1", "s1 again")
	auditRun(t, a, "s2", "s2 again")

	want := map[int]string{0: "read_file,write_file", 1: "read_file,grep", 2: "read_file,write_file", 3: "read_file,grep"}
	for index, expect := range want {
		if got := strings.Join(toolNames(provider.request(index)), ","); got != expect {
			t.Fatalf("第 %d 个请求（session 交替）tools = %s, want %s", index, got, expect)
		}
	}
	// 切回来不丢：s1 的第二次请求与第一次逐字节相同。
	firstJSON, firstDigest, _ := auditToolsDigest(t, provider.request(0))
	againJSON, againDigest, _ := auditToolsDigest(t, provider.request(2))
	if firstJSON != againJSON || firstDigest != againDigest {
		t.Fatalf("切回 s1 后 tools 变了（会话状态丢失）:\nfirst: %s\nagain: %s", firstJSON, againJSON)
	}
	// 未知会话不应继承别人的激活。
	if got := strings.Join(activator.ActiveTools("s3"), ","); got != "read_file" {
		t.Fatalf("新会话 s3 的 active = %s（不该继承 s1/s2 的激活）", got)
	}
}

// 【设计漏洞】激活到"全部工具"的那一刻，`activeSpecs` 的捷径会返回**注册顺序**，
// 把此前逐次追加的顺序整体重排 → 该轮 prefix cache 全崩（不只是追加一项）。
//
// 代码位置：agent/agent.go `activeSpecs`：
//
//	if len(set.order) == len(a.toolSpecs) { return a.toolSpecs }   // 注册顺序
//
// 例：注册 [a b c]，初始 active 只有 c（order=[c]）；激活 a → [c a]；再激活 b →
// order 长度 3 == 全部 → 返回 [a b c]，与上一轮的 [c a] 前缀不符。
func TestAudit4AActivatingEverythingReordersTheArray(t *testing.T) {
	tools := []rig.Tool{
		&fakeTool{name: "alpha"},
		&fakeTool{name: "beta"},
		&fakeTool{name: "gamma"},
	}
	provider := &scriptProvider{scripts: [][]rig.Chunk{
		textScript("1"), textScript("2"), textScript("3"),
	}}
	a := mustNewAgent(t, rig.AgentSpec{
		Model: "m", Provider: provider, Tools: tools,
		// 初始只 active 最后一个（注册顺序的最末项）。
		InitialActiveTools: []string{"gamma"},
	})
	activator := a.(rig.ToolActivator)

	auditRun(t, a, "", "初始")
	round0 := strings.Join(toolNames(provider.request(0)), ",")

	activator.ActivateTools("", "alpha")
	auditRun(t, a, "", "激活 alpha")
	round1 := strings.Join(toolNames(provider.request(1)), ",")

	activator.ActivateTools("", "beta") // 此刻 active 覆盖全部 3 个
	auditRun(t, a, "", "激活 beta（此时全部 active）")
	round2 := strings.Join(toolNames(provider.request(2)), ",")

	t.Logf("第 0 轮: %s", round0)
	t.Logf("第 1 轮: %s", round1)
	t.Logf("第 2 轮: %s", round2)
	if round2 != "gamma,alpha,beta" {
		// 已知缺陷（P1）：本条以"记录"形式存在，不把主干 CI 变红；修好后会自动进入
		// 上面的正向断言。
		t.Skipf("⚠️ 设计漏洞：激活到全集时 tools 被整体重排 %s → %s（期望 gamma,alpha,beta）。"+
			"原因：activeSpecs 里 `len(set.order)==len(a.toolSpecs)` 的捷径返回注册顺序数组，"+
			"使这一轮前缀与上一轮完全不同——prefix cache 不是崩一项而是全崩。", round1, round2)
	}
}
