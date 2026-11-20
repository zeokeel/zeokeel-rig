package kit_test

// 联网工具的默认值（用户要求：放开防护、让模型充分发挥能力；沙箱是唯一防护层）。
//
// DSH 对齐后 kit 曾把 `AllowNetwork=false` 当默认值，于是零配置下 `web_fetch`
// 被零配置体检摘掉——这是 DSH 没有的限制：网络本身不在沙箱承诺内（见 AGENTS §5.3），
// 能不能联网是"宿主给了什么工具"的问题，不该由 kit 悄悄替用户关掉。
//
// 现在 `AllowNetwork` 是三态：nil = 开（默认），显式 `kit.Bool(false)` 才关闭。

import (
	"testing"

	"github.com/zeokeel/zeokeel-rig/kit"
)

// TestWebFetchRegisteredByDefault：零配置（不设置 AllowNetwork）必须注册 web_fetch，
// 且沙箱策略本身也允许联网。
func TestWebFetchRegisteredByDefault(t *testing.T) {
	agent := mustNew(t, kit.Config{Model: "m", Root: t.TempDir(), Provider: &scriptProvider{}})
	if !contains(agent.RegisteredTools(), "web_fetch") {
		t.Fatalf("零配置没有注册 web_fetch（AllowNetwork 默认应为开）: %v", agent.RegisteredTools())
	}
	if !agent.Policy().NetworkAllowed() {
		t.Fatal("零配置的沙箱策略应当允许联网（AllowNetwork 默认开）")
	}
}

// TestWebFetchCanStillBeDisabled：显式 kit.Bool(false) 仍然能关掉网络与 web_fetch。
func TestWebFetchCanStillBeDisabled(t *testing.T) {
	agent := mustNew(t, kit.Config{
		Model: "m", Root: t.TempDir(), Provider: &scriptProvider{},
		AllowNetwork: kit.Bool(false),
	})
	if contains(agent.RegisteredTools(), "web_fetch") {
		t.Fatalf("AllowNetwork=false 时 web_fetch 应当被摘掉: %v", agent.RegisteredTools())
	}
	if agent.Policy().NetworkAllowed() {
		t.Fatal("AllowNetwork=false 时沙箱策略不应允许联网")
	}
}
