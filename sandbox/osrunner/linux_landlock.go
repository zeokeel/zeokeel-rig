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

	"github.com/zeokeel/zeokeel-rig/internal/procattr"
	"github.com/zeokeel/zeokeel-rig/internal/seccomp"
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

	// 网络访问权（ABI 4 引入）。Landlock 只能治理 TCP bind/connect；unix socket
	// 与 UDP 完全不在它的治理范围内（见 landlockNetworkCoverage）。
	landlockAccessNetBindTCP    = uint64(1 << 0)
	landlockAccessNetConnectTCP = uint64(1 << 1)

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
//
// Enforcement 恒为 partial：Landlock 治理文件的读/写/建/删，但完全不治理
// chmod/chown/utimensat 这类元数据变更（见 landlockEnforcement）；协商到的 ABI
// 只影响受治理的访问位数量，不改变这个结论。
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

	// Landlock 的 O_PATH 规则要求路径存在：先补建缺失的缓存目录（只建目录、
	// 0700；失败不阻断，留在 MissingToolchainCaches 上报）。
	ensureToolchainCacheRoots(policy)
	// 保护路径缺失 → 拒绝执行（Landlock 的 O_PATH 规则要求目标存在）。
	if profile, profileErr := BuildProfile(policy); profileErr == nil {
		if missingErr := checkMissingProtected(profile, policy, RunnerLandlock); missingErr != nil {
			return sandbox.ConfinedCommand{}, missingErr
		}
	}
	readOnly, readWrite := LandlockGrants(policy)
	trailer := make([]string, 0, 2*(len(readOnly)+len(readWrite))+2)
	for _, path := range readOnly {
		trailer = append(trailer, "--ro", path)
	}
	for _, path := range readWrite {
		trailer = append(trailer, "--rw", path)
	}
	// 网络：策略不允许时施加网络域（TCP bind/connect 默认拒绝）。ABI < 4 时
	// helper 会打一行提示并继续——文件规则仍然有效，RunnerLimits 会说明网络没隔离。
	if !policy.NetworkAllowed() {
		trailer = append(trailer, "--no-net")
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

// handledNetMask 返回指定 ABI 下可以治理的网络访问位：ABI 4 起支持 TCP
// bind/connect；更老的 ABI 完全没有网络治理能力（返回 0）。
func handledNetMask(abi int) uint64 {
	if abi < 4 {
		return 0
	}
	return landlockAccessNetBindTCP | landlockAccessNetConnectTCP
}

// landlockNetworkCoverage 描述本后端对 `AllowNetwork: false` 的实际覆盖度。
//
//   - full：ABI >= 4，TCP bind/connect 都被治理且不建任何放行规则（默认拒绝）；
//   - partial：ABI < 4，完全无法治理网络；
//   - 另外 unix socket（docker/podman/containerd 等宿主守护进程）在任何 ABI 下
//     都不受治理，见 RunnerLimits。
func landlockNetworkCoverage(abi int) string {
	if handledNetMask(abi) == 0 {
		return "none"
	}
	return "full"
}

// landlockEnforcement 返回指定 ABI 下的实际覆盖程度：始终是 partial。
//
// Landlock 的 handled_access_fs 里**没有**元数据变更位：chmod(2)、chown(2)、
// utimensat(2)、setxattr(2) 完全不受 Landlock 治理（已实测：read-only 域下仍能
// chmod/chown 任意路径，包括工作区外的文件）。因此即便 ABI >= 3（REFER 与
// TRUNCATE 都受治理、"内容写"这个维度已经完整），把整体报告成 full 也会高估
// 承诺——调用方据此以为“文件效果全被治理”就会漏掉权限位被改的风险。
//
// 取舍：默认按 partial 实现。若将来要把“内容写”维度单独说成 full，需要新增一个
// 分维度的覆盖报告（例如 ContentEnforcement），而不是放宽这里的整体判定。
func landlockEnforcement(_ int) sandbox.Enforcement {
	return sandbox.EnforcementPartial
}

// landlockRulesetAttr 是 struct landlock_ruleset_attr 的 Go 形式。
//
// handled_access_net 由 ABI 4 引入：它紧跟 handled_access_fs。内核按传入的 size
// 判断结构体版本，所以不治理网络时只传 8 字节（老内核也接受），治理时传 16 字节。
type landlockRulesetAttr struct {
	handledAccessFS  uint64
	handledAccessNet uint64
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
func applyLandlockRuleset(handled, handledNet uint64, grants []landlockGrant) error {
	if handled == 0 {
		return errors.New("landlock: no handled filesystem access for this kernel ABI")
	}
	attr := landlockRulesetAttr{handledAccessFS: handled, handledAccessNet: handledNet}
	size := unsafe.Sizeof(attr.handledAccessFS)
	if handledNet != 0 {
		size = unsafe.Sizeof(attr)
	}
	rulesetFD, err := landlockCreateRuleset(unsafe.Pointer(&attr), size, 0)
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
		if errors.Is(err, syscall.ENOENT) {
			// 保护路径展开会在宿主上枚举目录，被枚举的条目可能在 Confine 与
			// helper 执行之间消失。跳过缺失的授权路径是 fail closed 的（少一条
			// 写授权只会更严），因此这里不让整条命令失败。
			return nil
		}
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
	// noNetwork 表示策略不允许对外网络：施加网络域（TCP bind/connect 全部默认拒）。
	noNetwork bool
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
		case "--no-net":
			cfg.noNetwork = true
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
	handledNet := uint64(0)
	if cfg.noNetwork {
		handledNet = handledNetMask(abi)
		if handledNet == 0 {
			// ABI < 4：内核根本不支持网络治理。这不是失败路径——文件规则照样
			// 生效，只是网络没被隔离；宿主通过 RunnerLimits 让调用方知道。
			landlockWarnNoNetwork(abi)
		}
	}
	if err := applyLandlockRuleset(handled, handledNet, grants); err != nil {
		return landlockHelperFail(err)
	}
	// seccomp：Landlock 管不到 unix socket（只治理 TCP bind/connect），而宿主守护
	// 进程 socket（docker/podman/containerd/ssh-agent/dbus/…）是"完全逃逸面"。
	// helper 就是 re-exec 出来的子进程，在这里装过滤器不需要任何额外进程，也不
	// 需要宿主 main 配合——这是 Landlock 后端拿到 seccomp 的唯一低成本位置。
	if cfg.noNetwork {
		program, progErr := seccompProgram(false)
		if progErr != nil {
			// 策略要求断网却装不上过滤器：fail closed，不 exec。
			return landlockHelperFail(progErr)
		}
		if installErr := seccomp.Install(program); installErr != nil {
			return landlockHelperFail(fmt.Errorf("install seccomp network filter: %w", installErr))
		}
	}
	if err := syscall.Exec(cfg.argv[0], cfg.argv, os.Environ()); err != nil {
		return landlockHelperFail(fmt.Errorf("exec %q: %w", cfg.argv[0], err))
	}
	return 0
}

// landlockWarnNoNetwork 在 ABI < 4（无法治理网络）时向 stderr 打一行提示。
// 只提示不失败：文件围栏仍然有效，把它变成硬错误会让老内核完全不可用。
func landlockWarnNoNetwork(abi int) {
	fmt.Fprintf(os.Stderr, "%slandlock: ABI %d cannot govern network traffic; outbound connections are NOT isolated\n",
		landlockFailPrefix, abi)
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
	if err := applyLandlockRuleset(handled, 0, grants); err != nil {
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
	cmd := exec.CommandContext(ctx, exe, HelperArgLandlock, "--probe")
	procattr.HideWindow(cmd)
	out, err := cmd.Output()
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

// RunnerName 实现 sandbox.Reporter。
func (r *landlockRunner) RunnerName() string { return RunnerLandlock }

// RunnerEnforcement 实现 sandbox.Reporter：Landlock 恒为 partial——它治理文件的
// 读/写/建/删，但完全不治理 chmod/chown/utimensat 这类元数据变更。
func (r *landlockRunner) RunnerEnforcement() sandbox.Enforcement {
	return landlockEnforcement(r.abi)
}

// RunnerLimits 实现 sandbox.Reporter：说明 partial 的具体含义。
//
// 两句都必须说：① 元数据变更不受治理；② 并集语义导致"根内只读子路径"无法表达，
// git 元数据（.git）为了能提交而整根可写——即 `.git/config`、`.git/hooks` 在
// Landlock 下**不受 OS 层保护**（进程内围栏、bwrap、Seatbelt 与 Windows ACL 仍然
// 保护它们）。把这条藏起来会让调用方误以为四个后端的安全属性完全一样。
func (r *landlockRunner) RunnerLimits() string {
	gitLimit := ".git is granted read-write so `git commit` works, so .git/config and .git/hooks are writable at the OS layer here (the in-process fence, bwrap, Seatbelt and the Windows ACL backend still deny them)"
	if !LandlockGitDegradeEnabled() {
		gitLimit = ".git is NOT granted read-write (WithLandlockGitDegrade(false)), so .git/config and .git/hooks are read-only at the OS layer here — the price is that `git commit` cannot create .git/index.lock and fails in this backend"
	}
	limits := "metadata changes (chmod/chown/utimensat) are not governed, and union path-beneath rules cannot express a read-only subpath inside a writable root: " + gitLimit
	switch landlockNetworkCoverage(r.abi) {
	case "full":
		limits += "; TCP bind/connect are denied when the policy disallows network (ABI >= 4)"
	default:
		limits += "; network is NOT isolated in this backend (Landlock ABI < 4 has no net governance)"
	}
	limits += "; Landlock itself never governs unix sockets, and host daemon sockets (docker/podman/containerd/ssh-agent/dbus) are a host-action escape — when the policy disallows network the helper installs a seccomp filter that denies connect(2) with EPERM, so those sockets are unreachable here too (loopback included); a seccomp-less invocation (host built without the helper, or a non-Linux kernel) does not have that protection"
	return limits
}

// NetworkCoverage 实现 sandbox.NetworkReporter：ABI >= 4 可治理 TCP bind/connect
// （"full"），更老的 ABI 完全没有网络治理（"none"）。注意 unix socket 在任何
// ABI 下都不受治理。
func (r *landlockRunner) NetworkCoverage() string {
	if r == nil {
		return "none"
	}
	return landlockNetworkCoverage(r.abi)
}
