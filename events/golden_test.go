package events_test

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig/events"
)

// 本文件是线格式的字节级快照：整条事件流序列化后必须与 testdata/stream.ndjson
// 逐字节相等。任何字段改名、增删、顺序变化、时间格式化变化都会让它变红——
// 这正是"升级即破坏第三方消费者"的护栏。
//
// 需要有意变更线格式时：-update 重生成，并在报告中说明版本是否 +1。

var updateGolden = flag.Bool("update", false, "重新生成 testdata 快照")

// goldenStreamPath 是快照文件路径。
func goldenStreamPath() string {
	return filepath.Join("testdata", "stream.ndjson")
}

// goldenRow 是快照里的一行：相对基准时刻的偏移、kind 与 payload 值。
type goldenRow struct {
	offset time.Duration
	kind   string
	value  any
}

// goldenRows 覆盖全部 12 个 kind，并额外用两行 tool.result 区分成功/被拒。
func goldenRows() []goldenRow {
	return []goldenRow{
		{0, events.KindRunStart, events.RunStart{Model: "deepseek-v4.1-flash", Provider: "deepseek", Workspace: "/srv/ws"}},
		{1500 * time.Microsecond, events.KindMessage, events.Message{Role: "user", Text: "帮我改一下 README"}},
		{2 * time.Second, events.KindMessageDelta, events.MsgDelta{Role: "assistant", Text: "好的，"}},
		{2100 * time.Millisecond, events.KindReasoning, events.MsgDelta{Role: "assistant", Text: "先看目录"}},
		{3 * time.Second, events.KindToolCall, events.ToolCall{ID: "call-1", Name: "read_file", Arguments: `{"path":"README.md"}`}},
		{3100 * time.Millisecond, events.KindApprovalRequest, events.ApprovalRequest{Tool: "bash", Args: "go test ./...", Reason: "执行测试"}},
		{3200 * time.Millisecond, events.KindApprovalResponse, events.ApprovalResponse{Tool: "bash", Decision: "allow", Reason: "用户批准"}},
		{4 * time.Second, events.KindToolResult, events.ToolResult{ID: "call-1", Name: "read_file", Content: "# Title\n", DurationMS: 12}},
		{4100 * time.Millisecond, events.KindToolResult, events.ToolResult{ID: "call-2", Name: "bash", Content: "denied", IsError: true, Denied: true, DurationMS: 3}},
		{5 * time.Second, events.KindDiff, events.Diff{Path: "README.md", Op: "modify", Added: 3, Removed: 1}},
		{5100 * time.Millisecond, events.KindNotice, nil},
		{6 * time.Second, events.KindMessage, events.Message{Role: "assistant", Text: "改好了：<b>&</b>", ToolCalls: 2}},
		{7 * time.Second, events.KindError, events.Error{Stage: "provider", Message: "upstream 502"}},
		{8 * time.Second, events.KindRunEnd, events.RunEnd{Turns: 2, ToolCalls: 2, PromptTokens: 1200, CompletionTokens: 80}},
	}
}

// buildGoldenStream 用固定时刻与固定 payload 覆盖全部 kind。
//
// 全部走 EmitEnvelope 并显式给出 Time，保证与运行时刻无关（可复现）。
func buildGoldenStream(t *testing.T) []byte {
	t.Helper()

	var buf bytes.Buffer
	em := events.NewEmitter(&buf)
	for i, r := range goldenRows() {
		env := events.Envelope{
			Time: fixedBase.Add(r.offset),
			Kind: r.kind,
		}
		if r.value != nil {
			env.Payload = rawJSON(t, r.value)
		}
		if err := em.EmitEnvelope(env); err != nil {
			t.Fatalf("第 %d 行 EmitEnvelope: %v", i+1, err)
		}
	}
	return buf.Bytes()
}

// 整条流的字节级快照。
func TestGoldenStreamBytes(t *testing.T) {
	got := buildGoldenStream(t)
	path := goldenStreamPath()

	if *updateGolden {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("创建 testdata: %v", err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatalf("写入快照: %v", err)
		}
		t.Logf("已重新生成 %s（%d 字节）", path, len(got))
		return
	}

	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取快照 %s: %v（首次请用 -update 生成）", path, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("线格式与快照不一致（若是有意变更，请 -update 并考虑 Version+1）\n--- want ---\n%s\n--- got ---\n%s", want, got)
	}
}

// 快照本身必须能被回放，且行数/序号自洽（防止手工编辑破坏快照）。
func TestGoldenStreamReplays(t *testing.T) {
	want, err := os.ReadFile(goldenStreamPath())
	if err != nil {
		t.Fatalf("读取快照: %v", err)
	}
	got := readAll(t, want)
	rows := goldenRows()
	if len(got) != len(rows) {
		t.Fatalf("快照解码出 %d 条, want %d", len(got), len(rows))
	}
	seen := make(map[string]bool, len(got))
	for i, env := range got {
		if env.Seq != int64(i+1) {
			t.Fatalf("第 %d 条 seq = %d, want %d", i+1, env.Seq, i+1)
		}
		if env.Kind != rows[i].kind {
			t.Fatalf("第 %d 条 kind = %q, want %q", i+1, env.Kind, rows[i].kind)
		}
		seen[env.Kind] = true
	}
	if len(seen) != 12 {
		t.Fatalf("快照覆盖 %d 个 kind, want 全部 12 个: %v", len(seen), seen)
	}
	// 快照里必须出现"省略 payload"的一行（KindNotice）。
	for _, env := range got {
		if env.Kind == events.KindNotice {
			if len(env.Payload) != 0 {
				t.Fatalf("notice 行带有 payload: %q", env.Payload)
			}
			return
		}
	}
	t.Fatal("快照里没有 notice 行")
}
