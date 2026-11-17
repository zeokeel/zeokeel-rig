package kit_test

import (
	"context"
	"io"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/kit"
	"github.com/zeokeel/zeokeel-rig/session"
)

type compactProvider struct{ name string }

func (p compactProvider) Name() string { return p.name }
func (p compactProvider) Stream(context.Context, rig.Request) (rig.Stream, error) {
	return &emptyStream{}, nil
}

type emptyStream struct{}

func (s *emptyStream) Recv() (rig.Chunk, error) { return rig.Chunk{}, io.EOF }

type compactCompactor struct {
	out    []rig.Message
	budget int
}

func (c *compactCompactor) Compact(_ context.Context, _ string, _ []rig.Message, budget int) ([]rig.Message, error) {
	c.budget = budget
	return c.out, nil
}

func TestKitCompactNow(t *testing.T) {
	history := session.NewMemory()
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if err := history.Append(ctx, "s", rig.NewMessage(rig.RoleUser, "message")); err != nil {
			t.Fatal(err)
		}
	}
	compactor := &compactCompactor{out: []rig.Message{rig.NewMessage(rig.RoleUser, "compacted")}}
	a, err := kit.New(kit.Config{
		Model:         "m",
		Provider:      compactProvider{name: "fake"},
		Tools:         []kit.Tool{},
		History:       history,
		ContextBudget: 10,
		Compactor:     compactor,
	})
	if err != nil {
		t.Fatal(err)
	}
	ok, err := a.CompactNow(ctx, "s")
	if err != nil || !ok {
		t.Fatalf("CompactNow = %v, %v", ok, err)
	}
	if compactor.budget != 10 {
		t.Fatalf("budget = %d", compactor.budget)
	}
	messages, _ := history.Load(ctx, "s")
	if len(messages) != 1 || messages[0].Text() != "compacted" {
		t.Fatalf("history = %+v", messages)
	}
}

// TestKitCompactNowUsesDefaults：零配置下 kit 现在自带默认压缩器与预算
// （L1/L2 默认打开），因此手动 /compact 也开箱可用。
func TestKitCompactNowUsesDefaults(t *testing.T) {
	history := session.NewMemory()
	a, err := kit.New(kit.Config{
		Model: "m", Provider: compactProvider{name: "fake"}, Tools: []kit.Tool{},
		History: history,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := history.Append(ctx, "s", kit.NewMessage(kit.RoleUser, "hello")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	ok, err := a.CompactNow(ctx, "s")
	if err != nil || !ok {
		t.Fatalf("CompactNow with defaults = (%v, %v)", ok, err)
	}
}

// TestKitCompactNowDisabledByNegativeBudget：显式负预算表示关闭自动压缩，
// 此时手动压缩应返回可读错误。
func TestKitCompactNowDisabledByNegativeBudget(t *testing.T) {
	a, err := kit.New(kit.Config{
		Model: "m", Provider: compactProvider{name: "fake"}, Tools: []kit.Tool{},
		ContextBudget: -1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.CompactNow(context.Background(), "s"); err == nil {
		t.Fatal("ContextBudget < 0 时 CompactNow 必须报错")
	}
}
