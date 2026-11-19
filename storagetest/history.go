package storagetest

import (
	"context"
	"reflect"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
)

// 基线套件里的会话 ID 都写成显式值：storage.Storage.History() 绑定的是实现的
// 默认会话，而 rig.History 的每个方法都接受 sessionID 并优先使用它。
const (
	sessionA = "baseline-session-a"
	sessionB = "baseline-session-b"
	unknown  = "baseline-session-unknown"
)

// conversation 是"消息形态全集"的固定样本：文本、图片分片、工具调用、工具结果、
// 推理文本。任何一条在落盘/读回过程中被丢掉或改写，DeepEqual 都会红。
//
// 这不是过度设计：目录后端与 SQLite 用完全不同的编码（NDJSON / 列），而
// rig.Message 的字段恰恰是"模型下一回合还能不能正确继续"的全部依据。
func conversation() []rig.Message {
	return []rig.Message{
		rig.NewMessage(rig.RoleSystem, "你是 rig 的基线测试"),
		rig.NewMessage(rig.RoleUser, "帮我看看 a.go"),
		{
			Role: rig.RoleAssistant,
			Content: []rig.ContentPart{
				rig.TextPart("先读文件"),
				rig.ImagePart("https://example.com/shot.png", "high"),
			},
			ToolCalls: []rig.ToolCall{{
				ID:        "call-1",
				Name:      "read_file",
				Arguments: `{"path":"a.go"}`,
			}},
			Reasoning: "模型内部推理，不能丢",
		},
		{
			Role:       rig.RoleTool,
			ToolCallID: "call-1",
			Name:       "read_file",
			Content:    []rig.ContentPart{rig.TextPart("package a")},
		},
	}
}

func runHistoryCases(t *testing.T, caps Capability, newStorage Factory) {
	t.Run("HistoryRoundTrip", func(t *testing.T) {
		s := newStore(t, newStorage)
		history := requireHistory(t, s, caps)
		ctx := context.Background()
		want := conversation()

		if err := history.Append(ctx, sessionA, want...); err != nil {
			t.Fatalf("Append: %v", err)
		}
		got, err := history.Load(ctx, sessionA)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("往返后消息不一致：\n got = %+v\nwant = %+v", got, want)
		}
		// 第二次读必须一致：一读就自愈/改写的实现在续传时会漂移。
		again, err := history.Load(ctx, sessionA)
		if err != nil {
			t.Fatalf("第二次 Load: %v", err)
		}
		if !reflect.DeepEqual(again, want) {
			t.Fatalf("两次 Load 结果不同：\n1st = %+v\n2nd = %+v", got, again)
		}
	})

	t.Run("HistoryUnknownSession", func(t *testing.T) {
		s := newStore(t, newStorage)
		history := requireHistory(t, s, caps)
		// rig.History 契约：未知会话是"空历史"，不是错误。
		messages, err := history.Load(context.Background(), unknown)
		if err != nil {
			t.Fatalf("Load(未知会话) = %v，want nil error", err)
		}
		if len(messages) != 0 {
			t.Fatalf("Load(未知会话) 返回了 %d 条消息，want 0 条", len(messages))
		}
	})

	t.Run("HistoryAppendCreatesSession", func(t *testing.T) {
		s := newStore(t, newStorage)
		history := requireHistory(t, s, caps)
		ctx := context.Background()
		// "从 UI 新建对话开始一次 run" 就靠这条：不先 Create 也能写。
		if err := history.Append(ctx, sessionA, rig.NewMessage(rig.RoleUser, "第一条")); err != nil {
			t.Fatalf("Append 到未知会话: %v", err)
		}
		messages, err := history.Load(ctx, sessionA)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if len(messages) != 1 {
			t.Fatalf("Load 返回 %d 条，want 1 条", len(messages))
		}
	})

	t.Run("HistoryAppendKeepsOrder", func(t *testing.T) {
		s := newStore(t, newStorage)
		history := requireHistory(t, s, caps)
		ctx := context.Background()
		// 分批写入（一次多条的语义是"一个原子单位"），读回顺序必须与写入顺序一致。
		if err := history.Append(ctx, sessionA,
			rig.NewMessage(rig.RoleUser, "1"),
			rig.NewMessage(rig.RoleAssistant, "2"),
		); err != nil {
			t.Fatalf("Append batch1: %v", err)
		}
		if err := history.Append(ctx, sessionA, rig.NewMessage(rig.RoleUser, "3")); err != nil {
			t.Fatalf("Append batch2: %v", err)
		}
		messages, err := history.Load(ctx, sessionA)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		var got []string
		for _, message := range messages {
			got = append(got, message.Text())
		}
		if want := []string{"1", "2", "3"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("消息顺序/条数 = %v，want %v", got, want)
		}
	})

	t.Run("HistoryLoadReturnsCopy", func(t *testing.T) {
		s := newStore(t, newStorage)
		history := requireHistory(t, s, caps)
		ctx := context.Background()
		if err := history.Append(ctx, sessionA, conversation()...); err != nil {
			t.Fatalf("Append: %v", err)
		}
		first, err := history.Load(ctx, sessionA)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		// 调用方改自己的切片不得污染存储或后续读取（否则压缩/裁剪路径会悄悄改历史）。
		first[0].Content = nil
		first[0].Role = rig.RoleAssistant
		second, err := history.Load(ctx, sessionA)
		if err != nil {
			t.Fatalf("第二次 Load: %v", err)
		}
		if !reflect.DeepEqual(second, conversation()) {
			t.Fatalf("修改第一次 Load 的返回值影响了存储内容：\n got = %+v", second)
		}
	})

	t.Run("HistorySessionsAreIsolated", func(t *testing.T) {
		s := newStore(t, newStorage)
		history := requireHistory(t, s, caps)
		ctx := context.Background()
		if err := history.Append(ctx, sessionA, rig.NewMessage(rig.RoleUser, "A")); err != nil {
			t.Fatalf("Append A: %v", err)
		}
		if err := history.Append(ctx, sessionB, rig.NewMessage(rig.RoleUser, "B")); err != nil {
			t.Fatalf("Append B: %v", err)
		}
		a, err := history.Load(ctx, sessionA)
		if err != nil {
			t.Fatalf("Load A: %v", err)
		}
		b, err := history.Load(ctx, sessionB)
		if err != nil {
			t.Fatalf("Load B: %v", err)
		}
		if len(a) != 1 || a[0].Text() != "A" {
			t.Fatalf("会话 A = %+v，want 只有 1 条 \"A\"", a)
		}
		if len(b) != 1 || b[0].Text() != "B" {
			t.Fatalf("会话 B = %+v，want 只有 1 条 \"B\"", b)
		}
	})
}
