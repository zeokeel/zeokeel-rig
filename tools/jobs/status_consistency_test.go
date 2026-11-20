package jobs_test

// 同一次 `job_start` 响应里的文本状态头与 `Data["state"]` 必须来自**同一次**
// 状态快照。
//
// 这不是风格问题：修复前文本与 Data 各自 `snapshot()` 一次（当时的无参包装），
// 命令极快结束时两次快照之间任务已经退出，于是模型读到 "state: running"，宿主读到
// `{"state":"exited"}`（同一响应里两个字段互相矛盾，UI 与模型各说一套）。
//
// 命令用 `true`（立刻退出），靠"快"来命中窗口：修复前约几百次里会有一次不一致，
// 修复后结构上不可能（两个字段由同一个 status 渲染）。

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/tools/jobs"
)

func TestJobStartStatusAndDataAgree(t *testing.T) {
	// 管理器保留最近 64 个任务，所以分几轮用新管理器；总次数要足够多才能命中
	// "命令极快结束"的窗口（修复前约 1/300）。
	const rounds, perRound = 5, 60
	payload, err := json.Marshal(map[string]any{"command": "true"})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	attempt := 0
	for round := 0; round < rounds; round++ {
		manager := jobs.NewManager()
		var start rig.Tool
		for _, tool := range manager.Tools() {
			if tool.Name() == "job_start" {
				start = tool
			}
		}
		if start == nil {
			t.Fatal("没有 job_start 工具")
		}
		for iteration := 0; iteration < perRound; iteration++ {
			result, err := start.Invoke(context.Background(), rig.ToolCall{ID: "start", Name: "job_start", Arguments: string(payload)})
			if err != nil {
				t.Fatalf("job_start.Invoke: %v", err)
			}
			if result.IsError {
				t.Fatalf("job_start 失败: %q", result.Content)
			}
			data, _ := result.Data.(map[string]any)
			if data == nil {
				t.Fatalf("job_start 没有 Data")
			}
			state, _ := data["state"].(string)
			if state == "" {
				t.Fatalf("Data 里没有 state: %#v", data)
			}
			fromText := textState(t, result.Content)
			attempt++
			if fromText != state {
				t.Fatalf("第 %d 次：文本 state=%q 与 Data state=%q 不一致（同一次响应里两个字段互相矛盾）",
					attempt, fromText, state)
			}
		}
		_ = manager.CloseAll()
	}
}

// textState 抽出响应文本里的 `state: X` 行。
func textState(t *testing.T, content string) string {
	t.Helper()
	for _, line := range splitLines(content) {
		if len(line) > len("state: ") && line[:len("state: ")] == "state: " {
			return line[len("state: "):]
		}
	}
	t.Fatalf("响应文本里没有 state 行: %q", content)
	return ""
}

func splitLines(s string) []string {
	var lines []string
	start := 0
	for index := 0; index < len(s); index++ {
		if s[index] == '\n' {
			lines = append(lines, s[start:index])
			start = index + 1
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}
