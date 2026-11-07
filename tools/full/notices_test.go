package full

import (
	"context"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/tools/jobs"
)

type noticeFakeTool struct{}

func (noticeFakeTool) Name() string           { return "fake" }
func (noticeFakeTool) Description() string    { return "fake tool" }
func (noticeFakeTool) Schema() map[string]any { return map[string]any{"type": "object"} }
func (noticeFakeTool) Invoke(context.Context, rig.ToolCall) (rig.ToolResult, error) {
	return rig.ToolResult{Content: "ok"}, nil
}

func TestNoticeToolPendingNotices(t *testing.T) {
	wrapped := noticeTool{Tool: noticeFakeTool{}, drain: func() []string { return []string{"notice"} }}
	notices := wrapped.PendingNotices()
	if len(notices) != 1 || notices[0] != "notice" {
		t.Fatalf("PendingNotices = %v", notices)
	}
	if name := wrapped.Name(); name != "fake" {
		t.Fatalf("decorated tool name = %q", name)
	}
	if notices := (noticeTool{}).PendingNotices(); notices != nil {
		t.Fatalf("nil drain notices = %v", notices)
	}
}

func TestJobNoticeText(t *testing.T) {
	manager := jobs.NewManager()
	drain := jobNoticeText(manager)
	if notices := drain(); len(notices) != 0 {
		t.Fatalf("empty manager notices = %v", notices)
	}
	if notices := jobNoticeText(nil)(); notices != nil {
		t.Fatalf("nil manager notices = %v", notices)
	}
}
