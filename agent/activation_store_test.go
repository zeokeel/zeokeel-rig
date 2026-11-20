package agent_test

// 激活集持久化（rig.ToolActivationStore）在 agent 层的行为：
//
//  1. 两个 Agent 共用 store 与 SessionID 时，第二个构造后就能看到第一个的激活结果；
//  2. SessionID 为空时完全跳过持久化（Load/Save 都不发生）；
//  3. Save 失败只记日志，不改变激活结果（激活是内存操作，持久化是旁路）。

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
)

// activationStoreSpy 是测试用的 rig.ToolActivationStore：记录调用并保存数据。
type activationStoreSpy struct {
	mu      sync.Mutex
	data    map[string][]string
	loads   int
	saves   int
	saveErr error
}

func newActivationStoreSpy() *activationStoreSpy {
	return &activationStoreSpy{data: make(map[string][]string)}
}

func (s *activationStoreSpy) Load(_ context.Context, sessionID string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loads++
	return append([]string(nil), s.data[sessionID]...), nil
}

func (s *activationStoreSpy) Save(_ context.Context, sessionID string, tools []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saves++
	if s.saveErr != nil {
		return s.saveErr
	}
	s.data[sessionID] = append([]string(nil), tools...)
	return nil
}

func (s *activationStoreSpy) saved(sessionID string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.data[sessionID]...)
}

func (s *activationStoreSpy) callCounts() (loads, saves int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loads, s.saves
}

// activationStoreFixture 构造一个 3 工具、初始只 active read_file 的规格。
func activationStoreFixture(store rig.ToolActivationStore, provider rig.Provider) rig.AgentSpec {
	tools := []rig.Tool{
		&fakeTool{name: "read_file"},
		&fakeTool{name: "write_file"},
		&fakeTool{name: "grep"},
	}
	return rig.AgentSpec{
		Model:              "m",
		Provider:           provider,
		Tools:              tools,
		InitialActiveTools: []string{"read_file"},
		ActivationStore:    store,
	}
}

// TestActivationStoreRoundTripAcrossAgents：共用 store 与 SessionID 的两个 Agent
// 之间，激活结果必须可见（第二个 Agent 构造后 ActiveTools 就含第一个激活的名字）。
func TestActivationStoreRoundTripAcrossAgents(t *testing.T) {
	store := newActivationStoreSpy()
	first := mustNewAgent(t, activationStoreFixture(store, &scriptProvider{}))
	firstActivator := first.(rig.ToolActivator)
	if activated, unknown := firstActivator.ActivateTools("s1", "write_file"); len(activated) != 1 || len(unknown) != 0 {
		t.Fatalf("ActivateTools = (%v, %v)", activated, unknown)
	}
	if got := store.saved("s1"); strings.Join(got, ",") != "read_file,write_file" {
		t.Fatalf("store 里的 s1 = %v, want read_file,write_file", got)
	}

	// 第二个 Agent 用同一个 store / SessionID：构造后 active 集合就包含激活结果。
	provider := &scriptProvider{scripts: [][]rig.Chunk{textScript("ok")}}
	second := mustNewAgent(t, activationStoreFixture(store, provider))
	secondActivator := second.(rig.ToolActivator)
	if got := secondActivator.ActiveTools("s1"); strings.Join(got, ",") != "read_file,write_file" {
		t.Fatalf("第二个 Agent 的 s1 active = %v（没有从 store 恢复）", got)
	}
	// 别的会话不受影响。
	if got := secondActivator.ActiveTools("s2"); strings.Join(got, ",") != "read_file" {
		t.Fatalf("第二个 Agent 的 s2 active = %v（不该继承 s1 的持久化激活）", got)
	}

	// Run 路径也要带上恢复的集合（不能只有 ActiveTools 能看到）。
	if _, err := second.Run(context.Background(), rig.RunRequest{
		SessionID: "s1", Input: []rig.Message{userMessage("go")},
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := toolNames(provider.request(0)); strings.Join(got, ",") != "read_file,write_file" {
		t.Fatalf("Run(s1) 的 tools = %v, want read_file,write_file", got)
	}
}

// TestActivationStoreSkippedWithoutSessionID：SessionID 为空时既不 Load 也不 Save。
func TestActivationStoreSkippedWithoutSessionID(t *testing.T) {
	store := newActivationStoreSpy()
	a := mustNewAgent(t, activationStoreFixture(store, &scriptProvider{}))
	activator := a.(rig.ToolActivator)
	activator.ActivateTools("", "write_file")
	if got := activator.ActiveTools(""); strings.Join(got, ",") != "read_file,write_file" {
		t.Fatalf("默认会话 active = %v", got)
	}
	if loads, saves := store.callCounts(); loads != 0 || saves != 0 {
		t.Fatalf("SessionID 为空时不该碰 store：loads=%d saves=%d", loads, saves)
	}
}

// TestActivationStoreSaveFailureDoesNotBreakActivation：Save 失败只记日志，
// 激活本身照常生效并出现在 ActiveTools 里。
func TestActivationStoreSaveFailureDoesNotBreakActivation(t *testing.T) {
	store := newActivationStoreSpy()
	store.saveErr = errors.New("disk full")
	a := mustNewAgent(t, activationStoreFixture(store, &scriptProvider{}))
	activator := a.(rig.ToolActivator)
	activated, unknown := activator.ActivateTools("s1", "write_file")
	if len(activated) != 1 || activated[0] != "write_file" || len(unknown) != 0 {
		t.Fatalf("Save 失败改变了激活结果：ActivateTools = (%v, %v)", activated, unknown)
	}
	if got := activator.ActiveTools("s1"); strings.Join(got, ",") != "read_file,write_file" {
		t.Fatalf("Save 失败后 active = %v", got)
	}
	if _, saves := store.callCounts(); saves == 0 {
		t.Fatal("Save 没有被调用")
	}
}
