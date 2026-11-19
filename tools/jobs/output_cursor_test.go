package jobs_test

// 本文件钉住 job_output 的分页语义：达到 maxOutputBytes 时必须**只推进实际
// 交付的字节**，并给出续读偏移；否则被截断的尾巴永久读不到（修复前 consumed
// 已被推到 Total，第二次读直接 "(none)"，响应里也没有 offset）。

import (
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig/tools/jobs"
)

// outputSection 取出响应里 "output:" 之后的正文（去掉状态头）。
func outputSection(content string) string {
	index := strings.Index(content, "output:\n")
	if index < 0 {
		if strings.Contains(content, "output: (none)") {
			return ""
		}
		return ""
	}
	return content[index+len("output:\n"):]
}

// stripOutputMarkers 去掉分页提示行，剩下真正的程序输出。
func stripOutputMarkers(text string) string {
	var kept []string
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "…" || strings.HasPrefix(trimmed, "[truncated:") {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

// responseField 读取响应头/正文里的 "name: value" 字段。
func responseField(content, name string) (string, bool) {
	for _, line := range strings.Split(content, "\n") {
		if strings.HasPrefix(line, name+":") {
			return strings.TrimSpace(strings.TrimPrefix(line, name+":")), true
		}
	}
	return "", false
}

// TestJobOutputPagingNeverLosesTail：500 行、单次响应上限 200 字节时，连续读取
// 必须拼回完整输出（不丢尾、不重复），且每次截断都给出 next_offset。
func TestJobOutputPagingNeverLosesTail(t *testing.T) {
	_, tools := newJobTools(t, jobs.WithMaxOutputBytes(200))
	jobID := startJob(t, tools, `{"command":"seq 1 500"}`)

	var expected strings.Builder
	for i := 1; i <= 500; i++ {
		expected.WriteString(strconv.Itoa(i))
		expected.WriteString("\n")
	}

	var collected strings.Builder
	truncatedReads := 0
	lastNext := int64(-1)
	finished := false
	for attempt := 0; attempt < 100 && !finished; attempt++ {
		result := invokeJob(t, tools, jobs.ToolJobOutput,
			`{"job_id":"`+jobID+`","wait":true,"timeout_ms":5000}`)
		if result.IsError {
			t.Fatalf("job_output: %q", result.Content)
		}
		if _, ok := responseField(result.Content, "output_offset"); !ok {
			t.Fatalf("第 %d 次读取没有 output_offset 字段:\n%s", attempt+1, result.Content)
		}
		nextText, more := responseField(result.Content, "next_offset")
		if more {
			next, err := strconv.ParseInt(nextText, 10, 64)
			if err != nil {
				t.Fatalf("next_offset=%q 不是整数:\n%s", nextText, result.Content)
			}
			if next <= lastNext {
				t.Fatalf("next_offset 没有前进：%d → %d", lastNext, next)
			}
			lastNext = next
			truncatedReads++
			if truncatedReads == 1 {
				t.Logf("第一次截断的分页响应样例:\n%s", result.Content)
			}
			if !strings.Contains(result.Content, "output_truncated: true") {
				t.Fatalf("有 next_offset 却没有 output_truncated 标记:\n%s", result.Content)
			}
		} else {
			finished = true
		}
		collected.WriteString(stripOutputMarkers(outputSection(result.Content)))
	}
	if truncatedReads == 0 {
		t.Fatalf("单次上限 200 字节、输出约 %d 字节，却没有发生任何分页", expected.Len())
	}
	if got := strings.TrimRight(collected.String(), "\n"); got != strings.TrimRight(expected.String(), "\n") {
		t.Fatalf("分页读取丢数据/重复：收到 %d 字节，期望 %d 字节\n首段=%q\n末段=%q",
			len(got), expected.Len(), firstN(got, 120), lastN(got, 120))
	}
}

// TestJobOutputOffsetPagingIsRecoverable：offset 读取同样受上限约束并给出
// next_offset，模型可以据此逐页读回缓冲里的全部内容。
func TestJobOutputOffsetPagingIsRecoverable(t *testing.T) {
	_, tools := newJobTools(t, jobs.WithMaxOutputBytes(100))
	jobID := startJob(t, tools, `{"command":"seq 1 120"}`)
	invokeJob(t, tools, jobs.ToolJobOutput, `{"job_id":"`+jobID+`","wait":true,"timeout_ms":5000}`)

	// 用一条短任务验证 offset 分页：从头读，按 next_offset 逐页推进。
	_, tools2 := newJobTools(t, jobs.WithMaxOutputBytes(64))
	job2 := startJob(t, tools2, `{"command":"printf 'aaaaaaaaaaaaaaaaaaaa\\nbbbbbbbbbbbbbbbbbbbb\\ncccccccccccccccccccc\\ndddddddddddddddddddd\\n'"}`)
	invokeJob(t, tools2, jobs.ToolJobOutput, `{"job_id":"`+job2+`","wait":true,"timeout_ms":5000}`)

	var collected strings.Builder
	offset := int64(0)
	for attempt := 0; attempt < 20; attempt++ {
		result := invokeJob(t, tools2, jobs.ToolJobOutput,
			fmt.Sprintf(`{"job_id":%q,"offset":%d}`, job2, offset))
		if result.IsError {
			t.Fatalf("offset=%d: %q", offset, result.Content)
		}
		collected.WriteString(stripOutputMarkers(outputSection(result.Content)))
		nextText, more := responseField(result.Content, "next_offset")
		if !more {
			break
		}
		next, err := strconv.ParseInt(nextText, 10, 64)
		if err != nil || next <= offset {
			t.Fatalf("offset 分页没有前进：offset=%d next=%q", offset, nextText)
		}
		offset = next
	}
	want := "aaaaaaaaaaaaaaaaaaaa\nbbbbbbbbbbbbbbbbbbbb\ncccccccccccccccccccc\ndddddddddddddddddddd\n"
	if got := strings.TrimRight(collected.String(), "\n"); got != strings.TrimRight(want, "\n") {
		t.Fatalf("offset 分页结果 = %q，want %q", got, want)
	}
}

func firstN(text string, n int) string {
	if len(text) <= n {
		return text
	}
	return text[:n]
}

func lastN(text string, n int) string {
	if len(text) <= n {
		return text
	}
	return text[len(text)-n:]
}

// TestJobOutputPagingSingleLongLine：没有换行的超长单行（例如压缩后的 JSON）
// 也必须能分页读完——按字节切、不退化成"只给头部"。
func TestJobOutputPagingSingleLongLine(t *testing.T) {
	_, tools := newJobTools(t, jobs.WithMaxOutputBytes(64))
	jobID := startJob(t, tools, `{"command":"python3 -c \"print('x'*300)\""}`)

	var collected strings.Builder
	first := invokeJob(t, tools, jobs.ToolJobOutput, `{"job_id":"`+jobID+`","wait":true,"timeout_ms":5000}`)
	collected.WriteString(stripOutputMarkers(outputSection(first.Content)))
	for attempt := 0; attempt < 20; attempt++ {
		if _, more := responseField(first.Content, "next_offset"); !more {
			break
		}
		first = invokeJob(t, tools, jobs.ToolJobOutput, `{"job_id":"`+jobID+`"}`)
		if first.IsError {
			t.Fatalf("job_output: %q", first.Content)
		}
		collected.WriteString(stripOutputMarkers(outputSection(first.Content)))
	}
	if got := strings.TrimRight(collected.String(), "\n"); got != strings.Repeat("x", 300) {
		t.Fatalf("超长单行分页结果长度 = %d，want 300（x=%d, 换行=%d, 其余=%q）",
			len(got), strings.Count(got, "x"), strings.Count(got, "\n"),
			strings.Map(func(r rune) rune {
				if r == 'x' || r == '\n' {
					return -1
				}
				return r
			}, got))
	}
}
