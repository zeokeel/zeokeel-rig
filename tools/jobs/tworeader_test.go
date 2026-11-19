package jobs_test

// 本文件钉住 job_output 的**多读者**语义：`consumed` 是两个（或多个）读者共享的
// 单一游标，交付必须满足"每个字节最多投递一次、且不会因为并发丢尾"。
//
// 修复前 `newOutputChunk` 分两次取锁（读 from → 读缓冲 → 写 consumed），两个读者
// 可以同时从同一个 from 读到同一页 → 重复交付；而无条件赋值
// `consumed = read.Next` 还允许慢读者把游标推回去 → 下一个读者重复交付同一段。

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/tools/jobs"
)

// lineCollector 是并发安全的行计数器（每行出现次数必须恰好一次）。
type lineCollector struct {
	mu    sync.Mutex
	lines map[string]int
}

func newLineCollector() *lineCollector { return &lineCollector{lines: make(map[string]int)} }

// add 记录一页交付的正文，返回本次记录的行数。
func (c *lineCollector) add(content string) int {
	text := stripOutputMarkers(outputSection(content))
	added := 0
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, line := range strings.Split(text, "\n") {
		if line == "" {
			continue
		}
		c.lines[line]++
		added++
	}
	return added
}

func (c *lineCollector) report() (total int, duplicates []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for line, count := range c.lines {
		total++
		if count > 1 {
			duplicates = append(duplicates, fmt.Sprintf("%s×%d", line, count))
		}
	}
	return total, duplicates
}

func (c *lineCollector) countOf(line string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lines[line]
}

// jobState 从 job_list 里读任务状态（不消耗输出游标）。
func jobState(t *testing.T, tools map[string]rig.Tool, jobID string) string {
	t.Helper()
	result := invokeJob(t, tools, jobs.ToolJobList, `{}`)
	for _, line := range strings.Split(result.Content, "\n") {
		if !strings.Contains(line, jobID) {
			continue
		}
		for _, field := range strings.Fields(line) {
			if state, ok := strings.CutPrefix(field, "state="); ok {
				return state
			}
		}
	}
	return ""
}

// TestJobOutputTwoReadersNoDuplicateNoLoss：多个 goroutine 同时做"增量读"
// （无 offset），把各方交付的行合并后必须正好是 N 行、且每行只出现一次。
//
// 任务保持**运行中**读取：写者频繁持有缓冲锁，会放大"读 from 与推进 consumed
// 之间被另一个读者插入"的窗口——这正是修复前会重复交付的原因。
func TestJobOutputTwoReadersNoDuplicateNoLoss(t *testing.T) {
	const totalLines = 20000
	_, tools := newJobTools(t, jobs.WithMaxOutputBytes(64))
	jobID := startJob(t, tools, fmt.Sprintf(`{"command":"seq 1 %d"}`, totalLines))

	collector := newLineCollector()
	readOne := func(reader int) {
		emptyStreak := 0
		for attempt := 0; attempt < 200000; attempt++ {
			result, err := invokeTool(tools, jobs.ToolJobOutput, `{"job_id":"`+jobID+`"}`)
			if err != nil {
				t.Errorf("reader %d: %v", reader, err)
				return
			}
			if result.IsError {
				t.Errorf("reader %d: %q", reader, result.Content)
				return
			}
			if collector.add(result.Content) > 0 {
				emptyStreak = 0
				continue
			}
			// 空读：任务还在写就继续等；已结束且连续空读才算读完。
			if jobState(t, tools, jobID) == "exited" {
				emptyStreak++
				if emptyStreak >= 3 {
					return
				}
			}
			time.Sleep(time.Millisecond)
		}
		t.Errorf("reader %d: 读取次数用尽仍未读完", reader)
	}

	var wg sync.WaitGroup
	for reader := 0; reader < 4; reader++ {
		wg.Add(1)
		go func(reader int) {
			defer wg.Done()
			readOne(reader)
		}(reader)
	}
	wg.Wait()

	if state := jobState(t, tools, jobID); state != "exited" {
		t.Fatalf("任务状态 = %q，want exited", state)
	}
	total, duplicates := collector.report()
	if len(duplicates) > 0 {
		t.Fatalf("有 %d 行被重复交付（共享游标被并发读者重复推进）: %v（共 %d 行）",
			len(duplicates), headN(duplicates, 5), total)
	}
	if total != totalLines {
		t.Fatalf("合并后行数 = %d，want %d（并发读取丢了尾）", total, totalLines)
	}
	for _, line := range []string{"1", "2", "9999", "10000", fmt.Sprintf("%d", totalLines)} {
		if count := collector.countOf(line); count != 1 {
			t.Fatalf("行 %q 出现 %d 次，want 1", line, count)
		}
	}
}

// TestJobOutputOffsetReadDoesNotAdvanceCursor：显式 offset 读是"绝对读取"，不得
// 推进增量游标，也不得被增量游标影响。
func TestJobOutputOffsetReadDoesNotAdvanceCursor(t *testing.T) {
	_, tools := newJobTools(t, jobs.WithMaxOutputBytes(64))
	jobID := startJob(t, tools, `{"command":"seq 1 500"}`)
	invokeJob(t, tools, jobs.ToolJobOutput, `{"job_id":"`+jobID+`","wait":true,"timeout_ms":5000}`)

	// 增量读一次：拿到一页（500 行远大于 64 字节，必然还有剩余）。
	first := invokeJob(t, tools, jobs.ToolJobOutput, `{"job_id":"`+jobID+`"}`)
	firstText := strings.TrimSpace(stripOutputMarkers(outputSection(first.Content)))
	if firstText == "" {
		t.Fatalf("第一次增量读为空: %q", first.Content)
	}
	// offset 读把全文从头读一遍，不能把增量游标推回/推到末尾。
	var full strings.Builder
	offset := 0
	for attempt := 0; attempt < 200; attempt++ {
		result := invokeJob(t, tools, jobs.ToolJobOutput, fmt.Sprintf(`{"job_id":%q,"offset":%d}`, jobID, offset))
		full.WriteString(stripOutputMarkers(outputSection(result.Content)))
		nextText, more := responseField(result.Content, "next_offset")
		if !more {
			break
		}
		var next int
		if _, err := fmt.Sscanf(nextText, "%d", &next); err != nil || next <= offset {
			t.Fatalf("offset 分页没有前进: offset=%d next=%q", offset, nextText)
		}
		offset = next
	}
	if got := strings.TrimSpace(full.String()); !strings.HasPrefix(got, "1\n2\n3") || !strings.Contains(got, "500") {
		t.Fatalf("offset 读取没有拿到全文: %q", got)
	}
	// 增量读取必须从第一次交付的下一字节继续：不能重复第 1 行，也不能直接跳空。
	second := invokeJob(t, tools, jobs.ToolJobOutput, `{"job_id":"`+jobID+`"}`)
	secondText := strings.TrimSpace(stripOutputMarkers(outputSection(second.Content)))
	if secondText == "" {
		t.Fatalf("offset 读把增量游标推到末尾了，增量读直接为空")
	}
	for _, line := range strings.Split(secondText, "\n") {
		if strings.TrimSpace(line) == "1" {
			t.Fatalf("offset 读把增量游标推回去了，增量读重复交付第 1 行: %q", secondText)
		}
	}
}

func headN(items []string, n int) []string {
	if len(items) <= n {
		return items
	}
	return items[:n]
}

// invokeTool 是 goroutine 安全的工具调用（返回错误而不是 t.Fatalf）。
func invokeTool(tools map[string]rig.Tool, name, arguments string) (rig.ToolResult, error) {
	tool, ok := tools[name]
	if !ok {
		return rig.ToolResult{}, fmt.Errorf("tool %q not found", name)
	}
	return tool.Invoke(context.Background(), rig.ToolCall{Name: name, Arguments: arguments})
}
