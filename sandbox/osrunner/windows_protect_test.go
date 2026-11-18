package osrunner

// 本文件在任意宿主上单测 Windows ACL 的纯逻辑：self-relative 安全描述符与 ACE
// 的解析、"精确 ACE 已存在"的幂等判定、保护路径子树的加/撤遍历（含符号链接跳过
// 与失败路径）。真实 Win32 调用在 windows_acl_backend.go（//go:build windows），
// 其行为只能在 Windows 上验证，见 windows_runtime_test.go。

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// winTestSID 构造一个 SID 的原始字节（Revision 1 + NT authority 5 + 子权限）。
func winTestSID(subAuthorities ...uint32) []byte {
	sid := make([]byte, winSIDHeaderLength+4*len(subAuthorities))
	sid[0] = 1
	sid[1] = byte(len(subAuthorities))
	// IdentifierAuthority 是 6 字节大端；NT authority = 5。
	sid[7] = 5
	for index, value := range subAuthorities {
		binary.LittleEndian.PutUint32(sid[winSIDHeaderLength+4*index:], value)
	}
	return sid
}

// winTestACL 构造一个只含给定 ACE 的 ACL 字节缓冲。
func winTestACL(entries ...winACEView) []byte {
	size := winACLHeaderSize
	for _, entry := range entries {
		size += winACESIDOffset + len(entry.SID)
	}
	acl := make([]byte, size)
	acl[0] = winACLRevision
	binary.LittleEndian.PutUint16(acl[2:], uint16(size))
	binary.LittleEndian.PutUint16(acl[4:], uint16(len(entries)))
	offset := winACLHeaderSize
	for _, entry := range entries {
		aceSize := winACESIDOffset + len(entry.SID)
		acl[offset] = entry.Type
		acl[offset+1] = entry.Flags
		binary.LittleEndian.PutUint16(acl[offset+2:], uint16(aceSize))
		binary.LittleEndian.PutUint32(acl[offset+4:], entry.Mask)
		copy(acl[offset+winACESIDOffset:], entry.SID)
		offset += aceSize
	}
	return acl
}

// winTestDescriptor 构造一个含给定 DACL/SACL 的 self-relative 描述符。
func winTestDescriptor(dacl, sacl []byte) []byte {
	header := make([]byte, winDescriptorHeaderSize)
	header[0] = 1 // Revision
	total := winDescriptorHeaderSize + len(dacl) + len(sacl)
	descriptor := make([]byte, total)
	copy(descriptor, header)
	offset := winDescriptorHeaderSize
	if len(sacl) > 0 {
		binary.LittleEndian.PutUint32(descriptor[winDescriptorSACLOffsetAt:], uint32(offset))
		copy(descriptor[offset:], sacl)
		offset += len(sacl)
	}
	if len(dacl) > 0 {
		binary.LittleEndian.PutUint32(descriptor[winDescriptorDACLOffsetAt:], uint32(offset))
		copy(descriptor[offset:], dacl)
	}
	return descriptor
}

func TestWinACLEntriesAndExactMatch(t *testing.T) {
	capability := winTestSID(21, 1, 2, 3, 4)
	everyone := winTestSID(1, 0)
	acl := winTestACL(
		winACEView{Type: winACETypeAccessDenied, Flags: winProtectDenyFlags, Mask: winProtectDenyMask, SID: capability},
		winACEView{Type: winACETypeAccessAllowed, Flags: winGrantInherit, Mask: winGrantMask, SID: capability},
	)
	entries, err := winACLEntries(acl)
	if err != nil {
		t.Fatalf("winACLEntries: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(entries))
	}
	if entries[0].Type != winACETypeAccessDenied || entries[0].Mask != winProtectDenyMask || entries[0].Flags != winProtectDenyFlags {
		t.Fatalf("entry[0] = %+v", entries[0])
	}
	if got := binary.LittleEndian.Uint32(entries[0].SID[winSIDHeaderLength:]); got != 21 {
		t.Fatalf("entry[0] first subauthority = %d, want 21", got)
	}

	// 精确命中
	if present, err := winACLHasExactACE(acl, winACETypeAccessDenied, winProtectDenyFlags, winProtectDenyMask, capability); err != nil || !present {
		t.Fatalf("exact deny ACE = %t (%v), want true", present, err)
	}
	// 掩码、类型、标志、SID 任一不同都不算命中。
	cases := []struct {
		name     string
		aceType  byte
		aceFlags byte
		mask     uint32
		sid      []byte
	}{
		{name: "wrong mask", aceType: winACETypeAccessDenied, aceFlags: winProtectDenyFlags, mask: 0x2, sid: capability},
		{name: "wrong type", aceType: winACETypeAccessAllowed, aceFlags: winProtectDenyFlags, mask: winProtectDenyMask, sid: capability},
		{name: "wrong flags", aceType: winACETypeAccessDenied, aceFlags: winACEFlagContainerInherit, mask: winProtectDenyMask, sid: capability},
		{name: "wrong sid", aceType: winACETypeAccessDenied, aceFlags: winProtectDenyFlags, mask: winProtectDenyMask, sid: everyone},
	}
	for _, testCase := range cases {
		present, err := winACLHasExactACE(acl, testCase.aceType, testCase.aceFlags, testCase.mask, testCase.sid)
		if err != nil {
			t.Fatalf("%s: %v", testCase.name, err)
		}
		if present {
			t.Fatalf("%s: expected no exact match", testCase.name)
		}
	}
	// nil/空 SID 直接判否，不报错。
	if present, err := winACLHasExactACE(nil, winACETypeAccessDenied, winProtectDenyFlags, winProtectDenyMask, capability); err != nil || present {
		t.Fatalf("nil ACL = %t (%v), want false", present, err)
	}
	if present, err := winACLHasExactACE(acl, winACETypeAccessDenied, winProtectDenyFlags, winProtectDenyMask, nil); err != nil || present {
		t.Fatalf("nil SID = %t (%v), want false", present, err)
	}
}

func TestWinACLEntriesFailClosed(t *testing.T) {
	// 头部截断、AclSize 越界、ACE 尺寸越界、SID 越界都必须报错。
	short := make([]byte, 4)
	if _, err := winACLEntries(short); err == nil {
		t.Fatal("winACLEntries(short) = nil error")
	}
	oversized := winTestACL(winACEView{Type: winACETypeAccessDenied, SID: winTestSID(21, 1)})
	binary.LittleEndian.PutUint16(oversized[2:], uint16(len(oversized)+16))
	if _, err := winACLEntries(oversized); err == nil {
		t.Fatal("winACLEntries(oversized AclSize) = nil error")
	}
	badACE := winTestACL(winACEView{Type: winACETypeAccessDenied, SID: winTestSID(21, 1)})
	binary.LittleEndian.PutUint16(badACE[winACLHeaderSize+2:], 2)
	if _, err := winACLEntries(badACE); err == nil {
		t.Fatal("winACLEntries(bad ACE size) = nil error")
	}
	badSID := winTestACL(winACEView{Type: winACETypeAccessDenied, SID: winTestSID(21, 1)})
	badSID[winACLHeaderSize+winACESIDOffset+1] = 9 // SubAuthorityCount 超出缓冲
	if _, err := winACLEntries(badSID); err == nil {
		t.Fatal("winACLEntries(bad SID count) = nil error")
	}
}

func TestWinParseDescriptorSections(t *testing.T) {
	capability := winTestSID(21, 9, 9, 9, 9)
	dacl := winTestACL(winACEView{Type: winACETypeAccessDenied, Flags: winProtectDenyFlags, Mask: winProtectDenyMask, SID: capability})
	sacl := winTestACL(winACEView{Type: winACETypeMandatoryLabel, Flags: winGrantInherit, Mask: winMandatoryLabelNoWriteUp, SID: winTestSID(16, 4096)})
	descriptor := winTestDescriptor(dacl, sacl)
	// SE_DACL_PROTECTED（0x1000）：Control 位必须原样解析出来（诊断/守门用）。
	binary.LittleEndian.PutUint16(descriptor[2:], 0x1000)
	sections, err := winParseDescriptorSections(descriptor)
	if err != nil {
		t.Fatalf("winParseDescriptorSections: %v", err)
	}
	if sections.Control != 0x1000 {
		t.Fatalf("Control = 0x%x, want 0x1000", sections.Control)
	}
	if sections.DACLOffset == 0 || sections.SACLOffset == 0 {
		t.Fatalf("offsets = %d/%d, want both non-zero", sections.SACLOffset, sections.DACLOffset)
	}
	if len(sections.DACL) != len(dacl) || len(sections.SACL) != len(sacl) {
		t.Fatalf("section sizes = %d/%d, want %d/%d", len(sections.DACL), len(sections.SACL), len(dacl), len(sacl))
	}
	if string(sections.DACL) != string(dacl) || string(sections.SACL) != string(sacl) {
		t.Fatal("section bytes do not round trip")
	}

	// 无 SACL 的描述符：SACLOffset 必须是 0（RestoreShape 靠它判断"原始没有标签"）。
	noSACL, err := winParseDescriptorSections(winTestDescriptor(dacl, nil))
	if err != nil {
		t.Fatalf("winParseDescriptorSections(no SACL): %v", err)
	}
	if noSACL.SACLOffset != 0 || noSACL.SACL != nil {
		t.Fatalf("SACL = %d/%v, want absent", noSACL.SACLOffset, noSACL.SACL)
	}
	if noSACL.DACLOffset == 0 {
		t.Fatal("DACL offset = 0, want present")
	}

	// 形状异常一律 fail closed。
	if _, err := winParseDescriptorSections(make([]byte, winDescriptorHeaderSize-1)); err == nil {
		t.Fatal("winParseDescriptorSections(short) = nil error")
	}
	broken := winTestDescriptor(dacl, nil)
	binary.LittleEndian.PutUint32(broken[winDescriptorDACLOffsetAt:], uint32(len(broken)+8))
	if _, err := winParseDescriptorSections(broken); err == nil {
		t.Fatal("winParseDescriptorSections(DACL offset out of range) = nil error")
	}
}

// fakeProtectObjectSet 用内存模拟保护子树上每个对象的 DACL 状态。
type fakeProtectObjectSet struct {
	mu      sync.Mutex
	present map[string]map[string]bool
	applied []string
	revoked []string
	// failOn 是 winPathKey(path) 命中的对象，ApplyDeny 对它失败（覆盖失败路径）。
	failOn string
}

func newFakeProtectObjectSet() *fakeProtectObjectSet {
	return &fakeProtectObjectSet{present: make(map[string]map[string]bool)}
}

// Object 实现 winACLBackend 需要的对象工厂（也用于保护遍历测试）。
func (set *fakeProtectObjectSet) Object(path string) winProtectObject {
	if path == "" {
		return nil
	}
	return &fakeProtectObject{set: set, path: path}
}

// has 报告某个对象上是否有该 SID 的 deny ACE。
func (set *fakeProtectObjectSet) has(path, sid string) bool {
	set.mu.Lock()
	defer set.mu.Unlock()
	return set.present[winPathKey(path)][sid]
}

// count 返回有 ACE 的对象数。
func (set *fakeProtectObjectSet) count(sid string) int {
	set.mu.Lock()
	defer set.mu.Unlock()
	total := 0
	for _, entries := range set.present {
		if entries[sid] {
			total++
		}
	}
	return total
}

type fakeProtectObject struct {
	set  *fakeProtectObjectSet
	path string
}

func (object *fakeProtectObject) key() string { return winPathKey(object.path) }

func (object *fakeProtectObject) DenyPresent(capabilitySID string) (bool, error) {
	object.set.mu.Lock()
	defer object.set.mu.Unlock()
	return object.set.present[object.key()][capabilitySID], nil
}

func (object *fakeProtectObject) ApplyDeny(capabilitySID string) error {
	object.set.mu.Lock()
	defer object.set.mu.Unlock()
	if object.set.failOn != "" && object.set.failOn == object.key() {
		return errors.New("simulated WRITE_DAC failure")
	}
	if object.set.present[object.key()] == nil {
		object.set.present[object.key()] = make(map[string]bool)
	}
	object.set.present[object.key()][capabilitySID] = true
	object.set.applied = append(object.set.applied, object.path)
	return nil
}

func (object *fakeProtectObject) RevokeDeny(capabilitySID string) error {
	object.set.mu.Lock()
	defer object.set.mu.Unlock()
	delete(object.set.present[object.key()], capabilitySID)
	object.set.revoked = append(object.set.revoked, object.path)
	return nil
}

// winProtectTreeFixture 建立一个含子目录、文件与一个符号链接的保护子树。
func winProtectTreeFixture(t *testing.T) (root string, outside string, targets []string) {
	t.Helper()
	root = filepath.Join(t.TempDir(), "protected")
	nested := filepath.Join(root, "nested")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatalf("mkdir %q: %v", nested, err)
	}
	for _, path := range []string{
		filepath.Join(root, "HEAD"),
		filepath.Join(nested, "index"),
	} {
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatalf("write %q: %v", path, err)
		}
	}
	outside = filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
		t.Fatalf("write %q: %v", outside, err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(outside, link); err != nil {
		// Windows 上创建符号链接需要额外权限；没有链接时这个用例仍然有效。
		t.Logf("symlink unavailable: %v", err)
	}
	return root, outside, []string{root, filepath.Join(root, "HEAD"), nested, filepath.Join(nested, "index")}
}

func TestWinEnforceProtectTreeIsIdempotent(t *testing.T) {
	root, outside, targets := winProtectTreeFixture(t)
	objects := newFakeProtectObjectSet()
	sid := "S-1-5-21-1-2-3-4"

	stats, err := winEnforceProtectTree(objects.Object, root, sid)
	if err != nil {
		t.Fatalf("winEnforceProtectTree: %v", err)
	}
	if stats.Applied != len(targets) || stats.Visited != len(targets) {
		t.Fatalf("stats = %+v, want applied/visited = %d", stats, len(targets))
	}
	for _, path := range targets {
		if !objects.has(path, sid) {
			t.Fatalf("protected object %q has no deny ACE", path)
		}
	}
	// 符号链接不能被加 ACE（ACE 会落到链接目标上，可能在保护子树之外）。
	if _, err := os.Lstat(filepath.Join(root, "link")); err == nil {
		if objects.has(filepath.Join(root, "link"), sid) {
			t.Fatal("deny ACE was applied to a symlink entry")
		}
		if objects.has(outside, sid) {
			t.Fatal("deny ACE leaked to the symlink target outside the protected tree")
		}
	}

	// 第二次遍历：全部命中幂等判定，一次写入都不能发生。
	second, err := winEnforceProtectTree(objects.Object, root, sid)
	if err != nil {
		t.Fatalf("winEnforceProtectTree(second): %v", err)
	}
	if second.Applied != 0 || second.Present != len(targets) {
		t.Fatalf("second stats = %+v, want applied=0 present=%d", second, len(targets))
	}
	if len(objects.applied) != len(targets) {
		t.Fatalf("apply calls = %d, want %d (idempotent skip must not write)", len(objects.applied), len(targets))
	}
}

func TestWinEnforceProtectTreeFailsClosed(t *testing.T) {
	root, _, _ := winProtectTreeFixture(t)
	objects := newFakeProtectObjectSet()
	if _, err := winEnforceProtectTree(objects.Object, filepath.Join(root, "missing"), "S-1-5-21-1-2-3-4"); err == nil {
		t.Fatal("winEnforceProtectTree(missing root) = nil error, want fail closed")
	}
	objects.failOn = winPathKey(filepath.Join(root, "nested", "index"))
	if _, err := winEnforceProtectTree(objects.Object, root, "S-1-5-21-1-2-3-4"); err == nil {
		t.Fatal("winEnforceProtectTree(apply failure) = nil error, want fail closed")
	}
}

func TestWinRelaxProtectTreeRevokesOnlyExisting(t *testing.T) {
	root, _, targets := winProtectTreeFixture(t)
	objects := newFakeProtectObjectSet()
	sid := "S-1-5-21-9-9-9-9"
	if _, err := winEnforceProtectTree(objects.Object, root, sid); err != nil {
		t.Fatalf("winEnforceProtectTree: %v", err)
	}
	stats, err := winRelaxProtectTree(objects.Object, root, sid)
	if err != nil {
		t.Fatalf("winRelaxProtectTree: %v", err)
	}
	if stats.Applied != len(targets) {
		t.Fatalf("relax stats = %+v, want applied=%d", stats, len(targets))
	}
	if remaining := objects.count(sid); remaining != 0 {
		t.Fatalf("deny ACEs left on %d objects, want 0", remaining)
	}
	// 再撤一次：没有任何 ACE 需要移除，不能产生写入。
	objects.revoked = nil
	second, err := winRelaxProtectTree(objects.Object, root, sid)
	if err != nil {
		t.Fatalf("winRelaxProtectTree(second): %v", err)
	}
	if second.Applied != 0 || len(objects.revoked) != 0 {
		t.Fatalf("second relax = %+v (revoked %d), want a no-op", second, len(objects.revoked))
	}
	// 根已经不存在：视为不需要撤销，不报错。
	if _, err := winRelaxProtectTree(objects.Object, filepath.Join(root, "gone"), sid); err != nil {
		t.Fatalf("winRelaxProtectTree(missing root): %v", err)
	}
}

func TestWinProtectSetOperations(t *testing.T) {
	previous := []string{filepath.FromSlash("/work/.git"), filepath.FromSlash("/work/secrets")}
	desired := []string{filepath.FromSlash("/WORK/.git"), filepath.FromSlash("/work/new")}
	added := winProtectDifference(desired, previous)
	removed := winProtectDifference(previous, desired)
	unchanged := winProtectIntersection(previous, desired)
	if len(added) != 1 || winPathKey(added[0]) != winPathKey(filepath.FromSlash("/work/new")) {
		t.Fatalf("added = %v, want [/work/new]", added)
	}
	if len(removed) != 1 || winPathKey(removed[0]) != winPathKey(filepath.FromSlash("/work/secrets")) {
		t.Fatalf("removed = %v, want [/work/secrets]", removed)
	}
	// 大小写不同但同一路径算"未变化"。
	if len(unchanged) != 1 || winPathKey(unchanged[0]) != winPathKey(filepath.FromSlash("/work/.git")) {
		t.Fatalf("unchanged = %v, want [.git]", unchanged)
	}
}

// TestWinProtectDenyMaskMatchesDocumentedBits 是常量自比陷阱的防线：掩码必须由
// 独立字面量拼出来并逐位核对，否则"注释写 0x10156、常量写 0x10076"这类错误在
// 纯逻辑测试里永远发现不了。
func TestWinProtectDenyMaskMatchesDocumentedBits(t *testing.T) {
	if winProtectDenyMask != winProtectDenyMaskLiteral {
		t.Fatalf("winProtectDenyMask = 0x%08X, want 0x%08X (literal composition)", winProtectDenyMask, winProtectDenyMaskLiteral)
	}
	if winProtectDenyMask != 0x00010156 {
		t.Fatalf("winProtectDenyMask = 0x%08X, want the documented 0x00010156", winProtectDenyMask)
	}
	for _, bit := range []uint32{winFileWriteData, winFileAppendData, winFileWriteEA, winFileWriteAttributes, winFileDeleteChild, winDelete} {
		if winProtectDenyMask&bit == 0 {
			t.Fatalf("winProtectDenyMask = 0x%08X, missing documented bit 0x%08X", winProtectDenyMask, bit)
		}
	}
	// 保护路径仍必须可读可执行：这两个位绝不能出现在 deny 掩码里。
	for _, bit := range []uint32{winFileExecute, 0x0001 /* FILE_READ_DATA */, 0x0080 /* FILE_READ_ATTRIBUTES */} {
		if winProtectDenyMask&bit != 0 {
			t.Fatalf("winProtectDenyMask = 0x%08X, must not deny bit 0x%08X", winProtectDenyMask, bit)
		}
	}
}

// TestWinGrantMaskCoversGenericWrite 断言能力 ACE 覆盖 GENERIC_WRITE 的映射位：
// 缺 READ_CONTROL 时用 GENERIC_WRITE 打开文件会被整条拒绝。
func TestWinGrantMaskCoversGenericWrite(t *testing.T) {
	// GENERIC_WRITE = FILE_WRITE_DATA|FILE_APPEND_DATA|FILE_WRITE_EA|
	// FILE_WRITE_ATTRIBUTES|READ_CONTROL|SYNCHRONIZE = 0x00120116
	const genericWrite = 0x00120116
	if winGrantMask&genericWrite != genericWrite {
		t.Fatalf("winGrantMask = 0x%08X does not cover GENERIC_WRITE (0x%08X)", winGrantMask, genericWrite)
	}
	for _, bit := range []uint32{0x00040000 /* WRITE_DAC */, 0x00080000 /* WRITE_OWNER */} {
		if winGrantMask&bit != 0 {
			t.Fatalf("winGrantMask must not grant 0x%08X", bit)
		}
	}
}

// 给**文件**加保护（git worktree 的 `.git` 指针文件）：WalkDir 只访问它自己，
// 不递归、也不失败。这是"worktree 里的 Windows 沙箱能跑起来"的前提。
func TestWinEnforceProtectTreeOnFile(t *testing.T) {
	root := t.TempDir()
	pointer := filepath.Join(root, ".git")
	if err := os.WriteFile(pointer, []byte("gitdir: /elsewhere/.git/worktrees/x\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	set := newFakeProtectObjectSet()
	stats, err := winEnforceProtectTree(set.Object, pointer, "S-1-5-21-7-7-7-7")
	if err != nil {
		t.Fatalf("winEnforceProtectTree(file): %v", err)
	}
	if stats.Visited != 1 || stats.Applied != 1 || stats.Skipped != 0 {
		t.Fatalf("stats = %+v, want 只访问并保护该文件", stats)
	}
	if !set.has(pointer, "S-1-5-21-7-7-7-7") {
		t.Fatalf("文件上没有 deny ACE")
	}
}
