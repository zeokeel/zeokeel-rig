package osrunner

import (
	"errors"
	"fmt"
	"time"

	"github.com/zeokeel/zeokeel-rig/sandbox"
)

// Runner 身份。它们同时用于 DenialSignatures 的查表与诊断输出。
const (
	// RunnerBwrap 是 Linux 上的 bubblewrap 后端。
	RunnerBwrap = "bwrap"
	// RunnerLandlock 是 Linux 上自带的纯 Go Landlock 启动器。
	RunnerLandlock = "landlock"
	// RunnerSeatbelt 是 macOS 上的 sandbox-exec（Seatbelt/SBPL）后端。
	RunnerSeatbelt = "seatbelt"
	// RunnerWindowsACL 是 Windows 上的受限令牌 + ACL 后端。
	RunnerWindowsACL = "windows-acl"
)

// DefaultProbeTimeout 是每个候选 runner 功能探测的默认超时。
const DefaultProbeTimeout = 5 * time.Second

// Config 配置 runner 的选择与探测。
type Config struct {
	// ProbeTimeout 是每个候选 runner 的功能探测超时；<=0 时使用
	// DefaultProbeTimeout。
	ProbeTimeout time.Duration
	// RunnerCommand 覆盖平台链：命令（通常是 bwrap 兼容的包装器）会收到本包的
	// bwrap profile 参数、"--"，然后是目标 argv；选择时跳过平台链与功能探测。
	// 必须同时提供 RunnerFailureSignatures。
	RunnerCommand []string
	// RunnerFailureSignatures 是自定义 runner 自身失败时 stderr 出现的大小写
	// 不敏感子串；只有 RunnerCommand 非空时有效，且至少要有一条。
	RunnerFailureSignatures []string
}

// probeTimeout 返回生效的探测超时。
func (c Config) probeTimeout() time.Duration {
	if c.ProbeTimeout <= 0 {
		return DefaultProbeTimeout
	}
	return c.ProbeTimeout
}

// New 选择本机可用的 OS runner。
//
// 选择顺序：RunnerCommand 覆盖（跳过探测）→ 平台链（Linux bwrap → Landlock；
// macOS Seatbelt；Windows ACL）。平台链里多个候选按顺序做一次功能探测，第一个
// 可用者被选中并缓存到返回的 runner 内。没有任何可用候选时返回
// sandbox.ErrSandboxUnavailable——调用方必须拒绝执行，而不是裸跑。
func New(cfg Config) (sandbox.Runner, error) {
	if len(cfg.RunnerCommand) > 0 {
		if len(cfg.RunnerFailureSignatures) == 0 {
			return nil, errors.New("osrunner: RunnerCommand requires at least one RunnerFailureSignatures entry")
		}
		for _, signature := range cfg.RunnerFailureSignatures {
			if signature == "" || containsNewline(signature) {
				return nil, errors.New("osrunner: RunnerFailureSignatures entries must be non-empty single-line strings")
			}
		}
		return &customRunner{command: append([]string(nil), cfg.RunnerCommand...)}, nil
	}
	return newPlatformRunner(cfg)
}

// customRunner 是运维断言的自定义 runner：它按 bwrap profile 形状包装 argv，
// 并假定该命令诚实实现 bwrap 兼容语义（因此不做功能探测）。
type customRunner struct {
	command []string
}

var _ sandbox.Runner = (*customRunner)(nil)

// Confine 实现 sandbox.Runner。
func (r *customRunner) Confine(argv []string, policy *sandbox.Policy) (sandbox.ConfinedCommand, error) {
	if policy == nil {
		return sandbox.ConfinedCommand{}, fmt.Errorf("osrunner: custom runner requires a policy: %w", sandbox.ErrSandboxUnavailable)
	}
	if policy.Mode() == sandbox.ModeDangerFullAccess {
		return passthrough(argv), nil
	}
	wrapped := make([]string, 0, len(r.command)+len(argv)+8)
	wrapped = append(wrapped, r.command...)
	wrapped = append(wrapped, BwrapArgs(policy)...)
	wrapped = append(wrapped, "--")
	wrapped = append(wrapped, argv...)
	return sandbox.ConfinedCommand{
		Argv:             wrapped,
		Enforcement:      sandbox.EnforcementFull,
		DenialSignatures: DenialSignatures(RunnerBwrap),
	}, nil
}

// passthrough 返回 danger-full-access 下的未包装 argv。
func passthrough(argv []string) sandbox.ConfinedCommand {
	return sandbox.ConfinedCommand{
		Argv:        append([]string(nil), argv...),
		Enforcement: sandbox.EnforcementFull,
	}
}

// wrapped 构造一次 argv 前缀式包装的结果。
func wrapped(prefix []string, trailer []string, argv []string, enforcement sandbox.Enforcement, signatures []string) sandbox.ConfinedCommand {
	out := make([]string, 0, len(prefix)+len(trailer)+len(argv)+1)
	out = append(out, prefix...)
	out = append(out, trailer...)
	needsSeparator := true
	if len(trailer) > 0 && trailer[len(trailer)-1] == "--" {
		needsSeparator = false
	}
	if needsSeparator {
		out = append(out, "--")
	}
	out = append(out, argv...)
	return sandbox.ConfinedCommand{
		Argv:             out,
		Enforcement:      enforcement,
		DenialSignatures: append([]string(nil), signatures...),
	}
}

// containsNewline 报告字符串是否含换行（用于拒绝多行失败签名）。
func containsNewline(s string) bool {
	for _, r := range s {
		if r == '\n' || r == '\r' {
			return true
		}
	}
	return false
}
