package jobs_test

// 评审 review-2 的低危观察：background.Job.Read 只能回传文本、没有 cursor 字段，
// 改成按偏移读之后截断标记被丢掉了，宿主会静默少读一截。这里钉住"截断时必须显式
// 标记并给出续读偏移"。

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig/tools/internal/background"
	"github.com/zeokeel/zeokeel-rig/tools/jobs"
)

func TestBackgroundReadKeepsTruncationMarker(t *testing.T) {
	manager := jobs.NewManager(jobs.WithMaxOutputBytes(200))
	t.Cleanup(func() { _ = manager.CloseAll() })

	job, err := manager.StartProcess(background.ProcessSpec{
		Kind:    string(background.KindBash),
		Label:   "seq",
		Command: "seq 1 500",
		Argv:    []string{"/bin/sh", "-c", "seq 1 500"},
	})
	if err != nil {
		t.Fatalf("StartProcess: %v", err)
	}
	if _, _, err := job.Wait(context.Background(), 10*time.Second); err != nil {
		t.Fatalf("Wait: %v", err)
	}

	text, _ := job.Read()
	if !strings.Contains(text, "[truncated: output exceeded 200 bytes") {
		t.Fatalf("background Read 未标记截断（宿主会静默少读）: %q", text)
	}
	if !strings.Contains(text, "next offset:") {
		t.Fatalf("background Read 未给出续读偏移: %q", text)
	}
}
