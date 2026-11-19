package jobs_test

// zz_fix_reader_test.go —— `job_output` 的 reader 游标是显式、可预测的（TDD）。
//
// 旧语义：所有增量读者共享同一个 `consumed` 游标——两个消费者交替读时，后读者
// 只能看到前一个读走之后的部分。新语义：同一个 `reader` 名共享它自己的游标，
// 不传则共享默认游标（旧行为，每字节最多交付一次）。

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/tools/jobs"
)

const readerJobCommand = `{"command":"printf 'alpha\n'; sleep 0.2; printf 'beta\n'"}`

// startAndFinish 起一个短命 job 并等它结束。
//
// 刻意**不用 job_wait** 等：job_wait 会排空默认游标（旧行为），那样后续 reader 读
// 到的空串会被误读成"reader 正常工作"。改用显式 offset 读轮询（不碰任何游标）。
func startAndFinish(t *testing.T, tools map[string]rig.Tool) string {
	t.Helper()
	jobID := startJob(t, tools, readerJobCommand)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		content := invokeJob(t, tools, jobs.ToolJobOutput, fmt.Sprintf(`{"job_id":%q,"offset":0}`, jobID)).Content
		if strings.Contains(content, "state: exited") {
			return jobID
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("job %s 没有在期限内结束", jobID)
	return ""
}

// 两个 reader 各自从头到尾看到完整输出，互不影响。
func TestJobOutputReaderCursorsAreIndependent(t *testing.T) {
	_, tools := newJobTools(t)
	jobID := startAndFinish(t, tools)

	read := func(reader string) string {
		return invokeJob(t, tools, jobs.ToolJobOutput, fmt.Sprintf(`{"job_id":%q,"reader":%q}`, jobID, reader)).Content
	}

	raw := read("a")
	t.Logf("reader=a 首次读:\n%s", raw)
	first := outputSection(raw)
	if !strings.Contains(first, "alpha") || !strings.Contains(first, "beta") {
		t.Fatalf("reader a 应看到完整输出，得到 %q", first)
	}
	second := outputSection(read("b"))
	if !strings.Contains(second, "alpha") || !strings.Contains(second, "beta") {
		t.Fatalf("reader b 必须也看到完整输出（独立游标），得到 %q", second)
	}
	// 再读同一 reader：没有新输出。
	if again := outputSection(read("a")); strings.Contains(again, "alpha") || strings.Contains(again, "beta") {
		t.Fatalf("reader a 的游标应已推进，第二次读不该重复交付，得到 %q", again)
	}
}

// reader 表**没有**人为上限：登记 200 个 reader 后第 1 个的游标仍在（继续读不重复）。
func TestJobOutputReaderCursorsAreUnbounded(t *testing.T) {
	_, tools := newJobTools(t)
	jobID := startAndFinish(t, tools)

	read := func(reader string) string {
		return invokeJob(t, tools, jobs.ToolJobOutput, fmt.Sprintf(`{"job_id":%q,"reader":%q}`, jobID, reader)).Content
	}

	// 第 1 个 reader 先读到全部输出。
	if out := outputSection(read("r0")); !strings.Contains(out, "alpha") {
		t.Fatalf("r0 首次读应看到完整输出，得到 %q", out)
	}
	// 再登记 200 个 reader。
	for i := 1; i <= 200; i++ {
		reader := fmt.Sprintf("r%d", i)
		if out := outputSection(read(reader)); !strings.Contains(out, "alpha") {
			t.Fatalf("%s 首次读应看到完整输出，得到 %q", reader, out)
		}
	}
	// r0 的游标必须还在（没有淘汰）：再读它不该重复交付。
	if out := outputSection(read("r0")); strings.Contains(out, "alpha") || strings.Contains(out, "beta") {
		t.Fatalf("r0 的游标应仍被保留（无淘汰），得到 %q", out)
	}
}

// WithMaxOutputBytes(0) = 不限：一次读回全部 100 KiB。
func TestJobOutputUnlimitedWhenMaxIsZero(t *testing.T) {
	_, tools := newJobTools(t, jobs.WithMaxOutputBytes(0))
	jobID := startJob(t, tools, `{"command":"dd if=/dev/zero bs=1024 count=100 2>/dev/null | tr '\\0' x"}`)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
		content := invokeJob(t, tools, jobs.ToolJobOutput, fmt.Sprintf(`{"job_id":%q,"offset":0}`, jobID)).Content
		if strings.Contains(content, "state: exited") {
			break
		}
	}
	out := outputSection(invokeJob(t, tools, jobs.ToolJobOutput, fmt.Sprintf(`{"job_id":%q,"reader":"big"}`, jobID)).Content)
	if len(out) < 100*1024 {
		t.Fatalf("WithMaxOutputBytes(0) 应一次读回全部 100 KiB，实际只有 %d 字节", len(out))
	}
	if strings.Contains(out, "output_truncated") || strings.Contains(out, "next_offset") {
		t.Fatalf("不限时不该出现分页标记: %.200s", out)
	}
}
