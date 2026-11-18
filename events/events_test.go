package events_test

// 动态工具加载事件的线格式契约。

import (
	"encoding/json"
	"testing"

	"github.com/zeokeel/zeokeel-rig/events"
)

// TestToolActivatedPayloadIsStable 钉住动态工具加载事件的线格式：kind 字符串与
// payload 字段名是契约（宿主 UI 与日志解析依赖它们）。
func TestToolActivatedPayloadIsStable(t *testing.T) {
	if events.KindToolActivated != "tool.activated" {
		t.Fatalf("KindToolActivated = %q, want tool.activated", events.KindToolActivated)
	}
	encoded, err := json.Marshal(events.ToolActivated{Tools: []string{"web_fetch"}, Trigger: "model"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"tools":["web_fetch"],"trigger":"model"}`
	if string(encoded) != want {
		t.Fatalf("payload = %s, want %s", encoded, want)
	}
}
