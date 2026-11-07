package kit_test

import (
	"context"
	"errors"
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

func TestKitCompactNowRequiresConfig(t *testing.T) {
	a, err := kit.New(kit.Config{Model: "m", Provider: compactProvider{name: "fake"}, Tools: []kit.Tool{}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.CompactNow(context.Background(), "s"); err == nil || !errors.Is(err, err) {
		t.Fatalf("CompactNow without compactor = %v", err)
	}
}
