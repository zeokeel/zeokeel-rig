//go:build linux

package osrunner

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"github.com/zeokeel/zeokeel-rig/sandbox"
)

// Landlock 规则与 ABI 常量。
const (
	// landlockCreateRulesetVersion 让 landlock_create_ruleset(2) 返回内核 ABI 版本。
	landlockCreateRulesetVersion = 1
	// landlockRulePathBeneath 是 path-beneath 规则类型。
	landlockRulePathBeneath = 1

	// 文件系统访问权位。ABI 1 定义 0..12；REFER、TRUNCATE、IOCTL_DEV 分别由
	// ABI 2、3、5 引入。
	landlockAccessFSExecute    = uint64(1 << 0)
	landlockAccessFSWriteFile  = uint64(1 << 1)
	landlockAccessFSReadFile   = uint64(1 << 2)
	landlockAccessFSReadDir    = uint64(1 << 3)
	landlockAccessFSRemoveDir  = uint64(1 << 4)
	landlockAccessFSRemoveFile = uint64(1 << 5)
	landlockAccessFSMakeChar   = uint64(1 << 6)
	landlockAccessFSMakeDir    = uint64(1 << 7)
	landlockAccessFSMakeReg    = uint64(1 << 8)
	landlockAccessFSMakeSock   = uint64(1 << 9)
	landlockAccessFSMakeFIFO   = uint64(1 << 10)
	landlockAccessFSMakeBlock  = uint64(1 << 11)
	landlockAccessFSMakeSym    = uint64(1 << 12)
	landlockAccessFSRefer      = uint64(1 << 13) // ABI 2
	landlockAccessFSTruncate   = uint64(1 << 14) // ABI 3
	landlockAccessFSIoctlDev   = uint64(1 << 15) // ABI 5

	// oPath 是 O_PATH 打开标志（0x200000）。它在所有 Linux 架构上相同，但
	// syscall 包在 amd64 上没有导出该常量，因此这里本地定义。
	oPath = 0x200000

	// landlockProbePrefix 是 --probe 子命令输出中 ABI 报告的前缀。
	landlockProbePrefix = "rig-sandbox: landlock ABI="
	// landlockHelperExitCode 是 helper 任何失败时的退出码。
	landlockHelperExitCode = 125
	// landlockFailPrefix 是 helper 失败输出的固定前缀（便于宿主识别 runner 自身
	// 失败而不是被限制命令的失败）。
	landlockFailPrefix = "rig-sandbox: "
)

// landlockRunner 是用内置 helper 施加 Landlock 域限制的纯 Go runner。
//
// 它自己不包装目标命令的系统调用，而是把 [exe, HelperArgLandlock, --ro ..., --rw
// ..., --, argv...] 交给宿主二进制；宿主在 main 最前面调用 MaybeRunHelper 后，
// 同一个可执行文件以 helper 身份施加 ruleset 再 syscall.Exec 目标命令。
// Enforcement 取决于探测时协商到的 ABI：只有 REFER 与 TRUNCATE 都受治理
// （ABI >= 3）才报告 full，否则报告 partial。
type landlockRunner struct {
	// exe 是宿主可执行文件（helper 的 argv[0]）。
	exe string
	// abi 是选择 runner 时协商到的 Landlock ABI。
	abi int
}

var _ sandbox.Runner = (*landlockRunner)(nil)

// Confine 实现 sandbox.Runner。
func (r *landlockRunner) Confine(argv []string, policy *sandbox.Policy) (sandbox.ConfinedCommand, error) {
	if policy == nil {
		return sandbox.ConfinedCommand{}, fmt.Errorf("osrunner: landlock runner requires a policy: %w", sandbox.ErrSandboxUnavailable)
	}
	if r.exe == "" {
		return sandbox.ConfinedCommand{}, fmt.Errorf("osrunner: landlock runner has no host executable: %w", sandbox.ErrSandboxUnavailable)
	}
	mode := policy.Mode()
	if mode == sandbox.ModeDangerFullAccess {
		return passthrough(argv), nil
	}
	if mode != sandbox.ModeReadOnly && mode != sandbox.ModeWorkspaceWrite {
		return sandbox.ConfinedCommand{}, fmt.Errorf("osrunner: landlock runner: unsupported policy mode %q: %w", mode, sandbox.ErrSandboxUnavailable)
	}
	if len(argv) == 0 {
		return sandbox.ConfinedCommand{}, fmt.Errorf("osrunner: landlock runner: empty argv: %w", sandbox.ErrSandboxUnavailable)
	}

	readOnly, readWrite := LandlockGrants(policy)
	trailer := make([]string, 0, 2*(len(readOnly)+len(readWrite))+1)
	for _, path := range readOnly {
		trailer = append(trailer, "--ro", path)
	}
	for _, path := range readWrite {
		trailer = append(trailer, "--rw", path)
	}
	trailer = append(trailer, "--")
	return wrapped(
		[]string{r.exe, HelperArgLandlock},
		trailer,
		argv,
		landlockEnforcement(r.abi),
		DenialSignatures(RunnerLandlock),
	), nil
}

// handledAccessMask 返回指定 ABI 下内核理解的全部文件系统访问权位。
//
// ABI 1 定义 0..12；ABI 2 追加 REFER（跨目录 rename/link），ABI 3 追加 TRUNCATE，
// ABI 5 追加 IOCTL_DEV。ABI 4 只增加网络访问权，不在 handled_access_fs 中，因此
// 文件系统掩码与 ABI 3 相同。
func handledAccessMask(abi int) uint64 {
	if abi < 1 {
		return 0
	}
	mask := landlockAccessFSExecute |
		landlockAccessFSWriteFile |
		landlockAccessFSReadFile |
		landlockAccessFSReadDir |
		landlockAccessFSRemoveDir |
		landlockAccessFSRemoveFile |
		landlockAccessFSMakeChar |
		landlockAccessFSMakeDir |
		landlockAccessFSMakeReg |
		landlockAccessFSMakeSock |
		landlockAccessFSMakeFIFO |
		landlockAccessFSMakeBlock |
		landlockAccessFSMakeSym
	if abi >= 2 {
		mask |= landlockAccessFSRefer
	}
	if abi >= 3 {
		mask |= landlockAccessFSTruncate
	}
	if abi >= 5 {
		mask |= landlockAccessFSIoctlDev
	}
	return mask
}

// landlockEnforcement 返回指定 ABI 下的实际覆盖程度。
//
// 只有 ABI >= 3 时 REFER 与 TRUNCATE 都受治理，写入效果才完整；更老的 ABI 下
// 跨目录 rename/link 与 truncate(2) 不受 Landlock 约束，因此报告 partial。
func landlockEnforcement(abi int) sandbox.Enforcement {
	if abi >= 3 {
		return sandbox.EnforcementFull
	}
	return sandbox.EnforcementPartial
}

// landlockRulesetAttr 是 struct landlock_ruleset_attr 的 Go 形式。
type landlockRulesetAttr struct {
	handledAccessFS uint64
}

// landlockGrant 是一条 path-beneath 规则：path 上放行 access 位。
type landlockGrant struct {
	path   string
	access uint64
}

// queryLandlockABI 返回内核协商到的 Landlock ABI；内核不支持时返回错误。
func queryLandlockABI() (int, error) {
	abi, err := landlockCreateRuleset(nil, 0, landlockCreateRulesetVersion)
	if err != nil {
		return 0, fmt.Errorf("landlock_create_ruleset(LANDLOCK_CREATE_RULESET_VERSION): %w", err)
	}
	return abi, nil
}

// applyLandlockRuleset 新建 ruleset、逐条添加 path-beneath 规则，然后
// PR_SET_NO_NEW_PRIVS + landlock_restrict_self 把域施加到当前进程。
func applyLandlockRuleset(handled uint64, grants []landlockGrant) error {
	if handled == 0 {
		return errors.New("landlock: no handled filesystem access for this kernel ABI")
	}
	attr := landlockRulesetAttr{handledAccessFS: handled}
	rulesetFD, err := landlockCreateRuleset(unsafe.Pointer(&attr), unsafe.Sizeof(attr), 0)
	if err != nil {
		return fmt.Errorf("landlock_create_ruleset: %w", err)
	}
	defer func() { _ = syscall.Close(rulesetFD) }()
	for _, grant := range grants {
		if err := addLandlockPathRule(rulesetFD, grant.path, grant.access&handled); err != nil {
			return err
		}
	}
	if err := landlockSetNoNewPrivileges(); err != nil {
		return fmt.Errorf("prctl(PR_SET_NO_NEW_PRIVS): %w", err)
	}
	if err := landlockRestrictSelf(rulesetFD, 0); err != nil {
		return fmt.Errorf("landlock_restrict_self: %w", err)
	}
	return nil
}

// landlockFileAccessMask 是可以施加在单个文件（非目录）上的访问权。
//
// MAKE_* 与 REMOVE_* 作用于父目录，对文件规则内核返回 EINVAL；因此对 /dev/null
// 这类文件目标要先裁掉目录专用位，否则整条规则失败。
const landlockFileAccessMask = landlockAccessFSExecute |
	landlockAccessFSWriteFile |
	landlockAccessFSReadFile |
	landlockAccessFSTruncate |
	landlockAccessFSIoctlDev

// addLandlockPathRule 用 O_PATH 打开 path，并添加一条 path-beneath 规则。
//
// struct landlock_path_beneath_attr 在内核里是 packed 的 12 字节
// （__u64 allowed_access + __s32 parent_fd），Go struct 会补齐到 16 字节，因此这里
// 显式构造 12 字节缓冲区，而不是传 &struct 指针。文件目标只保留文件适用的访问位。
func addLandlockPathRule(rulesetFD int, path string, access uint64) error {
	if access == 0 {
		return nil
	}
	fd, err := syscall.Open(path, oPath|syscall.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open %q: %w", path, err)
	}
	defer func() { _ = syscall.Close(fd) }()

	var stat syscall.Stat_t
	if err := syscall.Fstat(fd, &stat); err != nil {
		return fmt.Errorf("fstat %q: %w", path, err)
	}
	if stat.Mode&syscall.S_IFMT != syscall.S_IFDIR {
		access &= landlockFileAccessMask
		if access == 0 {
			return nil
		}
	}

	var attr [12]byte
	// amd64 与 arm64 都是小端；Landlock 属性使用原生字节序。
	binary.LittleEndian.PutUint64(attr[0:8], access)
	binary.LittleEndian.PutUint32(attr[8:12], uint32(fd))
	if err := landlockAddRule(rulesetFD, landlockRulePathBeneath, unsafe.Pointer(&attr[0]), 0); err != nil {
		return fmt.Errorf("landlock_add_rule(%q): %w", path, err)
	}
	return nil
}

// landlockHelperConfig 是 helper 从 argv 解析出的授权与目标命令。
type landlockHelperConfig struct {
	readOnly  []string
	readWrite []string
	argv      []string
}

// parseLandlockHelperArgs 解析 helper 参数：
//
//	[--ro path]... [--rw path]... -- argv...
//
// 任何缺失、空路径或未知参数都 fail closed（返回错误），绝不猜测目标命令。
func parseLandlockHelperArgs(args []string) (landlockHelperConfig, error) {
	var cfg landlockHelperConfig
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--":
			if i+1 >= len(args) {
				return cfg, errors.New("landlock helper: missing command after --")
			}
			cfg.argv = append([]string(nil), args[i+1:]...)
			return cfg, nil
		case "--ro", "--rw":
			flag := args[i]
			if i+1 >= len(args) {
				return cfg, fmt.Errorf("landlock helper: %s requires a path", flag)
			}
			path := args[i+1]
			if path == "" {
				return cfg, fmt.Errorf("landlock helper: %s requires a non-empty path", flag)
			}
			i++
			if flag == "--ro" {
				cfg.readOnly = append(cfg.readOnly, path)
			} else {
				cfg.readWrite = append(cfg.readWrite, path)
			}
		case "--probe":
			return cfg, errors.New("landlock helper: --probe cannot be combined with a target command")
		default:
			return cfg, fmt.Errorf("landlock helper: unexpected argument %q", args[i])
		}
	}
	return cfg, errors.New("landlock helper: missing -- command separator")
}

// runLandlockHelper 执行 helper 子命令并返回退出码。
//
// 正常路径下施加规则后 syscall.Exec 会覆盖当前进程、永不返回；返回 125 表示
// helper 自身失败（参数错误、内核拒绝、exec 失败），此时绝不回退到裸跑。
func runLandlockHelper(args []string) int {
	if len(args) == 1 && args[0] == "--probe" {
		return runLandlockProbe()
	}
	cfg, err := parseLandlockHelperArgs(args)
	if err != nil {
		return landlockHelperFail(err)
	}
	abi, err := queryLandlockABI()
	if err != nil {
		return landlockHelperFail(err)
	}
	handled := handledAccessMask(abi)
	if handled == 0 {
		return landlockHelperFail(fmt.Errorf("kernel Landlock ABI %d exposes no filesystem access", abi))
	}
	grants := make([]landlockGrant, 0, len(cfg.readOnly)+len(cfg.readWrite))
	for _, path := range cfg.readOnly {
		grants = append(grants, landlockGrant{
			path:   path,
			access: landlockAccessFSExecute | landlockAccessFSReadFile | landlockAccessFSReadDir,
		})
	}
	for _, path := range cfg.readWrite {
		grants = append(grants, landlockGrant{path: path, access: handled})
	}
	if err := applyLandlockRuleset(handled, grants); err != nil {
		return landlockHelperFail(err)
	}
	if err := syscall.Exec(cfg.argv[0], cfg.argv, os.Environ()); err != nil {
		return landlockHelperFail(fmt.Errorf("exec %q: %w", cfg.argv[0], err))
	}
	return 0
}

// runLandlockProbe 构建最大权限集合的 ruleset 并自我限制，验证内核支持后报告
// 协商到的 ABI。它不执行任何目标命令。
func runLandlockProbe() int {
	abi, err := queryLandlockABI()
	if err != nil {
		return landlockHelperFail(err)
	}
	if abi < 1 {
		return landlockHelperFail(fmt.Errorf("kernel Landlock ABI unavailable"))
	}
	handled := handledAccessMask(abi)
	grants := []landlockGrant{{
		path:   "/",
		access: landlockAccessFSExecute | landlockAccessFSReadFile | landlockAccessFSReadDir,
	}}
	if err := applyLandlockRuleset(handled, grants); err != nil {
		return landlockHelperFail(err)
	}
	fmt.Printf("%s%d\n", landlockProbePrefix, abi)
	return 0
}

// landlockHelperFail 打印带固定前缀的失败详情并返回 helper 失败退出码。
func landlockHelperFail(err error) int {
	fmt.Fprintf(os.Stderr, "%s%v\n", landlockFailPrefix, err)
	return landlockHelperExitCode
}

// probeLandlock 通过 helper 子进程做一次真实的功能探测：创建 ruleset、施加规则
// 并自我限制，成功后返回协商到的 ABI。任何失败都表示本机不可用（不区分原因）。
func probeLandlock(timeout time.Duration) (int, bool) {
	if !landlockSupported {
		return 0, false
	}
	exe, err := os.Executable()
	if err != nil {
		return 0, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, exe, HelperArgLandlock, "--probe").Output()
	if err != nil {
		return 0, false
	}
	return parseLandlockProbeOutput(string(out))
}

// parseLandlockProbeOutput 从 helper --probe 的输出中解析 ABI 版本。
func parseLandlockProbeOutput(output string) (int, bool) {
	index := strings.LastIndex(output, landlockProbePrefix)
	if index < 0 {
		return 0, false
	}
	rest := strings.TrimSpace(output[index+len(landlockProbePrefix):])
	abi, err := strconv.Atoi(rest)
	if err != nil || abi < 1 {
		return 0, false
	}
	return abi, true
}
