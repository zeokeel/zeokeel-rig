package kit_test

// 事件桥的导出形态：不经过 kit 装配（直接 agent.New）的宿主也要能把 rig.Event
// 写成 NDJSON 事件流，而不是自己抄一份映射表——抄出来的东西一升级就跟不上。

import (
	"bytes"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/events"
	"github.com/zeokeel/zeokeel-rig/kit"
)

func TestEventSinkEmitsDecodableEnvelopes(t *testing.T) {
	var buffer bytes.Buffer
	sink := kit.NewEventSink(&buffer)
	if sink == nil {
		t.Fatal("NewEventSink 返回 nil")
	}

	sink.Emit(rig.Event{Type: rig.EventTextDelta, Text: "hello"})
	sink.Emit(rig.Event{
		Type: rig.EventToolCall,
		Call: &rig.ToolCall{ID: "c1", Name: "bash", Arguments: `{"command":"ls"}`},
	})
	sink.Emit(rig.Event{
		Type:  rig.EventUsage,
		Usage: &rig.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
	})
	if err := sink.Err(); err != nil {
		t.Fatalf("Err: %v", err)
	}

	decoder := events.NewDecoder(strings.NewReader(buffer.String()))
	var kinds []string
	for {
		envelope, err := decoder.Next()
		if err != nil {
			break
		}
		kinds = append(kinds, envelope.Kind)
	}
	// usage 走 notice（事件协议里没有独立的 usage kind）。
	want := []string{events.KindMessageDelta, events.KindToolCall, events.KindNotice}
	if len(kinds) != len(want) {
		t.Fatalf("事件数 %d, want %d（%v）", len(kinds), len(want), kinds)
	}
	for index, kind := range want {
		if kinds[index] != kind {
			t.Fatalf("第 %d 个事件 kind = %q, want %q（全部：%v）", index, kinds[index], kind, kinds)
		}
	}
}

func TestEventSinkNilWriterIsInert(t *testing.T) {
	sink := kit.NewEventSink(nil)
	sink.Emit(rig.Event{Type: rig.EventTextDelta, Text: "x"})
	if err := sink.Err(); err != nil {
		t.Fatalf("没有写入器时不该报错：%v", err)
	}
}
