//go:build windows

package osrunner

import (
	"errors"
	"fmt"
	"runtime"
	"syscall"
	"unsafe"
)

// 本文件构造 Windows 受限令牌：打开当前进程令牌、取出登录 SID、用
// CreateRestrictedToken(WRITE_RESTRICTED) 生成受限主令牌，再按需扩展默认 DACL
// 并把完整性级别降到 Low。每一步失败都返回错误，调用方据此 fail closed。

const (
	// winEveryoneSIDString 是 Everyone 的众所周知 SID。
	winEveryoneSIDString = "S-1-1-0"
	// winLowIntegritySIDString 是 Low 强制完整性级别的 SID（S-1-16-4096）。
	winLowIntegritySIDString = "S-1-16-4096"
)

// windowsToken 是一次运行的受限令牌。
//
// 令牌句柄在子进程创建后即可关闭；sids 持有所有 Go 分配的 SID，防止 GC 在
// Win32 调用期间回收它们。
type windowsToken struct {
	handle syscall.Token
	sids   []*syscall.SID
}

// close 释放令牌句柄（幂等）。
func (t *windowsToken) close() {
	if t == nil || t.handle == 0 {
		return
	}
	_ = syscall.CloseHandle(syscall.Handle(t.handle))
	t.handle = 0
}

// winParseSID 用 ConvertStringSidToSidW 解析 SDDL 形式的 SID。
//
// syscall.StringToSid 内部即 ConvertStringSidToSidW + CopySid，返回 Go 自有内存，
// 因此可以安全地跨越多次 Win32 调用（配合调用点的 runtime.KeepAlive）。
func winParseSID(text string) (*syscall.SID, error) {
	if text == "" {
		return nil, errors.New("ConvertStringSidToSidW: empty SID")
	}
	sid, err := syscall.StringToSid(text)
	if err != nil {
		return nil, fmt.Errorf("ConvertStringSidToSidW(%q): %w", text, err)
	}
	return sid, nil
}

// winOpenCurrentProcessToken 打开当前进程令牌，带上 CreateRestrictedToken 与
// SetTokenInformation 需要的访问位。
func winOpenCurrentProcessToken() (syscall.Token, error) {
	process, err := syscall.GetCurrentProcess()
	if err != nil {
		return 0, fmt.Errorf("GetCurrentProcess: %w", err)
	}
	var token syscall.Token
	access := uint32(winTokenAssignPrimary | winTokenDuplicate | winTokenQuery |
		winTokenAdjustDefault | winTokenAdjustSessionID)
	if err := syscall.OpenProcessToken(process, access, &token); err != nil {
		return 0, fmt.Errorf("OpenProcessToken: %w", err)
	}
	return token, nil
}

// winTokenInformation 读取一类令牌信息。
func winTokenInformation(token syscall.Token, class uint32) ([]byte, error) {
	var needed uint32
	sizeErr := syscall.GetTokenInformation(token, class, nil, 0, &needed)
	if needed == 0 {
		if sizeErr != nil {
			return nil, fmt.Errorf("GetTokenInformation(class %d): size query: %w", class, sizeErr)
		}
		return nil, fmt.Errorf("GetTokenInformation(class %d): empty size query", class)
	}
	buffer := make([]byte, needed)
	if err := syscall.GetTokenInformation(token, class, &buffer[0], needed, &needed); err != nil {
		return nil, fmt.Errorf("GetTokenInformation(class %d): %w", class, err)
	}
	if len(buffer) < int(needed) {
		return nil, fmt.Errorf("GetTokenInformation(class %d): returned %d bytes, want %d", class, len(buffer), needed)
	}
	return buffer, nil
}

// winLogonSID 从令牌的组列表里取出登录会话 SID（属性 SE_GROUP_LOGON_ID）。
//
// 登录 SID 是保活组的一部分：没有它，受限子进程的早期 DLL 初始化会以
// 0xC0000142 失败。这里把它转成字符串再解析回来，得到一份不依赖令牌缓冲区
// 生命周期的 Go 自有 SID。
func winLogonSID(token syscall.Token) (*syscall.SID, error) {
	groups, err := winTokenInformation(token, winTokenGroups)
	if err != nil {
		return nil, err
	}
	if len(groups) < winTokenGroupsHeaderSize {
		return nil, fmt.Errorf("GetTokenInformation(TokenGroups): %d bytes is shorter than the header", len(groups))
	}
	count := int(winReadUint32(groups, 0))
	for index := 0; index < count; index++ {
		offset := winTokenGroupsHeaderSize + index*winSIDAndAttributesSize
		if offset+winSIDAndAttributesSize > len(groups) {
			return nil, fmt.Errorf("GetTokenInformation(TokenGroups): group %d exceeds %d bytes", index, len(groups))
		}
		attributes := winReadUint32(groups, offset+8)
		if attributes&winSEGroupLogonID != winSEGroupLogonID {
			continue
		}
		sidPointer := winReadPointer(groups, offset)
		if sidPointer == nil {
			continue
		}
		text, err := (*syscall.SID)(sidPointer).String()
		if err != nil {
			return nil, fmt.Errorf("ConvertSidToStringSidW(logon): %w", err)
		}
		return winParseSID(text)
	}
	return nil, fmt.Errorf("CreateRestrictedToken: no logon SID among %d token groups", count)
}

// winCurrentLogonSID 返回当前进程令牌的登录 SID 字符串。
func winCurrentLogonSID() (string, error) {
	token, err := winOpenCurrentProcessToken()
	if err != nil {
		return "", err
	}
	defer func() { _ = syscall.CloseHandle(syscall.Handle(token)) }()
	sid, err := winLogonSID(token)
	if err != nil {
		return "", err
	}
	text, err := sid.String()
	if err != nil {
		return "", fmt.Errorf("ConvertSidToStringSidW(logon): %w", err)
	}
	return text, nil
}

// winNewRestrictedToken 创建 WRITE_RESTRICTED 受限主令牌。
//
// restricting 是完整 restricting SID 列表（字符串形式），第一个应当是登录 SID。
// 失败返回错误（绝不退化为未受限令牌）。
func winNewRestrictedToken(restricting []string) (*windowsToken, error) {
	if len(restricting) == 0 {
		return nil, errors.New("CreateRestrictedToken: empty restricting SID list")
	}
	current, err := winOpenCurrentProcessToken()
	if err != nil {
		return nil, err
	}
	defer func() { _ = syscall.CloseHandle(syscall.Handle(current)) }()

	sids := make([]*syscall.SID, 0, len(restricting))
	for _, text := range restricting {
		sid, err := winParseSID(text)
		if err != nil {
			return nil, err
		}
		sids = append(sids, sid)
	}
	flags := uint32(winDisableMaxPrivilege | winLUAToken | winWriteRestricted)
	handle, err := winCreateRestrictedToken(current, flags, sids)
	if err != nil {
		return nil, err
	}
	return &windowsToken{handle: handle, sids: sids}, nil
}

// winSetTokenDefaultDACLGrant 往令牌的默认 DACL 里加一条针对 sid 的完全控制允许
// ACE。受限令牌会原样继承用户的默认 DACL，而用户 SID 不在 restricting 列表里，
// 因此受限令牌新建的对象（匿名管道等）在写检查的第二遍会失败；把 restricting
// 列表里的某个 SID 写进默认 DACL 可以让这些对象继续可用，同时对象创建本身仍受
// 父目录 DACL 约束。
func winSetTokenDefaultDACLGrant(token syscall.Token, sid *syscall.SID) error {
	info, err := winTokenInformation(token, winTokenDefaultDACL)
	if err != nil {
		return err
	}
	if len(info) < 8 {
		return fmt.Errorf("TOKEN_DEFAULT_DACL: %d bytes is shorter than a pointer", len(info))
	}
	currentDACL := winReadPointer(info, 0)
	if currentDACL == nil {
		return errors.New("TOKEN_DEFAULT_DACL: the token carries no default DACL")
	}
	entries := []winExplicitAccess{winExplicitAccessFor(sid, winGrantAccess, winFileAllAccess, winGrantInherit)}
	newDACL, err := winSetEntriesInAcl(entries, currentDACL)
	runtime.KeepAlive(sid)
	if err != nil {
		return err
	}
	defer func() { _ = winLocalFree(newDACL) }()
	updated := make([]byte, 8)
	*(*unsafe.Pointer)(unsafe.Pointer(&updated[0])) = newDACL
	if err := winSetTokenInformation(token, winTokenDefaultDACL, unsafe.Pointer(&updated[0]), uint32(len(updated))); err != nil {
		return err
	}
	runtime.KeepAlive(updated)
	return nil
}

// winRestrictTokenIntegrity 把令牌的完整性级别降到 Low（S-1-16-4096）。
func winRestrictTokenIntegrity(token syscall.Token, lowLabel *syscall.SID) error {
	label := syscall.SIDAndAttributes{Sid: lowLabel, Attributes: winSEGroupIntegrity}
	size := uint32(unsafe.Sizeof(label))
	if err := winSetTokenInformation(token, winTokenIntegrityLevel, unsafe.Pointer(&label), size); err != nil {
		return err
	}
	runtime.KeepAlive(label)
	return nil
}
