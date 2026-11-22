package kit_test

// `tool_search` 的覆盖面与可发现性在 kit 层的契约：
//
//  1. 清单覆盖**全部注册工具**（包括只在 kit.Config.Tools 里出现的自建工具，它们
//     不在 tools/catalog 里）；
//  2. 声明 Discoverable() == false 的工具不出现在清单里，但宿主仍能用
//     ActivateTools 直接激活、模型也仍能直接调用它；
//  3. 已激活的工具不出现在清单里（清单只回答"还能加载什么"），取消激活后回到清单。

import (
	"context"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig/kit"
)

// hiddenEchoTool 是声明了不可发现的工具：不进 tool_search 清单，但仍可激活/调用。
type hiddenEchoTool struct {
	wiringEchoTool
}

func (t *hiddenEchoTool) Discoverable() bool { return false }

// hiddenHostTools 是一组宿主自建工具：一个正常、一个不可发现，两个都**不在**
// tools/catalog 里。
func hiddenHostTools() []kit.Tool {
	return []kit.Tool{
		&wiringEchoTool{name: "read_file"},
		&wiringEchoTool{name: "pdf_extract"},
		&hiddenEchoTool{wiringEchoTool{name: "secret_tool"}},
	}
}

// toolSearchConfig 是这组用例共用的配置：宿主自建工具 + 显式打开 discovery。
func toolSearchConfig(t *testing.T, provider *scriptProvider) kit.Config {
	t.Helper()
	return kit.Config{
		Model:         "m",
		Provider:      provider,
		Root:          t.TempDir(),
		Tools:         hiddenHostTools(),
		Approval:      kit.ApprovalAuto,
		ToolDiscovery: kit.Bool(true),
	}
}

// lastToolMessage 返回结果里最后一条 tool 消息（tool_search 的清单/回执）。
func lastToolMessage(t *testing.T, result kit.RunResult) string {
	t.Helper()
	content := ""
	for _, message := range result.Messages {
		if message.Role == kit.RoleTool {
			content = message.Text()
		}
	}
	if content == "" {
		t.Fatal("运行结果里没有工具结果消息")
	}
	return content
}

// TestToolSearchCoversHostRegisteredTools：只在 Config.Tools 里的自建工具也能被
// tool_search 搜到并激活（清单不是 catalog.Static 的副本）。
func TestToolSearchCoversHostRegisteredTools(t *testing.T) {
	provider := &scriptProvider{scripts: [][]kit.Chunk{
		toolCallScript(kit.ToolCall{ID: "c1", Name: "tool_search", Arguments: `{"query":"pdf"}`}),
		textScript("done"),
		toolCallScript(kit.ToolCall{ID: "c2", Name: "tool_search", Arguments: `{"activate":["pdf_extract"]}`}),
		textScript("done"),
	}}
	agent := mustNewKitAgent(t, toolSearchConfig(t, provider))

	listing, err := agent.Run(context.Background(), kit.RunRequest{
		SessionID: "s", Input: []kit.Message{kit.NewMessage(kit.RoleUser, "pdf?")},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if content := lastToolMessage(t, listing); !strings.Contains(content, "pdf_extract") {
		t.Fatalf("清单缺少宿主自建工具 pdf_extract:\n%s", content)
	}

	activated, err := agent.Run(context.Background(), kit.RunRequest{
		SessionID: "s", Input: []kit.Message{kit.NewMessage(kit.RoleUser, "load it")},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := requestToolNames(provider.request(3)); !contains(got, "pdf_extract") {
		t.Fatalf("激活后请求缺少 pdf_extract: %v", got)
	}
	if content := lastToolMessage(t, activated); !strings.Contains(content, "activated: pdf_extract") {
		t.Fatalf("tool_search 的激活回执不对:\n%s", content)
	}
}

// TestToolSearchSkipsNonDiscoverableTools：不可发现的工具不进清单，但宿主仍可
// 激活它、模型也仍能直接调用它。
func TestToolSearchSkipsNonDiscoverableTools(t *testing.T) {
	provider := &scriptProvider{scripts: [][]kit.Chunk{
		toolCallScript(kit.ToolCall{ID: "c1", Name: "tool_search", Arguments: `{}`}),
		textScript("done"),
		toolCallScript(kit.ToolCall{ID: "c2", Name: "secret_tool", Arguments: `{"x":1}`}),
		textScript("done"),
	}}
	agent := mustNewKitAgent(t, toolSearchConfig(t, provider))

	result, err := agent.Run(context.Background(), kit.RunRequest{
		SessionID: "s", Input: []kit.Message{kit.NewMessage(kit.RoleUser, "what can I use?")},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	listing := lastToolMessage(t, result)
	if strings.Contains(listing, "secret_tool") {
		t.Fatalf("不可发现的工具出现在清单里:\n%s", listing)
	}
	if !strings.Contains(listing, "pdf_extract") {
		t.Fatalf("可发现的宿主工具反而没出现:\n%s", listing)
	}

	// 宿主显式激活后，模型可以直接调用它（不可发现只是"搜不到"）。
	if activated, unknown := agent.ActivateTools("s", "secret_tool"); len(activated) != 1 || len(unknown) != 0 {
		t.Fatalf("ActivateTools(secret_tool) = (%v, %v)", activated, unknown)
	}
	callResult, err := agent.Run(context.Background(), kit.RunRequest{
		SessionID: "s", Input: []kit.Message{kit.NewMessage(kit.RoleUser, "use it")},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if content := lastToolMessage(t, callResult); !strings.Contains(content, "echo:") {
		t.Fatalf("不可发现的工具没能被直接调用:\n%s", content)
	}
}

// TestToolSearchOmitsAlreadyActiveTools：清单只列尚未激活的工具；取消激活后它
// 重新出现在清单里。
func TestToolSearchOmitsAlreadyActiveTools(t *testing.T) {
	provider := &scriptProvider{scripts: [][]kit.Chunk{
		toolCallScript(kit.ToolCall{ID: "c1", Name: "tool_search", Arguments: `{}`}),
		textScript("done"),
		toolCallScript(kit.ToolCall{ID: "c2", Name: "tool_search", Arguments: `{}`}),
		textScript("done"),
	}}
	agent := mustNewKitAgent(t, toolSearchConfig(t, provider))
	if _, unknown := agent.ActivateTools("s", "pdf_extract"); len(unknown) != 0 {
		t.Fatalf("ActivateTools 失败: %v", unknown)
	}

	result, err := agent.Run(context.Background(), kit.RunRequest{
		SessionID: "s", Input: []kit.Message{kit.NewMessage(kit.RoleUser, "what else?")},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if listing := lastToolMessage(t, result); strings.Contains(listing, "pdf_extract") {
		t.Fatalf("已激活的工具仍出现在清单里:\n%s", listing)
	}

	if _, err := agent.DeactivateTools("s", "pdf_extract"); err != nil {
		t.Fatalf("DeactivateTools: %v", err)
	}
	if got := agent.ToolsFor("s"); contains(got, "pdf_extract") {
		t.Fatalf("取消激活后仍 active: %v", got)
	}
	result, err = agent.Run(context.Background(), kit.RunRequest{
		SessionID: "s", Input: []kit.Message{kit.NewMessage(kit.RoleUser, "and now?")},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if listing := lastToolMessage(t, result); !strings.Contains(listing, "pdf_extract") {
		t.Fatalf("取消激活后工具没有回到清单:\n%s", listing)
	}
}

// TestKitDeactivateToolsWiring：kit.Agent 转发取消激活，且拒绝强制常驻工具。
func TestKitDeactivateToolsWiring(t *testing.T) {
	agent := mustNewKitAgent(t, kit.Config{
		Model: "m", Provider: &scriptProvider{scripts: [][]kit.Chunk{textScript("ok")}},
		Root: t.TempDir(), Tools: lazyConfiguredTools(), AllowNetwork: kit.Bool(true),
		ToolDiscovery: kit.Bool(true),
	})
	if _, unknown := agent.ActivateTools("s", "web_fetch"); len(unknown) != 0 {
		t.Fatalf("ActivateTools 失败: %v", unknown)
	}
	removed, err := agent.DeactivateTools("s", "web_fetch")
	if err != nil {
		t.Fatalf("DeactivateTools: %v", err)
	}
	if strings.Join(removed, ",") != "web_fetch" {
		t.Fatalf("removed = %v", removed)
	}
	if got := agent.ActiveTools("s"); contains(got, "web_fetch") {
		t.Fatalf("取消激活后仍 active: %v", got)
	}
	for _, mandatory := range []string{"tool_search", "memory", "skill"} {
		if _, err := agent.DeactivateTools("s", mandatory); err == nil {
			t.Fatalf("DeactivateTools(%q) 应当报错", mandatory)
		}
	}
}
