package jobs_test

// 宿主侧可用的任务视图与结构化工具结果（rig.ToolResult.Data）。模型看到的文本
// 一个字都不改：这里断言的是"宿主不必解析人读文本"这条契约。

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/tools/jobs"
)

// jobData 取出一次工具结果里的结构化数据。
func jobData(t *testing.T, result rig.ToolResult) map[string]any {
	t.Helper()
	if result.Data == nil {
		t.Fatalf("%q: ToolResult.Data is nil, want structured host data", firstLine(result.Content))
	}
	data, ok := result.Data.(map[string]any)
	if !ok {
		t.Fatalf("%q: ToolResult.Data = %T, want map[string]any", firstLine(result.Content), result.Data)
	}
	return data
}

func firstLine(text string) string {
	if index := strings.Index(text, "\n"); index >= 0 {
		return text[:index]
	}
	return text
}

func dataString(t *testing.T, data map[string]any, key string) string {
	t.Helper()
	value, ok := data[key]
	if !ok {
		t.Fatalf("Data has no %q: %v", key, data)
	}
	text, ok := value.(string)
	if !ok {
		t.Fatalf("Data[%q] = %T, want string", key, value)
	}
	return text
}

func dataInt(t *testing.T, data map[string]any, key string) int {
	t.Helper()
	value, ok := data[key]
	if !ok {
		t.Fatalf("Data has no %q: %v", key, data)
	}
	switch number := value.(type) {
	case int:
		return number
	case int64:
		return int(number)
	case float64:
		return int(number)
	}
	t.Fatalf("Data[%q] = %T, want an integer", key, value)
	return 0
}

func dataInt64(t *testing.T, data map[string]any, key string) int64 {
	t.Helper()
	return int64(dataInt(t, data, key))
}

func dataBool(t *testing.T, data map[string]any, key string) bool {
	t.Helper()
	value, ok := data[key]
	if !ok {
		t.Fatalf("Data has no %q: %v", key, data)
	}
	flag, ok := value.(bool)
	if !ok {
		t.Fatalf("Data[%q] = %T, want bool", key, value)
	}
	return flag
}

// dataJobs 取出 job_list 的 Data["jobs"]，接受 []map[string]any 与 []any 两种形态。
func dataJobs(t *testing.T, data map[string]any) []map[string]any {
	t.Helper()
	value, ok := data["jobs"]
	if !ok {
		t.Fatalf("Data has no \"jobs\": %v", data)
	}
	switch items := value.(type) {
	case []map[string]any:
		return items
	case []any:
		out := make([]map[string]any, 0, len(items))
		for _, item := range items {
			entry, ok := item.(map[string]any)
			if !ok {
				t.Fatalf("Data[\"jobs\"] contains %T, want map[string]any", item)
			}
			out = append(out, entry)
		}
		return out
	}
	t.Fatalf("Data[\"jobs\"] = %T, want a slice of maps", value)
	return nil
}

// blockJobOnFIFO 启动一个阻塞在命名管道上的任务，返回任务 id 与"放行"函数。
// 用命名管道同步而不是 sleep：任务在被放行之前一定处于 running。
func blockJobOnFIFO(t *testing.T, tools map[string]rig.Tool) (string, func()) {
	t.Helper()
	fifo := filepath.Join(t.TempDir(), "gate")
	if output, err := exec.Command("mkfifo", fifo).CombinedOutput(); err != nil {
		t.Fatalf("mkfifo: %v (%s)", err, output)
	}
	jobID := startJob(t, tools, `{"command":"cat `+jsonEscape(fifo)+`"}`)
	var once sync.Once
	release := func() {
		once.Do(func() {
			done := make(chan error, 1)
			go func() {
				file, err := os.OpenFile(fifo, os.O_WRONLY, 0)
				if err != nil {
					done <- err
					return
				}
				if _, err := file.WriteString("released\n"); err != nil {
					_ = file.Close()
					done <- err
					return
				}
				done <- file.Close()
			}()
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("release fifo: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Errorf("release fifo: timed out")
			}
		})
	}
	t.Cleanup(release)
	return jobID, release
}

// TestManagerSnapshotTracksJobLifecycle：Snapshot() 里的状态必须从 running 变成
// exited，且退出码、结束时间、产出字节数随之更新。
func TestManagerSnapshotTracksJobLifecycle(t *testing.T) {
	manager, tools := newJobTools(t)
	jobID, release := blockJobOnFIFO(t, tools)

	running := manager.Snapshot()
	if len(running) != 1 {
		t.Fatalf("Snapshot() = %d jobs, want 1: %+v", len(running), running)
	}
	entry := running[0]
	if entry.ID != jobID {
		t.Fatalf("Snapshot()[0].ID = %q, want %q", entry.ID, jobID)
	}
	if entry.State != "running" {
		t.Fatalf("running snapshot State = %q, want running", entry.State)
	}
	if !entry.Finished.IsZero() {
		t.Fatalf("running snapshot Finished = %s, want zero", entry.Finished)
	}
	if entry.Started.IsZero() || entry.Started.After(time.Now()) {
		t.Fatalf("running snapshot Started = %s, want a real timestamp", entry.Started)
	}
	if entry.ExitCode != -1 {
		t.Fatalf("running snapshot ExitCode = %d, want -1", entry.ExitCode)
	}
	if !strings.Contains(entry.Command, "cat") {
		t.Fatalf("running snapshot Command = %q, want the command text", entry.Command)
	}
	if entry.OutputBytes != 0 {
		t.Fatalf("running snapshot OutputBytes = %d, want 0", entry.OutputBytes)
	}

	release()
	waitForState(t, tools, jobID, "exited")

	finished := manager.Snapshot()
	if len(finished) != 1 {
		t.Fatalf("Snapshot() after exit = %d jobs, want 1", len(finished))
	}
	entry = finished[0]
	if entry.State != "exited" {
		t.Fatalf("finished snapshot State = %q, want exited", entry.State)
	}
	if entry.ExitCode != 0 {
		t.Fatalf("finished snapshot ExitCode = %d, want 0", entry.ExitCode)
	}
	if entry.Finished.IsZero() || entry.Finished.Before(entry.Started) {
		t.Fatalf("finished snapshot Finished = %s (started %s)", entry.Finished, entry.Started)
	}
	if entry.OutputBytes != len("released\n") {
		t.Fatalf("finished snapshot OutputBytes = %d, want %d", entry.OutputBytes, len("released\n"))
	}
}

// TestManagerSnapshotOrderedByStartTime：Snapshot() 按 Started 升序排列。
func TestManagerSnapshotOrderedByStartTime(t *testing.T) {
	manager, tools := newJobTools(t)
	first := startJob(t, tools, `{"command":"echo first"}`)
	waitForState(t, tools, first, "exited")
	second := startJob(t, tools, `{"command":"echo second"}`)
	waitForState(t, tools, second, "exited")

	snapshot := manager.Snapshot()
	if len(snapshot) != 2 {
		t.Fatalf("Snapshot() = %d jobs, want 2: %+v", len(snapshot), snapshot)
	}
	if snapshot[0].ID != first || snapshot[1].ID != second {
		t.Fatalf("Snapshot() order = %q,%q, want %q,%q",
			snapshot[0].ID, snapshot[1].ID, first, second)
	}
	if snapshot[1].Started.Before(snapshot[0].Started) {
		t.Fatalf("Snapshot() is not sorted by Started: %s then %s",
			snapshot[0].Started, snapshot[1].Started)
	}
}

// TestJobOutputDataMirrorsPaging：job_output 的 Data 必须与人读文本里的分页字段
// 逐个一致，并且仅靠 Data 就能把输出读完。
func TestJobOutputDataMirrorsPaging(t *testing.T) {
	_, tools := newJobTools(t, jobs.WithMaxOutputBytes(64))
	jobID := startJob(t, tools, `{"command":"seq 1 60"}`)

	result := invokeJob(t, tools, jobs.ToolJobOutput,
		`{"job_id":"`+jobID+`","wait":true,"timeout_ms":5000}`)
	if result.IsError {
		t.Fatalf("job_output: %q", result.Content)
	}
	data := jobData(t, result)
	if got := dataString(t, data, "id"); got != jobID {
		t.Fatalf("Data[id] = %q, want %q", got, jobID)
	}
	if got := dataString(t, data, "state"); got != "exited" {
		t.Fatalf("Data[state] = %q, want exited", got)
	}
	if got := dataInt(t, data, "exit_code"); got != 0 {
		t.Fatalf("Data[exit_code] = %d, want 0", got)
	}

	// 第一页必然被 64 字节上限截断：Data 与文本必须说同一件事。
	if !dataBool(t, data, "output_truncated") {
		t.Fatalf("Data[output_truncated] = false, want true:\n%s", result.Content)
	}
	if !strings.Contains(result.Content, "output_truncated: true") {
		t.Fatalf("text has no output_truncated: true:\n%s", result.Content)
	}
	textOffset, ok := responseField(result.Content, "output_offset")
	if !ok {
		t.Fatalf("text has no output_offset:\n%s", result.Content)
	}
	textNext, ok := responseField(result.Content, "next_offset")
	if !ok {
		t.Fatalf("text has no next_offset:\n%s", result.Content)
	}
	if got, want := dataInt64(t, data, "output_offset"), mustParseInt(t, textOffset); got != want {
		t.Fatalf("Data[output_offset] = %d, text = %q", got, textOffset)
	}
	if got, want := dataInt64(t, data, "next_offset"), mustParseInt(t, textNext); got != want {
		t.Fatalf("Data[next_offset] = %d, text = %q", got, textNext)
	}
	firstPage := stripOutputMarkers(outputSection(result.Content))

	// 只用 Data 里的 next_offset / output_truncated 逐页续读。
	var collected strings.Builder
	collected.WriteString(firstPage)
	offset := dataInt64(t, data, "next_offset")
	for attempt := 0; dataBool(t, data, "output_truncated"); attempt++ {
		if attempt > 100 {
			t.Fatalf("paging did not terminate; offset = %d", offset)
		}
		next := invokeJob(t, tools, jobs.ToolJobOutput,
			`{"job_id":`+strconv.Quote(jobID)+`,"offset":`+strconv.FormatInt(offset, 10)+`}`)
		if next.IsError {
			t.Fatalf("offset=%d: %q", offset, next.Content)
		}
		data = jobData(t, next)
		collected.WriteString(stripOutputMarkers(outputSection(next.Content)))
		if dataBool(t, data, "output_truncated") {
			if textNext, ok := responseField(next.Content, "next_offset"); ok {
				if got, want := dataInt64(t, data, "next_offset"), mustParseInt(t, textNext); got != want {
					t.Fatalf("paged Data[next_offset] = %d, text = %q", got, textNext)
				}
			}
			offset = dataInt64(t, data, "next_offset")
			if offset <= 0 {
				t.Fatalf("paged next_offset = %d, want a forward offset", offset)
			}
			continue
		}
		if _, more := responseField(next.Content, "next_offset"); more {
			t.Fatalf("Data says not truncated but text has next_offset:\n%s", next.Content)
		}
	}

	var want strings.Builder
	for i := 1; i <= 60; i++ {
		want.WriteString(strconv.Itoa(i))
		want.WriteString("\n")
	}
	if got := strings.TrimRight(collected.String(), "\n"); got != strings.TrimRight(want.String(), "\n") {
		t.Fatalf("paging by Data lost or duplicated output: %d bytes, want %d", len(got), want.Len())
	}
}

func mustParseInt(t *testing.T, text string) int64 {
	t.Helper()
	value, err := strconv.ParseInt(strings.TrimSpace(text), 10, 64)
	if err != nil {
		t.Fatalf("value %q is not an integer: %v", text, err)
	}
	return value
}

// TestJobListDataMirrorsTextLines：job_list 的 Data.jobs 条数必须等于文本里的
// 任务行数，且每项至少带 id/state/exit_code。
func TestJobListDataMirrorsTextLines(t *testing.T) {
	_, tools := newJobTools(t)
	first := startJob(t, tools, `{"command":"echo first"}`)
	waitForState(t, tools, first, "exited")
	second := startJob(t, tools, `{"command":"echo second; exit 3"}`)
	waitForState(t, tools, second, "exited")

	result := invokeJob(t, tools, jobs.ToolJobList, `{}`)
	if result.IsError {
		t.Fatalf("job_list: %q", result.Content)
	}
	items := dataJobs(t, jobData(t, result))

	lines := 0
	for _, line := range strings.Split(result.Content, "\n") {
		if strings.HasPrefix(line, "- ") {
			lines++
		}
	}
	if lines != 2 {
		t.Fatalf("text lists %d job lines, want 2:\n%s", lines, result.Content)
	}
	if len(items) != lines {
		t.Fatalf("Data.jobs has %d entries, text has %d lines:\n%s", len(items), lines, result.Content)
	}
	if got := dataString(t, items[0], "id"); got != first {
		t.Fatalf("Data.jobs[0].id = %q, want %q", got, first)
	}
	if got := dataString(t, items[1], "id"); got != second {
		t.Fatalf("Data.jobs[1].id = %q, want %q", got, second)
	}
	for index, item := range items {
		if got := dataString(t, item, "state"); got != "exited" {
			t.Fatalf("Data.jobs[%d].state = %q, want exited", index, got)
		}
		if _, ok := item["exit_code"]; !ok {
			t.Fatalf("Data.jobs[%d] has no exit_code: %v", index, item)
		}
	}
	if got := dataInt(t, items[1], "exit_code"); got != 3 {
		t.Fatalf("Data.jobs[1].exit_code = %d, want 3", got)
	}
}

// TestJobToolDataForStartWaitKill：job_start/job_wait/job_kill 也给出结构化数据，
// 且 id 与模型可见文本一致。
func TestJobToolDataForStartWaitKill(t *testing.T) {
	_, tools := newJobTools(t)
	// job_start：用一个确实在跑的任务（sleep 30）断言 state=running。
	started := invokeJob(t, tools, jobs.ToolJobStart, `{"command":"sleep 30"}`)
	if started.IsError {
		t.Fatalf("job_start: %q", started.Content)
	}
	startData := jobData(t, started)
	jobID := dataString(t, startData, "id")
	if jobID == "" {
		t.Fatalf("Data[id] is empty")
	}
	if !strings.Contains(started.Content, "job_id: "+jobID+"\n") {
		t.Fatalf("Data id %q does not match the visible text:\n%s", jobID, started.Content)
	}
	if got := dataString(t, startData, "state"); got != "running" {
		t.Fatalf("job_start Data[state] = %q, want running", got)
	}

	// job_wait：短任务结束后给出 exited 与退出码。
	waited := invokeJob(t, tools, jobs.ToolJobStart, `{"command":"echo hi"}`)
	waitedData := jobData(t, waited)
	waitedID := dataString(t, waitedData, "id")
	waited = invokeJob(t, tools, jobs.ToolJobWait, `{"job_id":`+strconv.Quote(waitedID)+`}`)
	waitData := jobData(t, waited)
	if got := dataString(t, waitData, "id"); got != waitedID {
		t.Fatalf("job_wait Data[id] = %q, want %q", got, waitedID)
	}
	if got := dataString(t, waitData, "state"); got != "exited" {
		t.Fatalf("job_wait Data[state] = %q, want exited", got)
	}
	if got := dataInt(t, waitData, "exit_code"); got != 0 {
		t.Fatalf("job_wait Data[exit_code] = %d, want 0", got)
	}

	// job_kill：杀死那个还在跑的任务，Data 与文本状态一致。
	killed := invokeJob(t, tools, jobs.ToolJobKill, `{"job_id":`+strconv.Quote(jobID)+`}`)
	if killed.IsError {
		t.Fatalf("job_kill: %q", killed.Content)
	}
	killData := jobData(t, killed)
	if got := dataString(t, killData, "id"); got != jobID {
		t.Fatalf("job_kill Data[id] = %q, want %q", got, jobID)
	}
	if got := dataString(t, killData, "state"); got != "killed" {
		t.Fatalf("job_kill Data[state] = %q, want killed", got)
	}
	if !strings.Contains(killed.Content, "state: killed") {
		t.Fatalf("job_kill text and Data disagree:\n%s", killed.Content)
	}
}
