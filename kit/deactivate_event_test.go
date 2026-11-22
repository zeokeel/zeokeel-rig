package kit_test

// 取消激活的事件面：`ActivateTools` 写 `tool.activated`，`DeactivateTools` 必须写
// 对称的 `tool.deactivated`（trigger 恒为 host——模型没有取消激活的入口）。
//
// 为什么必须补：宿主按事件流镜像 active 集合（docs/AI-SDK.md 把事件当激活面）时，
// 只写激活不写取消会让 UI/回放一直显示"这个工具还开着"。

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"testing"

	"github.com/zeokeel/zeokeel-rig/events"
	"github.com/zeokeel/zeokeel-rig/kit"
)

// TestDeactivateEmitsToolDeactivatedEvent：宿主取消激活后事件流里恰好有一条
// `tool.deactivated`，payload 为 ToolDeactivated{Tools, Trigger=host}。
func TestDeactivateEmitsToolDeactivatedEvent(t *testing.T) {
	var buffer bytes.Buffer
	agent := mustNewKitAgent(t, kit.Config{
		Model:         "m",
		Provider:      &scriptProvider{scripts: [][]kit.Chunk{textScript("ok")}},
		Root:          t.TempDir(),
		Tools:         lazyConfiguredTools(),
		EventWriter:   &buffer,
		AllowNetwork:  kit.Bool(true),
		ToolDiscovery: kit.Bool(true),
	})
	if _, unknown := agent.ActivateTools("s", "web_fetch"); len(unknown) != 0 {
		t.Fatalf("ActivateTools 失败: %v", unknown)
	}
	removed, err := agent.DeactivateTools("s", "web_fetch")
	if err != nil || len(removed) != 1 {
		t.Fatalf("DeactivateTools = (%v, %v)", removed, err)
	}

	var deactivated []events.ToolDeactivated
	decoder := events.NewDecoder(bytes.NewReader(buffer.Bytes()))
	for {
		envelope, err := decoder.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if envelope.Kind != events.KindToolDeactivated {
			continue
		}
		var payload events.ToolDeactivated
		if err := json.Unmarshal(envelope.Payload, &payload); err != nil {
			t.Fatalf("decode tool.deactivated: %v", err)
		}
		deactivated = append(deactivated, payload)
	}
	if len(deactivated) != 1 {
		t.Fatalf("tool.deactivated 事件数 = %d，want 1:\n%s", len(deactivated), buffer.String())
	}
	if deactivated[0].Trigger != "host" {
		t.Fatalf("trigger = %q, want host", deactivated[0].Trigger)
	}
	if len(deactivated[0].Tools) != 1 || deactivated[0].Tools[0] != "web_fetch" {
		t.Fatalf("payload.Tools = %v, want [web_fetch]", deactivated[0].Tools)
	}
}
