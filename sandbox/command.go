package sandbox

import (
	"path/filepath"
	"runtime"
	"slices"
	"strings"
)

// CheckCommand 按命令策略校验一条命令，并确认 dir（它的工作目录，"" 或 "." 表示
// 主根）位于配置的根内。
//
// 沙箱命令必须以裸名字调用：argv[0] 去空白后与允许列表匹配，路径形式
// （"./git"、"/usr/bin/git"、"..\\git"）会被拒绝，从而避免通过给工作区可控的
// 二进制一个允许列表中的基名来执行它。名字比较使用平台语义：Windows 上忽略
// 大小写与 .exe 后缀（"pwsh.exe" 与 allowlist 的 "pwsh" 等价），其他平台按字面
// 比较。前置环境变量赋值
// （"PATH=. git status"、"LD_PRELOAD=... git status"）同样会被拒绝：它们会改变
// shell 解析或加载命令的方式，使允许列表退化为仅作参考。拒绝列表始终优先于允许
// 列表。没有允许列表的策略拒绝所有命令；请用 WithCommandAllowlist 启用 shell
// 执行。
func (p *Policy) CheckCommand(argv []string, dir string) error {
	return p.checkCommand(argv, dir, runtime.GOOS == "windows")
}

// CheckConfinedCommand 是 CheckCommand 的"执行侧已被 OS runner 兜住"版本：只跳过
// "read-only 档不许执行命令"这一条，其余一切照旧。
//
// 它用于 read-only + 有 OS runner 的组合：那一档的写围栏由 runner 承担（它把
// workspace 与 /tmp 都排除在可写面外），所以 `sh -c 'echo x > "$TMPDIR/f"'` 这类
// 带写的命令应当能跑。**不**放宽的东西：命令允许/拒绝名单、裸名字与环境变量赋值
// 限制、workdir 必须位于根内、NUL 字节检查。
//
// 调用方必须先确认真的挂了 runner（且它的 Confine 是 fail closed）；在没有 runner
// 的组合里用它等于把写围栏关掉，请改用 CheckCommand。
func (p *Policy) CheckConfinedCommand(argv []string, dir string) error {
	return p.checkCommandFor(argv, dir, runtime.GOOS == "windows", true)
}

// checkCommand 是 CheckCommand 的平台参数化实现：windows 决定命令名比较语义
// （大小写与 .exe 后缀）。生产路径传入 runtime.GOOS == "windows"，测试可以传入
// true 在任意宿主上验证 Windows 语义。
func (p *Policy) checkCommand(argv []string, dir string, windows bool) error {
	return p.checkCommandFor(argv, dir, windows, false)
}

// checkCommandFor 是两个公开入口的唯一实现。confined 表示"执行侧已被 OS runner
// 约束"，此时 read-only 不再等同于"不许跑命令"。
func (p *Policy) checkCommandFor(argv []string, dir string, windows, confined bool) error {
	if p == nil {
		return deny("nil policy")
	}
	if err := p.modeDenial(); err != nil {
		return err
	}
	if p.ReadOnly() && !confined {
		return deny("shell commands are disabled in read-only mode")
	}
	if len(argv) == 0 {
		return deny("empty command")
	}
	for _, arg := range argv {
		if strings.ContainsRune(arg, 0) {
			return deny("command argument contains a NUL byte")
		}
	}
	name := strings.TrimSpace(argv[0])
	if name == "" || name == "." || name == string(filepathSeparator()) {
		return deny("empty command name")
	}
	if p.fullAccess() {
		// danger-full-access 移除裸名字/允许列表围栏；显式拒绝与 NUL 字节检查
		// 仍然生效。
		if containsCommandName(p.commandDeny, filepath.Base(name), windows) {
			return deny("command %q is denied by the policy", name)
		}
		workdir := strings.TrimSpace(dir)
		if workdir == "" {
			workdir = "."
		}
		if _, err := p.ResolvePath(workdir); err != nil {
			return err
		}
		return nil
	}
	if isEnvAssignment(name) {
		return deny("environment variable assignments before the command are not allowed in sandboxed commands")
	}
	if strings.ContainsAny(name, `/\`) {
		return deny("command %q must be a bare command name: paths are not allowed (use the allow-listed name)", name)
	}
	if strings.HasPrefix(name, "-") {
		return deny("command name %q must not start with '-'", name)
	}

	// allowAnyCommand 是个人客户端的默认姿态：允许执行任意程序，由上层
	// （命令风险判定 + 人工审批）担任守卫。允许列表是"能力限制"式的企业
	// 设计——个人用户自己的机器上，把 go test / npm 逐个写进白名单只会制造
	// 摩擦，而真正的危险命令已经被 gate 拦掉了。
	if !p.anyCommand {
		if len(p.commandAllow) == 0 {
			return deny("shell commands are disabled: no command allowlist configured")
		}
		if !containsCommandName(p.commandAllow, name, windows) {
			return deny("command %q is not in the allowlist", name)
		}
	}
	if containsCommandName(p.commandDeny, name, windows) {
		return deny("command %q is denied by the policy", name)
	}

	workdir := strings.TrimSpace(dir)
	if workdir == "" {
		workdir = "."
	}
	if _, err := p.ResolvePath(workdir); err != nil {
		return err
	}
	return nil
}

// filepathSeparator 以字符串形式返回平台路径分隔符，用于错误信息与空名字检查。
func filepathSeparator() string {
	if runtime.GOOS == "windows" {
		return `\`
	}
	return "/"
}

// isEnvAssignment 报告 token 是否为 shell 的 VAR=value 前缀（例如 "PATH=."、
// "LD_PRELOAD=/tmp/x.so" 或 "FOO=1"）。shell 会在启动命令前应用它们，因此沙箱
// 命令不得使用。
func isEnvAssignment(token string) bool {
	token = strings.TrimSpace(token)
	name, _, ok := strings.Cut(token, "=")
	if !ok || name == "" {
		return false
	}
	for index, r := range name {
		switch {
		case r == '_', r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
		case index > 0 && r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return true
}

// CommandEnv 返回沙箱命令应当继承的环境变量。
//
// **默认原样继承**（DSH 对齐）：没有显式 WithEnvAllowlist 时，命令看到的就是
// base（宿主环境）本身——`PYTHONPATH`、`VIRTUAL_ENV`、`GOPATH` 以及用户自定义键
// 都不再被过滤。这是刻意的决定：防护由沙箱（容器/VM/OS runner）负责，进程内的
// 环境过滤只是"DSH 没有、我们额外加的"约束，会挡住模型本可以正常使用的工具链。
// 代价是凭据也会被继承，见 SECURITY.md「凭据」。
//
// 只有宿主**显式**给出 WithEnvAllowlist(...) 时才按名字过滤（此时同名变量在
// Unix 上区分大小写、在 Windows 上不区分大小写，与操作系统环境语义一致）。
//
// 设置了 scratch（WithScratchDir）时，TMPDIR/TMP/TEMP 三个键一律被覆盖成 scratch
// ——包括白名单模式下（临时目录是命令能跑起来的必要条件，不该被白名单挡掉）。
// 未设置 scratch 时行为与以前逐字节一致。
func (p *Policy) CommandEnv(base []string) []string {
	if p == nil {
		return nil
	}
	if !p.envSet {
		// 原样继承，但仍返回副本：调用方改返回值不能污染 os.Environ() 之类的入参。
		return withScratchEnv(slices.Clone(base), p.scratch)
	}
	allowed := p.envAllow
	if len(allowed) == 0 {
		return withScratchEnv(nil, p.scratch)
	}
	allow := make(map[string]struct{}, len(allowed))
	for _, name := range allowed {
		allow[envKey(name)] = struct{}{}
	}
	out := make([]string, 0, len(allowed))
	seen := make(map[string]struct{}, len(allowed))
	for _, entry := range base {
		key, _, ok := strings.Cut(entry, "=")
		if !ok || key == "" {
			continue
		}
		lookup := envKey(key)
		if _, permitted := allow[lookup]; !permitted {
			continue
		}
		if _, duplicate := seen[lookup]; duplicate {
			continue
		}
		seen[lookup] = struct{}{}
		out = append(out, entry)
	}
	return withScratchEnv(out, p.scratch)
}

// containsCommandName 报告 name 是否以该平台语义出现在 names 中（names 已按基名
// 归一化）。windows 由调用方从 runtime.GOOS 计算后传入，测试可以直接覆盖两个分支。
func containsCommandName(names []string, name string, windows bool) bool {
	return slices.ContainsFunc(names, func(candidate string) bool {
		return commandNameMatches(candidate, name, windows)
	})
}

// commandNameMatches 报告 candidate（策略列表里的基名）与 name（argv[0] 的基名）
// 在给定平台语义下是否指同一个命令。Windows 上比较忽略大小写与 .exe 后缀
// （"powershell" 与 "PowerShell.EXE" 等价）；其他平台按字节精确比较，.exe 不被
// 剥离。
func commandNameMatches(candidate, name string, windows bool) bool {
	if !windows {
		return candidate == name
	}
	return normalizeCommandName(candidate, windows) == normalizeCommandName(name, windows)
}

// commandExeSuffix 是 Windows 上可执行文件可选的后缀（.com 等不在归一化范围内：
// allowlist 按命令名写法匹配，而不是解析 PE 格式）。
const commandExeSuffix = ".exe"

// normalizeCommandName 按给定平台语义归一化命令名，使 allowlist/denylist 中的
// "pwsh" 与 argv[0] 的 "pwsh.exe"（及大小写变体）可以双向等价。Windows 上先把
// 名字转小写，再反复剥掉末尾的 .exe，因此归一化是幂等的——策略构造期与比较期各
// 调用一次也不会多剥；长度判断保证像 ".exe" 这样的短名不越界。非 Windows 平台原样
// 返回，不得剥离 .exe。
func normalizeCommandName(name string, windows bool) string {
	if !windows {
		return name
	}
	name = strings.ToLower(name)
	for len(name) > len(commandExeSuffix) && strings.HasSuffix(name, commandExeSuffix) {
		name = name[:len(name)-len(commandExeSuffix)]
	}
	return name
}

// envKey 归一化环境变量名以便匹配。
func envKey(name string) string {
	if runtime.GOOS == "windows" {
		return strings.ToUpper(name)
	}
	return name
}
