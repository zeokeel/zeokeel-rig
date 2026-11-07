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
// 二进制一个允许列表中的基名来执行它。前置环境变量赋值
// （"PATH=. git status"、"LD_PRELOAD=... git status"）同样会被拒绝：它们会改变
// shell 解析或加载命令的方式，使允许列表退化为仅作参考。拒绝列表始终优先于允许
// 列表。没有允许列表的策略拒绝所有命令；请用 WithCommandAllowlist 启用 shell
// 执行。
func (p *Policy) CheckCommand(argv []string, dir string) error {
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
		if containsCommandName(p.commandDeny, filepath.Base(name)) {
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
	if !containsCommandName(p.commandAllow, name) {
		return deny("command %q is not in the allowlist", name)
	}
	if containsCommandName(p.commandDeny, name) {
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

// containsCommandName 报告 name 是否在 names 中（基名语义，Windows 上不区分
// 大小写）。
func containsCommandName(names []string, name string) bool {
	if runtime.GOOS == "windows" {
		return slices.ContainsFunc(names, func(candidate string) bool { return strings.EqualFold(candidate, name) })
	}
	return slices.Contains(names, name)
}

// envKey 归一化环境变量名以便匹配。
func envKey(name string) string {
	if runtime.GOOS == "windows" {
		return strings.ToUpper(name)
	}
	return name
}
