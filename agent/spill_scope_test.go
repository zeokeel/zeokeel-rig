package agent

// 本文件固定 spill 的三条审计结论（P1）：
//
//  1. 回收必须**按 owner/session 分区**：多个会话/子 agent 共用一个目录时，
//     一个 policy 的 MaxFiles 不得删掉别人的句柄；
//  2. agent 层不再默认 `os.TempDir()`：没给 Dir 时**不写盘**，返回可读错误
//     （旧默认把结果写到 read_file 的 roots 之外 → 模型永远读不回来）；
//  3. ctx 必须传下去：取消后不再继续写盘。

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	rig "github.com/zeokeel/zeokeel-rig"
)

// TestSpillPruneIsScopedToOwner：A 的 MaxFiles=1 只回收 A 自己的文件。
func TestSpillPruneIsScopedToOwner(t *testing.T) {
	dir := t.TempDir()
	policyA := rig.SpillPolicy{Dir: dir, ThresholdBytes: 8, MaxFiles: 1}
	policyB := rig.SpillPolicy{Dir: dir, ThresholdBytes: 8, MaxFiles: 1}
	ctxA := withToolSession(context.Background(), "session-a")
	ctxB := withToolSession(context.Background(), "session-b")

	first, err := spillToolResult(ctxA, policyA, rig.ToolCall{Name: "bash", ID: "a1"}, strings.Repeat("a", 32))
	if err != nil {
		t.Fatalf("spill A#1: %v", err)
	}
	second, err := spillToolResult(ctxA, policyA, rig.ToolCall{Name: "bash", ID: "a2"}, strings.Repeat("b", 32))
	if err != nil {
		t.Fatalf("spill A#2: %v", err)
	}
	other, err := spillToolResult(ctxB, policyB, rig.ToolCall{Name: "bash", ID: "b1"}, strings.Repeat("c", 32))
	if err != nil {
		t.Fatalf("spill B: %v", err)
	}

	firstPath := spillPathOf(t, first)
	secondPath := spillPathOf(t, second)
	otherPath := spillPathOf(t, other)

	if _, statErr := os.Stat(secondPath); statErr != nil {
		t.Fatalf("A 的最新文件被误删: %v", statErr)
	}
	if _, statErr := os.Stat(otherPath); statErr != nil {
		t.Fatalf("B 的文件被 A 的 MaxFiles 删掉了（跨会话互删）: %v", statErr)
	}
	if _, statErr := os.Stat(firstPath); !os.IsNotExist(statErr) {
		t.Fatalf("A 超过 MaxFiles=1 的旧文件没有被回收: %v", statErr)
	}
	// 两个会话的文件都必须留在同一目录里（分区靠文件名前缀，不靠子目录）。
	entries, _ := os.ReadDir(dir)
	if len(entries) != 2 {
		t.Fatalf("目录里应只剩 A 最新 + B 各一份，实际 %d 个", len(entries))
	}
}

// TestSpillWithoutDirFailsLoudly：agent 层不再往 os.TempDir() 写。
func TestSpillWithoutDirFailsLoudly(t *testing.T) {
	ctx := withToolSession(context.Background(), "s")
	summary, err := spillToolResult(ctx, rig.SpillPolicy{ThresholdBytes: 8},
		rig.ToolCall{Name: "bash", ID: "x"}, strings.Repeat("z", 32))
	if err == nil {
		t.Fatalf("没有 Dir 时不应静默落盘，summary=%q", summary)
	}
	if !strings.Contains(err.Error(), "Dir") {
		t.Fatalf("错误应指明要设置 Dir: %v", err)
	}
	if summary != "" {
		t.Fatalf("失败时不应返回摘要: %q", summary)
	}
	// 不能有任何东西落进系统临时目录下的旧默认位置。
	legacy := filepath.Join(os.TempDir(), "zeokeel-rig-spill")
	if entries, readErr := os.ReadDir(legacy); readErr == nil {
		for _, entry := range entries {
			if strings.Contains(entry.Name(), "bash") {
				t.Fatalf("仍在写旧的临时目录: %s", entry.Name())
			}
		}
	}
}

// TestSpillHonoursCancelledContext：ctx 取消 → 返回错误且不写盘。
func TestSpillHonoursCancelledContext(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(withToolSession(context.Background(), "s"))
	cancel()
	summary, err := spillToolResult(ctx, rig.SpillPolicy{Dir: dir, ThresholdBytes: 8},
		rig.ToolCall{Name: "bash", ID: "c"}, strings.Repeat("q", 64))
	if err == nil {
		t.Fatalf("取消后仍写盘成功: %q", summary)
	}
	if !strings.Contains(err.Error(), context.Canceled.Error()) {
		t.Fatalf("错误应包装 context.Canceled: %v", err)
	}
	entries, readErr := os.ReadDir(dir)
	if readErr != nil {
		t.Fatalf("ReadDir: %v", readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("取消后仍写出了 %d 个文件", len(entries))
	}
}

// spillPathOf 从摘要里取出 `path: ` 那一行。
func spillPathOf(t *testing.T, summary string) string {
	t.Helper()
	for _, line := range strings.Split(summary, "\n") {
		if path, ok := strings.CutPrefix(line, "path: "); ok {
			return strings.TrimSpace(path)
		}
	}
	t.Fatalf("摘要里没有 path 行: %q", summary)
	return ""
}
