package events_test

// 取消激活事件的线格式契约：kind 字符串与 payload 形状（与 tool.activated 同结构）
// 都是宿主 UI 与日志解析依赖的稳定面。

import (
	"encoding/json"
	"testing"

	"github.com/zeokeel/zeokeel-rig/events"
)

// TestToolDeactivatedPayloadIsStable 钉住 tool.deactivated 的线格式。
func TestToolDeactivatedPayloadIsStable(t *testing.T) {
	if events.KindToolDeactivated != "tool.deactivated" {
		t.Fatalf("KindToolDeactivated = %q, want tool.deactivated", events.KindToolDeactivated)
	}
	encoded, err := json.Marshal(events.ToolDeactivated{Tools: []string{"web_fetch"}, Trigger: "host"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"tools":["web_fetch"],"trigger":"host"}`
	if string(encoded) != want {
		t.Fatalf("payload = %s, want %s", encoded, want)
	}
}
