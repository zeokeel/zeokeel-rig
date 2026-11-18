package osrunner

// 本文件是 Windows ACL 的**纯逻辑**部分：self-relative 安全描述符与 ACE 的解析、
// "精确 ACE 是否存在"的幂等判定、保护路径子树的遍历（加/撤 deny ACE）。它不带
// build tag：Win32 调用在 windows_acl_backend.go（//go:build windows），这里只
// 处理字节与文件系统树，因此可以在 Linux 宿主上完整单测。
//
// # 为什么需要子树遍历（而不是只给保护目录加一条继承 ACE）
//
// Windows 的继承 ACE 只对**之后新建**的子对象生效：给目录新加一条可继承 ACE
// 不会追认既有子对象的 DACL（系统只在创建对象或显式请求自动传播时才计算继承）。
// 因此"给 .git 加一条继承 deny ACE"并不能阻止受限子进程改写 .git 里**已存在**的
// 文件（.git/config、.git/index…），这等于没有保护。后端改为显式遍历保护子树，
// 给每个既有对象加一条精确的 deny ACE（目录上的那条仍然带 OI|CI，覆盖之后新建的
// 对象）。
//
// # 幂等
//
// 每条 ACE 的形状都是确定的：trustee = 由规范化路径派生的 capability SID、
// 掩码 = winProtectDenyMask、标志 = OI|CI、类型 = ACCESS_DENIED。遍历时先读对象
// DACL 判断"精确 ACE 已存在"，存在就不写（避免大仓库上每次命令都重写全树 ACL，
// 那是分钟级开销）。

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
)

// Win32 ACE/ACL 常量（数值与 winnt.h 一致）。
const (
	// winACLRevision 是 ACL_REVISION。
	winACLRevision = 2
	// winACLHeaderSize 是 ACL 头部字节数（AclRevision/Sbz1/AclSize/AceCount/Sbz2）。
	winACLHeaderSize = 8
	// winACESIDOffset 是 ACE 内 SID 相对 ACE 起点的偏移（ACE_HEADER 4 + ACCESS_MASK 4）。
	winACESIDOffset = 8
	// winSIDHeaderLength 是 SID 结构头部字节数（Revision/SubAuthorityCount/Authority）。
	winSIDHeaderLength = 8
	// winDescriptorHeaderSize 是 SECURITY_DESCRIPTOR_RELATIVE 头部字节数。
	winDescriptorHeaderSize = 20
	// self-relative 描述符头部内各偏移字段的位置。
	winDescriptorSACLOffsetAt = 12
	winDescriptorDACLOffsetAt = 16

	// ACE 类型（ACCESS_ALLOWED_ACE_TYPE / ACCESS_DENIED_ACE_TYPE /
	// SYSTEM_MANDATORY_LABEL_ACE_TYPE）。
	winACETypeAccessAllowed  = 0x00
	winACETypeAccessDenied   = 0x01
	winACETypeMandatoryLabel = 0x11
	// ACE 标志。
	winACEFlagObjectInherit    = 0x01
	winACEFlagContainerInherit = 0x02
	// 单个访问位（把掩码拆成可读的字面量，测试用它断言"常量与文档语义一致"）。
	winFileWriteData       = 0x00000002
	winFileAppendData      = 0x00000004
	winFileWriteEA         = 0x00000010
	winFileExecute         = 0x00000020
	winFileReadAttributes  = 0x00000080
	winFileDeleteChild     = 0x00000040
	winFileWriteAttributes = 0x00000100
	winDelete              = 0x00010000
	winReadControl         = 0x00020000
	winSynchronize         = 0x00100000
	// 文件访问掩码与继承标志（Windows 语义常量，后端与纯逻辑测试共用）。
	//
	// winGrantMask 是能力 SID 获得的“写+删+删子项”掩码：
	// FILE_WRITE_DATA|FILE_APPEND_DATA|FILE_WRITE_EA|FILE_WRITE_ATTRIBUTES|
	// DELETE|FILE_DELETE_CHILD|SYNCHRONIZE|READ_CONTROL。
	//
	// READ_CONTROL 是必需的：GENERIC_WRITE 会映射到 0x120116（含
	// STANDARD_RIGHTS_WRITE = READ_CONTROL），ACE 缺这一位时"用 GENERIC_WRITE
	// 打开文件"会被整条拒绝；SYNCHRONIZE 是同步 I/O 的常规请求位。
	// 刻意不含 WRITE_DAC/WRITE_OWNER：受限子进程不能改写 DACL 或取得所有权。
	winGrantMask        = 0x00130156
	winGrantInherit     = winACEFlagObjectInherit | winACEFlagContainerInherit
	winContainerInherit = winACEFlagContainerInherit
	// winMandatoryLabelNoWriteUp 是 SYSTEM_MANDATORY_LABEL_NO_WRITE_UP。
	winMandatoryLabelNoWriteUp = 1

	// winProtectDenyMask 是保护路径上拒绝的访问位：
	// FILE_WRITE_DATA | FILE_APPEND_DATA | FILE_WRITE_EA | FILE_WRITE_ATTRIBUTES |
	// DELETE | FILE_DELETE_CHILD。
	//
	// 与工作区能力 ACE 的 winGrantMask 相比，刻意**不**拒绝读/执行/READ_ATTRIBUTES
	// 与 SYNCHRONIZE：保护路径仍然可读（.git 里的对象需要能读，否则 git 命令会
	// 因为读不到配置而失败），只把写/删/改元数据钉死。
	//
	// 数值必须与上面的注释逐位对应（0x10156）。这里曾经写成 0x10076，等于
	// **漏掉 FILE_WRITE_ATTRIBUTES(0x100)**（改文件属性/时间戳仍成功）又
	// **多拒了 FILE_EXECUTE(0x20)**（与"仍可执行"的说明矛盾）；测试当时用同一个
	// 常量自比所以发现不了。下面的 winProtectDenyMaskLiteral 就是防这种错误的
	// 字面量断言锚点。
	winProtectDenyMask = 0x00010156
	// winProtectDenyMaskLiteral 由独立的字面量拼出同一掩码，供测试断言"常量与
	// 文档语义一致"，避免再次出现常量自比。
	winProtectDenyMaskLiteral = winFileWriteData | winFileAppendData | winFileWriteEA |
		winFileWriteAttributes | winFileDeleteChild | winDelete
	// winProtectDenyFlags 让 deny ACE 覆盖子树（对象继承 + 容器继承）。
	winProtectDenyFlags = winACEFlagObjectInherit | winACEFlagContainerInherit
)

// winDescriptorSections 是 self-relative SECURITY_DESCRIPTOR 的分段视图。
//
// 偏移字段用于在原始字节上取指针（SetEntriesInAclW 需要 DACL 指针，恢复标签需要
// SACL 指针）；0 表示该分段为 NULL（例如没有 SACL 的目录）。
type winDescriptorSections struct {
	// Control 是 SECURITY_DESCRIPTOR_CONTROL 位（含 SE_DACL_PROTECTED 等）。
	Control uint16
	// DACL/SACL 是分段的字节视图（不含描述符头部）。
	DACL []byte
	SACL []byte
	// DACLOffset/SACLOffset 是分段相对描述符起点的偏移（0 = 分段不存在）。
	DACLOffset int
	SACLOffset int
}

// winParseDescriptorSections 解析 self-relative 描述符头部并切出 DACL/SACL。
//
// 任何形状异常（太短、偏移越界、AclSize 越界）都返回错误：调用方据此 fail
// closed，绝不用一个只解析了一半的描述符去改真实目录。
func winParseDescriptorSections(descriptor []byte) (winDescriptorSections, error) {
	sections := winDescriptorSections{}
	if len(descriptor) < winDescriptorHeaderSize {
		return sections, fmt.Errorf("windows-acl: security descriptor is %d bytes, want at least %d", len(descriptor), winDescriptorHeaderSize)
	}
	sections.Control = binary.LittleEndian.Uint16(descriptor[2:])
	var err error
	if sections.DACLOffset, sections.DACL, err = winDescriptorACL(descriptor, winDescriptorDACLOffsetAt, "DACL"); err != nil {
		return winDescriptorSections{}, err
	}
	if sections.SACLOffset, sections.SACL, err = winDescriptorACL(descriptor, winDescriptorSACLOffsetAt, "SACL"); err != nil {
		return winDescriptorSections{}, err
	}
	return sections, nil
}

// winDescriptorACL 从描述符的某个偏移字段切出一个 ACL 分段。
func winDescriptorACL(descriptor []byte, offsetAt int, name string) (int, []byte, error) {
	offset := int(binary.LittleEndian.Uint32(descriptor[offsetAt:]))
	if offset == 0 {
		return 0, nil, nil
	}
	if offset < winDescriptorHeaderSize || offset+winACLHeaderSize > len(descriptor) {
		return 0, nil, fmt.Errorf("windows-acl: %s offset %d is outside the %d-byte descriptor", name, offset, len(descriptor))
	}
	size := int(binary.LittleEndian.Uint16(descriptor[offset+2:]))
	if size < winACLHeaderSize || offset+size > len(descriptor) {
		return 0, nil, fmt.Errorf("windows-acl: %s size %d at offset %d is outside the %d-byte descriptor", name, size, offset, len(descriptor))
	}
	return offset, descriptor[offset : offset+size], nil
}

// winACEView 是一条 ACE 的解析视图。
type winACEView struct {
	Type  byte
	Flags byte
	Mask  uint32
	// SID 是 SID 的原始字节（SID 结构本身，不含 ACE 头与掩码）。
	SID []byte
}

// winACLEntries 把 ACL 字节解析成 ACE 列表。
func winACLEntries(acl []byte) ([]winACEView, error) {
	if acl == nil {
		return nil, nil
	}
	if len(acl) < winACLHeaderSize {
		return nil, fmt.Errorf("windows-acl: ACL is %d bytes, shorter than the %d-byte header", len(acl), winACLHeaderSize)
	}
	size := int(binary.LittleEndian.Uint16(acl[2:]))
	if size < winACLHeaderSize || size > len(acl) {
		return nil, fmt.Errorf("windows-acl: ACL AclSize %d is outside the %d-byte buffer", size, len(acl))
	}
	count := int(binary.LittleEndian.Uint16(acl[4:]))
	entries := make([]winACEView, 0, count)
	offset := winACLHeaderSize
	for index := 0; index < count; index++ {
		if offset+4 > size {
			return nil, fmt.Errorf("windows-acl: ACE %d header exceeds the %d-byte ACL", index, size)
		}
		aceSize := int(binary.LittleEndian.Uint16(acl[offset+2:]))
		if aceSize < winACESIDOffset || offset+aceSize > size {
			return nil, fmt.Errorf("windows-acl: ACE %d size %d exceeds the %d-byte ACL", index, aceSize, size)
		}
		ace := winACEView{
			Type:  acl[offset],
			Flags: acl[offset+1],
			Mask:  binary.LittleEndian.Uint32(acl[offset+4:]),
		}
		sid := acl[offset+winACESIDOffset : offset+aceSize]
		sidLength, err := winSIDByteLength(sid)
		if err != nil {
			return nil, fmt.Errorf("windows-acl: ACE %d: %w", index, err)
		}
		ace.SID = sid[:sidLength]
		entries = append(entries, ace)
		offset += aceSize
	}
	return entries, nil
}

// winSIDByteLength 返回 SID 结构自身的字节长度（8 + 4*SubAuthorityCount）。
func winSIDByteLength(sid []byte) (int, error) {
	if len(sid) < winSIDHeaderLength {
		return 0, fmt.Errorf("windows-acl: SID is %d bytes, shorter than the %d-byte header", len(sid), winSIDHeaderLength)
	}
	length := winSIDHeaderLength + 4*int(sid[1])
	if length > len(sid) {
		return 0, fmt.Errorf("windows-acl: SID subauthority count %d needs %d bytes, only %d available", sid[1], length, len(sid))
	}
	return length, nil
}

// winACLHasExactACE 报告 ACL 里是否存在类型、标志、掩码与 SID 全部相等的 ACE。
//
// 这是幂等判定的唯一依据：只有四项全等才算"已经是我们要的形状"，否则重写。
// 注意比较的是**精确标志**：继承来的同一掩码 ACE（带 INHERITED_ACE 标志）不算
// 命中，因为恢复时需要的是我们自己那条显式 ACE。
func winACLHasExactACE(acl []byte, aceType, aceFlags byte, mask uint32, sid []byte) (bool, error) {
	if acl == nil || len(sid) == 0 {
		return false, nil
	}
	entries, err := winACLEntries(acl)
	if err != nil {
		return false, err
	}
	for _, entry := range entries {
		if entry.Type != aceType || entry.Flags != aceFlags || entry.Mask != mask {
			continue
		}
		if len(entry.SID) != len(sid) {
			continue
		}
		equal := true
		for index := range sid {
			if entry.SID[index] != sid[index] {
				equal = false
				break
			}
		}
		if equal {
			return true, nil
		}
	}
	return false, nil
}

// winProtectStats 是一次保护子树遍历的统计（用于诊断与测试断言）。
type winProtectStats struct {
	// Visited 是遍历过的对象数（含保护根自身）。
	Visited int
	// Applied 是实际写入 ACE 的对象数。
	Applied int
	// Present 是已经有精确 ACE、被幂等跳过的对象数。
	Present int
	// Skipped 是因为符号链接/重解析点被跳过的条目数（ACE 会落到链接目标上，
	// 可能在工作区之外，绝不能碰）。
	Skipped int
}

// winProtectObject 是保护子树里单个对象的 ACE 操作接缝。
//
// 真实实现由 windows_acl_backend.go 提供；Linux 单测用 fake 记录调用，因此遍历、
// 幂等判定与失败路径都可以在没有 Win32 的宿主上验证。
type winProtectObject interface {
	// DenyPresent 报告对象 DACL 里是否已有精确的 protect deny ACE。
	DenyPresent(capabilitySID string) (bool, error)
	// ApplyDeny 添加精确的 protect deny ACE（调用方保证 DenyPresent 为 false）。
	ApplyDeny(capabilitySID string) error
	// RevokeDeny 移除该 capability SID 的 protect deny ACE。
	RevokeDeny(capabilitySID string) error
}

// winProtectObjects 按路径返回对象接缝。
type winProtectObjects func(path string) winProtectObject

// winEnforceProtectTree 遍历 root 子树，给每个既有对象加上精确的 protect deny
// ACE；已经有精确 ACE 的对象跳过。
//
// 遍历用 filepath.WalkDir + winProtectedEntrySkip：条目本身是重解析点（符号链接
// **或 junction**）时跳过并计数——给链接路径设置安全信息会落在**目标**上（可能是
// 工作区外的任意文件），必须避免。
//
// 注意：Go 的 fs.ModeSymlink 只对**符号链接**置位；junction
// （IO_REPARSE_TAG_MOUNT_POINT）在 WalkDir 眼里是普通目录，会**下钻**。因此判定
// 必须看文件属性里的 FILE_ATTRIBUTE_REPARSE_POINT（见 winEntryNeedsSkip）。
//
// 保护根不存在或不可遍历时返回错误（fail closed）：调用方据此不 spawn 子进程，
// 而不是让调用方以为已经保护。
func winEnforceProtectTree(objects winProtectObjects, root, capabilitySID string) (winProtectStats, error) {
	return winEnforceProtectTreeWithSkip(objects, root, capabilitySID, winProtectedEntrySkip)
}

// winEnforceProtectTreeWithSkip 与 winEnforceProtectTree 相同，但跳过判定可注入。
//
// `skip` 返回 true 表示"不要给这个条目加 ACE，也不要下钻"。生产实现是
// winProtectedEntrySkip（Windows 上查 FILE_ATTRIBUTE_REPARSE_POINT，其它平台退回
// ModeSymlink）；注入点让"junction 被当成普通目录"这类问题能在任意宿主上用 fake
// 覆盖，而不必真机造 junction。
func winEnforceProtectTreeWithSkip(objects winProtectObjects, root, capabilitySID string, skip func(path string, entry fs.DirEntry) (bool, error)) (winProtectStats, error) {
	stats := winProtectStats{}
	if skip == nil {
		skip = winProtectedEntrySkip
	}
	walkErr := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("windows-acl: walk protected path %q: %w", path, err)
		}
		skipped, skipErr := skip(path, entry)
		if skipErr != nil {
			// 判定不了就 fail closed：保护遍历宁可让 helper 不 spawn，也不要
			// 把 deny ACE 加到可能位于工作区外的对象上。
			return fmt.Errorf("windows-acl: classify protected entry %q: %w", path, skipErr)
		}
		if skipped {
			stats.Skipped++
			if entry.IsDir() {
				// 跳过的**目录**必须显式 SkipDir：返回 nil 只会跳过它自己，
				// WalkDir 仍会继续下钻它的子项（正是 junction 场景要避免的）。
				return fs.SkipDir
			}
			return nil
		}
		stats.Visited++
		object := objects(path)
		if object == nil {
			return fmt.Errorf("windows-acl: no ACL handle for protected path %q", path)
		}
		present, presentErr := object.DenyPresent(capabilitySID)
		if presentErr != nil {
			return fmt.Errorf("windows-acl: read protected ACL %q: %w", path, presentErr)
		}
		if present {
			stats.Present++
			return nil
		}
		if applyErr := object.ApplyDeny(capabilitySID); applyErr != nil {
			return fmt.Errorf("windows-acl: apply protected deny ACE to %q: %w", path, applyErr)
		}
		stats.Applied++
		return nil
	})
	if walkErr != nil {
		return stats, walkErr
	}
	return stats, nil
}

// winRelaxProtectTree 遍历 root 子树，撤掉该 capability SID 的 protect deny ACE。
//
// 只有确实存在的 ACE 才写（每次写都会让对象的安全描述符失效并触发一次元数据
// 更新）；保护根已经不存在时视为成功（没有东西需要撤销）。
func winRelaxProtectTree(objects winProtectObjects, root, capabilitySID string) (winProtectStats, error) {
	stats := winProtectStats{}
	walkErr := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return fs.SkipAll
			}
			return fmt.Errorf("windows-acl: walk protected path %q: %w", path, err)
		}
		if entry.Type()&fs.ModeSymlink != 0 {
			stats.Skipped++
			return nil
		}
		stats.Visited++
		object := objects(path)
		if object == nil {
			return fmt.Errorf("windows-acl: no ACL handle for protected path %q", path)
		}
		present, presentErr := object.DenyPresent(capabilitySID)
		if presentErr != nil {
			return fmt.Errorf("windows-acl: read protected ACL %q: %w", path, presentErr)
		}
		if !present {
			stats.Present++
			return nil
		}
		if revokeErr := object.RevokeDeny(capabilitySID); revokeErr != nil {
			return fmt.Errorf("windows-acl: revoke protected deny ACE on %q: %w", path, revokeErr)
		}
		stats.Applied++
		return nil
	})
	if walkErr != nil && !errors.Is(walkErr, fs.ErrNotExist) {
		return stats, walkErr
	}
	return stats, nil
}

// winProtectDifference 返回 in 里存在、但 notIn 里没有的路径（大小写不敏感）。
func winProtectDifference(in, notIn []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(notIn))
	for _, path := range notIn {
		seen[winPathKey(path)] = struct{}{}
	}
	out := make([]string, 0, len(in))
	for _, path := range in {
		if _, exists := seen[winPathKey(path)]; exists {
			continue
		}
		out = append(out, path)
	}
	return out
}

// winProtectIntersection 返回同时存在于 a 与 b 的路径（取 a 的写法，大小写不敏感）。
func winProtectIntersection(a, b []string) []string {
	if len(a) == 0 || len(b) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(b))
	for _, path := range b {
		seen[winPathKey(path)] = struct{}{}
	}
	out := make([]string, 0, len(a))
	for _, path := range a {
		if _, exists := seen[winPathKey(path)]; !exists {
			continue
		}
		out = append(out, path)
	}
	return out
}
