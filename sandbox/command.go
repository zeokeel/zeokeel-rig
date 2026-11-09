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

// checkCommand 是 CheckCommand 的平台参数化实现：windows 决定命令名比较语义
// （大小写与 .exe 后缀）。生产路径传入 runtime.GOOS == "windows"，测试可以传入
// true 在任意宿主上验证 Windows 语义。
func (p *Policy) checkCommand(argv []string, dir string, windows bool) error {
	if p == nil {
		return deny("nil policy")
	}
	if err := p.modeDenial(); err != nil {
		return err
	}
	if p.ReadOnly() {
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

	if len(p.commandAllow) == 0 {
		return deny("shell commands are disabled: no command allowlist configured")
	}
	if !containsCommandName(p.commandAllow, name, windows) {
		return deny("command %q is not in the allowlist", name)
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

// CommandEnv 把 base 过滤为沙箱命令可以看到的变量。使用 WithEnvAllowlist 时只
// 保留列出的名字；否则应用最小的 PATH/HOME/LANG 集合。变量在 Unix 上区分大小写，
// 在 Windows 上不区分大小写，与操作系统环境语义一致。
func (p *Policy) CommandEnv(base []string) []string {
	if p == nil {
		return nil
	}
	allowed := defaultEnvAllowlist
	if p.envSet {
		allowed = p.envAllow
	}
	if len(allowed) == 0 {
		return nil
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
	return out
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
