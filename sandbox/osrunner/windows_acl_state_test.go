package osrunner

// 本文件在 Linux 上单测 Windows ACL 协调器的纯状态机：路径哈希/命名 mutex、
// 状态文件校验、并发 grant/cleanup、死 grant purge、崩溃残留、缓存语义与
// 失败回滚（B1/B2 的不变量）。真实 Win32 DACL/令牌行为不可在 Linux 实跑，见
// windows_runtime_test.go。

import (
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// fakeACLBackend 用内存模拟一个目录的 DACL/标签与状态文件；WithACLMutex 用 Go
// mutex 模拟跨进程命名 mutex 的串行化。Object 委托给 fakeProtectObjectSet，
// 因此保护子树遍历也走同一套 fake。
type fakeACLBackend struct {
	mu       sync.Mutex
	markerMu sync.Mutex
	original string
	state    *winACLState

	// 目录"真实"的 ACL 形状。
	deny  bool
	label bool
	allow map[string]bool
	// markers 是仍存活的 grant marker。
	markers map[string]bool

	objects *fakeProtectObjectSet

	// 计数器与注入的失败。
	captures         int
	shapeApplies     int
	restoreShapes    int
	restoreOriginals int
	removes          int
	loadErr          error
	failLabel        bool
	failShape        bool
	// saveCalls 是 SaveState 的调用次数；failSaveAt 非 0 时第 N 次调用失败
	// （1 起算，用来分别覆盖"改 ACL 之前落盘失败"与"改完之后落盘失败"）。
	saveCalls  int
	failSaveAt int
}

func newFakeACLBackend() *fakeACLBackend {
	return &fakeACLBackend{
		original: base64.StdEncoding.EncodeToString([]byte("original-security-descriptor")),
		allow:    make(map[string]bool),
		markers:  make(map[string]bool),
		objects:  newFakeProtectObjectSet(),
	}
}

func (f *fakeACLBackend) cloneState() *winACLState {
	if f.state == nil {
		return nil
	}
	copied := *f.state
	copied.Grants = append([]winACLGrantRecord(nil), f.state.Grants...)
	copied.Protected = append([]string(nil), f.state.Protected...)
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
	f.saveCalls++
	if f.failSaveAt != 0 && f.saveCalls == f.failSaveAt {
		return errors.New("simulated state write failure")
	}
	f.state = state
	return nil
}

func (f *fakeACLBackend) RemoveState() error {
	if f.state != nil {
		f.removes++
	}
	f.state = nil
	return nil
}

func (f *fakeACLBackend) WithACLMutex(_ string, fn func() error) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return fn()
}

func (f *fakeACLBackend) CaptureOriginal() (string, error) {
	f.captures++
	return f.original, nil
}

func (f *fakeACLBackend) ACLShape(capabilitySID string) (winACLShape, error) {
	return winACLShape{
		CapabilityAllow: f.allow[capabilitySID],
		WorkspaceDeny:   f.deny,
		LowLabel:        f.label,
	}, nil
}

func (f *fakeACLBackend) ApplyShape(request winShapeRequest) (winACLShape, error) {
	if f.failShape {
		return winACLShape{}, errors.New("simulated SetNamedSecurityInfoW failure")
	}
	f.shapeApplies++
	f.allow[request.CapabilitySID] = true
	if request.WorkspaceDeny {
		f.deny = true
	}
	shape := winACLShape{CapabilityAllow: true, WorkspaceDeny: request.WorkspaceDeny && f.deny}
	if request.LowLabel && !f.failLabel {
		f.label = true
		shape.LowLabel = true
	}
	return shape, nil
}

func (f *fakeACLBackend) RestoreShape(state *winACLState, capabilitySID string, restoreLabel bool) error {
	f.restoreShapes++
	if state == nil || state.Descriptor != f.original {
		return errors.New("descriptor mismatch")
	}
	f.deny = false
	if restoreLabel {
		f.label = false
	}
	// 缓存资产保留：capability 允许 ACE 不撤。
	f.allow[capabilitySID] = true
	return nil
}

func (f *fakeACLBackend) RestoreOriginal(state *winACLState, restoreLabel bool) error {
	f.restoreOriginals++
	if state == nil || state.Descriptor != f.original {
		return errors.New("descriptor mismatch")
	}
	f.deny = false
	if restoreLabel {
		f.label = false
	}
	f.allow = make(map[string]bool)
	return nil
}

func (f *fakeACLBackend) RevokeCapabilityACE(capabilitySID string) error {
	delete(f.allow, capabilitySID)
	return nil
}

func (f *fakeACLBackend) Object(path string) winProtectObject {
	return f.objects.Object(path)
}

func (f *fakeACLBackend) MarkerAlive(marker string) bool {
	f.markerMu.Lock()
	defer f.markerMu.Unlock()
	return f.markers[marker]
}

func (f *fakeACLBackend) CreateMarker(name string) (any, error) {
	f.markerMu.Lock()
	defer f.markerMu.Unlock()
	f.markers[name] = true
	return name, nil
}

func (f *fakeACLBackend) CloseMarker(handle any) {
	f.markerMu.Lock()
	defer f.markerMu.Unlock()
	if name, ok := handle.(string); ok {
		delete(f.markers, name)
	}
}

// grantWorkspaceACL grants one workspace session（缓存 capability ACE）。
func grantWorkspaceACL(t *testing.T, backend *fakeACLBackend, path, sid string, options winACLSessionOptions) *winACLSession {
	t.Helper()
	options.CacheCapability = true
	session := newWinACLSession(backend, path, sid, options)
	labelApplied, err := session.Grant()
	if err != nil {
		t.Fatalf("Grant(%s): %v", sid, err)
	}
	if !labelApplied {
		t.Fatalf("Grant(%s) labelApplied = false, want true", sid)
	}
	return session
}

// grantTempACL grants one ephemeral private-temp session（不缓存 capability ACE）。
func grantTempACL(t *testing.T, backend *fakeACLBackend, path, sid string) *winACLSession {
	t.Helper()
	session := newWinACLSession(backend, path, sid, winACLSessionOptions{
		WorkspaceDeny:   true,
		LowLabel:        true,
		CacheCapability: false,
	})
	labelApplied, err := session.Grant()
	if err != nil {
		t.Fatalf("Grant(%s): %v", sid, err)
	}
	if !labelApplied {
		t.Fatalf("Grant(%s) labelApplied = false, want true", sid)
	}
	return session
}

// workspaceOptions 是 workspace-write 下 workspace 会话的标准选项。
func workspaceOptions(protected ...string) winACLSessionOptions {
	return winACLSessionOptions{
		Protected:       protected,
		WorkspaceDeny:   true,
		LowLabel:        true,
		CacheCapability: true,
	}
}

// TestWinACLPathHashAndNames pins the cross-process naming contract.
func TestWinACLPathHashAndNames(t *testing.T) {
	path := `C:\work\project`
	hash := winACLPathHash(path)
	if len(hash) != 64 || hash != winACLPathHash("  "+path+"  ") {
		t.Fatalf("winACLPathHash = %q, want a stable 64-char sha256", hash)
	}
	// Windows 路径大小写不敏感：同一目录的两种写法必须共享状态文件与 mutex，
	// 否则并发协调会失效。
	if other := winACLPathHash(strings.ToUpper(path)); other != hash {
		t.Fatalf("winACLPathHash(case variant) = %q, want %q", other, hash)
	}
	if other := winACLPathHash(`C:\work\other`); other == hash {
		t.Fatal("different paths share a hash")
	}
	if got, want := winACLStateMutexName(hash), `Local\ZeoKeelSandboxACL-`+hash; got != want {
		t.Fatalf("state mutex = %q, want %q", got, want)
	}
	sidHash := winACLSIDHash("S-1-5-21-1-2-3-4")
	first := winACLGrantMarkerName(hash, sidHash, "aaaaaaaaaaaaaaaa")
	second := winACLGrantMarkerName(hash, sidHash, "bbbbbbbbbbbbbbbb")
	if first == second || !strings.HasPrefix(first, `Local\ZeoKeelSandboxGrant-`) {
		t.Fatalf("grant markers = %q/%q, want distinct per-grant names", first, second)
	}
	// 每次 grant 的 nonce 必须不同（并发 helper 共享同一个确定性 SID，身份靠 marker）。
	nonceOne, err := winACLGrantNonce()
	if err != nil {
		t.Fatalf("winACLGrantNonce: %v", err)
	}
	nonceTwo, err := winACLGrantNonce()
	if err != nil {
		t.Fatalf("winACLGrantNonce: %v", err)
	}
	if len(nonceOne) != 16 || nonceOne == nonceTwo {
		t.Fatalf("grant nonces = %q/%q, want distinct 16-char hex", nonceOne, nonceTwo)
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
	// 校验要求"绝对路径"：Windows 上必须带盘符（`\work\.git` 没有卷，不算绝对）。
	protected := filepath.FromSlash("/work/.git")
	if runtime.GOOS == "windows" {
		protected = `C:\work\.git`
	}
	state := &winACLState{
		Version: winACLStateVersion, Path: `C:\work`, PathHash: "abc", Descriptor: descriptor,
		DenyAdded: true, LabelApplied: true,
		Protected: []string{protected},
		Grants:    []winACLGrantRecord{{SID: "S-1-5-21-1", Marker: `Local\ZeoKeelSandboxGrant-x`}},
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
	if len(parsed.Protected) != 1 || parsed.Protected[0] != protected {
		t.Fatalf("parsed protected = %v, want [%q]", parsed.Protected, protected)
	}
	invalid := []*winACLState{
		nil,
		{Version: 999, PathHash: "abc"},
		{Version: winACLStateVersion, PathHash: "abc", Descriptor: "not-base64!", DenyAdded: true},
		{Version: winACLStateVersion, PathHash: "abc", DenyAdded: true},
		{Version: winACLStateVersion, PathHash: "abc", Descriptor: descriptor,
			Grants: []winACLGrantRecord{{SID: "S-1-5-21-1", Marker: "m"}, {SID: "S-1-5-21-2", Marker: "m"}}},
		{Version: winACLStateVersion, PathHash: "abc", Descriptor: descriptor,
			Grants: []winACLGrantRecord{{SID: "", Marker: "m"}}},
		// protect 账本必须是非空绝对路径，且不能重复。
		{Version: winACLStateVersion, PathHash: "abc", Descriptor: descriptor, Protected: []string{""}},
		{Version: winACLStateVersion, PathHash: "abc", Descriptor: descriptor, Protected: []string{"relative/path"}},
		{Version: winACLStateVersion, PathHash: "abc", Descriptor: descriptor, Protected: []string{protected, protected}},
		// 有已应用改动却缺原始描述符。
		{Version: winACLStateVersion, PathHash: "abc", Protected: []string{protected}},
	}
	for index, candidate := range invalid {
		if err := validateWinACLState(candidate); err == nil {
			t.Fatalf("validateWinACLState(%d) = nil, want an error", index)
		}
	}
	// v1（v0.19）状态仍然可读：迁移路径依赖它，不能直接判为损坏。
	legacy := &winACLState{Version: winACLStateVersionLegacy, PathHash: "abc", Descriptor: descriptor, DenyAdded: true}
	if err := validateWinACLState(legacy); err != nil {
		t.Fatalf("validateWinACLState(v1): %v", err)
	}
	if !legacy.legacy() {
		t.Fatal("v1 state does not report legacy()")
	}
	if state.legacy() {
		t.Fatal("v2 state reports legacy()")
	}
	if winACLModeWritesACL(winModeReadOnly) {
		t.Fatal("read-only mode reports that it writes ACL state")
	}
	if !winACLModeWritesACL(winModeWorkspaceWrite) {
		t.Fatal("workspace-write mode does not write ACL state")
	}
}

// TestWinACLConcurrentGrantsKeepEachOther covers the concurrency invariant: B's
// grant must not remove/steal A's capability ACE, and A's cleanup must keep the
// shared per-run parts while B is still alive.
func TestWinACLConcurrentGrantsKeepEachOther(t *testing.T) {
	backend := newFakeACLBackend()
	path := `C:\work`
	sidA, sidB := "S-1-5-21-111", "S-1-5-21-222"
	sessionA := grantWorkspaceACL(t, backend, path, sidA, workspaceOptions())
	sessionB := grantWorkspaceACL(t, backend, path, sidB, workspaceOptions())

	backend.mu.Lock()
	if !backend.deny || !backend.label {
		backend.mu.Unlock()
		t.Fatal("workspace deny/label missing after two grants")
	}
	if !backend.allow[sidA] || !backend.allow[sidB] {
		backend.mu.Unlock()
		t.Fatalf("capability ACEs = %v, want both grants", backend.allow)
	}
	if len(backend.state.Grants) != 2 {
		backend.mu.Unlock()
		t.Fatalf("state grants = %+v, want two active grants", backend.state.Grants)
	}
	backend.mu.Unlock()

	// A exits while B is still working: only A's grant record goes away; the
	// shared deny/label and B's ACE must survive.
	if err := sessionA.Cleanup(); err != nil {
		t.Fatalf("Cleanup(A): %v", err)
	}
	backend.mu.Lock()
	if !backend.allow[sidB] || !backend.deny || !backend.label {
		backend.mu.Unlock()
		t.Fatal("A's cleanup restored the shared ACL while B was still alive")
	}
	if backend.restoreShapes != 0 || backend.restoreOriginals != 0 {
		backend.mu.Unlock()
		t.Fatalf("restores (%d/%d) while another grant was alive", backend.restoreShapes, backend.restoreOriginals)
	}
	if backend.state == nil || len(backend.state.Grants) != 1 || backend.state.Grants[0].SID != sidB {
		backend.mu.Unlock()
		t.Fatalf("state after A exits = %+v, want only B's grant", backend.state)
	}
	backend.mu.Unlock()

	// B is the last grant: cleanup restores the per-run parts but keeps the
	// cached capability ACEs and the state file (cache semantics).
	if err := sessionB.Cleanup(); err != nil {
		t.Fatalf("Cleanup(B): %v", err)
	}
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if backend.deny || backend.label {
		t.Fatalf("last cleanup did not restore the per-run deny/label (deny=%t label=%t)", backend.deny, backend.label)
	}
	if !backend.allow[sidB] {
		t.Fatal("cached capability ACE was revoked by the last cleanup")
	}
	if backend.restoreShapes != 1 || backend.restoreOriginals != 0 {
		t.Fatalf("restores = %d/%d, want exactly one per-run restore", backend.restoreShapes, backend.restoreOriginals)
	}
	if backend.state == nil || len(backend.state.Grants) != 0 || backend.state.DenyAdded || backend.state.LabelApplied {
		t.Fatalf("state after last cleanup = %+v, want a grant-free cache record", backend.state)
	}
	if backend.state.Descriptor == "" {
		t.Fatal("state kept no recovery descriptor")
	}
}

// TestWinACLSecondRunReusesCachedShape covers the B2 fix: the second run on the
// same workspace must not re-apply the ACL (deterministic SID + exact-ACE check).
func TestWinACLSecondRunReusesCachedShape(t *testing.T) {
	backend := newFakeACLBackend()
	path := `C:\work`
	sid := "S-1-5-21-cached"
	first := grantWorkspaceACL(t, backend, path, sid, workspaceOptions())
	if backend.shapeApplies != 1 {
		t.Fatalf("first grant applied the shape %d times, want 1", backend.shapeApplies)
	}
	if err := first.Cleanup(); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	second := grantWorkspaceACL(t, backend, path, sid, workspaceOptions())
	// 形状还在（capability ACE 缓存 + per-run 部分已恢复）：per-run 部分必须
	// 重新建立（1 次），但绝不能因为 ACE 重复而反复重写整棵树。
	if backend.shapeApplies != 2 {
		t.Fatalf("shape applies = %d, want 2 (one per run, never more)", backend.shapeApplies)
	}
	if !backend.deny || !backend.label {
		t.Fatalf("second run did not restore per-run parts (deny=%t label=%t)", backend.deny, backend.label)
	}
	// 第二次运行刷新了基线快照（首次运行以来第三方对 DACL 的改动会进入基线）。
	if backend.captures != 2 {
		t.Fatalf("captures = %d, want 2 (one per run)", backend.captures)
	}
	if err := second.Cleanup(); err != nil {
		t.Fatalf("Cleanup(second): %v", err)
	}
}

// TestWinACLGrantSkipsApplyWhenShapePresent covers the idempotent fast path used
// by concurrent helpers: if the directory already carries our shape, no write
// happens at all.
func TestWinACLGrantSkipsApplyWhenShapePresent(t *testing.T) {
	backend := newFakeACLBackend()
	sid := "S-1-5-21-present"
	backend.allow[sid] = true
	backend.deny, backend.label = true, true
	session := grantWorkspaceACL(t, backend, `C:\work`, sid, workspaceOptions())
	if backend.shapeApplies != 0 {
		t.Fatalf("shape applies = %d, want 0 (shape already present)", backend.shapeApplies)
	}
	backend.mu.Lock()
	if !backend.state.DenyAdded || !backend.state.LabelApplied {
		backend.mu.Unlock()
		t.Fatalf("state = %+v, want the adopted per-run parts recorded", backend.state)
	}
	backend.mu.Unlock()
	if err := session.Cleanup(); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if backend.deny || backend.label {
		t.Fatal("cleanup did not restore the adopted per-run parts")
	}
}

// TestWinACLDeadGrantIsPurged covers crash recovery: a grant whose marker mutex
// disappeared is removed (record and, when the SID is not the shared cache, ACE).
func TestWinACLDeadGrantIsPurged(t *testing.T) {
	backend := newFakeACLBackend()
	path := `C:\work`
	sidB := "S-1-5-21-bbb"
	sessionB := grantWorkspaceACL(t, backend, path, sidB, workspaceOptions())

	// 另一条死记录（不同的 SID，例如旧版本遗留的随机能力 SID）：它的 ACE 必须
	// 被撤掉，记录也必须从状态里消失。
	deadSID := "S-1-5-21-dead"
	backend.allow[deadSID] = true
	backend.state.Grants = append(backend.state.Grants, winACLGrantRecord{
		SID:    deadSID,
		Marker: winACLGrantMarkerName(backend.state.PathHash, winACLSIDHash(deadSID), "deadbeefdeadbeef"),
	})
	// 共享 SID 的死记录（同一工作区的另一个 helper 崩了）：记录要清，但缓存的
	// capability ACE 绝不能撤——否则活着的 helper 会立刻失去写授权。
	backend.state.Grants = append([]winACLGrantRecord{{
		SID:    sidB,
		Marker: `Local\ZeoKeelSandboxGrant-dead-shared`,
	}}, backend.state.Grants...)

	sessionC := grantWorkspaceACL(t, backend, path, sidB, workspaceOptions())
	backend.mu.Lock()
	if backend.allow[deadSID] {
		backend.mu.Unlock()
		t.Fatal("dead grant's capability ACE survived the purge")
	}
	if !backend.allow[sidB] {
		backend.mu.Unlock()
		t.Fatal("purge revoked the live/shared capability ACE")
	}
	// 两条活着的记录都属于 sidB：marker 每次 grant 唯一，因此并发的同 SID helper
	// 各有一条记录（这正是"先退出者不会误判自己是最后一个"的依据）。
	if backend.state == nil || len(backend.state.Grants) != 2 {
		backend.mu.Unlock()
		t.Fatalf("state after purge = %+v (allow %v), want the two live grants", backend.state, backend.allow)
	}
	for _, grant := range backend.state.Grants {
		if grant.SID != sidB {
			backend.mu.Unlock()
			t.Fatalf("dead grant record survived: %+v", grant)
		}
	}
	backend.mu.Unlock()
	if err := sessionC.Cleanup(); err != nil {
		t.Fatalf("Cleanup(C): %v", err)
	}
	if err := sessionB.Cleanup(); err != nil {
		t.Fatalf("Cleanup(B): %v", err)
	}
}

// TestWinACLStalePerRunResidueIsAdopted covers a helper that was killed after
// applying the per-run parts: the next grant adopts them (no extra writes, no
// baseline refresh) and its cleanup finally restores them.
func TestWinACLStalePerRunResidueIsAdopted(t *testing.T) {
	backend := newFakeACLBackend()
	backend.state = &winACLState{
		Version: winACLStateVersion, Path: `C:\work`, PathHash: winACLPathHash(`C:\work`),
		Descriptor: backend.original, DenyAdded: true, LabelApplied: true,
	}
	backend.deny, backend.label = true, true
	sid := "S-1-5-21-fresh"
	session := grantWorkspaceACL(t, backend, `C:\work`, sid, workspaceOptions())

	if backend.shapeApplies != 1 {
		// 唯一需要写的是 capability ACE（残留里没有它）。
		t.Fatalf("shape applies = %d, want 1 (capability ACE only)", backend.shapeApplies)
	}
	if backend.captures != 0 {
		t.Fatalf("captures = %d, want 0: 有残留时不能刷新基线（会把 Everyone 拒绝/Low 标签写进基线）", backend.captures)
	}
	if err := session.Cleanup(); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if backend.deny || backend.label {
		t.Fatal("cleanup did not restore the adopted residue")
	}
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if backend.state == nil || backend.state.DenyAdded || backend.state.LabelApplied {
		t.Fatalf("state after cleanup = %+v, want clean per-run flags", backend.state)
	}
}

// TestWinACLShapeApplyFailureRollsBack is the B1 regression: when the (single)
// shape write fails, the grant must roll back completely — the fresh state file
// is removed again, and no deny/label residue is left on the real directory.
func TestWinACLShapeApplyFailureRollsBack(t *testing.T) {
	backend := newFakeACLBackend()
	backend.failShape = true
	protected := t.TempDir()
	backend.objects.present[winPathKey(protected)] = map[string]bool{}
	session := newWinACLSession(backend, `C:\work`, "S-1-5-21-fail", workspaceOptions(protected))
	if _, err := session.Grant(); err == nil {
		t.Fatal("Grant with a failing shape write = nil error, want fail closed")
	}
	if backend.deny || backend.label {
		t.Fatalf("failed grant left per-run residue (deny=%t label=%t)", backend.deny, backend.label)
	}
	if backend.restoreOriginals != 1 {
		t.Fatalf("restore originals = %d, want 1 (rollback)", backend.restoreOriginals)
	}
	if backend.state != nil {
		t.Fatalf("state = %+v, want removed after rollback", backend.state)
	}
	if backend.allow["S-1-5-21-fail"] {
		t.Fatal("rollback left the capability ACE behind")
	}
	// 回滚后 helper 的 Cleanup 不能报错（状态已经没了）。
	if err := session.Cleanup(); err != nil {
		t.Fatalf("Cleanup after rollback: %v", err)
	}
}

// TestWinACLSaveFailureBeforeApply WritesNothing covers the failure of the very
// first state write (the B1 anchor): nothing has been applied yet, so the grant
// must fail closed without touching the ACL and without a needless restore.
func TestWinACLSaveFailureBeforeApplyWritesNothing(t *testing.T) {
	backend := newFakeACLBackend()
	backend.failSaveAt = 1
	session := newWinACLSession(backend, `C:\work`, "S-1-5-21-nosave", workspaceOptions())
	if _, err := session.Grant(); err == nil {
		t.Fatal("Grant with a failing state write = nil error, want fail closed")
	}
	if backend.deny || backend.label || backend.allow["S-1-5-21-nosave"] || backend.shapeApplies != 0 {
		t.Fatalf("failed grant touched the ACL: deny=%t label=%t allow=%v applies=%d",
			backend.deny, backend.label, backend.allow, backend.shapeApplies)
	}
	if backend.state != nil {
		t.Fatalf("state = %+v, want none", backend.state)
	}
	if err := session.Cleanup(); err != nil {
		t.Fatalf("Cleanup after the failed grant: %v", err)
	}
}

// TestWinACLSaveFailureAfterApplyRollsBack 是 B1 的另一半：形状已经改到真实目录
// 上、但状态落盘失败时必须回滚（旧实现正是在这里留下 Everyone 拒绝与 Low 标签）。
func TestWinACLSaveFailureAfterApplyRollsBack(t *testing.T) {
	backend := newFakeACLBackend()
	backend.failSaveAt = 2 // 第 1 次是 B1 锚点，第 2 次是记录 grant
	protected := t.TempDir()
	session := newWinACLSession(backend, `C:\work`, "S-1-5-21-saveafter", workspaceOptions(protected))
	if _, err := session.Grant(); err == nil {
		t.Fatal("Grant with a failing grant-record write = nil error, want fail closed")
	}
	if backend.deny || backend.label || backend.allow["S-1-5-21-saveafter"] {
		t.Fatalf("failed grant left shape: deny=%t label=%t allow=%v", backend.deny, backend.label, backend.allow)
	}
	if backend.objects.has(protected, "S-1-5-21-saveafter") {
		t.Fatal("failed grant left a protect deny ACE behind")
	}
	if backend.restoreOriginals != 1 || backend.state != nil {
		t.Fatalf("rollback = %d/%+v, want a full restore and no state", backend.restoreOriginals, backend.state)
	}
	if err := session.Cleanup(); err != nil {
		t.Fatalf("Cleanup after rollback: %v", err)
	}
}

// TestWinACLProtectLedgerReconciles covers the protect ledger: apply only the
// new paths, skip the unchanged ones (no tree walk), and revoke the paths the
// policy no longer protects.
func TestWinACLProtectLedgerReconciles(t *testing.T) {
	backend := newFakeACLBackend()
	path := `C:\work`
	sid := "S-1-5-21-protect"
	gitDir := t.TempDir()
	secrets := t.TempDir()
	if err := os.MkdirAll(filepath.Join(gitDir, "objects"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(gitDir, "HEAD"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	first := grantWorkspaceACL(t, backend, path, sid, workspaceOptions(gitDir, secrets))
	if !backend.objects.has(gitDir, sid) || !backend.objects.has(secrets, sid) {
		t.Fatal("protect deny ACE missing on a protected root")
	}
	if !backend.objects.has(filepath.Join(gitDir, "HEAD"), sid) || !backend.objects.has(filepath.Join(gitDir, "objects"), sid) {
		t.Fatal("protect deny ACE did not cover the existing subtree")
	}
	backend.mu.Lock()
	if len(backend.state.Protected) != 2 {
		backend.mu.Unlock()
		t.Fatalf("ledger = %v, want both paths", backend.state.Protected)
	}
	backend.mu.Unlock()
	appliedAfterFirst := len(backend.objects.applied)
	if err := first.Cleanup(); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	// 缓存语义：退出后保护仍在（capability deny ACE 是缓存资产）。
	if !backend.objects.has(gitDir, sid) {
		t.Fatal("cleanup revoked the cached protect deny ACE")
	}

	// 第二次运行：账本命中且根上的 ACE 还在 → 一次写入都不能发生。
	second := grantWorkspaceACL(t, backend, path, sid, workspaceOptions(gitDir, secrets))
	if len(backend.objects.applied) != appliedAfterFirst {
		t.Fatalf("second run wrote %d ACEs, want 0 (ledger hit)", len(backend.objects.applied)-appliedAfterFirst)
	}
	if err := second.Cleanup(); err != nil {
		t.Fatalf("Cleanup(second): %v", err)
	}

	// 策略只保留 gitDir：secrets 的 deny ACE 必须被撤掉（否则"取消保护"静默失效）。
	third := grantWorkspaceACL(t, backend, path, sid, workspaceOptions(gitDir))
	if backend.objects.has(secrets, sid) {
		t.Fatal("deny ACE of a dropped protect path survived")
	}
	if !backend.objects.has(gitDir, sid) {
		t.Fatal("kept protect path lost its deny ACE")
	}
	backend.mu.Lock()
	if len(backend.state.Protected) != 1 || winPathKey(backend.state.Protected[0]) != winPathKey(gitDir) {
		backend.mu.Unlock()
		t.Fatalf("ledger = %v, want only %q", backend.state.Protected, gitDir)
	}
	backend.mu.Unlock()
	if err := third.Cleanup(); err != nil {
		t.Fatalf("Cleanup(third): %v", err)
	}

	// 自愈：有人手工删掉了根上的 ACE → 下一次运行重放整棵子树。
	delete(backend.objects.present[winPathKey(gitDir)], sid)
	fourth := grantWorkspaceACL(t, backend, path, sid, workspaceOptions(gitDir))
	if !backend.objects.has(gitDir, sid) {
		t.Fatal("protect ACE was not re-applied after being stripped")
	}
	if err := fourth.Cleanup(); err != nil {
		t.Fatalf("Cleanup(fourth): %v", err)
	}
}

// TestWinACLProtectApplyFailureRollsBack covers a failure in the middle of the
// protect walk: the grant aborts and the protect ACEs it already wrote are
// revoked (no half-protected, half-leaked directory).
func TestWinACLProtectApplyFailureRollsBack(t *testing.T) {
	backend := newFakeACLBackend()
	protected := t.TempDir()
	if err := os.WriteFile(filepath.Join(protected, "HEAD"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	backend.objects.failOn = winPathKey(filepath.Join(protected, "HEAD"))
	session := newWinACLSession(backend, `C:\work`, "S-1-5-21-protectfail", workspaceOptions(protected))
	if _, err := session.Grant(); err == nil {
		t.Fatal("Grant with a failing protect apply = nil error, want fail closed")
	}
	if backend.objects.has(protected, "S-1-5-21-protectfail") {
		t.Fatal("rollback left a protect deny ACE behind")
	}
	if backend.restoreOriginals != 1 || backend.state != nil {
		t.Fatalf("rollback = %d/%+v, want a full restore and no state", backend.restoreOriginals, backend.state)
	}
	if err := session.Cleanup(); err != nil {
		t.Fatalf("Cleanup after rollback: %v", err)
	}
}

// TestWinACLCleanupWithoutStateStillRevokes covers an ephemeral temp session
// whose state file is gone: cleanup still revokes its own capability ACE.
func TestWinACLCleanupWithoutStateStillRevokes(t *testing.T) {
	backend := newFakeACLBackend()
	sid := "S-1-5-21-nostate"
	backend.allow[sid] = true // leftover ACE, state file never written
	session := newWinACLSession(backend, `C:\work`, sid, winACLSessionOptions{WorkspaceDeny: true, LowLabel: true})
	if err := session.Cleanup(); err != nil {
		t.Fatalf("Cleanup without state: %v", err)
	}
	if backend.allow[sid] {
		t.Fatal("cleanup without state left our capability ACE behind")
	}
}

// TestWinACLWorkspaceCleanupWithoutStateKeepsACE 是缓存语义的反向约束：workspace
// 会话在没有状态文件时**不能**撤销 capability ACE（它是跨运行复用的缓存资产，
// 撤销会让下一次运行重新付一次全树 ACL 开销）。
func TestWinACLWorkspaceCleanupWithoutStateKeepsACE(t *testing.T) {
	backend := newFakeACLBackend()
	sid := "S-1-5-21-cached-nostate"
	backend.allow[sid] = true
	session := newWinACLSession(backend, `C:\work`, sid, workspaceOptions())
	if err := session.Cleanup(); err != nil {
		t.Fatalf("Cleanup without state: %v", err)
	}
	if !backend.allow[sid] {
		t.Fatal("workspace cleanup revoked the cached capability ACE")
	}
}

// TestWinACLTempSessionRestoresOriginal covers the ephemeral private-temp
// lifecycle: last cleanup restores the snapshot and drops the state file.
func TestWinACLTempSessionRestoresOriginal(t *testing.T) {
	backend := newFakeACLBackend()
	sid := "S-1-5-21-temp"
	session := grantTempACL(t, backend, `C:\temp\rig-sbx-tmp-1`, sid)
	backend.mu.Lock()
	if !backend.allow[sid] || !backend.deny || !backend.label {
		backend.mu.Unlock()
		t.Fatalf("temp grant shape = allow=%v deny=%t label=%t", backend.allow, backend.deny, backend.label)
	}
	backend.mu.Unlock()
	if err := session.Cleanup(); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if backend.allow[sid] || backend.deny || backend.label {
		t.Fatalf("temp cleanup left shape: allow=%v deny=%t label=%t", backend.allow, backend.deny, backend.label)
	}
	if backend.restoreOriginals != 1 || backend.state != nil {
		t.Fatalf("temp cleanup = %d/%+v, want a full restore and no state", backend.restoreOriginals, backend.state)
	}
}

// TestWinACLLabelFailureDowngrades covers the best-effort Low label: the grant
// still succeeds (restricted token remains), state records LabelApplied=false.
func TestWinACLLabelFailureDowngrades(t *testing.T) {
	backend := newFakeACLBackend()
	backend.failLabel = true
	session := newWinACLSession(backend, `C:\work`, "S-1-5-21-nolabel", workspaceOptions())
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

// TestWinACLLegacyStateIsMigrated covers the v1 → v2 upgrade path: the old
// state is fully restored (removing the random-SID ACEs and any residue) and
// rebuilt with the new semantics.
func TestWinACLLegacyStateIsMigrated(t *testing.T) {
	backend := newFakeACLBackend()
	legacySID := "S-1-5-21-legacy"
	backend.state = &winACLState{
		Version: winACLStateVersionLegacy, Path: `C:\work`, PathHash: winACLPathHash(`C:\work`),
		Descriptor: backend.original, DenyAdded: true, LabelApplied: true,
	}
	backend.allow[legacySID] = true
	backend.deny, backend.label = true, true
	sid := "S-1-5-21-new"
	session := grantWorkspaceACL(t, backend, `C:\work`, sid, workspaceOptions())
	backend.mu.Lock()
	if backend.restoreOriginals != 1 {
		backend.mu.Unlock()
		t.Fatalf("restore originals = %d, want the legacy state restored once", backend.restoreOriginals)
	}
	if backend.state == nil || backend.state.Version != winACLStateVersion {
		backend.mu.Unlock()
		t.Fatalf("state = %+v, want a v%d state", backend.state, winACLStateVersion)
	}
	if backend.allow[legacySID] {
		backend.mu.Unlock()
		t.Fatal("legacy capability ACE survived the migration")
	}
	backend.mu.Unlock()
	if err := session.Cleanup(); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
}

// TestWinACLCorruptStateFailsClosed covers corrupted state: Grant aborts before
// touching the directory ACL (fail closed, helper will not spawn).
func TestWinACLCorruptStateFailsClosed(t *testing.T) {
	backend := newFakeACLBackend()
	backend.loadErr = errors.New("corrupt state json")
	session := newWinACLSession(backend, `C:\work`, "S-1-5-21-corrupt", workspaceOptions())
	if _, err := session.Grant(); err == nil {
		t.Fatal("Grant with corrupt state = nil error, want fail-closed error")
	}
	if backend.deny || backend.label || len(backend.allow) != 0 || backend.state != nil {
		t.Fatalf("corrupt-state grant touched the ACL: deny=%t label=%t allow=%v", backend.deny, backend.label, backend.allow)
	}
	if backend.restoreOriginals != 0 || backend.removes != 0 {
		t.Fatalf("corrupt-state grant rewrote state (restores=%d removes=%d)", backend.restoreOriginals, backend.removes)
	}
	if err := session.Cleanup(); err == nil {
		t.Fatal("Cleanup with corrupt state = nil error, want fail-closed error")
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
			session := newWinACLSession(backend, `C:\work`, sid, workspaceOptions())
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
	if len(backend.state.Grants) != 4 || len(backend.allow) != 4 || !backend.deny || !backend.label {
		backend.mu.Unlock()
		t.Fatalf("state after concurrent grants = %+v (allow %v)", backend.state, backend.allow)
	}
	backend.mu.Unlock()
	for index := len(sessions) - 1; index >= 0; index-- {
		if err := sessions[index].Cleanup(); err != nil {
			t.Fatalf("Cleanup(%d): %v", index, err)
		}
	}
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if backend.deny || backend.label {
		t.Fatalf("final cleanup did not restore per-run parts (deny=%t label=%t)", backend.deny, backend.label)
	}
	if backend.state == nil || len(backend.state.Grants) != 0 {
		t.Fatalf("final state = %+v, want a grant-free cache record", backend.state)
	}
}

// TestWinACLAbortDoesNotRollBackSharedShape 固定"失败时不能撤掉别人的授权面"这条
// 不变量：另一个 helper 还在工作时，失败的 grant 只记录账本、不改共享形状。
func TestWinACLAbortDoesNotRollBackSharedShape(t *testing.T) {
	backend := newFakeACLBackend()
	path := `C:\work`
	liveSID := "S-1-5-21-live"
	live := grantWorkspaceACL(t, backend, path, liveSID, workspaceOptions())

	protected := t.TempDir()
	if err := os.WriteFile(filepath.Join(protected, "HEAD"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	backend.objects.failOn = winPathKey(filepath.Join(protected, "HEAD"))
	failingSID := "S-1-5-21-failing"
	failing := newWinACLSession(backend, path, failingSID, workspaceOptions(protected))
	if _, err := failing.Grant(); err == nil {
		t.Fatal("Grant with a failing protect apply = nil error, want fail closed")
	}

	backend.mu.Lock()
	if backend.restoreOriginals != 0 || backend.restoreShapes != 0 {
		backend.mu.Unlock()
		t.Fatalf("abort rolled back the shared shape (restores %d/%d)", backend.restoreOriginals, backend.restoreShapes)
	}
	if !backend.deny || !backend.label || !backend.allow[liveSID] {
		backend.mu.Unlock()
		t.Fatalf("abort damaged the live grant's shape: deny=%t label=%t allow=%v", backend.deny, backend.label, backend.allow)
	}
	state := backend.cloneState()
	backend.mu.Unlock()
	if state == nil {
		t.Fatal("abort removed the shared state file")
	}
	// 账本必须记下本次写到一半的保护路径：否则那条 deny ACE 永远撤不掉。
	if len(state.Protected) != 1 || winPathKey(state.Protected[0]) != winPathKey(protected) {
		t.Fatalf("ledger = %v, want [%q]", state.Protected, protected)
	}
	if err := failing.Cleanup(); err != nil {
		t.Fatalf("Cleanup(failing): %v", err)
	}
	if err := live.Cleanup(); err != nil {
		t.Fatalf("Cleanup(live): %v", err)
	}
	if backend.deny || backend.label {
		t.Fatalf("final cleanup left per-run parts: deny=%t label=%t", backend.deny, backend.label)
	}
}

// TestWinACLSameSIDGrantsStayIndependent 是确定性 SID 带来的新并发不变量：同一
// 工作区的两个 helper 共享同一个 capability SID（因此共享同一条 ACE），但它们的
// grant 记录与 marker 是各自独立的。先退出者绝不能：
//   - 删掉后退出者的 grant 记录（否则后退出者清理时以为自己也是最后一个）；
//   - 恢复 Everyone 拒绝 / Low 标签（那会在后退出者的子进程还在跑时削弱边界）。
func TestWinACLSameSIDGrantsStayIndependent(t *testing.T) {
	backend := newFakeACLBackend()
	path := `C:\work`
	sharedSID := "S-1-5-21-deterministic"
	first := grantWorkspaceACL(t, backend, path, sharedSID, workspaceOptions())
	second := grantWorkspaceACL(t, backend, path, sharedSID, workspaceOptions())

	backend.mu.Lock()
	if len(backend.state.Grants) != 2 {
		backend.mu.Unlock()
		t.Fatalf("grants = %+v, want two records for the shared SID", backend.state.Grants)
	}
	backend.mu.Unlock()

	if err := first.Cleanup(); err != nil {
		t.Fatalf("Cleanup(first): %v", err)
	}
	backend.mu.Lock()
	if backend.restoreShapes != 0 || backend.restoreOriginals != 0 {
		backend.mu.Unlock()
		t.Fatalf("first cleanup restored the shared shape while a peer was alive (%d/%d)",
			backend.restoreShapes, backend.restoreOriginals)
	}
	if !backend.deny || !backend.label {
		backend.mu.Unlock()
		t.Fatalf("first cleanup dropped per-run parts: deny=%t label=%t", backend.deny, backend.label)
	}
	if len(backend.state.Grants) != 1 || backend.state.Grants[0].SID != sharedSID {
		backend.mu.Unlock()
		t.Fatalf("grants after the first exit = %+v, want only the peer's record", backend.state.Grants)
	}
	backend.mu.Unlock()

	if err := second.Cleanup(); err != nil {
		t.Fatalf("Cleanup(second): %v", err)
	}
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if backend.deny || backend.label {
		t.Fatalf("last cleanup did not restore per-run parts: deny=%t label=%t", backend.deny, backend.label)
	}
	if backend.state == nil || len(backend.state.Grants) != 0 {
		t.Fatalf("final state = %+v, want a grant-free cache record", backend.state)
	}
}
