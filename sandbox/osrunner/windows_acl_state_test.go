package osrunner

// 本文件在 Linux 上单测 Windows ACL 协调器的纯状态机：路径哈希/命名 mutex、
// 状态文件校验、并发 grant/cleanup、死 grant purge、崩溃残留恢复与 fail-closed。
// 真实 Win32 DACL/令牌行为不可在 Linux 实跑，见 windows_runtime_test.go。

import (
	"encoding/base64"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// fakeACLBackend 用内存模拟目录 ACL 与状态文件；WithACLMutex 用 Go mutex 模拟
// 跨进程命名 mutex 的串行化。
type fakeACLBackend struct {
	mu       sync.Mutex
	original string
	state    *winACLState

	deny          bool
	label         bool
	aces          map[string]bool
	markerAlive   map[string]bool
	restoreCalls  int
	removedStates int
	loadErr       error
	failLabel     bool
}

func newFakeACLBackend() *fakeACLBackend {
	return &fakeACLBackend{
		original:    base64.StdEncoding.EncodeToString([]byte("original-security-descriptor")),
		aces:        make(map[string]bool),
		markerAlive: make(map[string]bool),
	}
}

func (f *fakeACLBackend) cloneState() *winACLState {
	if f.state == nil {
		return nil
	}
	copied := *f.state
	copied.Grants = append([]winACLGrantRecord(nil), f.state.Grants...)
	return &copied
}

func (f *fakeACLBackend) LoadState() (*winACLState, error) {
	if f.loadErr != nil {
		return nil, f.loadErr
	}
	return f.cloneState(), nil
}

func (f *fakeACLBackend) SaveState(state *winACLState) error {
	if err := validateWinACLState(state); err != nil {
		return err
	}
	f.state = state
	return nil
}

func (f *fakeACLBackend) RemoveState() error {
	if f.state != nil {
		f.removedStates++
	}
	f.state = nil
	return nil
}

func (f *fakeACLBackend) WithACLMutex(_ string, fn func() error) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return fn()
}

func (f *fakeACLBackend) CaptureOriginal() (string, error) { return f.original, nil }

func (f *fakeACLBackend) AddWorkspaceDeny() error {
	f.deny = true
	return nil
}

func (f *fakeACLBackend) ApplyLowLabel() (bool, error) {
	if f.failLabel {
		return false, errors.New("label ACL unavailable")
	}
	f.label = true
	return true, nil
}

func (f *fakeACLBackend) AddCapabilityACE(sid string) error {
	f.aces[sid] = true
	return nil
}

func (f *fakeACLBackend) RevokeCapabilityACE(sid string) error {
	delete(f.aces, sid)
	return nil
}

func (f *fakeACLBackend) RestoreOriginal(state *winACLState, restoreLabel bool) error {
	f.restoreCalls++
	if state == nil || state.Descriptor != f.original {
		return errors.New("descriptor mismatch")
	}
	f.deny = false
	if restoreLabel {
		f.label = false
	}
	return nil
}

func (f *fakeACLBackend) MarkerAlive(marker string) bool { return f.markerAlive[marker] }

func (f *fakeACLBackend) CreateMarker(name string) (any, error) {
	f.markerAlive[name] = true
	return name, nil
}

func (f *fakeACLBackend) CloseMarker(handle any) {
	if name, ok := handle.(string); ok {
		delete(f.markerAlive, name)
	}
}

// grantFakeACL grants one capability SID on the fake backend.
func grantFakeACL(t *testing.T, backend *fakeACLBackend, path, sid string) *winACLSession {
	t.Helper()
	session := newWinACLSession(backend, path, sid)
	labelApplied, err := session.Grant()
	if err != nil {
		t.Fatalf("Grant(%s): %v", sid, err)
	}
	if !labelApplied {
		t.Fatalf("Grant(%s) labelApplied = false, want true", sid)
	}
	return session
}

// TestWinACLPathHashAndNames pins the cross-process naming contract.
func TestWinACLPathHashAndNames(t *testing.T) {
	path := `C:\work\project`
	hash := winACLPathHash(path)
	if len(hash) != 64 || hash != winACLPathHash("  "+path+"  ") {
		t.Fatalf("winACLPathHash = %q, want a stable 64-char sha256", hash)
	}
	if other := winACLPathHash(`C:\work\other`); other == hash {
		t.Fatal("different paths share a hash")
	}
	if got, want := winACLStateMutexName(hash), `Local\ZeoKeelSandboxACL-`+hash; got != want {
		t.Fatalf("state mutex = %q, want %q", got, want)
	}
	first := winACLGrantMarkerName(hash, winACLSIDHash("S-1-5-21-1-2-3-4"))
	second := winACLGrantMarkerName(hash, winACLSIDHash("S-1-5-21-5-6-7-8"))
	if first == second || !strings.HasPrefix(first, `Local\ZeoKeelSandboxGrant-`) {
		t.Fatalf("grant markers = %q/%q, want distinct per-SID names", first, second)
	}
	dir := winACLStateDirectory(`C:\Users\me\AppData\Local`, `C:\Users\me\AppData\Local\Temp`)
	if dir != filepath.Join(`C:\Users\me\AppData\Local`, "ZeoKeel", "sandbox-acl") {
		t.Fatalf("state dir = %q, want LOCALAPPDATA preference", dir)
	}
	fallback := winACLStateDirectory("", `C:\Temp`)
	if fallback != filepath.Join(`C:\Temp`, "ZeoKeel", "sandbox-acl") {
		t.Fatalf("state dir fallback = %q, want TEMP", fallback)
	}
	if winACLStateDirectory("", "") != "" {
		t.Fatal("state dir without LOCALAPPDATA/TEMP = non-empty")
	}
	statePath := winACLStateFilePath(`C:\Users\me\AppData\Local`, "", hash)
	if want := filepath.Join(`C:\Users\me\AppData\Local`, "ZeoKeel", "sandbox-acl", hash+".json"); statePath != want {
		t.Fatalf("state path = %q, want %q", statePath, want)
	}
}

// TestWinACLStateSerializationAndValidation pins the fail-closed state format.
func TestWinACLStateSerializationAndValidation(t *testing.T) {
	descriptor := base64.StdEncoding.EncodeToString([]byte("original-descriptor"))
	state := &winACLState{
		Version: winACLStateVersion, Path: `C:\work`, PathHash: "abc", Descriptor: descriptor,
		DenyAdded: true, LabelApplied: true,
		Grants: []winACLGrantRecord{{SID: "S-1-5-21-1", Marker: `Local\ZeoKeelSandboxGrant-x`}},
	}
	encoded, err := marshalWinACLState(state)
	if err != nil {
		t.Fatalf("marshalWinACLState: %v", err)
	}
	parsed, err := parseWinACLState(encoded)
	if err != nil {
		t.Fatalf("parseWinACLState: %v", err)
	}
	if parsed.Descriptor != descriptor || !parsed.DenyAdded || !parsed.LabelApplied || len(parsed.Grants) != 1 {
		t.Fatalf("parsed state = %+v", parsed)
	}
	invalid := []*winACLState{
		nil,
		{Version: 999, PathHash: "abc"},
		{Version: winACLStateVersion, PathHash: "abc", Descriptor: "not-base64!", DenyAdded: true},
		{Version: winACLStateVersion, PathHash: "abc", DenyAdded: true},
		{Version: winACLStateVersion, PathHash: "abc", Descriptor: descriptor,
			Grants: []winACLGrantRecord{{SID: "S-1-5-21-1", Marker: "m"}, {SID: "S-1-5-21-1", Marker: "m2"}}},
		{Version: winACLStateVersion, PathHash: "abc", Descriptor: descriptor,
			Grants: []winACLGrantRecord{{SID: "", Marker: "m"}}},
	}
	for index, candidate := range invalid {
		if err := validateWinACLState(candidate); err == nil {
			t.Fatalf("validateWinACLState(%d) = nil, want an error", index)
		}
	}
	if winACLModeWritesACL(winModeReadOnly) {
		t.Fatal("read-only mode reports that it writes ACL state")
	}
	if !winACLModeWritesACL(winModeWorkspaceWrite) {
		t.Fatal("workspace-write mode does not write ACL state")
	}
}

// TestWinACLConcurrentGrantsKeepEachOther covers the root-cause fix: B's grant
// must not remove/steal A's capability ACE, and A's cleanup must keep the shared
// deny/label while B is still alive.
func TestWinACLConcurrentGrantsKeepEachOther(t *testing.T) {
	backend := newFakeACLBackend()
	path := `C:\work`
	sidA, sidB := "S-1-5-21-111", "S-1-5-21-222"
	sessionA := grantFakeACL(t, backend, path, sidA)
	sessionB := grantFakeACL(t, backend, path, sidB)

	backend.mu.Lock()
	if !backend.deny || !backend.label {
		backend.mu.Unlock()
		t.Fatal("workspace deny/label missing after two grants")
	}
	if !backend.aces[sidA] || !backend.aces[sidB] {
		backend.mu.Unlock()
		t.Fatalf("capability ACEs = %v, want both grants", backend.aces)
	}
	if len(backend.state.Grants) != 2 {
		backend.mu.Unlock()
		t.Fatalf("state grants = %+v, want two active grants", backend.state.Grants)
	}
	backend.mu.Unlock()

	// A exits while B is still working: only A's ACE goes away; the shared
	// deny/label and B's ACE must survive.
	if err := sessionA.Cleanup(); err != nil {
		t.Fatalf("Cleanup(A): %v", err)
	}
	backend.mu.Lock()
	if backend.aces[sidA] || !backend.aces[sidB] {
		backend.mu.Unlock()
		t.Fatalf("ACEs after A exits = %v, want only B", backend.aces)
	}
	if !backend.deny || !backend.label {
		backend.mu.Unlock()
		t.Fatal("A's cleanup restored the shared ACL while B was still alive")
	}
	if backend.state == nil || len(backend.state.Grants) != 1 || backend.state.Grants[0].SID != sidB {
		backend.mu.Unlock()
		t.Fatalf("state after A exits = %+v, want only B's grant", backend.state)
	}
	backend.mu.Unlock()

	// B is the last grant: its cleanup restores the original descriptor.
	if err := sessionB.Cleanup(); err != nil {
		t.Fatalf("Cleanup(B): %v", err)
	}
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if backend.deny || backend.label {
		backend.mu.Unlock()
		t.Fatal("last cleanup did not restore the original deny/label")
	}
	if len(backend.aces) != 0 || backend.state != nil {
		backend.mu.Unlock()
		t.Fatalf("last cleanup left ACEs/state: aces=%v state=%+v", backend.aces, backend.state)
	}
	if backend.restoreCalls == 0 {
		t.Fatal("last cleanup did not restore the original security descriptor")
	}
}

// TestWinACLDeadGrantIsPurged covers crash recovery: a grant whose marker mutex
// disappeared is removed (ACE and record) by the next helper.
func TestWinACLDeadGrantIsPurged(t *testing.T) {
	backend := newFakeACLBackend()
	path := `C:\work`
	sidA, sidB := "S-1-5-21-aaa", "S-1-5-21-bbb"
	sessionA := grantFakeACL(t, backend, path, sidA)
	// Simulate A's process dying: its marker disappears.
	backend.mu.Lock()
	backend.CloseMarker(sessionA.marker)
	backend.mu.Unlock()

	sessionB := grantFakeACL(t, backend, path, sidB)
	backend.mu.Lock()
	if backend.aces[sidA] {
		backend.mu.Unlock()
		t.Fatal("dead grant's capability ACE survived the purge")
	}
	if !backend.aces[sidB] || backend.state == nil || len(backend.state.Grants) != 1 || backend.state.Grants[0].SID != sidB {
		backend.mu.Unlock()
		t.Fatalf("state after purge = %+v (aces %v), want only B", backend.state, backend.aces)
	}
	backend.mu.Unlock()
	if err := sessionB.Cleanup(); err != nil {
		t.Fatalf("Cleanup(B): %v", err)
	}
}

// TestWinACLStaleStateIsRestoredBeforeFreshGrant covers leftover state with no
// active grant: the next helper restores the original descriptor first, then
// starts a clean grant.
func TestWinACLStaleStateIsRestoredBeforeFreshGrant(t *testing.T) {
	backend := newFakeACLBackend()
	backend.state = &winACLState{
		Version: winACLStateVersion, Path: `C:\work`, PathHash: winACLPathHash(`C:\work`),
		Descriptor: backend.original, DenyAdded: true, LabelApplied: true,
	}
	backend.deny, backend.label = true, true
	sid := "S-1-5-21-fresh"
	session := grantFakeACL(t, backend, `C:\work`, sid)

	backend.mu.Lock()
	restoreCalls := backend.restoreCalls
	freshDeny, freshLabel, freshACE := backend.deny, backend.label, backend.aces[sid]
	freshState := backend.cloneState()
	backend.mu.Unlock()
	if restoreCalls != 1 {
		t.Fatalf("restore calls = %d, want the stale state restored once before the fresh grant", restoreCalls)
	}
	if !freshDeny || !freshLabel || !freshACE {
		t.Fatalf("fresh grant state = deny=%t label=%t ace=%t", freshDeny, freshLabel, freshACE)
	}
	if freshState == nil || len(freshState.Grants) != 1 || freshState.Grants[0].SID != sid {
		t.Fatalf("fresh state = %+v, want only the new grant", freshState)
	}
	if err := session.Cleanup(); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
}

// TestWinACLCorruptStateFailsClosed covers corrupted state: Grant aborts before
// touching the directory ACL (fail closed, helper will not spawn).
func TestWinACLCorruptStateFailsClosed(t *testing.T) {
	backend := newFakeACLBackend()
	backend.loadErr = errors.New("corrupt state json")
	session := newWinACLSession(backend, `C:\work`, "S-1-5-21-corrupt")
	if _, err := session.Grant(); err == nil {
		t.Fatal("Grant with corrupt state = nil error, want fail-closed error")
	}
	if backend.deny || backend.label || len(backend.aces) != 0 || backend.state != nil {
		t.Fatalf("corrupt-state grant touched the ACL: deny=%t label=%t aces=%v", backend.deny, backend.label, backend.aces)
	}
	if err := session.Cleanup(); err == nil {
		t.Fatal("Cleanup with corrupt state = nil error, want fail-closed error")
	}
}

// TestWinACLCleanupWithoutStateStillRevokes covers a crash/save failure between
// adding the ACE and persisting the state: cleanup still removes our own ACE.
func TestWinACLCleanupWithoutStateStillRevokes(t *testing.T) {
	backend := newFakeACLBackend()
	sid := "S-1-5-21-nostate"
	backend.aces[sid] = true // leftover ACE, state file never written
	session := newWinACLSession(backend, `C:\work`, sid)
	if err := session.Cleanup(); err != nil {
		t.Fatalf("Cleanup without state: %v", err)
	}
	if backend.aces[sid] {
		t.Fatal("cleanup without state left our capability ACE behind")
	}
}

// TestWinACLLabelFailureDowngrades covers the best-effort Low label: the grant
// still succeeds (restricted token remains), state records LabelApplied=false.
func TestWinACLLabelFailureDowngrades(t *testing.T) {
	backend := newFakeACLBackend()
	backend.failLabel = true
	session := newWinACLSession(backend, `C:\work`, "S-1-5-21-nolabel")
	labelApplied, err := session.Grant()
	if err != nil {
		t.Fatalf("Grant with label failure: %v", err)
	}
	if labelApplied {
		t.Fatal("labelApplied = true despite the label failure")
	}
	backend.mu.Lock()
	if backend.state == nil || backend.state.LabelApplied || !backend.state.DenyAdded {
		backend.mu.Unlock()
		t.Fatalf("state after label failure = %+v, want deny only", backend.state)
	}
	backend.mu.Unlock()
	if err := session.Cleanup(); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
}

// TestWinACLConcurrentGrantsAreSerialized covers two goroutines granting the
// same directory through the shared mutex: both survive and the final cleanup
// restores the directory.
func TestWinACLConcurrentGrantsAreSerialized(t *testing.T) {
	backend := newFakeACLBackend()
	var wg sync.WaitGroup
	sessions := make([]*winACLSession, 0, 4)
	var mu sync.Mutex
	for index := 0; index < 4; index++ {
		sid := "S-1-5-21-concurrent-" + string(rune('a'+index))
		wg.Add(1)
		go func(sid string) {
			defer wg.Done()
			session := newWinACLSession(backend, `C:\work`, sid)
			if _, err := session.Grant(); err != nil {
				t.Errorf("concurrent Grant(%s): %v", sid, err)
				return
			}
			mu.Lock()
			sessions = append(sessions, session)
			mu.Unlock()
		}(sid)
	}
	wg.Wait()
	if len(sessions) != 4 {
		t.Fatalf("successful concurrent grants = %d, want 4", len(sessions))
	}
	backend.mu.Lock()
	if len(backend.state.Grants) != 4 || len(backend.aces) != 4 || !backend.deny || !backend.label {
		backend.mu.Unlock()
		t.Fatalf("state after concurrent grants = %+v (aces %v)", backend.state, backend.aces)
	}
	backend.mu.Unlock()
	for index := len(sessions) - 1; index >= 0; index-- {
		if err := sessions[index].Cleanup(); err != nil {
			t.Fatalf("Cleanup(%d): %v", index, err)
		}
	}
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if backend.state != nil || len(backend.aces) != 0 || backend.deny || backend.label {
		t.Fatalf("final state = %+v (aces %v, deny=%t label=%t), want restored", backend.state, backend.aces, backend.deny, backend.label)
	}
}
