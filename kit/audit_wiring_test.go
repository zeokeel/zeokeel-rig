package kit_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/zeokeel/zeokeel-rig/events"
	"github.com/zeokeel/zeokeel-rig/kit"
	"github.com/zeokeel/zeokeel-rig/secrets"
	"github.com/zeokeel/zeokeel-rig/tools/undo"
)

// 本文件从**外部调用者视角**审计 kit 的接线：事件桥、undo 自动快照、
// SessionStoreDir、以及"非法工具集 + Redactor"这类组合路径。

const leadAuditSecret = "sk-auditdeadbeefdeadbeefdeadbeef"

// leadAuditFailingWriter 是一个永远写失败的 writer。
type leadAuditFailingWriter struct{ err error }

func (w leadAuditFailingWriter) Write([]byte) (int, error) { return 0, w.err }

// leadAuditSyncBuffer 是并发安全的缓冲（events.Emitter 只在单次 Emit 内持锁，
// 不保证底层 writer 并发安全）。
type leadAuditSyncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *leadAuditSyncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *leadAuditSyncBuffer) replay(t *testing.T) []events.Envelope {
	t.Helper()
	b.mu.Lock()
	data := append([]byte(nil), b.buf.Bytes()...)
	b.mu.Unlock()
	var envelopes []events.Envelope
	if err := events.Replay(bytes.NewReader(data), func(envelope events.Envelope) error {
		envelopes = append(envelopes, envelope)
		return nil
	}); err != nil {
		t.Fatalf("Replay: %v", err)
	}
	return envelopes
}

// TestAuditKitNilToolElementWithRedactor 断言"nil 工具 + Redactor"仍然返回可读错误
// 而不是 panic。
//
// 现状 bug：redactTools 把 nil 包进 redactingTool，随后 agent.New 调用 Name() 时
// 因为内嵌接口为 nil 而 panic；kit.New 文档承诺"自定义 Tool 集合非法（nil 工具…）
// 时返回校验错误"。
func TestAuditKitNilToolElementWithRedactor(t *testing.T) {
	root := t.TempDir()
	panicked := func() (recovered any) {
		defer func() { recovered = recover() }()
		_, err := kit.New(kit.Config{
			Model:    "m",
			Provider: &scriptProvider{},
			Root:     root,
			Tools:    []kit.Tool{nil},
			Redactor: secrets.New(),
		})
		if err == nil {
			t.Fatal("非法工具集应当返回错误")
		}
		return nil
	}()
	if panicked != nil {
		t.Fatalf("kit.New 因为 nil 工具 panic 了（应返回校验错误）: %v", panicked)
	}
	// 对照：不接 Redactor 时就是干净的校验错误。
	if _, err := kit.New(kit.Config{
		Model: "m", Provider: &scriptProvider{}, Root: root, Tools: []kit.Tool{nil},
	}); err == nil {
		t.Fatal("对照用例也应当报错")
	}
}

// TestAuditEventsEndToEnd 断言 EventWriter 接上之后：NDJSON 可解析、seq 连续、
// 工具结果在事件流里**已经脱敏**（脱敏发生在工具边界，事件桥看到的就是脱敏后文本）。
func TestAuditEventsEndToEnd(t *testing.T) {
	root := t.TempDir()
	sink := &leadAuditSyncBuffer{}
	leaky := &fakeTool{name: "leaky", fn: func(context.Context, kit.ToolCall) (kit.ToolResult, error) {
		return kit.ToolResult{Content: "token=" + leadAuditSecret}, nil
	}}
	provider := &scriptProvider{scripts: [][]kit.Chunk{
		toolCallScript(kit.ToolCall{ID: "c1", Name: "leaky", Arguments: `{}`}),
		textScript("done"),
	}}
	a := mustNew(t, kit.Config{
		Model:       "m",
		Provider:    provider,
		Root:        root,
		Tools:       []kit.Tool{leaky},
		Redactor:    secrets.New(),
		EventWriter: sink,
		Approval:    kit.ApprovalAuto,
	})

	if _, _, err := a.RunText(context.Background(), "s1", "go", nil); err != nil {
		t.Fatalf("RunText: %v", err)
	}
	envelopes := sink.replay(t)
	if len(envelopes) == 0 {
		t.Fatal("没有写出任何事件")
	}
	for index, envelope := range envelopes {
		if envelope.Seq != int64(index+1) {
			t.Fatalf("seq = %d at %d（应当从 1 连续递增）", envelope.Seq, index)
		}
	}
	var sawToolResult bool
	for _, envelope := range envelopes {
		if envelope.Kind != events.KindToolResult {
			continue
		}
		sawToolResult = true
		if strings.Contains(string(envelope.Payload), leadAuditSecret) {
			t.Fatalf("事件流里的工具结果未脱敏: %s", envelope.Payload)
		}
	}
	if !sawToolResult {
		t.Fatal("事件流里没有 tool.result")
	}
	if err := a.EventError(); err != nil {
		t.Fatalf("EventError = %v", err)
	}
}

// TestAuditEventsWithNilEmitCallback 断言 req.Emit 为 nil 时包装仍然安全：事件照写、
// 不 panic（wrap 会把 nil 回调替换成"只写事件流"的函数）。
func TestAuditEventsWithNilEmitCallback(t *testing.T) {
	root := t.TempDir()
	sink := &leadAuditSyncBuffer{}
	a := mustNew(t, kit.Config{
		Model: "m", Provider: &leadAuditLoopProvider{}, Root: root, EventWriter: sink,
	})
	if _, err := a.Run(context.Background(), kit.RunRequest{
		SessionID: "s1",
		Input:     []kit.Message{kit.NewMessage(kit.RoleUser, "hi")},
		// Emit 故意为 nil
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if envelopes := sink.replay(t); len(envelopes) == 0 {
		t.Fatal("Emit 为 nil 时事件流为空")
	}
	if err := a.EventError(); err != nil {
		t.Fatalf("EventError = %v", err)
	}
}

// TestAuditEventsWriteFailureKeepsRunAlive 断言事件写失败不影响 Run，错误可读。
func TestAuditEventsWriteFailureKeepsRunAlive(t *testing.T) {
	root := t.TempDir()
	boom := errors.New("disk full")
	provider := &scriptProvider{scripts: [][]kit.Chunk{
		toolCallScript(kit.ToolCall{ID: "c1", Name: "echo", Arguments: `{}`}),
		textScript("done"),
		textScript("again"),
	}}
	echo := &fakeTool{name: "echo", fn: func(context.Context, kit.ToolCall) (kit.ToolResult, error) {
		return kit.ToolResult{Content: "ok"}, nil
	}}
	a := mustNew(t, kit.Config{
		Model: "m", Provider: provider, Root: root,
		Tools: []kit.Tool{echo}, Approval: kit.ApprovalAuto,
		EventWriter: leadAuditFailingWriter{err: boom},
	})

	if text, _, err := a.RunText(context.Background(), "s1", "one", nil); err != nil || text != "done" {
		t.Fatalf("事件写失败影响了 Run: text=%q err=%v", text, err)
	}
	if err := a.EventError(); !errors.Is(err, boom) {
		t.Fatalf("EventError = %v, want %v", err, boom)
	}
	// 第二次 Run 仍然不受影响（错误是粘性的，但不会打断循环）。
	if text, _, err := a.RunText(context.Background(), "s2", "two", nil); err != nil || text != "again" {
		t.Fatalf("第二次 Run 受影响: text=%q err=%v", text, err)
	}
}

// TestAuditEventsConcurrentRuns 断言同一个 Agent 并发 Run 时事件流不撕裂
// （配合 -race 运行）。
func TestAuditEventsConcurrentRuns(t *testing.T) {
	root := t.TempDir()
	sink := &leadAuditSyncBuffer{}
	echo := &fakeTool{name: "echo", fn: func(context.Context, kit.ToolCall) (kit.ToolResult, error) {
		return kit.ToolResult{Content: "ok"}, nil
	}}
	provider := &leadAuditLoopProvider{}
	a := mustNew(t, kit.Config{
		Model: "m", Provider: provider, Root: root,
		Tools: []kit.Tool{echo}, Approval: kit.ApprovalAuto, EventWriter: sink,
	})

	var wait sync.WaitGroup
	for index := 0; index < 4; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if _, _, err := a.RunText(context.Background(), "s", "hi", nil); err != nil {
				t.Errorf("并发 RunText: %v", err)
			}
		}()
	}
	wait.Wait()
	if err := a.EventError(); err != nil {
		t.Fatalf("EventError = %v", err)
	}
	envelopes := sink.replay(t)
	seen := make(map[int64]bool, len(envelopes))
	for _, envelope := range envelopes {
		if seen[envelope.Seq] {
			t.Fatalf("seq 重复: %d", envelope.Seq)
		}
		seen[envelope.Seq] = true
	}
	t.Logf("并发 4 次 Run 共写出 %d 条事件，seq 唯一", len(envelopes))
}

// leadAuditLoopProvider 每次 Stream 都返回一段文本，供并发用例使用。
type leadAuditLoopProvider struct {
	mu    sync.Mutex
	count int
}

func (p *leadAuditLoopProvider) Name() string { return "audit-loop" }

func (p *leadAuditLoopProvider) Stream(ctx context.Context, req kit.Request) (kit.Stream, error) {
	p.mu.Lock()
	p.count++
	p.mu.Unlock()
	return &chunkStream{chunks: []kit.Chunk{{Delta: "ok"}, {FinishReason: "stop"}}}, nil
}

// TestAuditUndoSnapshotPerRun 断言每次 Run 恰好 Begin 一次快照。
func TestAuditUndoSnapshotPerRun(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.md"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tracker := undo.NewTracker(root, undo.Options{})
	a := mustNew(t, kit.Config{
		Model: "m", Provider: &leadAuditLoopProvider{}, Root: root, UndoTracker: tracker,
	})
	for index := 0; index < 3; index++ {
		if _, _, err := a.RunText(context.Background(), "s", "hi", nil); err != nil {
			t.Fatalf("Run %d: %v", index, err)
		}
	}
	turns := tracker.Turns()
	if len(turns) != 3 || turns[0] != 1 || turns[2] != 3 {
		t.Fatalf("回合号 = %v, want [1 2 3]（每 Run 恰好一次 Begin）", turns)
	}
}

// TestAuditUndoBeginFailureDoesNotBreakRun 断言 Begin 失败只影响 undo，不影响 Run。
func TestAuditUndoBeginFailureDoesNotBreakRun(t *testing.T) {
	root := t.TempDir()
	// 绑定到一个不存在的根：Begin 必然失败。
	tracker := undo.NewTracker(filepath.Join(root, "missing"), undo.Options{})
	sink := &leadAuditSyncBuffer{}
	a := mustNew(t, kit.Config{
		Model: "m", Provider: &leadAuditLoopProvider{}, Root: root,
		UndoTracker: tracker, EventWriter: sink,
	})
	text, _, err := a.RunText(context.Background(), "s", "hi", nil)
	if err != nil {
		t.Fatalf("Begin 失败不应影响 Run: %v", err)
	}
	if text != "ok" {
		t.Fatalf("text = %q", text)
	}
	if turns := tracker.Turns(); len(turns) != 0 {
		t.Fatalf("失败时不应留下快照: %v", turns)
	}
	found := false
	for _, envelope := range sink.replay(t) {
		if envelope.Kind == events.KindNotice && strings.Contains(string(envelope.Payload), "undo snapshot failed") {
			found = true
		}
	}
	if !found {
		t.Fatal("Begin 失败应当在事件流里留下一条 notice")
	}
}

// TestAuditUndoRetentionEvictsOldTurns 记录默认保留上限（10）在频繁 Run 下的后果。
func TestAuditUndoRetentionEvictsOldTurns(t *testing.T) {
	root := t.TempDir()
	tracker := undo.NewTracker(root, undo.Options{})
	a := mustNew(t, kit.Config{
		Model: "m", Provider: &leadAuditLoopProvider{}, Root: root, UndoTracker: tracker,
	})
	for index := 0; index < undo.DefaultRetain+2; index++ {
		if _, _, err := a.RunText(context.Background(), "s", "hi", nil); err != nil {
			t.Fatalf("Run %d: %v", index, err)
		}
	}
	turns := tracker.Turns()
	if len(turns) != undo.DefaultRetain {
		t.Fatalf("保留回合数 = %d, want %d", len(turns), undo.DefaultRetain)
	}
	if turns[0] != 3 {
		t.Fatalf("最早的回合号 = %d, want 3（前两个被挤掉）", turns[0])
	}
	t.Logf("现状：Run %d 次之后只剩回合 %v；每次 Run 都消耗一个快照名额", undo.DefaultRetain+2, turns)
}

// TestAuditUndoConcurrentRuns 断言并发 Run 时回合号不重复（配合 -race）。
func TestAuditUndoConcurrentRuns(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.md"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tracker := undo.NewTracker(root, undo.Options{})
	a := mustNew(t, kit.Config{
		Model: "m", Provider: &leadAuditLoopProvider{}, Root: root, UndoTracker: tracker,
	})
	var wait sync.WaitGroup
	for index := 0; index < 2; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if _, _, err := a.RunText(context.Background(), "s", "hi", nil); err != nil {
				t.Errorf("并发 RunText: %v", err)
			}
		}()
	}
	wait.Wait()
	if turns := tracker.Turns(); len(turns) != 2 || turns[0] == turns[1] {
		t.Fatalf("并发回合号 = %v, want 两个不同回合", turns)
	}
}

// TestAuditSessionStoreConfig 覆盖 SessionStoreDir 的三条契约。
func TestAuditSessionStoreConfig(t *testing.T) {
	t.Run("与 History 互斥", func(t *testing.T) {
		root := t.TempDir()
		_, err := kit.New(kit.Config{
			Model: "m", Provider: &scriptProvider{}, Root: root,
			History: newRecordingHistory(), SessionStoreDir: filepath.Join(root, "store"),
		})
		if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("目录不存在时自动创建", func(t *testing.T) {
		root := t.TempDir()
		dir := filepath.Join(root, "nested", "store")
		a := mustNew(t, kit.Config{
			Model: "m", Provider: &leadAuditLoopProvider{}, Root: root, SessionStoreDir: dir,
		})
		if _, _, err := a.RunText(context.Background(), "session-x", "hi", nil); err != nil {
			t.Fatalf("RunText: %v", err)
		}
		if info, err := os.Stat(dir); err != nil || !info.IsDir() {
			t.Fatalf("store 目录没有创建: %v", err)
		}
	})

	t.Run("路径是文件时错误可读", func(t *testing.T) {
		root := t.TempDir()
		file := filepath.Join(root, "not-a-dir")
		if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := kit.New(kit.Config{
			Model: "m", Provider: &scriptProvider{}, Root: root, SessionStoreDir: file,
		})
		if err == nil {
			t.Fatal("路径是文件时应当报错")
		}
		if !strings.Contains(err.Error(), "kit: session store") {
			t.Fatalf("错误缺少上下文: %v", err)
		}
	})
}

// TestAuditSessionStoreSequentialReuse 断言**受支持**的用法：顺序使用同一目录
// （打开 → 跑完 → 丢弃实例 → 再打开）能读回历史。
//
// kit 不持有 store 的关闭权，因此"重启"场景的正确姿势就是不再持有旧实例并重新
// New 一个；store 的 doc 写明"一个目录由一个实例使用"。
func TestAuditSessionStoreSequentialReuse(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "store")

	firstProvider := &scriptProvider{scripts: [][]kit.Chunk{textScript("ok")}}
	first := mustNew(t, kit.Config{Model: "m", Provider: firstProvider, Root: root, SessionStoreDir: dir})
	if _, _, err := first.RunText(context.Background(), "shared", "first-message", nil); err != nil {
		t.Fatalf("first RunText: %v", err)
	}

	// 不再使用 first（丢引用），重新打开同一目录。
	secondProvider := &scriptProvider{scripts: [][]kit.Chunk{textScript("ok2")}}
	second := mustNew(t, kit.Config{Model: "m", Provider: secondProvider, Root: root, SessionStoreDir: dir})
	if _, _, err := second.RunText(context.Background(), "shared", "second-message", nil); err != nil {
		t.Fatalf("second RunText: %v", err)
	}

	request := secondProvider.request(0)
	if len(request.Messages) != 3 {
		t.Fatalf("重开后请求消息数 = %d, want 3（两条历史 + 新输入）", len(request.Messages))
	}
	if !strings.Contains(request.Messages[0].Text(), "first-message") {
		t.Fatalf("历史未加载: 第一条 = %q", request.Messages[0].Text())
	}
}

// TestAuditSessionStoreTwoInstancesAtOnce 断言**两个实例同时打开同一目录**也能
// 互相看见对方会话（v0.20：session/store 改成自愈式重载）。
//
// 历史：这条用例原来是"已知限制"的 characterization 占位（t.Skip），因为旧实现
// 的内存索引不会感知另一个实例的写入，第二个实例会报 "session not found" 或丢历史。
// 现在 store 在未命中时按节流策略从磁盘重载（并收养只有文件的会话），因此这里
// 断言的是受支持用法：**旧实例不必被回收**，两个 kit 交替使用同一 sessionID 时
// 历史只增不减。
func TestAuditSessionStoreTwoInstancesAtOnce(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "store")

	firstProvider := &scriptProvider{scripts: [][]kit.Chunk{
		textScript("first-answer"),
		textScript("third-answer"),
	}}
	first := mustNew(t, kit.Config{Model: "m", Provider: firstProvider, Root: root, SessionStoreDir: dir})
	if _, _, err := first.RunText(context.Background(), "shared", "first-message", nil); err != nil {
		t.Fatalf("first RunText: %v", err)
	}

	// 第二个实例在第一个实例**仍然存活**时打开同一目录。
	secondProvider := &scriptProvider{scripts: [][]kit.Chunk{textScript("second-answer")}}
	second := mustNew(t, kit.Config{Model: "m", Provider: secondProvider, Root: root, SessionStoreDir: dir})
	if _, _, err := second.RunText(context.Background(), "shared", "second-message", nil); err != nil {
		t.Fatalf("第二个实例 RunText（旧实例仍存活）: %v", err)
	}
	secondRequest := secondProvider.request(0)
	if len(secondRequest.Messages) != 3 { // first-message + first-answer + 本次输入
		t.Fatalf("第二个实例请求消息数 = %d, want 3（必须看见第一个实例写的历史）",
			len(secondRequest.Messages))
	}
	if !strings.Contains(secondRequest.Messages[0].Text(), "first-message") {
		t.Fatalf("第二个实例没看到第一个实例的历史: %q", secondRequest.Messages[0].Text())
	}

	// 反向：第一个实例继续同一会话，必须看见第二个实例刚写的消息。
	if _, _, err := first.RunText(context.Background(), "shared", "third-message", nil); err != nil {
		t.Fatalf("第一个实例 RunText（另一实例已写入后）: %v", err)
	}
	thirdRequest := firstProvider.request(1)
	// 历史 4 条（first-message/first-answer/second-message/second-answer）+ 本次输入。
	if len(thirdRequest.Messages) != 5 {
		t.Fatalf("第一个实例第二次请求消息数 = %d, want 5（两个实例的历史都在）",
			len(thirdRequest.Messages))
	}
	sawSecond := false
	for _, message := range thirdRequest.Messages {
		if strings.Contains(message.Text(), "second-message") {
			sawSecond = true
		}
	}
	if !sawSecond {
		t.Fatal("第一个实例没看到第二个实例写的历史")
	}
}

// TestAuditUndoToolHiddenWithoutTracker 断言没有 UndoTracker 时 `undo` 工具被摘掉：
// 它只会返回"没有可用快照"，而且它是破坏性工具，不该白白暴露给模型。
func TestAuditUndoToolHiddenWithoutTracker(t *testing.T) {
	root := t.TempDir()
	call := kit.ToolCall{ID: "c1", Name: "undo", Arguments: `{}`}

	withoutProvider := &scriptProvider{scripts: [][]kit.Chunk{toolCallScript(call), textScript("done")}}
	without := mustNew(t, kit.Config{
		Model: "m", Provider: withoutProvider, Root: root, Approval: kit.ApprovalAuto,
	})
	if _, _, err := without.RunText(context.Background(), "s1", "go", nil); err != nil {
		t.Fatalf("RunText: %v", err)
	}
	message, ok := findToolMessage(withoutProvider.request(1).Messages, "undo")
	if !ok {
		t.Fatalf("没有 undo 的 tool 消息: %+v", withoutProvider.request(1).Messages)
	}
	if !strings.Contains(message.Text(), "tool not found") {
		t.Fatalf("没有 UndoTracker 时 undo 仍然可用: %q", message.Text())
	}

	withProvider := &scriptProvider{scripts: [][]kit.Chunk{toolCallScript(call), textScript("done")}}
	tracker := undo.NewTracker(root, undo.Options{})
	with := mustNew(t, kit.Config{
		Model: "m", Provider: withProvider, Root: root, Approval: kit.ApprovalAuto, UndoTracker: tracker,
	})
	if _, _, err := with.RunText(context.Background(), "s1", "go", nil); err != nil {
		t.Fatalf("RunText: %v", err)
	}
	message, ok = findToolMessage(withProvider.request(1).Messages, "undo")
	if !ok {
		t.Fatalf("配置了 UndoTracker 却没有 undo 工具: %+v", withProvider.request(1).Messages)
	}
	if strings.Contains(message.Text(), "tool not found") {
		t.Fatalf("配置了 UndoTracker 时 undo 应该存在: %q", message.Text())
	}
	t.Logf("配置 UndoTracker 后调用 undo 的结果: %q", message.Text())
}

// TestAuditZeroValueAgent 记录零值 Agent（未经 New 构造）的行为。
func TestAuditZeroValueAgent(t *testing.T) {
	var zero kit.Agent
	if err := zero.EventError(); err != nil {
		t.Fatalf("零值 Agent.EventError() = %v", err)
	}
	defer func() {
		if recovered := recover(); recovered == nil {
			t.Fatal("零值 Agent.Run 目前会 panic（记录现状）")
		} else {
			t.Logf("现状：零值 Agent.Run panic: %v", recovered)
		}
	}()
	_, _ = zero.Run(context.Background(), kit.RunRequest{Input: []kit.Message{kit.NewMessage(kit.RoleUser, "hi")}})
}
