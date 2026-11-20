package agent_test

// 激活集持久化（rig.ToolActivationStore）在 agent 层的行为：
//
//  1. 两个 Agent 共用 store 与 SessionID 时，第二个构造后就能看到第一个的激活结果；
//  2. SessionID 为空时完全跳过持久化（Load/Save 都不发生）；
//  3. Save 失败只记日志，不改变激活结果（激活是内存操作，持久化是旁路）。

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig"
)

// activationStoreSpy 是测试用的 rig.ToolActivationStore：记录调用并保存数据。
type activationStoreSpy struct {
	mu      sync.Mutex
	data    map[string][]string
	loads   int
	saves   int
	saveErr error
	// loadErr 非 nil 时，前 failLoads 次 Load 返回它（复现"Load 失败"）。
	loadErr   error
	failLoads int
	// saveEntered / saveRelease 只用于并行激活用例：第一次 Save 一进入就关闭
	// saveEntered，等 saveRelease 关闭后才真正落盘（确定性地制造交错）。
	saveEntered     chan struct{}
	saveRelease     chan struct{}
	saveEnteredOnce sync.Once
}

func newActivationStoreSpy() *activationStoreSpy {
	return &activationStoreSpy{data: make(map[string][]string)}
}

func (s *activationStoreSpy) Load(_ context.Context, sessionID string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loads++
	if s.loadErr != nil && s.failLoads > 0 {
		s.failLoads--
		return nil, s.loadErr
	}
	return append([]string(nil), s.data[sessionID]...), nil
}

func (s *activationStoreSpy) Save(_ context.Context, sessionID string, tools []string) error {
	s.mu.Lock()
	s.saves++
	first := s.saves == 1
	entered, release := s.saveEntered, s.saveRelease
	s.mu.Unlock()
	if first && entered != nil {
		s.saveEnteredOnce.Do(func() { close(entered) })
		<-release
	}
	s.mu.Lock()
	defer s.mu.Unlock()
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

// sortedNames 返回排序后的名字串（集合比较用，与激活顺序无关）。
func sortedNames(names []string) string {
	clone := append([]string(nil), names...)
	sort.Strings(clone)
	return strings.Join(clone, ",")
}

// TestActivationStoreSharedStoreKeepsBothActivations：两个 Agent 共用 store 与同一
// SessionID 时，后写者必须先把 store 里的最新集合并回来再 Save——否则它的陈旧
// 内存集会整体覆盖掉另一个 Agent 的激活。
func TestActivationStoreSharedStoreKeepsBothActivations(t *testing.T) {
	store := newActivationStoreSpy()
	first := mustNewAgent(t, activationStoreFixture(store, &scriptProvider{}))
	second := mustNewAgent(t, activationStoreFixture(store, &scriptProvider{}))
	firstActivator := first.(rig.ToolActivator)
	secondActivator := second.(rig.ToolActivator)
	// first 先 Load（此时 store 为空）并记住自己的内存集合。
	if got := strings.Join(firstActivator.ActiveTools("s1"), ","); got != "read_file" {
		t.Fatalf("first 的 s1 active = %v", got)
	}
	secondActivator.ActivateTools("s1", "grep")
	if got := strings.Join(store.saved("s1"), ","); got != "read_file,grep" {
		t.Fatalf("store = %v, want read_file,grep", got)
	}
	firstActivator.ActivateTools("s1", "write_file")
	if got := sortedNames(store.saved("s1")); got != "grep,read_file,write_file" {
		t.Fatalf("后写者覆盖了别的 Agent 的激活：store = %v", store.saved("s1"))
	}
	if got := sortedNames(firstActivator.ActiveTools("s1")); got != "grep,read_file,write_file" {
		t.Fatalf("first 再读 ActiveTools 看不到 second 的激活：%v", got)
	}
}

// TestActivationStoreConcurrentActivatePersistsWholeSet：同一 Agent 上两次并发
// ActivateTools 必须串行落盘；最终 store 与内存 active 集合一致，且不丢名字。
func TestActivationStoreConcurrentActivatePersistsWholeSet(t *testing.T) {
	store := newActivationStoreSpy()
	store.saveEntered = make(chan struct{})
	store.saveRelease = make(chan struct{})
	a := mustNewAgent(t, activationStoreFixture(store, &scriptProvider{}))
	activator := a.(rig.ToolActivator)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		activator.ActivateTools("s1", "write_file")
	}()
	select {
	case <-store.saveEntered:
	case <-time.After(5 * time.Second):
		close(store.saveRelease)
		t.Fatal("第一次 Save 迟迟没有开始")
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		activator.ActivateTools("s1", "grep")
	}()
	close(store.saveRelease)
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("并发激活卡住了（死锁）")
	}
	if got, want := sortedNames(store.saved("s1")), sortedNames(activator.ActiveTools("s1")); got != want {
		t.Fatalf("落盘集合与内存集合不一致：store=%s active=%s", got, want)
	}
	if got := sortedNames(store.saved("s1")); got != "grep,read_file,write_file" {
		t.Fatalf("并发激活丢了名字：store = %v", store.saved("s1"))
	}
}

// TestActivationStoreLoadFailureDoesNotErasePersistedSet：Load 失败期间的激活
// 绝不能 Save（否则用不含持久化记录的全集把旧激活抹掉）；Load 恢复后旧记录与
// 本次激活必须并存。
func TestActivationStoreLoadFailureDoesNotErasePersistedSet(t *testing.T) {
	store := newActivationStoreSpy()
	store.data["s1"] = []string{"read_file", "write_file"}
	store.loadErr = errors.New("store unavailable")
	store.failLoads = 2
	a := mustNewAgent(t, activationStoreFixture(store, &scriptProvider{}))
	activator := a.(rig.ToolActivator)
	activator.ActivateTools("s1", "grep")
	if _, saves := store.callCounts(); saves != 0 {
		t.Fatalf("Load 失败后仍然 Save 了 %d 次", saves)
	}
	if got := sortedNames(store.saved("s1")); got != "read_file,write_file" {
		t.Fatalf("Load 失败期间持久化记录被改动：%v", store.saved("s1"))
	}
	if got := sortedNames(activator.ActiveTools("s1")); got != "grep,read_file" {
		t.Fatalf("Load 失败不该影响内存里的激活：%v", got)
	}
	// 第二次 Load 仍失败（ActiveTools 已在上面重试过一次），第三次成功：合并后落盘。
	activator.ActivateTools("s1", "read_file")
	if got := sortedNames(store.saved("s1")); got != "grep,read_file,write_file" {
		t.Fatalf("Load 恢复后 store = %v, want 三个名字并存", store.saved("s1"))
	}
}

// TestActivationStoreNilInitialActiveSkipsPersistence：InitialActiveTools == nil
// 表示"全部 active"，激活没有意义，因此 Load 与 Save 都完全不碰 store。
func TestActivationStoreNilInitialActiveSkipsPersistence(t *testing.T) {
	store := newActivationStoreSpy()
	a := mustNewAgent(t, rig.AgentSpec{
		Model:    "m",
		Provider: &scriptProvider{},
		Tools: []rig.Tool{
			&fakeTool{name: "read_file"},
			&fakeTool{name: "write_file"},
			&fakeTool{name: "grep"},
		},
		ActivationStore: store,
	})
	a.(rig.ToolActivator).ActivateTools("s1", "write_file")
	if loads, saves := store.callCounts(); loads != 0 || saves != 0 {
		t.Fatalf("InitialActiveTools == nil 时不该碰 store：loads=%d saves=%d", loads, saves)
	}
	if got := store.saved("s1"); len(got) != 0 {
		t.Fatalf("InitialActiveTools == nil 时 store 被写入：%v", got)
	}
}

// TestActivationStoreRestoredBeforeFirstRun：构造后**不碰** ActiveTools 直接 Run，
// 第一次模型请求的 tools 里就必须带上 store 里的激活工具（守护 runTurn 的恢复调用）。
func TestActivationStoreRestoredBeforeFirstRun(t *testing.T) {
	store := newActivationStoreSpy()
	store.data["s1"] = []string{"write_file"}
	provider := &scriptProvider{scripts: [][]rig.Chunk{textScript("ok")}}
	a := mustNewAgent(t, activationStoreFixture(store, provider))
	if _, err := a.Run(context.Background(), rig.RunRequest{
		SessionID: "s1", Input: []rig.Message{userMessage("go")},
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := strings.Join(toolNames(provider.request(0)), ","); got != "read_file,write_file" {
		t.Fatalf("Run(s1) 的 tools = %v, want read_file,write_file（Run 前没恢复）", got)
	}
}
