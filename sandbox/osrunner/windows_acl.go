//go:build windows

package osrunner

import (
	"fmt"
	"runtime"
	"syscall"
	"unsafe"
)

// 本文件通过 GetNamedSecurityInfoW + SetEntriesInAclW + SetNamedSecurityInfoW
// 给工作区根与私有临时目录追加“能力 SID 写+删”允许 ACE，并尽力打上 Low 强制
// 完整性标签。授权记录保存原始安全描述符，helper 退出时恢复，尽量不在真实目录
// 上留下时效性残留。
//
// 授权形状（每次合并为一次 SetNamedSecurityInfoW）：
//   - Everyone 的 FILE_DELETE_CHILD 拒绝 ACE（只继承到容器）：Windows 可以凭父
//     目录的 FILE_DELETE_CHILD 授权删除，而 write-restricted 令牌的第二遍检查
//     覆盖不到这条路径；拒绝它之后，授权根内的删除只能来自能力 ACE 的 DELETE。
//   - 能力 SID 的写+删允许 ACE（继承到子容器与子对象）。
//
// 这不是原子事务：并发修改同一目录安全描述符的其它进程可能在恢复时被覆盖，
// 这是文档化的限制。

// winACLGrant 是一次目录授权的可回滚记录。
type winACLGrant struct {
	path         string
	security     *winSecurityDescriptor
	labelApplied bool
}

// winGrantWrite 给 path 追加能力 SID 写+删允许 ACE 与 Everyone 的环境性删除拒绝。
func winGrantWrite(path string, capability, everyone *syscall.SID) (*winACLGrant, error) {
	security, err := winGetNamedSecurityInfo(path)
	if err != nil {
		return nil, fmt.Errorf("grant %q: %w", path, err)
	}
	entries := []winExplicitAccess{
		winExplicitAccessFor(everyone, winDenyAccess, winFileDeleteChild, winContainerInherit),
		winExplicitAccessFor(capability, winGrantAccess, winGrantMask, winGrantInherit),
	}
	newACL, err := winSetEntriesInAcl(entries, security.dacl)
	runtime.KeepAlive(entries)
	// entries 里用 uintptr 保存 SID 指针，GC 不会因此保活 SID；显式 KeepAlive
	// 保证 ACL 在 Win32 调用期间读到的 SID 仍然有效。
	runtime.KeepAlive(capability)
	runtime.KeepAlive(everyone)
	if err != nil {
		_ = winLocalFree(security.descriptor)
		return nil, fmt.Errorf("grant %q: %w", path, err)
	}
	defer func() { _ = winLocalFree(newACL) }()
	if err := winSetNamedSecurityInfo(path, winDACLSecurityInformation, newACL, nil); err != nil {
		_ = winLocalFree(security.descriptor)
		return nil, fmt.Errorf("grant %q: %w", path, err)
	}
	return &winACLGrant{path: path, security: security}, nil
}

// applyLowLabel 尽力给授权目录打 Low 强制完整性标签。
//
// 标签位于 SACL，写入需要 WRITE_OWNER（通常还需要 SeSecurityPrivilege）；不可行
// 时返回错误，由调用方决定降级为“只依赖受限令牌”。成功后 restore 会恢复原始
// SACL。
func (g *winACLGrant) applyLowLabel(lowLabel *syscall.SID) error {
	labelACL, err := winBuildLowLabelACL(lowLabel)
	if err != nil {
		return fmt.Errorf("label %q: %w", g.path, err)
	}
	if err := winSetNamedSecurityInfo(g.path, winLabelSecurityInformation, nil, unsafe.Pointer(&labelACL[0])); err != nil {
		return fmt.Errorf("label %q: %w", g.path, err)
	}
	runtime.KeepAlive(labelACL)
	g.labelApplied = true
	return nil
}

// restore 恢复目录进入前的 DACL（以及打过标签时的完整性标签），并释放安全
// 描述符分配。它幂等。
func (g *winACLGrant) restore() error {
	if g.security == nil {
		return nil
	}
	securityInfo := uint32(winDACLSecurityInformation)
	if g.labelApplied {
		securityInfo |= winLabelSecurityInformation
	}
	err := winSetNamedSecurityInfo(g.path, securityInfo, g.security.dacl, g.security.sacl)
	freeErr := winLocalFree(g.security.descriptor)
	g.security = nil
	if err != nil {
		return fmt.Errorf("restore %q: %w", g.path, err)
	}
	if freeErr != nil {
		return fmt.Errorf("restore %q: %w", g.path, freeErr)
	}
	return nil
}
