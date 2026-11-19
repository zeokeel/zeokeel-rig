package kit_test

// 第二轮审计（2H）：一次完整 Run 的事件流一致性。
//
// 事件流是个人客户端的 UI 与成本账本的数据源，因此要能回答：
//   - seq 是否连续、kind 是否合法；
//   - run.start / run.end 是否成对出现、run.end 的数字是否与 RunResult 一致；
//   - tool.call / tool.result 是否按 id 配对；
//   - 审批过程（等待人工确认）在事件流里可见吗？

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/events"
	"github.com/zeokeel/zeokeel-rig/kit"
)

// auditKnownKinds 是 events 包定义的全部合法 kind。
var auditKnownKinds = map[string]bool{
	events.KindRunStart:         true,
	events.KindRunEnd:           true,
	events.KindMessage:          true,
	events.KindMessageDelta:     true,
	events.KindReasoning:        true,
	events.KindToolCall:         true,
	events.KindToolResult:       true,
	events.KindApprovalRequest:  true,
	events.KindApprovalResponse: true,
	events.KindDiff:             true,
	events.KindNotice:           true,
	events.KindError:            true,
}

// auditDecodeAll 解析整段事件流。
func auditDecodeAll(t *testing.T, raw []byte) []events.Envelope {
	t.Helper()
	decoder := events.NewDecoder(bytes.NewReader(raw))
	var out []events.Envelope
	for {
		envelope, err := decoder.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("Next: %v（流内容：\n%s）", err, raw)
		}
		out = append(out, envelope)
	}
}

// TestAudit2HEventSequenceIsConsistent：一次带工具调用的完整 Run。
func TestAudit2HEventSequenceIsConsistent(t *testing.T) {
	provider := &scriptProvider{scripts: [][]kit.Chunk{
		toolCallScript(kit.ToolCall{ID: "call_1", Name: "echoer", Arguments: `{"x":1}`}),
		textScript("done"),
	}}
	root := t.TempDir()
	tool := &wiringEchoTool{name: "echoer"}
	var buffer bytes.Buffer
	agent, err := kit.New(kit.Config{
		Model:       "m",
		Provider:    provider,
		Root:        root,
		Tools:       []kit.Tool{tool},
		Approval:    "auto", // 不接 Asker，避免审批阻塞
		EventWriter: &buffer,
	})
	if err != nil {
		t.Fatalf("kit.New: %v", err)
	}
	result, err := agent.Run(context.Background(), kit.RunRequest{
		SessionID: "s",
		Input:     []kit.Message{kit.NewMessage(kit.RoleUser, "go")},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	envelopes := auditDecodeAll(t, buffer.Bytes())
	if len(envelopes) < 6 {
		t.Fatalf("事件数 = %d，太少:\n%s", len(envelopes), buffer.String())
	}
	// 1) seq 从 1 开始且严格连续。
	auditAssertSeqContiguous(t, envelopes, "一次完整 Run")
	// 2) kind 全部合法。
	for _, envelope := range envelopes {
		if !auditKnownKinds[envelope.Kind] {
			t.Fatalf("未知 kind %q", envelope.Kind)
		}
	}
	// 3) run.start 在首、run.end 在尾且唯一。
	if envelopes[0].Kind != events.KindRunStart {
		t.Fatalf("首条事件 = %q, want run.start", envelopes[0].Kind)
	}
	last := envelopes[len(envelopes)-1]
	if last.Kind != events.KindRunEnd {
		t.Fatalf("末条事件 = %q, want run.end", last.Kind)
	}
	var starts, ends int
	for _, envelope := range envelopes {
		switch envelope.Kind {
		case events.KindRunStart:
			starts++
		case events.KindRunEnd:
			ends++
		}
	}
	if starts != 1 || ends != 1 {
		t.Fatalf("run.start=%d run.end=%d", starts, ends)
	}
	// 4) tool.call 与 tool.result 按 id 配对。
	var callIDs, resultIDs []string
	for _, envelope := range envelopes {
		switch envelope.Kind {
		case events.KindToolCall:
			var payload events.ToolCall
			if err := decodePayload(t, envelope, &payload); err != nil {
				t.Fatalf("tool.call payload: %v", err)
			}
			callIDs = append(callIDs, payload.ID)
		case events.KindToolResult:
			var payload events.ToolResult
			if err := decodePayload(t, envelope, &payload); err != nil {
				t.Fatalf("tool.result payload: %v", err)
			}
			resultIDs = append(resultIDs, payload.ID)
		}
	}
	if len(callIDs) != 1 || len(resultIDs) != 1 || callIDs[0] != resultIDs[0] || callIDs[0] != "call_1" {
		t.Fatalf("tool.call=%v tool.result=%v（应一一配对）", callIDs, resultIDs)
	}
	// 5) run.end 的 tokens 与 RunResult 一致。
	var runEnd events.RunEnd
	if err := decodePayload(t, last, &runEnd); err != nil {
		t.Fatalf("run.end payload: %v", err)
	}
	if runEnd.PromptTokens != result.Usage.PromptTokens || runEnd.CompletionTokens != result.Usage.CompletionTokens {
		t.Fatalf("run.end tokens = %d/%d, RunResult = %d/%d",
			runEnd.PromptTokens, runEnd.CompletionTokens, result.Usage.PromptTokens, result.Usage.CompletionTokens)
	}
	// 6) 2H-P5 修复：Turns 是模型轮数，Messages 是消息条数，两者各归各位。
	if runEnd.Turns != result.Iterations {
		t.Fatalf("run.end.Turns = %d, want Iterations = %d", runEnd.Turns, result.Iterations)
	}
	if runEnd.Messages != len(result.Messages) {
		t.Fatalf("run.end.Messages = %d, want len(RunResult.Messages) = %d", runEnd.Messages, len(result.Messages))
	}
	// 7) tool_calls 计数必须与事件流里的 tool.call 条数一致。
	if runEnd.ToolCalls != len(callIDs) {
		t.Fatalf("run.end.ToolCalls = %d, want %d", runEnd.ToolCalls, len(callIDs))
	}
}

// TestAudit2HApprovalIsVisibleInEvents 覆盖 2H-P5 修复：需要人工审批的调用在
// 事件流里产生 approval.request（等待确认）与 approval.response（结论）两条事件，
// UI 才能显示"正在等你点确认"。
func TestAudit2HApprovalIsVisibleInEvents(t *testing.T) {
	provider := &scriptProvider{scripts: [][]kit.Chunk{
		toolCallScript(kit.ToolCall{ID: "call_1", Name: "writer", Arguments: `{"x":1}`}),
		textScript("denied, ok"),
	}}
	// 走个人客户端默认路径：只给 Asker，门禁由 kit 自己装配（也由 kit 注入观察者）。
	var approvals int
	var buffer bytes.Buffer
	agent, err := kit.New(kit.Config{
		Model: "m", Provider: provider, Root: t.TempDir(),
		Tools:    []kit.Tool{&wiringEchoTool{name: "writer"}},
		Approval: kit.ApprovalAsk,
		Asker: askerFunc(func(_ context.Context, _ string, _ []string) (string, error) {
			approvals++
			return "deny", nil
		}),
		EventWriter: &buffer,
	})
	if err != nil {
		t.Fatalf("kit.New: %v", err)
	}
	if _, err := agent.Run(context.Background(), kit.RunRequest{
		SessionID: "s", Input: []kit.Message{kit.NewMessage(kit.RoleUser, "write it")},
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if approvals == 0 {
		t.Fatal("审批回调没有被调用：本用例没有测到目标")
	}
	envelopes := auditDecodeAll(t, buffer.Bytes())
	var requested, responded int
	var decisions []string
	for _, envelope := range envelopes {
		switch envelope.Kind {
		case events.KindApprovalRequest:
			requested++
			var payload events.ApprovalRequest
			if err := decodePayload(t, envelope, &payload); err != nil {
				t.Fatalf("approval.request payload: %v", err)
			}
			if payload.Tool != "writer" {
				t.Fatalf("approval.request.Tool = %q", payload.Tool)
			}
		case events.KindApprovalResponse:
			responded++
			var payload events.ApprovalResponse
			if err := decodePayload(t, envelope, &payload); err != nil {
				t.Fatalf("approval.response payload: %v", err)
			}
			decisions = append(decisions, payload.Decision)
		}
	}
	if requested != 1 || responded != 1 {
		t.Fatalf("approval.request=%d approval.response=%d（应各一条）", requested, responded)
	}
	if len(decisions) != 1 || decisions[0] != string(rig.DecisionDeny) {
		t.Fatalf("approval.response.Decision = %v", decisions)
	}
}

// askerFunc 适配 rig.Asker。
type askerFunc func(ctx context.Context, question string, options []string) (string, error)

func (f askerFunc) Ask(ctx context.Context, question string, options []string) (string, error) {
	return f(ctx, question, options)
}

// TestAudit2HEventErrorIsRecordedAndRunEnded：provider 失败时事件流仍要收尾。
func TestAudit2HEventErrorIsRecordedAndRunEnded(t *testing.T) {
	provider := &scriptProvider{scripts: nil} // 没有脚本 → Stream 直接报错
	var buffer bytes.Buffer
	agent, err := kit.New(kit.Config{
		Model: "m", Provider: provider, Root: t.TempDir(),
		Tools: []kit.Tool{}, Approval: "auto", EventWriter: &buffer,
	})
	if err != nil {
		t.Fatalf("kit.New: %v", err)
	}
	_, runErr := agent.Run(context.Background(), kit.RunRequest{
		SessionID: "s", Input: []kit.Message{kit.NewMessage(kit.RoleUser, "go")},
	})
	if runErr == nil {
		t.Fatal("provider 失败时 Run 必须返回错误")
	}
	envelopes := auditDecodeAll(t, buffer.Bytes())
	var sawError, sawEnd bool
	for _, envelope := range envelopes {
		switch envelope.Kind {
		case events.KindError:
			sawError = true
		case events.KindRunEnd:
			sawEnd = true
			var payload events.RunEnd
			if err := decodePayload(t, envelope, &payload); err != nil {
				t.Fatalf("run.end payload: %v", err)
			}
			if payload.Err == "" {
				t.Fatal("失败的 run.end 里 err 为空（UI 无法区分成功/失败）")
			}
		}
	}
	if !sawError || !sawEnd {
		t.Fatalf("事件流收尾不完整: error=%v run.end=%v\n%s", sawError, sawEnd, buffer.String())
	}
	// seq 仍然连续。
	auditAssertSeqContiguous(t, envelopes, "provider 失败的 Run")
}

// TestAudit2HRunsShareOneContiguousStream：同一个 agent 上的并发 Run 必须共用
// **一条**事件流（seq 全局连续），而不是各开一个 emitter。
//
// 2026-11 钉子复核（docs/PLATFORM.md §10）：原来"seq 连续"只在一个串行 Run 上
// 断言，测的是 emitter 的计数逻辑；这条换到架构面上——"一个 agent 一个事件汇"。
// 写路径收编进存储后，最容易犯的错是"每个会话/每个 Run 一个 sink"，那时每个
// 通道各自从 1 开始，UI 的增量渲染与去重（按 seq）会直接把两条流搅在一起。
// 这里两个会话并发跑，事件必须仍然是一条从 1 开始的连续序列，且两对 run.start/end。
func TestAudit2HRunsShareOneContiguousStream(t *testing.T) {
	buffer := &auditSyncBuffer{}
	agent, err := kit.New(kit.Config{
		Model: "m", Provider: &scriptProvider{scripts: [][]kit.Chunk{
			textScript("first"), textScript("second"),
		}}, Root: t.TempDir(), Approval: "auto", EventWriter: buffer,
	})
	if err != nil {
		t.Fatalf("kit.New: %v", err)
	}
	var wait sync.WaitGroup
	for _, sessionID := range []string{"s1", "s2"} {
		wait.Add(1)
		go func(sessionID string) {
			defer wait.Done()
			if _, err := agent.Run(context.Background(), kit.RunRequest{
				SessionID: sessionID,
				Input:     []kit.Message{kit.NewMessage(kit.RoleUser, "hi")},
			}); err != nil {
				t.Errorf("Run(%s): %v", sessionID, err)
			}
		}(sessionID)
	}
	wait.Wait()
	envelopes := auditDecodeAll(t, buffer.Bytes())
	auditAssertSeqContiguous(t, envelopes, "并发 Run 共用一条事件流")
	var starts, ends int
	for _, envelope := range envelopes {
		switch envelope.Kind {
		case events.KindRunStart:
			starts++
		case events.KindRunEnd:
			ends++
		}
	}
	if starts != 2 || ends != 2 {
		t.Fatalf("run.start=%d run.end=%d，want 各 2（每个 Run 一对）", starts, ends)
	}
}

// auditSyncBuffer 是并发安全的 writer（events.Emitter 只在单次 Emit 内持锁，
// 不保证底层 writer 并发安全）。
type auditSyncBuffer struct {
	mu   sync.Mutex
	data []byte
}

func (b *auditSyncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.data = append(b.data, p...)
	return len(p), nil
}

// Bytes 返回已写入内容的副本。
func (b *auditSyncBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.data...)
}

// auditAssertSeqContiguous 是"事件流 seq 从 1 起严格连续"的**唯一**定义。
//
// 这条不变量是用户可见契约（UI 按 seq 增量渲染与去重），但它今天只作用在 NDJSON
// 导出通道上（`Config.EventWriter`）。写路径收编进存储后必须对"入库通道"再断言
// 一遍同样的形状——那时复用这一个函数即可，别在旁边另写一份判据。
func auditAssertSeqContiguous(t *testing.T, envelopes []events.Envelope, label string) {
	t.Helper()
	for index, envelope := range envelopes {
		if want := int64(index + 1); envelope.Seq != want {
			t.Fatalf("%s：第 %d 条事件 seq = %d, want %d（seq 必须从 1 起严格连续）",
				label, index, envelope.Seq, want)
		}
	}
}

// TestAudit2HConcurrentEmitKeepsSeqContiguous 事件 emitter 在并发写时 seq 必须连续
// （UI 靠它做增量渲染与去重）。
func TestAudit2HConcurrentEmitKeepsSeqContiguous(t *testing.T) {
	var buffer bytes.Buffer
	emitter := events.NewEmitter(&buffer)
	var wg sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < 25; i++ {
				if err := emitter.Emit(events.KindNotice, map[string]any{"worker": worker, "i": i}); err != nil {
					t.Errorf("Emit: %v", err)
					return
				}
			}
		}(worker)
	}
	wg.Wait()
	envelopes := auditDecodeAll(t, buffer.Bytes())
	if len(envelopes) != 200 {
		t.Fatalf("事件数 = %d, want 200", len(envelopes))
	}
	auditAssertSeqContiguous(t, envelopes, "8 个 goroutine 并发 Emit")
}

// decodePayload 把一条事件的 payload 解码到 target。
func decodePayload(t *testing.T, envelope events.Envelope, target any) error {
	t.Helper()
	if len(envelope.Payload) == 0 {
		return errors.New("payload 为空")
	}
	return jsonUnmarshal(envelope.Payload, target)
}

// jsonUnmarshal 是 encoding/json 的薄包装（避免在多个用例里重复 import）。
func jsonUnmarshal(raw []byte, target any) error {
	return json.Unmarshal(raw, target)
}
