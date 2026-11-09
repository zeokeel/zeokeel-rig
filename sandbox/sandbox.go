// Package sandbox 实现 ZeoKeel-Rig 的 agent 沙箱策略层。
//
// Policy 是对“agent 的工具可以碰什么”的一份不可变描述：哪些文件系统根可见、是否
// 允许写、哪些 shell 命令可以运行、命令继承哪些环境变量、是否允许使用网络、输出
// 与时间上限，以及始终拒绝的路径 glob（例如 "**/.env"）。内置工具通过
// tools/builtin 的构造器接收 *Policy；只需要默认行为的宿主可以继续使用
// NewFS/NewShell/NewBuiltins 之类的便捷构造。
//
// 本包只依赖标准库；Policy 在 New 返回之后并发安全：选项在构造时应用，字段之后
// 不再被修改。
//
// # 主要类型
//
//   - Policy：不可变沙箱策略，用 New + Option 构造。
//   - Mode：粗粒度权限模式（workspace-write / read-only / danger-full-access），
//     与 DeepSeek Harness 的权限模型对应。
//   - ErrDenied：所有策略拒绝都会包装的哨兵错误，可用 errors.Is 判断。
//   - 路径：ResolvePath、CheckPath、OpenPath、DeniedPath。
//   - 命令：ParseShellCommand、CheckShellCommand、CheckCommand、CommandEnv。
//   - 网络：CheckURL、ResolveHost。
//   - 选项：WithRoots、WithMode、WithReadOnly、WithCommandAllowlist、
//     WithCommandDenylist、WithEnvAllowlist、WithNetwork、WithMaxOutputBytes、
//     WithTimeout、WithDeniedGlobs、WithAllowShellOperators。
//
// # 典型用法
//
// 构造一个“工作区可写、只允许 git/go/ls、可访问公网”的策略，再交给内置工具：
//
//	import (
//		"github.com/zeokeel/zeokeel-rig/sandbox"
//		"github.com/zeokeel/zeokeel-rig/tools/full"
//	)
//
//	policy := sandbox.New(
//		sandbox.WithRoots("/srv/workspace"),
//		sandbox.WithMode(sandbox.ModeWorkspaceWrite),
//		sandbox.WithCommandAllowlist("git", "go", "ls", "cat"),
//		sandbox.WithDeniedGlobs("**/.env", "**/*.pem"),
//		sandbox.WithNetwork(true),
//	)
//	tools := full.All(full.AllOptions{Policy: policy, Root: "/srv/workspace"})
//
//	// 也可以只做校验：
//	path, err := policy.ResolvePath("internal/agent.go")   // 越界/.. 会返回包装 ErrDenied 的错误
//	argv, err := policy.ParseShellCommand("go test ./...") // 解析 + 校验 shell 语法
//	err = policy.CheckCommand(argv, "/srv/workspace")      // 校验命令与工作目录
//
// # 与 agent 循环和工具装配的关系
//
// Policy 不直接参与 agent 循环：工具在构造时接收它并把约束绑定进各自的 Invoke
// 路径，agent 只负责按模型请求调用工具。装配时由 tools/full（或各工具包）把同一个
// *Policy 传给文件、shell、terminal、jobs、web 等工具，因此一次策略配置就覆盖了
// 整轮 agent 能触达的副作用面。
//
// # 失败即拒绝（fail closed）
//
// 零值策略（New() 不带任何选项）什么都不允许：不能访问文件、不能执行 shell、不能
// 联网。拒绝会以包装 ErrDenied 的错误返回，因此面向模型的工具可以区分“沙箱拒绝”
// 与“操作失败”。
//
// # 路径解析
//
// ResolvePath 按配置的根校验一个请求路径。它会拒绝 NUL 字节、任何 ".." 分量、
// 位于所有根之外的绝对路径，以及逃逸出根目录的符号链接（对最深的已存在祖先调用
// filepath.EvalSymlinks，结果必须仍在某个根内；剩余不存在的分量不可能是符号
// 链接）。返回值是真实的绝对路径（已解析符号链接）。OpenPath 额外返回包含该路径
// 的根的 *os.Root 句柄，使工具可以在 os.Root 于系统调用邻近层提供的同一约束下
// 完成实际 I/O。
//
// 相对路径相对第一个配置的根（主根）解析；绝对路径可以指向任意已配置的根。
//
// # 策略不是操作系统安全边界
//
// 策略层约束的是 Rig 内置工具，它不是 OS 沙箱：shell 命令以当前用户身份运行，
// 因此审批 Gate 仍然是真正的边界；一个忽略策略的工具可以做进程能做的任何事。
// 默认强制两项加固措施：
//
//   - ParseShellCommand / CheckShellCommand 模拟 /bin/sh 会如何读取该命令
//     （引号、转义、POSIX 续行、注释），并拒绝任何能启动或拼接另一个程序的构造：
//     未加引号的控制操作符、命令替换 $(...) 与反引号（即使在双引号内）、进程
//     替换和换行。WithAllowShellOperators 可显式关闭该检查，使命令允许列表退化为
//     仅作参考（advisory-only）。允许列表仍然是策略层而不是边界：把解释器
//     （sh、bash、env、xargs、python 等）加入允许列表，按设计就等于授予任意执行
//     能力。
//   - ResolveHost 会解析并校验主机名的每个地址，使网络客户端可以固定使用校验过的
//     IP。CheckURL 刻意不解析 DNS；客户端必须调用 ResolveHost 才能关闭 DNS
//     重绑定（rebinding）缺口。
package sandbox

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"
)

// ErrDenied 会被每一次策略拒绝包装。调用方可以用 errors.Is 判断它，从而区分
// 沙箱拒绝与工具自身失败。
var ErrDenied = errors.New("sandbox: denied")

// 未配置对应选项时使用的默认上限。它们与内置工具的默认值保持一致，因此在放开
// roots/命令之后，零值策略的行为与 v0.2 版本对齐。
const (
	// DefaultMaxOutputBytes 是面向模型的默认输出上限（32 KiB）。
	DefaultMaxOutputBytes = 32 << 10
	// DefaultTimeout 是默认命令超时（30s）。
	DefaultTimeout = 30 * time.Second
)

// defaultEnvAllowlist 是未使用 WithEnvAllowlist 时命令继承的最小环境变量集合：
// PATH、HOME 与 LANG。其余变量（API key、token、凭据）都会在命令启动前被丢弃。
var defaultEnvAllowlist = []string{"PATH", "HOME", "LANG"}

// Mode 是 Policy 的粗粒度文件效果/命令权限级别，与 DeepSeek Harness 的权限模型
// 对应。
type Mode string

const (
	// ModeWorkspaceWrite 只允许在配置的根（工作区）内修改文件，并且只运行允许
	// 列表中的 shell 命令。它是默认值，也是推荐的生产模式。
	ModeWorkspaceWrite Mode = "workspace-write"
	// ModeReadOnly 拒绝文件修改与 shell 执行；只读工具继续可用。它是工具层围栏，
	// 不是内核边界。
	ModeReadOnly Mode = "read-only"
	// ModeDangerFullAccess 移除根围栏与命令允许列表：文件工具可以修改进程能修改的
	// 任意路径，shell 执行也不再局限于允许列表中的命令。显式 denylist 仍然生效。
	// 宿主只能在与自动/显式审批搭配时选择它。
	ModeDangerFullAccess Mode = "danger-full-access"
)

// Policy 是一份不可变的沙箱策略。请用 New 与 Option 函数构造它；不要直接修改
// 字段。
type Policy struct {
	roots          []string // 真实的绝对根路径，第一个是主根
	readOnly       bool
	mode           Mode
	modeErr        error
	commandAllow   []string // 命令基名
	commandDeny    []string // 命令基名（优先于允许列表）
	envAllow       []string
	envSet         bool
	network        bool
	maxOutputBytes int
	timeout        time.Duration
	deniedGlobs    []string // 以 / 分隔、相对于某个根

	allowShellOperators bool
	lookupIP            func(ctx context.Context, host string) ([]net.IP, error)
}

// Option 配置一个 Policy。New 会按传入顺序应用选项。
type Option func(*Policy)

// New 根据 opts 构造 Policy。不带选项的策略拒绝一切。
func New(opts ...Option) *Policy {
	policy := &Policy{
		mode:           ModeWorkspaceWrite,
		maxOutputBytes: DefaultMaxOutputBytes,
		timeout:        DefaultTimeout,
		lookupIP:       defaultLookupIP,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(policy)
		}
	}
	return policy
}

// WithRoots 设置该策略暴露的文件系统根。它接受绝对或相对路径；相对路径在构造时
// 相对进程工作目录解析，尽可能解析符号链接，并去重。不传任何根调用 WithRoots
// 会移除全部访问权限。第一个根是用来解析相对路径参数的主根。
func WithRoots(roots ...string) Option {
	return func(policy *Policy) {
		policy.roots = normalizeRoots(roots)
	}
}

// WithReadOnly 把策略标记为只读：写文件的工具拒绝运行，shell 命令被禁用（shell
// 可以写文件）。读工具继续可用。它是 WithMode(ModeReadOnly) 的兼容写法。
func WithReadOnly(readOnly bool) Option {
	return func(policy *Policy) {
		policy.readOnly = readOnly
		if readOnly {
			policy.mode = ModeReadOnly
		}
	}
}

// WithMode 选择粗粒度权限模式：ModeWorkspaceWrite（默认）、ModeReadOnly 或
// ModeDangerFullAccess。未知模式失败即拒绝（fail closed）：所有路径/命令检查都会
// 以模式错误拒绝。
func WithMode(mode Mode) Option {
	return func(policy *Policy) {
		switch mode {
		case ModeWorkspaceWrite, ModeReadOnly, ModeDangerFullAccess:
			policy.mode = mode
			if mode == ModeReadOnly {
				policy.readOnly = true
			}
		default:
			policy.modeErr = fmt.Errorf("unknown permission mode %q (want %q, %q or %q)", mode, ModeWorkspaceWrite, ModeReadOnly, ModeDangerFullAccess)
		}
	}
}

// WithCommandAllowlist 只允许列出的命令名。名字与 argv[0] 的基名匹配
// （因此 "/usr/bin/git" 会匹配 "git"）。在 Windows 上比较忽略大小写并忽略
// ".exe" 后缀（"pwsh" 与 "pwsh.exe" 等价，宿主可以按不带后缀的常用写法配置）；
// 其他平台按字面比较。不传任何名字（或从不调用它）会完全禁用 shell 执行。
func WithCommandAllowlist(commands ...string) Option {
	return func(policy *Policy) {
		policy.commandAllow = normalizeCommandNames(commands, runtime.GOOS == "windows")
	}
}

// WithCommandDenylist 拒绝列出的命令名，即使它们同时出现在允许列表中。名字匹配
// 方式与 WithCommandAllowlist 相同（Windows 上同样忽略大小写与 ".exe" 后缀）。
func WithCommandDenylist(commands ...string) Option {
	return func(policy *Policy) {
		policy.commandDeny = normalizeCommandNames(commands, runtime.GOOS == "windows")
	}
}

// WithEnvAllowlist 控制 shell 命令继承哪些环境变量。不传任何名字时命令以空环境
// 运行。完全未使用该选项时，应用 DefaultEnvAllowlist（PATH/HOME/LANG）。
func WithEnvAllowlist(names ...string) Option {
	return func(policy *Policy) {
		policy.envSet = true
		policy.envAllow = normalizeNames(names)
	}
}

// WithNetwork 允许对外网络访问。即使启用，CheckURL 仍会拒绝非 HTTP(S) 协议以及
// loopback/私有/链路本地/云元数据地址，因此该选项只放开公共 HTTP(S) 目标。
func WithNetwork(allow bool) Option {
	return func(policy *Policy) { policy.network = allow }
}

// WithMaxOutputBytes 限制策略感知工具面向模型的输出。非正值保持
// DefaultMaxOutputBytes。
func WithMaxOutputBytes(maxBytes int) Option {
	return func(policy *Policy) {
		if maxBytes > 0 {
			policy.maxOutputBytes = maxBytes
		}
	}
}

// WithTimeout 设置命令超时上限。非正值保持 DefaultTimeout。shell 工具把它同时
// 用作默认值与单次调用超时的上限。
func WithTimeout(timeout time.Duration) Option {
	return func(policy *Policy) {
		if timeout > 0 {
			policy.timeout = timeout
		}
	}
}

// WithDeniedGlobs 拒绝匹配任意模式的路径，例如 "**/.env"、"**/*.pem" 或
// "secrets/**"。模式按相对于所属根的路径匹配（"*" 与 "?" 在单个路径段内匹配，
// "**" 匹配任意多段）；不含 "/" 的模式还会在任意深度匹配文件基名。被拒绝的路径
// 会由 ResolvePath/CheckPath 拒绝，并从 glob/grep/list_dir 结果中隐藏。
func WithDeniedGlobs(patterns ...string) Option {
	return func(policy *Policy) { policy.deniedGlobs = normalizeGlobs(patterns) }
}

// WithAllowShellOperators 关闭 CheckShellCommand 对控制操作符的拒绝，使策略可以
// 运行 shell 管道与重定向（例如 "git log | head"）。它把 /bin/sh -c 的命令
// 允许列表弱化为仅作参考：一个操作符就能启动任意程序，因此只有审批 Gate 已经
// 限定风险时才应启用。
func WithAllowShellOperators() Option {
	return func(policy *Policy) { policy.allowShellOperators = true }
}

// AllowShellOperators 报告是否放开了 shell 控制操作符。
func (p *Policy) AllowShellOperators() bool { return p != nil && p.allowShellOperators }

// Roots 返回配置的根（真实的绝对路径，第一个是主根）。
func (p *Policy) Roots() []string {
	if p == nil {
		return nil
	}
	return slices.Clone(p.roots)
}

// ReadOnly 报告策略是否为只读。
func (p *Policy) ReadOnly() bool {
	if p == nil {
		return false
	}
	return p.readOnly || p.mode == ModeReadOnly
}

// Mode 报告生效的权限模式（未设置时为 ModeWorkspaceWrite）。
func (p *Policy) Mode() Mode {
	if p == nil {
		return ModeWorkspaceWrite
	}
	if p.readOnly {
		return ModeReadOnly
	}
	if p.mode == "" {
		return ModeWorkspaceWrite
	}
	return p.mode
}

// fullAccess 报告策略是否移除了根围栏与命令允许列表（danger-full-access）。
func (p *Policy) fullAccess() bool { return p != nil && !p.readOnly && p.mode == ModeDangerFullAccess }

// modeDenial 返回保存的非法模式错误，没有则返回 nil。
func (p *Policy) modeDenial() error {
	if p == nil {
		return nil
	}
	if p.modeErr != nil {
		return deny("%v", p.modeErr)
	}
	return nil
}

// NetworkAllowed 报告是否启用了对外网络访问。
func (p *Policy) NetworkAllowed() bool { return p != nil && p.network }

// Timeout 返回命令超时上限（未设置时为 DefaultTimeout）。
func (p *Policy) Timeout() time.Duration {
	if p == nil || p.timeout <= 0 {
		return DefaultTimeout
	}
	return p.timeout
}

// MaxOutputBytes 返回面向模型的输出上限（未设置时为 DefaultMaxOutputBytes）。
func (p *Policy) MaxOutputBytes() int {
	if p == nil || p.maxOutputBytes <= 0 {
		return DefaultMaxOutputBytes
	}
	return p.maxOutputBytes
}

func deny(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrDenied, fmt.Sprintf(format, args...))
}

// normalizeRoots 把根解析为真实的绝对路径，并丢掉空项与重复项。
func normalizeRoots(roots []string) []string {
	out := make([]string, 0, len(roots))
	seen := make(map[string]struct{}, len(roots))
	for _, root := range roots {
		root = strings.TrimSpace(root)
		if root == "" {
			continue
		}
		abs, err := filepath.Abs(root)
		if err != nil {
			continue
		}
		abs = filepath.Clean(abs)
		if real, err := filepath.EvalSymlinks(abs); err == nil {
			abs = filepath.Clean(real)
		}
		// 失败即拒绝：当前无法打开的根不会被暴露。
		rootHandle, err := os.OpenRoot(abs)
		if err != nil {
			continue
		}
		rootHandle.Close()
		if _, exists := seen[abs]; exists {
			continue
		}
		seen[abs] = struct{}{}
		out = append(out, abs)
	}
	return out
}

// normalizeCommandNames 只保留基名，并做去空白、平台归一化与去重。Windows 上
// 名字先按 Windows 语义归一化（小写、去 .exe），因此 "pwsh" 与 "pwsh.exe" 会被
// 视为同一个名字；其他平台保持原样（仍按大小写不敏感去重，与既有行为一致）。
func normalizeCommandNames(commands []string, windows bool) []string {
	out := make([]string, 0, len(commands))
	seen := make(map[string]struct{}, len(commands))
	for _, command := range commands {
		command = strings.TrimSpace(command)
		if command == "" {
			continue
		}
		command = filepath.Base(command)
		command = normalizeCommandName(command, windows)
		key := strings.ToLower(command)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, command)
	}
	return out
}

// normalizeNames 对环境变量名做去空白与去重。
func normalizeNames(names []string) []string {
	out := make([]string, 0, len(names))
	seen := make(map[string]struct{}, len(names))
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if _, exists := seen[name]; exists {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	return out
}

// normalizeGlobs 对拒绝路径 glob 做去空白、斜杠归一化与去重。
func normalizeGlobs(patterns []string) []string {
	out := make([]string, 0, len(patterns))
	seen := make(map[string]struct{}, len(patterns))
	for _, pattern := range patterns {
		pattern = strings.TrimSpace(filepath.ToSlash(pattern))
		pattern = strings.TrimPrefix(pattern, "./")
		if pattern == "" || pattern == "." {
			continue
		}
		if _, exists := seen[pattern]; exists {
			continue
		}
		seen[pattern] = struct{}{}
		out = append(out, pattern)
	}
	return out
}
