package kit_test

// v2 记忆后端的端到端接线：Run 结束捕获 → 阈值触发 commit → 启发式抽取 →
// 可检索；关闭捕获时只留 session 队列；Shutdown 冲刷 pending。

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/gate"
	"github.com/zeokeel/zeokeel-rig/kit"
	"github.com/zeokeel/zeokeel-rig/memoryvfs"
	"github.com/zeokeel/zeokeel-rig/tools/memory"
)

func TestMemoryV2CaptureCommitsAndExtracts(t *testing.T) {
	stateDir := useStateDir(t)
	root := t.TempDir()
	provider := &scriptProvider{scripts: [][]kit.Chunk{textScript("noted")}}
	a := mustNew(t, kit.Config{
		Model: "m", Provider: provider, Root: root,
		Gate: gate.AllowAll(), MemoryCommitTokens: 1,
	})
	defer func() { _ = a.Shutdown(context.Background()) }()

	if a.MemoryStore() == nil {
		t.Fatalf("MemoryStore 没有接上: %v", a.MemoryStoreError())
	}
	if err := a.MemoryStoreError(); err != nil {
		t.Fatalf("MemoryStoreError: %v", err)
	}
	if _, _, err := a.RunText(context.Background(), "s1", "I prefer dark mode for all editors.", nil); err != nil {
		t.Fatalf("RunText: %v", err)
	}

	store := a.MemoryStore()
	result, err := store.Find(context.Background(), memoryvfs.Query{
		Text: "dark mode", Kinds: []memoryvfs.Kind{memoryvfs.KindMemory}, Limit: 5,
	})
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if len(result.Entries) == 0 {
		t.Fatalf("抽取后检索不到记忆（Store=%s）", stateDir)
	}
	found := false
	for _, entry := range result.Entries {
		if entry.Type == memoryvfs.TypePreferences && strings.Contains(entry.Summary, "dark mode") {
			found = true
		}
	}
	if !found {
		t.Fatalf("没有 preferences 类型的记忆: %+v", result.Entries)
	}

	// 会话归档也必须存在（commit 的原文保留）。
	archives, err := store.List(context.Background(), memoryvfs.URI("mem://sessions/s1"))
	if err != nil || len(archives) == 0 {
		t.Fatalf("会话归档缺失: %v len=%d", err, len(archives))
	}
}

func TestMemoryV2CaptureCanBeDisabled(t *testing.T) {
	useStateDir(t)
	root := t.TempDir()
	provider := &scriptProvider{scripts: [][]kit.Chunk{textScript("ok")}}
	a := mustNew(t, kit.Config{
		Model: "m", Provider: provider, Root: root, Gate: gate.AllowAll(),
		MemoryCapture: kit.Bool(false), MemoryCommitTokens: 1,
	})
	defer func() { _ = a.Shutdown(context.Background()) }()

	if _, _, err := a.RunText(context.Background(), "s1", "I prefer tabs.", nil); err != nil {
		t.Fatalf("RunText: %v", err)
	}
	store := a.MemoryStore()
	if store == nil {
		t.Fatal("MemoryStore 为 nil")
	}
	state, err := store.Pending(context.Background(), "s1")
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if state.Pending != 0 {
		t.Fatalf("关闭捕获后仍有 pending: %+v", state)
	}
	result, findErr := store.Find(context.Background(), memoryvfs.Query{Text: "tabs", Limit: 5})
	if findErr != nil {
		t.Fatalf("Find: %v", findErr)
	}
	if len(result.Entries) != 0 {
		t.Fatalf("关闭捕获后仍写入了记忆: %+v", result.Entries)
	}
}

func TestMemoryV2ToolFindAfterRemember(t *testing.T) {
	useStateDir(t)
	root := t.TempDir()
	a := mustNew(t, kit.Config{
		Model: "m", Provider: &scriptProvider{scripts: [][]kit.Chunk{textScript("ok")}},
		Root: root, Gate: gate.AllowAll(),
	})
	defer func() { _ = a.Shutdown(context.Background()) }()

	tool, ok := a.Tool("memory")
	if !ok {
		t.Fatal("memory 工具没有注册")
	}
	active := a.ActiveTools("s")
	foundActive := false
	for _, name := range active {
		if name == "memory" {
			foundActive = true
		}
	}
	if !foundActive {
		t.Fatalf("memory 不在初始 active 集: %v", active)
	}
	result, err := tool.Invoke(context.Background(), kit.ToolCall{
		Name:      "memory",
		Arguments: `{"action":"remember","type":"preferences","summary":"Editor uses spaces","content":"Use 4 spaces, never tabs."}`,
	})
	if err != nil || result.IsError {
		t.Fatalf("remember: %v %+v", err, result)
	}
	result, err = tool.Invoke(context.Background(), kit.ToolCall{
		Name:      "memory",
		Arguments: `{"action":"find","query":"spaces tabs","kind":"memory"}`,
	})
	if err != nil || result.IsError {
		t.Fatalf("find: %v %+v", err, result)
	}
	if !strings.Contains(strings.ToLower(result.Content), "spaces") {
		t.Fatalf("find 没有命中刚写的记忆: %q", result.Content)
	}
}

func TestMemoryV2ShutdownFlushesPending(t *testing.T) {
	stateDir := useStateDir(t)
	root := t.TempDir()
	provider := &scriptProvider{scripts: [][]kit.Chunk{textScript("ok")}}
	a := mustNew(t, kit.Config{
		Model: "m", Provider: provider, Root: root, Gate: gate.AllowAll(),
		MemoryCommitTokens: 1 << 20, // 不在 Run 内触发
	})
	if _, _, err := a.RunText(context.Background(), "s1", "I prefer dark mode.", nil); err != nil {
		t.Fatalf("RunText: %v", err)
	}
	store := a.MemoryStore()
	state, err := store.Pending(context.Background(), "s1")
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if state.Pending == 0 {
		t.Fatal("阈值高时 Run 结束不该已经提交")
	}
	if err := a.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	// Shutdown 关掉了 kit 自建的后端；重新打开同一目录验证归档已落盘。
	workspaceID, err := memory.WorkspaceID(root)
	if err != nil {
		t.Fatalf("WorkspaceID: %v", err)
	}
	reopened, err := memory.NewFileStore(filepath.Join(stateDir, "rig", "memory", workspaceID, "v2"))
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = reopened.Close() }()
	archives, err := reopened.List(context.Background(), memoryvfs.URI("mem://sessions/s1"))
	if err != nil || len(archives) == 0 {
		t.Fatalf("Shutdown 没有冲刷 pending: err=%v len=%d", err, len(archives))
	}
}

func TestMemoryV2ShutdownIsIdempotent(t *testing.T) {
	useStateDir(t)
	provider := &scriptProvider{scripts: [][]kit.Chunk{textScript("ok")}}
	a := mustNew(t, kit.Config{
		Model: "m", Provider: provider, Root: t.TempDir(), Gate: gate.AllowAll(),
		MemoryCommitTokens: 1,
	})
	if _, _, err := a.RunText(context.Background(), "s1", "I prefer dark mode.", nil); err != nil {
		t.Fatalf("RunText: %v", err)
	}
	if err := a.Shutdown(context.Background()); err != nil {
		t.Fatalf("first Shutdown: %v", err)
	}
	if err := a.Shutdown(context.Background()); err != nil {
		t.Fatalf("second Shutdown 应当幂等（nil），得到: %v", err)
	}
}

func TestMemoryV2ShutdownAcceptsNilAndCanceledContext(t *testing.T) {
	useStateDir(t)
	newAgent := func() *kit.Agent {
		return mustNew(t, kit.Config{
			Model: "m", Provider: &scriptProvider{scripts: [][]kit.Chunk{textScript("ok")}},
			Root: t.TempDir(), Gate: gate.AllowAll(), MemoryCommitTokens: 1,
		})
	}
	first := newAgent()
	if _, _, err := first.RunText(context.Background(), "s-nil", "I prefer dark mode.", nil); err != nil {
		t.Fatalf("RunText: %v", err)
	}
	if err := first.Shutdown(nil); err != nil {
		t.Fatalf("Shutdown(nil) 不应 panic/报错: %v", err)
	}

	second := newAgent()
	if _, _, err := second.RunText(context.Background(), "s-canceled", "I prefer tabs.", nil); err != nil {
		t.Fatalf("RunText: %v", err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := second.Shutdown(canceled); err != nil {
		t.Fatalf("用已取消的 ctx 关机不应跳过记忆收尾: %v", err)
	}
}

func TestMemoryBackendDisabledWhenToolAbsent(t *testing.T) {
	useStateDir(t)
	a := mustNew(t, kit.Config{
		Model: "m", Provider: &scriptProvider{scripts: [][]kit.Chunk{textScript("ok")}},
		Root: t.TempDir(), Gate: gate.AllowAll(), Tools: []rig.Tool{},
	})
	if store := a.MemoryStore(); store != nil {
		t.Fatalf("Config.Tools 不含 memory 时不应开后端，得到 %T", store)
	}
}

func TestMemoryV2CaptureKeepsRepeatedTurns(t *testing.T) {
	useStateDir(t)
	provider := &scriptProvider{scripts: [][]kit.Chunk{textScript("ok"), textScript("ok")}}
	a := mustNew(t, kit.Config{
		Model: "m", Provider: provider, Root: t.TempDir(), Gate: gate.AllowAll(),
		MemoryCommitTokens: 1 << 20,
	})
	defer func() { _ = a.Shutdown(context.Background()) }()
	for index := 0; index < 2; index++ {
		if _, _, err := a.RunText(context.Background(), "repeat", "continue", nil); err != nil {
			t.Fatalf("RunText: %v", err)
		}
	}
	state, err := a.MemoryStore().Pending(context.Background(), "repeat")
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if state.Pending < 4 {
		t.Fatalf("重复轮次被批次去重吞掉: pending=%d, want >=4", state.Pending)
	}
}

func TestCanceledShutdownStillFlushesMemory(t *testing.T) {
	stateDir := useStateDir(t)
	root := t.TempDir()
	a := mustNew(t, kit.Config{
		Model: "m", Provider: &scriptProvider{scripts: [][]kit.Chunk{textScript("ok")}},
		Root: root, Gate: gate.AllowAll(), MemoryCommitTokens: 1 << 20,
	})
	if _, _, err := a.RunText(context.Background(), "s-cancel-flush", "I prefer tabs.", nil); err != nil {
		t.Fatalf("RunText: %v", err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := a.Shutdown(canceled); err != nil {
		t.Fatalf("Shutdown(canceled): %v", err)
	}
	workspaceID, err := memory.WorkspaceID(root)
	if err != nil {
		t.Fatalf("WorkspaceID: %v", err)
	}
	reopened, err := memory.NewFileStore(filepath.Join(stateDir, "rig", "memory", workspaceID, "v2"))
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = reopened.Close() }()
	archives, err := reopened.List(context.Background(), memoryvfs.URI("mem://sessions/s-cancel-flush"))
	if err != nil || len(archives) == 0 {
		t.Fatalf("取消 ctx 的 Shutdown 跳过了 flush: err=%v len=%d", err, len(archives))
	}
}

func TestMemoryBackendDisabledByMinimalPreset(t *testing.T) {
	useStateDir(t)
	a := mustNew(t, kit.Config{
		Model: "m", Provider: &scriptProvider{scripts: [][]kit.Chunk{textScript("ok")}},
		Root: t.TempDir(), Gate: gate.AllowAll(), ToolPreset: "minimal",
	})
	if store := a.MemoryStore(); store != nil {
		t.Fatalf("minimal 预设不含 memory 时不应开后端，得到 %T", store)
	}
}

func TestMemoryRecallInjectsRelevantContext(t *testing.T) {
	useStateDir(t)
	store := memory.NewMemoryStore()
	if _, err := store.Write(context.Background(), memoryvfs.Entry{
		URI: "mem://user/self/memories/preferences/dark-mode", Kind: memoryvfs.KindMemory,
		Type: "preferences", Summary: "user prefers dark mode", Content: "dark mode",
	}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	provider := &scriptProvider{scripts: [][]kit.Chunk{textScript("ok")}}
	a := mustNew(t, kit.Config{
		Model: "m", Provider: provider, Root: t.TempDir(), Gate: gate.AllowAll(),
		MemoryStore: store, MemoryCapture: kit.Bool(false),
	})
	if _, _, err := a.RunText(context.Background(), "recall", "Do I prefer dark mode?", nil); err != nil {
		t.Fatalf("RunText: %v", err)
	}
	req := provider.request(0)
	found := false
	for _, message := range req.Messages {
		if message.Role == rig.RoleSystem && strings.Contains(message.Text(), "user prefers dark mode") {
			found = true
		}
	}
	if !found {
		t.Fatalf("自动召回没有注入相关记忆: %+v", req.Messages)
	}
}

func TestMemoryToolIsCallableFromAgentLoop(t *testing.T) {
	useStateDir(t)
	provider := &scriptProvider{scripts: [][]kit.Chunk{
		toolCallScript(kit.ToolCall{ID: "m1", Name: "memory", Arguments: `{"action":"remember","type":"preferences","summary":"loop memory","content":"called through loop"}`}),
		textScript("done"),
	}}
	a := mustNew(t, kit.Config{
		Model: "m", Provider: provider, Root: t.TempDir(), Gate: gate.AllowAll(),
		MemoryCapture: kit.Bool(false), MemoryRecall: kit.Bool(false),
	})
	defer func() { _ = a.Shutdown(context.Background()) }()
	if _, _, err := a.RunText(context.Background(), "loop", "remember this", nil); err != nil {
		t.Fatalf("RunText: %v", err)
	}
	result, err := a.MemoryStore().Find(context.Background(), memoryvfs.Query{Text: "loop memory", Limit: 5})
	if err != nil || len(result.Entries) == 0 {
		t.Fatalf("模型通过 loop 调用 memory 后没有落盘: %v %+v", err, result.Entries)
	}
}

func TestMemoryProfileInjectedOnFirstSessionRun(t *testing.T) {
	useStateDir(t)
	store := memory.NewMemoryStore()
	if _, err := store.Write(context.Background(), memoryvfs.Entry{
		URI: "mem://user/self/memories/profile/me", Kind: memoryvfs.KindMemory,
		Type: "profile", Summary: "software engineer", Content: "engineer",
	}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	provider := &scriptProvider{scripts: [][]kit.Chunk{textScript("ok")}}
	a := mustNew(t, kit.Config{
		Model: "m", Provider: provider, Root: t.TempDir(), Gate: gate.AllowAll(),
		MemoryStore: store, MemoryCapture: kit.Bool(false),
	})
	if _, _, err := a.RunText(context.Background(), "profile", "hello there", nil); err != nil {
		t.Fatalf("RunText: %v", err)
	}
	req := provider.request(0)
	found := false
	for _, message := range req.Messages {
		if message.Role == rig.RoleSystem && strings.Contains(message.Text(), "software engineer") {
			found = true
		}
	}
	if !found {
		t.Fatalf("首轮没有注入 profile/index: %+v", req.Messages)
	}
}

func TestMemoryReplaysPendingOnFirstRun(t *testing.T) {
	useStateDir(t)
	store := memory.NewMemoryStore()
	ctx := context.Background()
	if err := store.Capture(ctx, "replay", memoryvfs.Message{Role: "user", Content: "pending from crash"}); err != nil {
		t.Fatalf("Capture: %v", err)
	}
	a := mustNew(t, kit.Config{
		Model: "m", Provider: &scriptProvider{scripts: [][]kit.Chunk{textScript("ok")}},
		Root: t.TempDir(), Gate: gate.AllowAll(), MemoryStore: store,
		MemoryCommitTokens: 1 << 20,
	})
	defer func() { _ = a.Shutdown(ctx) }()
	if _, _, err := a.RunText(ctx, "replay", "continue", nil); err != nil {
		t.Fatalf("RunText: %v", err)
	}
	archives, err := store.List(ctx, "mem://sessions/replay")
	if err != nil || len(archives) == 0 {
		t.Fatalf("首轮没有重放 pending: %v %d", err, len(archives))
	}
}

func TestMemorySkillCatalogInjectedOnFirstSessionRun(t *testing.T) {
	useStateDir(t)
	store := memory.NewMemoryStore()
	if _, err := store.Write(context.Background(), memoryvfs.Entry{
		URI: "mem://skills/build-flaky", Kind: memoryvfs.KindSkill,
		Title: "build-flaky", Summary: "diagnose flaky tests", Content: "steps",
	}); err != nil {
		t.Fatalf("Write skill: %v", err)
	}
	provider := &scriptProvider{scripts: [][]kit.Chunk{textScript("ok")}}
	a := mustNew(t, kit.Config{
		Model: "m", Provider: provider, Root: t.TempDir(), Gate: gate.AllowAll(),
		MemoryStore: store, MemoryCapture: kit.Bool(false),
	})
	if _, _, err := a.RunText(context.Background(), "skills", "hello there", nil); err != nil {
		t.Fatalf("RunText: %v", err)
	}
	req := provider.request(0)
	found := false
	for _, message := range req.Messages {
		if message.Role == rig.RoleSystem && strings.Contains(message.Text(), "<available-skills>") && strings.Contains(message.Text(), "build-flaky") {
			found = true
		}
	}
	if !found {
		t.Fatalf("首轮没有注入 skills catalog: %+v", req.Messages)
	}
}
