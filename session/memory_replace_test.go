package session_test

import (
	"context"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/session"
)

func TestMemoryReplaceImplementsHistoryReplacer(t *testing.T) {
	var _ rig.HistoryReplacer = session.NewMemory()
	ctx := context.Background()
	memory := session.NewMemory()
	if err := memory.Append(ctx, "s", rig.NewMessage(rig.RoleUser, "old")); err != nil {
		t.Fatal(err)
	}
	if err := memory.Replace(ctx, "s", []rig.Message{rig.NewMessage(rig.RoleUser, "new")}); err != nil {
		t.Fatal(err)
	}
	messages, err := memory.Load(ctx, "s")
	if err != nil || len(messages) != 1 || messages[0].Text() != "new" {
		t.Fatalf("load = %+v, %v", messages, err)
	}
	if err := memory.Replace(ctx, "s", nil); err != nil {
		t.Fatal(err)
	}
	if memory.Len("s") != 0 {
		t.Fatal("replace nil did not clear history")
	}
}
