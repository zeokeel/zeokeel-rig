package osrunner

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/zeokeel/zeokeel-rig/sandbox"
)

// 本文件是 Windows ACL runner 的纯计划层：它只做 argv 解析与拼装、能力 SID
// 生成、模式到授权形状的映射，不 import 任何 Windows 专属包，因此可以在任意
// 宿主上单测。真正的 Win32 令牌/ACL/spawn 在 windows_*.go（//go:build windows）。
//
// # 强制模型与部分保证
//
// helper 把一个 WRITE_RESTRICTED 受限令牌交给子进程，令牌的 restricting SID
// 列表只包含 [登录 SID, Everyone, 每个工作区的能力 SID, 临时目录的能力 SID]。
// Windows 对写类访问做两遍检查：正常 SID 与 restricting SID 必须同时通过，因此
// 子进程只能写入那些显式把能力 SID 授予写权限的目录（workspace-write 下是策略根
// 与一次运行的私有临时目录）。helper 还会把令牌降到 Low 完整性，并尽力给同样
// 这些目录打 Low 强制标签（需要 SeSecurityPrivilege/WRITE_OWNER；不可行时退化为
// 只依赖受限令牌）。
//
// 工作区能力 SID 由**规范化路径派生**（winWorkspaceCapabilitySID）：同一目录在
// 不同 helper 进程/会话里得到同一个 SID，因此目录上已有的那条 ACE 可以跨运行
// 复用（见 windows_acl_state.go 的缓存语义），不必每次命令重写整棵树。
//
// 这是一层“部分”强制（EnforcementPartial）：
//   - 只有写/删受治理，读完全不受限；
//   - 网络、进程可见性、设备访问不在承诺内；
//   - NTFS 硬链接是文件对象别名，工作区内的硬链接可能指到工作区外；
//   - 被其他 AppContainer 工具 ACL 过（带包 SID ACE）的对象可能不可读；
//   - 每次授权都会临时改写真实目录的安全描述符（含 Everyone 的
//     FILE_DELETE_CHILD 拒绝与 Low 标签），helper 退出时尽力恢复；进程被强杀
//     时可能留下时效性残留；
//   - 能力 SID 的允许 ACE 与保护路径的 deny ACE 按“缓存资产”保留（同一工作区
//     的 SID 是确定性的，残留 ACE 对其它进程没有意义，下次运行直接复用）；
//     Everyone 拒绝与 Low 标签是 per-run 的，最后一名 helper 退出时恢复；
//   - 安全描述符修改是对真实目录的持久改动，运行期间其它宿主进程也会看到该
//     DACL/标签；并发修改同一目录的第三方进程可能在恢复时被覆盖；
//   - 私有临时目录必须位于工作区之外（工作区包含 os.TempDir() 时该后端 fail
//     closed）；TMP/TEMP 会被改写到私有临时目录；
//   - read-only 意味着连临时目录也不授权：需要写 temp 的程序（例如 PowerShell
//     的启动探针）会降级或失败；
//   - 策略的保护路径（policy.ProtectedPaths，例如 .git）**被强制**：helper 把
//     每条 --protect 规范化并校验（必须是已存在的目录，否则 fail closed），只对
//     落在某个已授权工作区之内的路径生效；后端给保护目录**及其既有子树**的每个
//     对象加一条针对 capability SID 的继承 deny ACE
//     （FILE_WRITE_DATA|FILE_APPEND_DATA|FILE_WRITE_EA|FILE_WRITE_ATTRIBUTES|
//     DELETE|FILE_DELETE_CHILD，标志 OI|CI，deny 在 DACL 里优先于 allow）。落在
//     授权面之外的 protect 路径本来就不能写，helper 跳过并在 stderr 记一行诊断。
//     注意：保护路径**仍可读**（只钉死写/删/改元数据），且 Enforcement 仍是
//     partial——读与网络整体不受限；
//   - 控制台不隔离：子进程共享宿主控制台（CREATE_NO_WINDOW 会让部分解释器在
//     DLL 初始化时以 0xC0000142 退出），基于管道的 stdio 重定向不受影响。
//
// 这些是同世界（same-world）约束，不是容器/VM 级隔离。
const (
	// winModeReadOnly 是 Windows helper 的只读模式。
	winModeReadOnly = "read-only"
	// winModeWorkspaceWrite 是 Windows helper 的工作区可写模式。
	winModeWorkspaceWrite = "workspace-write"

	// winCommandProbe 是 helper 的功能探测参数：只验证令牌/ACL/spawn 管线。
	winCommandProbe = "--probe"
	// winFlagWorkspace 是 helper 的可写工作区根参数（可重复）。
	winFlagWorkspace = "--workspace"
	// winFlagMode 是 helper 的模式参数。
	winFlagMode = "--mode"
	// winFlagProtect 是 helper 的保护路径参数（可重复）：这些路径位于工作区根
	// 之下，但必须保持只读（例如 .git）。
	winFlagProtect = "--protect"
	// winArgSeparator 分隔 helper 参数与目标 argv。
	winArgSeparator = "--"
)

// winACLPlan 是 Windows ACL helper 的一次运行计划（helper argv 的解析结果）。
type winACLPlan struct {
	// Workspaces 是策略的写根；workspace-write 下这些目录会获得能力 SID 写授权，
	// read-only 下只用于校验（不授权）。
	Workspaces []string
	// Protected 是工作区根之下必须保持只读的路径（策略的 ProtectedPaths）。
	//
	// helper 会在 winResolveProtectedPaths 里校验：必须是绝对路径、必须存在且是
	// 目录（不存在 = fail closed），只有落在某个已授权工作区之内的项才生效；落在
	// 授权面之外的项被跳过并在 stderr 记一行诊断（它们本来就不能写）。
	Protected []string
	// Mode 是 winModeReadOnly 或 winModeWorkspaceWrite。
	Mode string
	// Probe 表示这是启动时功能探测，不执行任何子进程。
	Probe bool
	// Argv 是 "--" 之后要 spawn 的目标 argv。
	Argv []string
}

// parseWinACLArgs 解析 helper 参数（不含 HelperArgWindowsACL 本身）。
//
// 语法：
//
//	--probe
//	--workspace <dir> [--workspace <dir>...] [--protect <path>...]
//	--mode <read-only|workspace-write> -- <argv...>
//
// 任何缺失、重复歧义或未知参数都返回错误（helper 侧据此 fail closed）。
func parseWinACLArgs(args []string) (winACLPlan, error) {
	var plan winACLPlan
	index := 0
	separator := -1
	for index < len(args) {
		arg := args[index]
		if arg == winArgSeparator {
			separator = index
			break
		}
		switch arg {
		case winCommandProbe:
			if plan.Probe {
				return plan, errors.New("windows-acl: duplicate --probe")
			}
			plan.Probe = true
			index++
		case winFlagWorkspace:
			if index+1 >= len(args) || args[index+1] == winArgSeparator {
				return plan, errors.New("windows-acl: missing value after --workspace")
			}
			value := args[index+1]
			if strings.TrimSpace(value) == "" {
				return plan, errors.New("windows-acl: empty --workspace value")
			}
			plan.Workspaces = append(plan.Workspaces, value)
			index += 2
		case winFlagProtect:
			if index+1 >= len(args) || args[index+1] == winArgSeparator {
				return plan, errors.New("windows-acl: missing value after --protect")
			}
			value := args[index+1]
			if strings.TrimSpace(value) == "" {
				return plan, errors.New("windows-acl: empty --protect value")
			}
			plan.Protected = append(plan.Protected, value)
			index += 2
		case winFlagMode:
			if index+1 >= len(args) || args[index+1] == winArgSeparator {
				return plan, errors.New("windows-acl: missing value after --mode")
			}
			if plan.Mode != "" {
				return plan, errors.New("windows-acl: duplicate --mode")
			}
			plan.Mode = args[index+1]
			index += 2
		default:
			return plan, fmt.Errorf("windows-acl: unknown helper argument %q", arg)
		}
	}

	if plan.Probe {
		// 探测不接受任何运行参数：混入运行参数说明宿主实现有 bug，必须拒绝。
		if separator >= 0 || len(plan.Workspaces) != 0 || len(plan.Protected) != 0 || plan.Mode != "" {
			return plan, errors.New("windows-acl: --probe does not accept run arguments")
		}
		return plan, nil
	}
	if separator < 0 {
		return plan, errors.New(`windows-acl: missing "--" before the command argv`)
	}
	if len(plan.Workspaces) == 0 {
		return plan, errors.New("windows-acl: missing --workspace")
	}
	if _, err := windowsGrantShape(plan.Mode); err != nil {
		return plan, err
	}
	plan.Argv = append([]string(nil), args[separator+1:]...)
	if len(plan.Argv) == 0 {
		return plan, errors.New(`windows-acl: missing command after "--"`)
	}
	return plan, nil
}

// winGrantShape 描述某个模式对目录写入授权与低完整性的期望形状。
type winGrantShape struct {
	// WorkspaceWrite 表示给工作区根授予能力 SID 写+删权限。
	WorkspaceWrite bool
	// TempWrite 表示创建并授权一次运行的私有临时目录。
	TempWrite bool
	// LowIntegrityToken 表示把子进程令牌降到 Low 完整性。
	//
	// 两种模式都降：workspace-write 下配合授权目录上的 Low 标签，子进程写不进
	// 未被授权的 Medium 对象；read-only 下没有写授权也没有标签，Low 令牌意味着
	// 除本来就标为 Low 的对象外一律写不进去（即只读）。
	LowIntegrityToken bool
}

// windowsGrantShape 把 helper 模式映射为授权形状；未知模式返回错误。
func windowsGrantShape(mode string) (winGrantShape, error) {
	switch mode {
	case winModeReadOnly:
		return winGrantShape{
			WorkspaceWrite:    false,
			TempWrite:         false,
			LowIntegrityToken: true,
		}, nil
	case winModeWorkspaceWrite:
		return winGrantShape{
			WorkspaceWrite:    true,
			TempWrite:         true,
			LowIntegrityToken: true,
		}, nil
	default:
		return winGrantShape{}, fmt.Errorf("windows-acl: unknown mode %q (want %q or %q)", mode, winModeReadOnly, winModeWorkspaceWrite)
	}
}

// windowsRestrictingSIDStrings 返回受限令牌的 restricting SID 列表（字符串形式）。
//
// read-only 只有保活组 [登录 SID, Everyone]；workspace-write 在其后追加**每个**
// 工作区的能力 SID 与临时目录能力 SID（工作区 SID 由路径派生，见
// winWorkspaceCapabilitySID；临时目录 SID 每次运行随机）。保活组是刻意保留的：
// 没有它，早期 DLL 初始化会以 0xC0000142 失败，CNG 也会让 pwsh 崩溃——Everyone
// 自带的既有写授权是文档化的部分边界。
//
// 列表按出现顺序去重（大小写不敏感）：同一工作区被策略重复列出时不能出现两个
// 相同 SID（restricting 列表里的重复项没有意义，只会让诊断更难读）。
func windowsRestrictingSIDStrings(mode, logonSID, everyoneSID string, workspaceSIDs []string, tempSID string) ([]string, error) {
	if strings.TrimSpace(logonSID) == "" || strings.TrimSpace(everyoneSID) == "" {
		return nil, errors.New("windows-acl: restricting SID list requires the logon SID and Everyone")
	}
	out := make([]string, 0, 4+len(workspaceSIDs))
	seen := make(map[string]struct{}, 4+len(workspaceSIDs))
	add := func(sid string) {
		sid = strings.TrimSpace(sid)
		if sid == "" {
			return
		}
		key := strings.ToLower(sid)
		if _, exists := seen[key]; exists {
			return
		}
		seen[key] = struct{}{}
		out = append(out, sid)
	}
	switch mode {
	case winModeReadOnly:
		add(logonSID)
		add(everyoneSID)
		return out, nil
	case winModeWorkspaceWrite:
		if len(workspaceSIDs) == 0 {
			return nil, errors.New("windows-acl: workspace-write restricting list requires at least one workspace capability SID")
		}
		if strings.TrimSpace(tempSID) == "" {
			return nil, errors.New("windows-acl: workspace-write restricting list requires the temp capability SID")
		}
		add(logonSID)
		add(everyoneSID)
		for _, sid := range workspaceSIDs {
			if strings.TrimSpace(sid) == "" {
				return nil, errors.New("windows-acl: workspace capability SID list contains an empty SID")
			}
			add(sid)
		}
		add(tempSID)
		return out, nil
	default:
		return nil, fmt.Errorf("windows-acl: unknown mode %q", mode)
	}
}

// capabilitySIDFromReader 从一个随机源派生能力 SID：
// S-1-5-21-<a>-<b>-<c>-<d>（四个 32 位非零子权限）。
//
// 能力 SID 不需要对应任何真实账户：它只由“谁持有它”定义——令牌的 restricting
// 列表与目录 ACE 是它全部的权力来源。0 不是有意义的子权限，因此映射为 1。
func capabilitySIDFromReader(source io.Reader) (string, error) {
	var raw [16]byte
	if _, err := io.ReadFull(source, raw[:]); err != nil {
		return "", fmt.Errorf("windows-acl: read capability SID entropy: %w", err)
	}
	parts := make([]string, 4)
	for index := range parts {
		value := binary.LittleEndian.Uint32(raw[index*4:])
		if value == 0 {
			value = 1
		}
		parts[index] = strconv.FormatUint(uint64(value), 10)
	}
	return "S-1-5-21-" + strings.Join(parts, "-"), nil
}

// newCapabilitySID 用 crypto/rand 生成一个每次运行唯一的能力 SID。
func newCapabilitySID() (string, error) {
	return capabilitySIDFromReader(rand.Reader)
}

// winPathKey 返回 Windows 路径的比较键：大小写不敏感、分隔符统一、去尾部斜杠。
//
// 它只用于**比较与派生**（去重、包含判断的辅助、能力 SID 派生），不用于访问文件
// 系统：调用方传进来的路径已经由 Windows 侧的 filepath.Clean / os.Stat 规范过。
// 在 Linux 宿主上 clean 不会折叠 C:\a\..\b 这种"反斜杠路径"，因此这里只做
// 能跨宿主保持一致的三件事：TrimSpace、分隔符统一、大小写折叠。
func winPathKey(path string) string {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return ""
	}
	key := filepath.Clean(trimmed)
	key = strings.ReplaceAll(key, "\\", "/")
	for strings.Contains(key, "//") {
		key = strings.ReplaceAll(key, "//", "/")
	}
	// 去掉尾部斜杠，但保留文件系统根（"/"）与驱动器根（"C:/"）。
	if length := len(key); length > 1 && strings.HasSuffix(key, "/") {
		trimmedKey := strings.TrimRight(key, "/")
		if trimmedKey != "" && !strings.HasSuffix(trimmedKey, ":") && len(trimmedKey) > 1 {
			key = trimmedKey
		}
	}
	return strings.ToLower(key)
}

// winWorkspaceCapabilitySID 由规范化的工作区路径派生**确定性**能力 SID。
//
// 形状与随机能力 SID 一致（S-1-5-21-<a>-<b>-<c>-<d>），但取值只取决于路径：
// 同一目录无论大小写/分隔符如何书写、在哪个进程或会话里运行，都得到同一个 SID。
// 这是"跨进程/跨会话复用同一条 ACE"的前提（见 windows_acl_state.go 的缓存语义）：
// 目录上已经存在的 ACE 可以被后续运行直接认领，不必每次命令重写 ACL。
//
// 派生输入是 winPathKey（大小写折叠）。注意它是一个**路径键**而不是真实路径：
// 同一目录通过 junction/符号链接的另一种写法会得到不同的键——那种情况下两个键
// 各自对应一套 ACE，仍然安全（只是不能复用），因此不解析真实路径（解析失败会
// 让 helper 在奇怪的环境里 fail closed，代价更高）。
func winWorkspaceCapabilitySID(path string) (string, error) {
	key := winPathKey(path)
	if key == "" {
		return "", errors.New("windows-acl: cannot derive a capability SID from an empty workspace path")
	}
	sum := sha256.Sum256([]byte(key))
	return capabilitySIDFromReader(bytes.NewReader(sum[:16]))
}

// buildWindowsConfinedCommand 把 argv 包装成 Windows helper 前缀形式。
//
// 结果形状：
//
//	[os.Executable(), HelperArgWindowsACL,
//	 --workspace <root>..., --protect <path>...,
//	 --mode <read-only|workspace-write>,
//	 --, argv...]
//
// danger-full-access 原样返回 argv（EnforcementNone）；nil 策略、未知模式、空
// argv、无工作区根都 fail closed（包装 sandbox.ErrSandboxUnavailable）。
func buildWindowsConfinedCommand(executable string, argv []string, policy *sandbox.Policy) (sandbox.ConfinedCommand, error) {
	if executable == "" {
		return sandbox.ConfinedCommand{}, fmt.Errorf("osrunner: windows acl runner has no host executable: %w", sandbox.ErrSandboxUnavailable)
	}
	if policy == nil {
		return sandbox.ConfinedCommand{}, fmt.Errorf("osrunner: windows acl runner requires a policy: %w", sandbox.ErrSandboxUnavailable)
	}
	switch policy.Mode() {
	case sandbox.ModeDangerFullAccess:
		return passthrough(argv), nil
	case sandbox.ModeReadOnly:
		// read-only 连工作区都不授权，保护路径没有可保护的写面，因此不下发。
		return windowsConfinedCommand(executable, argv, policy.Roots(), nil, winModeReadOnly)
	case sandbox.ModeWorkspaceWrite:
		return windowsConfinedCommand(executable, argv, policy.Roots(), policy.ProtectedPaths(), winModeWorkspaceWrite)
	default:
		return sandbox.ConfinedCommand{}, fmt.Errorf("osrunner: windows acl runner does not support mode %q: %w", policy.Mode(), sandbox.ErrSandboxUnavailable)
	}
}

// windowsConfinedCommand 构造一次 argv 前缀包装。
func windowsConfinedCommand(executable string, argv, roots, protected []string, mode string) (sandbox.ConfinedCommand, error) {
	if len(argv) == 0 {
		return sandbox.ConfinedCommand{}, fmt.Errorf("osrunner: windows acl runner requires a command: %w", sandbox.ErrSandboxUnavailable)
	}
	if _, err := windowsGrantShape(mode); err != nil {
		return sandbox.ConfinedCommand{}, fmt.Errorf("osrunner: windows acl runner: %v: %w", err, sandbox.ErrSandboxUnavailable)
	}
	workspaces, err := windowsWorkspaceRoots(roots)
	if err != nil {
		return sandbox.ConfinedCommand{}, err
	}
	protects := windowsProtectedPaths(protected)
	wrapped := make([]string, 0, 2+2*len(workspaces)+2*len(protects)+3+len(argv))
	wrapped = append(wrapped, executable, HelperArgWindowsACL)
	for _, workspace := range workspaces {
		wrapped = append(wrapped, winFlagWorkspace, workspace)
	}
	for _, path := range protects {
		wrapped = append(wrapped, winFlagProtect, path)
	}
	wrapped = append(wrapped, winFlagMode, mode, winArgSeparator)
	wrapped = append(wrapped, argv...)
	return sandbox.ConfinedCommand{
		Argv:             wrapped,
		Enforcement:      sandbox.EnforcementPartial,
		DenialSignatures: DenialSignatures(RunnerWindowsACL),
	}, nil
}

// windowsProtectedPaths 规范化保护路径：去空白与空项、Clean、去重（保序、大小写
// 不敏感）。
//
// 与 windowsWorkspaceRoots 不同，空列表是合法的：没有保护路径就是没有。这里只做
// 形状规范化；**校验**（必须存在、必须是目录、必须落在已授权工作区之内）在 helper
// 侧的 winResolveProtectedPaths 里做——计划层在宿主上不知道工作区的真实存在性。
func windowsProtectedPaths(paths []string) []string {
	out := make([]string, 0, len(paths))
	seen := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		path = filepath.Clean(path)
		key := winPathKey(path)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, path)
	}
	return out
}

// winProtectTarget 是一条要强制的保护路径，以及它所归属的工作区根。
//
// 归属工作区决定用哪个 capability SID 写 deny ACE：受限令牌的 restricting 列表
// 包含所有工作区 SID，因此任一工作区 SID 的 deny ACE 都能拒绝该子进程的写；取
// 归属工作区只是让"同一目录的 ACE 跟着同一把钥匙走"，便于跨运行复用。
type winProtectTarget struct {
	// Path 是规范化后的保护路径（绝对、Clean）。
	Path string
	// Workspace 是包含它的工作区根（即策略写根）。
	Workspace string
}

// winProtectResolution 是 --protect 的解析/校验结果。
type winProtectResolution struct {
	// Enforced 是会真正加 deny ACE 的保护路径（按所属工作区分组）。
	Enforced []winProtectTarget
	// Notes 是要打印到 stderr 的诊断行（只对"落在授权面之外"的路径产生，不是错误）。
	Notes []string
}

// For 返回归属某个工作区的保护路径（保序）。
func (resolution winProtectResolution) For(workspace string) []string {
	key := winPathKey(workspace)
	var out []string
	for _, target := range resolution.Enforced {
		if winPathKey(target.Workspace) == key {
			out = append(out, target.Path)
		}
	}
	return out
}

// winResolveProtectedPaths 校验并规范化 helper 收到的 --protect 值。
//
// 规则（fail closed 优先）：
//   - 必须是绝对路径（相对路径的语义取决于 helper 的 CWD，不可信）；
//   - 必须存在且是**目录**：保护一个不存在的东西没有意义，静默忽略又会让调用方
//     以为已经受保护，所以直接报错；
//   - 等于某个工作区根本身是自相矛盾的（既授权写整个工作区、又整棵树只读），
//     报错而不是猜调用方想要哪一个；
//   - 落在**所有**已授权工作区之外时不产生 ACE（那本来就不能写），跳过并记一行
//     诊断，不算错误；
//   - 大小写不敏感去重（Windows 路径语义）。
//
// 归属工作区取包含它的**最具体**（最长）那个根：嵌套工作区时宁可让 ACE 跟着更
// 精确的那把钥匙。
func winResolveProtectedPaths(protected, workspaces []string) (winProtectResolution, error) {
	var resolution winProtectResolution
	seen := make(map[string]struct{}, len(protected))
	for _, raw := range protected {
		path := strings.TrimSpace(raw)
		if path == "" {
			continue
		}
		path = filepath.Clean(path)
		// 先按路径键去重再校验：同一目录的大小写/分隔符变体只验证一次。
		key := winPathKey(path)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		if !filepath.IsAbs(path) {
			return winProtectResolution{}, fmt.Errorf("windows-acl: protected path %q is not absolute", raw)
		}
		info, err := os.Stat(path)
		if err != nil {
			return winProtectResolution{}, fmt.Errorf("windows-acl: protected path %q: %w", path, err)
		}
		if !info.IsDir() {
			return winProtectResolution{}, fmt.Errorf("windows-acl: protected path %q is not a directory", path)
		}
		workspace, ok := winInnermostWorkspace(workspaces, path)
		if !ok {
			resolution.Notes = append(resolution.Notes,
				fmt.Sprintf("windows-acl: protected path %q is outside every granted workspace; skipping (it is not writable anyway)", path))
			continue
		}
		if winPathKey(workspace) == key {
			return winProtectResolution{}, fmt.Errorf("windows-acl: protected path %q is the granted workspace root itself", path)
		}
		resolution.Enforced = append(resolution.Enforced, winProtectTarget{Path: path, Workspace: workspace})
	}
	return resolution, nil
}

// winInnermostWorkspace 返回包含 path 的最具体工作区根（纯路径判断，不解析链接）。
func winInnermostWorkspace(workspaces []string, path string) (string, bool) {
	best := ""
	for _, workspace := range workspaces {
		workspace = filepath.Clean(strings.TrimSpace(workspace))
		if workspace == "" || workspace == "." {
			continue
		}
		if !winPathWithinFold(workspace, path) {
			continue
		}
		if len(winPathKey(workspace)) > len(winPathKey(best)) {
			best = workspace
		}
	}
	return best, best != ""
}

// windowsWorkspaceRoots 规范化策略根：去空白与空项、Clean、去重。
//
// 策略根在 sandbox.Policy 构造时已经是真实绝对路径，这里只做防御性清洗。
// 一个都没有时 fail closed：没有工作区的 workspace-write 无法表达授权面。
func windowsWorkspaceRoots(roots []string) ([]string, error) {
	out := make([]string, 0, len(roots))
	seen := make(map[string]struct{}, len(roots))
	for _, root := range roots {
		root = strings.TrimSpace(root)
		if root == "" {
			continue
		}
		root = filepath.Clean(root)
		// Windows 路径大小写不敏感：同一目录的两种写法不能各拿一条授权。
		key := winPathKey(root)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, root)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("osrunner: windows acl runner requires at least one workspace root: %w", sandbox.ErrSandboxUnavailable)
	}
	return out, nil
}

// windowsPathWithin 报告 candidate 是否等于 root 或位于 root 之下（纯路径判断，
// 不解析符号链接/重解析点）。
func windowsPathWithin(root, candidate string) bool {
	rel, err := filepath.Rel(root, candidate)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// windowsTempRootUsable 报告临时根是否可用：它不能位于任何一个工作区根之内，
// 否则私有临时目录会继承工作区授权，把两个授权面合并成一个。
func windowsTempRootUsable(workspaces []string, tempRoot string) bool {
	if strings.TrimSpace(tempRoot) == "" {
		return false
	}
	tempRoot = filepath.Clean(tempRoot)
	for _, workspace := range workspaces {
		workspace = filepath.Clean(workspace)
		if windowsPathWithin(workspace, tempRoot) {
			return false
		}
	}
	return true
}

// winTempBaseCandidates 返回 Windows 私有临时目录的基址候选，按优先级：
//
//  1. RIG_SANDBOX_TEMP（显式覆盖，便于受限环境指定可写目录）
//  2. extra：由 Windows 自己的 temp 机制提供（GetTempPath2W/GetTempPathW 与
//     用户 Known Folder 的 LocalAppData\Temp；见 windows_folders.go）
//  3. os.TempDir()（最后手段）
//
// SystemRoot/WINDIR 之下的候选会被剔除：Windows 的 GetTempPath 在 TEMP/TMP
// 未设置时会回退到 C:\WINDOWS，普通用户无法在那里创建目录；把它当临时基址
// 会让 helper 在 ACL 初始化阶段直接失败（退出码 127），命令完全跑不起来。
// 调用方（helper）还会用 Win32 GetWindowsDirectoryW 再过滤一次，即使环境变量
// 也被裁剪掉了。
func winTempBaseCandidates(getenv func(string) string, osTempDir string, extra ...string) []string {
	if getenv == nil {
		getenv = func(string) string { return "" }
	}
	var candidates []string
	seen := make(map[string]struct{}, 4)
	add := func(candidate string) {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			return
		}
		candidate = filepath.Clean(candidate)
		if candidate == "." {
			return
		}
		// Windows 路径大小写不敏感，且 / 与 \ 等价；按规范化后的键去重。
		key := strings.ToLower(strings.ReplaceAll(candidate, "\\", "/"))
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		candidates = append(candidates, candidate)
	}
	add(getenv("RIG_SANDBOX_TEMP"))
	for _, candidate := range extra {
		add(candidate)
	}
	add(osTempDir)
	systemRoot := strings.TrimSpace(getenv("SystemRoot"))
	if systemRoot == "" {
		systemRoot = strings.TrimSpace(getenv("WINDIR"))
	}
	if systemRoot != "" {
		filtered := candidates[:0]
		for _, candidate := range candidates {
			if winPathWithinFold(systemRoot, candidate) {
				continue
			}
			filtered = append(filtered, candidate)
		}
		candidates = filtered
	}
	return candidates
}

// winPathWithinFold 是不依赖宿主文件系统的、大小写不敏感的前缀包含判断，供
// 纯计划层过滤 Windows 路径候选（Linux 测试里也能用 C:\... 字符串验证）。
func winPathWithinFold(root, candidate string) bool {
	normalize := func(value string) string {
		value = strings.ReplaceAll(filepath.Clean(value), "\\", "/")
		return strings.ToLower(strings.TrimRight(value, "/"))
	}
	root = normalize(root)
	candidate = normalize(candidate)
	if root == "" {
		return false
	}
	return candidate == root || strings.HasPrefix(candidate, root+"/")
}
