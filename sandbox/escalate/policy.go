package escalate

// 宿主侧策略：允许什么、禁止什么、必须审批什么、哪些目录可以当工作目录、
// 客户端环境变量怎么过滤。
//
// Policy 是不可变值：Decide 只读字段、不碰任何共享状态，因此可以被并发调用，
// 也可以在测试里直接构造。

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Policy 是宿主侧策略。
//
// 语义（Decide 的判定顺序见下）：Allow 是白名单前缀，Deny 是黑名单前缀且优先级
// 最高，Roots 限定 Dir，Approval 标出"允许但必须人工审批"的前缀。空的 Allow 表示
// "一律不允许"（fail closed），不是"全放行"。
type Policy struct {
	// Allow 是允许的 argv 前缀列表：{"git","fetch"} 表示 git fetch 及其全部后续
	// 参数；{"curl"} 表示任意 curl 调用。条目为空（或含空字符串）视为配置错误。
	Allow [][]string
	// Deny 是禁止的 argv 前缀列表，优先级高于 Allow。
	//
	// 写法必须是**整个子命令**：Deny={{"git","push"}}。不要用 flag 级 Deny
	// （例如 Deny={{"git","push","--force"}}）——flag 有大量别名与等价形式
	// （--force / -f / --force-with-lease），逐条列举必然漏。
	//
	// 同理，尽量不要用"裸程序名"的宽 Allow（Allow={{"git"}}）：它等于把该程序的
	// 全部子命令与全部参数都交给沙箱，Deny 只能事后补救。能用子命令级 Allow
	// （Allow={{"git","fetch"}}）就不要用宽 Allow。
	Deny [][]string
	// Roots 是允许的工作目录根：Request.Dir 规范化后必须落在其中之一。
	// 为空表示任何目录都不允许。
	Roots []string

	// MaxOutputBytes 是 stdout / stderr 各自的截断上限；<=0 用
	// DefaultMaxOutputBytes。
	MaxOutputBytes int
	// MaxTimeout 是单条请求的时长硬上限；<=0 用 DefaultMaxTimeout。请求里的
	// TimeoutMS 只能更小，不能更大。
	MaxTimeout time.Duration

	// Approval 是"允许但必须人工审批"的 argv 前缀列表（可选扩展）。
	// 未配置 Approver 时，命中它的请求一律拒绝。
	Approval [][]string
	// MaxConcurrent 是同时在途的执行数上限；<=0 用 DefaultMaxConcurrent。
	MaxConcurrent int
	// MaxQueue 是"并发额度已满时允许排队等待"的请求数上限；超过就立刻拒绝
	// （Denied=true、DenyReason=QueueFullReason），让客户端退避重试，而不是无限
	// 排队被拖到超时。0 用 DefaultMaxQueue（64）；-1 表示不限（保留旧行为，
	// 慎用：恶意沙箱可以靠刷请求把执行无限期推迟）。
	MaxQueue int
	// EnvPrefixes 是客户端 Env 的键前缀白名单；<=0（空）时用
	// {"GIT_", "LC_"}。注意：即使前缀命中，高危变量仍会被 dropDangerousEnv 丢掉。
	EnvPrefixes []string
	// BaseEnv 是子进程继承的基线环境（"K=V" 形式）。为空时用 defaultBaseEnv：
	// 只保留 PATH/HOME/USER/LOGNAME/SHELL/TMPDIR/TZ/LANG/SSH_AUTH_SOCK 与 LC_*，
	// 避免把宿主上无关的密钥（AWS_*、GITHUB_TOKEN 之类）灌给子进程。
	BaseEnv []string
}

// Decide 判定一条请求是否可以执行，以及是否必须先经人工审批。
//
// 判定顺序是固定的，且每一步都 fail closed：
//
//  1. argv 非空（否则拒绝）
//  2. Reason 非空（否则拒绝：无法审计的提权不做）
//  3. Deny 命中 → 拒绝（最高优先级）
//  4. Allow 未命中 → 拒绝
//  5. Dir 校验：绝对路径 / 存在 / EvalSymlinks 后仍在 Roots 内 → 否则拒绝
//  6. Approval 命中 → 允许但需要审批
//
// Dir 的校验放在 Allow 之后，是为了让"命令本身不允许"先被拒，避免为一条根本
// 不会执行的命令去做文件系统解析。
func (p Policy) Decide(req Request) Decision {
	if len(req.Argv) == 0 || strings.TrimSpace(req.Argv[0]) == "" {
		return Decision{Reason: "argv 为空：提权请求必须给出要执行的命令"}
	}
	if strings.TrimSpace(req.Reason) == "" {
		return Decision{Reason: "必须给出提权理由（reason）：无法审计的提权一律拒绝"}
	}
	if entry, ok := matchPrefix(p.Deny, req.Argv); ok {
		return Decision{Reason: "命中禁止列表: " + strings.Join(entry, " ")}
	}
	// 子命令之前的全局选项会让位置匹配失效（`git -c alias.x='!sh' x`、
	// `git -C /elsewhere push`）：只要策略给这个程序配了子命令级的 Deny/Approval，
	// 就拒绝这种形态。
	if err := p.checkNoGlobalOptionBeforeSubcommand(req.Argv); err != nil {
		return Decision{Reason: err.Error()}
	}
	// 内置的"可执行参数"黑名单：--upload-pack 之类会把被允许的命令变成任意程序
	// 执行器，它们不是子命令，Host 无法靠 Deny 前缀逐条穷举。
	if flag, ok := matchBuiltinDangerousArgs(req.Argv); ok {
		return Decision{Reason: "命中内置危险参数（会把命令变成执行器或越界读写）: " + flag}
	}
	if _, ok := matchPrefix(p.Allow, req.Argv); !ok {
		return Decision{Reason: "未匹配任何允许前缀（allow list）: " + strings.Join(req.Argv, " ")}
	}
	if _, err := p.ResolveDir(req.Dir); err != nil {
		return Decision{Reason: err.Error()}
	}
	if entry, ok := matchPrefix(p.Approval, req.Argv); ok {
		return Decision{Allow: true, NeedApproval: true, Reason: "命中审批列表: " + strings.Join(entry, " ")}
	}
	return Decision{Allow: true}
}

// checkNoGlobalOptionBeforeSubcommand 拒绝"在子命令之前插入全局选项"的形态。
//
// 攻击：策略写"宽 Allow + 子命令级 Deny"（Allow={{"git"}} + Deny={{"git","push"}}）。
// 位置匹配在 argv[1] 处比较，因此
// `git -c core.pager='sh -c …' push`、`git -C /elsewhere push`、
// `git --git-dir=/x push` 都能整体躲过 Deny。
//
// 只要存在针对该程序的子命令级 Deny/Approval（条目长度 ≥2），就要求 argv[1] 不是
// 选项：需要换目录时用 Request.Dir，需要改配置时改策略。这是有意的过度拒绝——
// 位置匹配 + 可插入全局选项 = 无法用前缀表达"禁止某个子命令"。
func (p Policy) checkNoGlobalOptionBeforeSubcommand(argv []string) error {
	if len(argv) < 2 || !strings.HasPrefix(argv[1], "-") {
		return nil
	}
	program := argv[0]
	for _, list := range [][][]string{p.Deny, p.Approval} {
		for _, entry := range list {
			if len(entry) >= 2 && entry[0] == program {
				return fmt.Errorf("argv[1] 是全局选项 %q：策略对 %q 配了子命令级限制，在子命令前插入全局选项会绕过位置匹配", argv[1], program)
			}
		}
	}
	return nil
}

// builtinDangerousArgs 是"即使 Allow 允许、也必须拒绝"的参数表（按程序名）。
//
// 这些参数会把一条本来"只读/只拉取"的命令变成任意代码执行或越界读写：
//
//   - git --upload-pack/--receive-pack/--exec-path/--template：让 git 执行任意程序；
//   - git -c/--config-env：注入 core.pager、alias.*（`-c alias.x='!sh' x`）等配置，
//     等价于任意命令执行；
//   - git --ext-diff/--textconv：调用外部 diff/textconv 程序；
//   - git -O/--open-files-in-pager：直接执行指定的分页器；
//   - git --output：把输出写到任意路径（Roots 之外）；
//   - git --no-index：让 git 读写 Roots 之外的任意路径。
//
// 它们都不是子命令，Host 无法靠 Deny 前缀逐条穷举（doc.go 的示例配置
// Allow={"git","fetch"} 就会中招），因此放在内置表里一律拒绝。需要放开其中某项
// 时必须改这张表并补测试，而不是在策略里"忘掉一条"。
var builtinDangerousArgs = map[string][]string{
	"git": {
		"--upload-pack", "--receive-pack", "--exec-path", "--template",
		"-c", "--config-env",
		"--ext-diff", "--textconv",
		"-O", "--open-files-in-pager",
		"--output",
		"--no-index",
	},
}

// matchBuiltinDangerousArgs 报告 argv 是否含内置危险参数（支持 --flag=value 形式）。
func matchBuiltinDangerousArgs(argv []string) (string, bool) {
	if len(argv) == 0 {
		return "", false
	}
	flags, ok := builtinDangerousArgs[argv[0]]
	if !ok {
		return "", false
	}
	for _, arg := range argv[1:] {
		for _, flag := range flags {
			if arg == flag || strings.HasPrefix(arg, flag+"=") {
				return arg, true
			}
		}
	}
	return "", false
}

// ResolveDir 校验并规范化工作目录：必须是绝对路径、必须存在、EvalSymlinks 之后
// 必须仍落在 Roots 内，返回规范化后的路径。
//
// broker 在真正 spawn 之前会再调一次，把"判定时看到的目录"和"执行时用的目录"
// 收敛成同一个规范化路径，收窄符号链接被换掉的 TOCTOU 窗口。
func (p Policy) ResolveDir(dir string) (string, error) {
	if strings.TrimSpace(dir) == "" {
		return "", errors.New("dir 为空：提权执行必须指定工作目录")
	}
	if !filepath.IsAbs(dir) {
		return "", fmt.Errorf("dir 必须是绝对路径: %q", dir)
	}
	if len(p.Roots) == 0 {
		return "", errors.New("策略未配置 Roots：任何目录都不在允许范围内（fail closed）")
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", fmt.Errorf("dir 无法解析（必须存在且可访问）: %s: %v", dir, err)
	}
	if !filepath.IsAbs(resolved) {
		return "", fmt.Errorf("dir 规范化后不是绝对路径: %q", resolved)
	}
	for _, root := range p.Roots {
		rootResolved, rootErr := filepath.EvalSymlinks(root)
		if rootErr != nil || !filepath.IsAbs(rootResolved) {
			// 根本身不可解析时跳过它：宁可拒绝，也不能拿一个"看起来像根"的拼写来比较。
			continue
		}
		if isWithin(rootResolved, resolved) {
			return resolved, nil
		}
	}
	return "", fmt.Errorf("dir 不在允许的 Roots 内: %s", dir)
}

// FilterEnv 按白名单前缀过滤客户端提供的环境变量，返回保留项与被丢弃的键名。
//
// 只返回键名、绝不返回值：诊断会进 stderr 与审计日志，值可能是密钥。
func (p Policy) FilterEnv(env map[string]string) (kept map[string]string, dropped []string) {
	if len(env) == 0 {
		return nil, nil
	}
	prefixes := p.envPrefixes()
	for key, value := range env {
		switch {
		case key == "":
			dropped = append(dropped, "<空键>")
		case !validEnvName(key):
			dropped = append(dropped, key)
		case !envNameAllowed(key, prefixes):
			dropped = append(dropped, key)
		default:
			if kept == nil {
				kept = make(map[string]string, len(env))
			}
			kept[key] = value
		}
	}
	sort.Strings(dropped)
	return kept, dropped
}

// validate 在 NewBroker 阶段把配置错误暴露出来，而不是等第一条请求才炸。
func (p Policy) validate() error {
	if len(p.Roots) == 0 {
		return errors.New("escalate: Policy.Roots 不能为空（fail closed）")
	}
	for i, root := range p.Roots {
		if strings.TrimSpace(root) == "" {
			return fmt.Errorf("escalate: Policy.Roots[%d] 为空", i)
		}
		if !filepath.IsAbs(root) {
			return fmt.Errorf("escalate: Policy.Roots[%d] 必须是绝对路径: %q", i, root)
		}
		if _, err := filepath.EvalSymlinks(root); err != nil {
			return fmt.Errorf("escalate: Policy.Roots[%d] 无法解析: %s: %v", i, root, err)
		}
	}
	for name, list := range map[string][][]string{"Allow": p.Allow, "Deny": p.Deny, "Approval": p.Approval} {
		for i, entry := range list {
			if len(entry) == 0 {
				return fmt.Errorf("escalate: Policy.%s[%d] 为空前缀（空前缀会匹配一切）", name, i)
			}
			for j, token := range entry {
				if strings.TrimSpace(token) == "" {
					return fmt.Errorf("escalate: Policy.%s[%d][%d] 为空字符串", name, i, j)
				}
			}
		}
	}
	if p.MaxOutputBytes < 0 {
		return fmt.Errorf("escalate: Policy.MaxOutputBytes 不能为负: %d", p.MaxOutputBytes)
	}
	if p.MaxTimeout < 0 {
		return fmt.Errorf("escalate: Policy.MaxTimeout 不能为负: %s", p.MaxTimeout)
	}
	if p.MaxConcurrent < 0 {
		return fmt.Errorf("escalate: Policy.MaxConcurrent 不能为负: %d", p.MaxConcurrent)
	}
	if p.MaxQueue < -1 {
		return fmt.Errorf("escalate: Policy.MaxQueue 只能是 -1（不限）或非负: %d", p.MaxQueue)
	}
	return nil
}

// maxOutputBytes 返回生效的输出上限。
func (p Policy) maxOutputBytes() int {
	if p.MaxOutputBytes > 0 {
		return p.MaxOutputBytes
	}
	return DefaultMaxOutputBytes
}

// maxTimeout 返回生效的超时上限。
func (p Policy) maxTimeout() time.Duration {
	if p.MaxTimeout > 0 {
		return p.MaxTimeout
	}
	return DefaultMaxTimeout
}

// maxQueue 返回生效的排队上限；返回 0 表示"不限"（Policy.MaxQueue == -1）。
func (p Policy) maxQueue() int {
	switch {
	case p.MaxQueue < 0:
		return 0
	case p.MaxQueue == 0:
		return DefaultMaxQueue
	default:
		return p.MaxQueue
	}
}

// maxConcurrent 返回生效的并发上限。
func (p Policy) maxConcurrent() int {
	if p.MaxConcurrent > 0 {
		return p.MaxConcurrent
	}
	return DefaultMaxConcurrent
}

// envPrefixes 返回生效的客户端环境变量前缀白名单。
func (p Policy) envPrefixes() []string {
	if len(p.EnvPrefixes) > 0 {
		return p.EnvPrefixes
	}
	return []string{"GIT_", "LC_"}
}

// matchPrefix 在 list 里找第一个"argv 的前缀"，返回命中的条目。
//
// 比较是逐元素相等而不是字符串包含：{"git"} 不会匹配 {"gitea"}。
// 唯一的放宽在**最后一个元素**：它同时匹配 "token=value" 形式，因此
// {"git","fetch","--upload-pack"} 能拦住 --upload-pack=/tmp/x（否则只要加个
// 等号就能绕过 Deny），而 --upload-package 仍然不匹配。
func matchPrefix(list [][]string, argv []string) ([]string, bool) {
	for _, entry := range list {
		if len(entry) == 0 || len(entry) > len(argv) {
			continue
		}
		matched := true
		for i, token := range entry {
			if argv[i] == token {
				continue
			}
			if i == len(entry)-1 && strings.HasPrefix(argv[i], token+"=") {
				continue
			}
			matched = false
			break
		}
		if matched {
			return entry, true
		}
	}
	return nil, false
}

// isWithin 判断 path 是否等于 root 或位于 root 之下（两者都必须已规范化）。
func isWithin(root, path string) bool {
	if path == root {
		return true
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	return !filepath.IsAbs(rel)
}

// dangerousEnvNames 是"即使前缀命中也要丢弃"的高危变量。
//
// 这些名字能让一条本来无害的命令变成任意代码执行（GIT_SSH_COMMAND 会被 git 当
// 命令跑，GIT_CONFIG_* 能注入 http.proxyCommand，GIT_EXTERNAL_DIFF 会在 diff 时
// 被执行）。沙箱内进程若能设置它们，就等于绕过了整条提权通道。
var dangerousEnvNames = map[string]struct{}{
	"GIT_SSH":                          {},
	"GIT_SSH_COMMAND":                  {},
	"GIT_EXTERNAL_DIFF":                {},
	"GIT_PROXY_COMMAND":                {},
	"GIT_ASKPASS":                      {},
	"GIT_PAGER":                        {},
	"GIT_EDITOR":                       {},
	"GIT_SEQUENCE_EDITOR":              {},
	"GIT_DIR":                          {},
	"GIT_WORK_TREE":                    {},
	"GIT_INDEX_FILE":                   {},
	"GIT_OBJECT_DIRECTORY":             {},
	"GIT_ALTERNATE_OBJECT_DIRECTORIES": {},
	"GIT_CONFIG":                       {},
	"GIT_CONFIG_GLOBAL":                {},
	"GIT_CONFIG_SYSTEM":                {},
	"GIT_CONFIG_COUNT":                 {},
	"GIT_CONFIG_PARAMETERS":            {},
	"GIT_NAMESPACE":                    {},
	"GIT_TEMPLATE_DIR":                 {},
	// GIT_EXEC_PATH 会把 git 的 helper（git-upload-pack、git-remote-*）解析到
	// 攻击者目录：`GIT_EXEC_PATH=/tmp/evil git fetch <本地仓库>` 等价于任意代码执行。
	"GIT_EXEC_PATH": {},
	"GIT_EXECPATH":  {},
	"GIT_DIFF_OPTS": {},
}

// dangerousEnvPatterns 是按前缀判定的高危变量（git 的 trace 族与 GIT_CONFIG_KEY_/VALUE_）。
var dangerousEnvPatterns = []string{
	"GIT_CONFIG_KEY_",
	"GIT_CONFIG_VALUE_",
	"GIT_TRACE",
}

// envNameAllowed 判断一个环境变量键是否在白名单内且不属于高危名字。
//
// 高危名单按**大小写不敏感**比较：Windows 的环境块不区分大小写，因此
// `GIT_ssh_command` 在那边就是 GIT_SSH_COMMAND；只在 Linux 上做精确比较等于给
// Windows 留了一个任意命令执行的口子。前缀白名单本身仍按字面匹配（`GIT_` 不等于
// `git_`），因此大小写变体既不会被误放行、也不会被误拦。
func envNameAllowed(key string, prefixes []string) bool {
	upper := strings.ToUpper(key)
	if _, bad := dangerousEnvNames[upper]; bad {
		return false
	}
	for _, pattern := range dangerousEnvPatterns {
		if strings.HasPrefix(upper, pattern) {
			return false
		}
	}
	for _, prefix := range prefixes {
		if prefix != "" && strings.HasPrefix(key, prefix) {
			return true
		}
	}
	return false
}

// validEnvName 报告 key 是否是合法的环境变量名（POSIX：字母/下划线开头，后接
// 字母、数字、下划线）。
//
// 为什么必须校验：键里含 '=' 会改写变量名（"GIT_A=1" → 实际设置 GIT_A）；
// 键里含 NUL 会在 exec 的环境块里截断出一条**新变量**（"GIT_A\x00LD_PRELOAD"
// → 多出一个 LD_PRELOAD），这正是把过滤后的白名单变成任意环境注入的方式。
// 非法键一律丢弃（宁可少一个变量，也不能多出一个变量）。
func validEnvName(key string) bool {
	if key == "" {
		return false
	}
	for i, r := range key {
		switch {
		case r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z':
		case i > 0 && r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return true
}

// defaultBaseEnvNames 是子进程默认继承的宿主变量名（不是前缀）。
//
// 选择标准：命令自身运行所需（PATH/HOME/TMPDIR）、身份与本地化（USER/LOGNAME/
// SHELL/TZ/LANG）、以及最常见的"用宿主凭据"入口（SSH_AUTH_SOCK）。其余宿主变量
// 一律不继承——提权不是"把宿主环境全抖给沙箱"。
var defaultBaseEnvNames = []string{
	"PATH", "HOME", "USER", "LOGNAME", "SHELL", "TMPDIR", "TZ", "LANG", "SSH_AUTH_SOCK",
}

// baseEnv 返回子进程的基线环境（"K=V" 的有序切片）。
func (p Policy) baseEnv() []string {
	if len(p.BaseEnv) > 0 {
		out := make([]string, 0, len(p.BaseEnv))
		for _, item := range p.BaseEnv {
			if strings.Contains(item, "=") {
				out = append(out, item)
			}
		}
		sort.Strings(out)
		return out
	}
	allowed := make(map[string]struct{}, len(defaultBaseEnvNames))
	for _, name := range defaultBaseEnvNames {
		allowed[name] = struct{}{}
	}
	var out []string
	for _, item := range os.Environ() {
		key, _, ok := strings.Cut(item, "=")
		if !ok {
			continue
		}
		if _, want := allowed[key]; want || strings.HasPrefix(key, "LC_") {
			out = append(out, item)
		}
	}
	sort.Strings(out)
	return out
}
