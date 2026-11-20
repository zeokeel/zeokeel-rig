package sandbox

// 命令级风险判定。
//
// gate 的 SandboxReadOnlyTools 只在工具粒度放行：一旦放行 bash，其中 `ls` 与
// `rm -rf /` 无差别。本文件把"一条命令会造成什么副作用"补上，使门禁可以在命令
// 粒度上做决定（只读自动放行、危险直接拒绝、其余交给人工审批）。
//
// # 判定模型
//
// 判定只依赖命令文本，不做任何 I/O 或环境探测，因此是确定性的、可离线测试的：
//
//   - RiskReadOnly  无持久副作用（读文件、打印、查询状态）；
//   - RiskMutating  会写文件或改状态（cp/mkdir/sed -i/git commit/重定向写文件）；
//   - RiskDangerous 明确破坏性（rm -rf 根、dd 到块设备、mkfs、关机、下载即执行、
//     写系统目录、写 git 控制面（.git/hooks、.git/config、.git/info、
//     .git/modules/**/config）、Windows 的 format/diskpart/rd /s /q C:\）；
//   - RiskUnknown   无法静态判定（未收录的程序、动态命令名、解析失败）。
//
// 判定遵循"宁严勿松"：拿不到证据就降级为 unknown（= 需要审批），绝不给
// "看起来像只读"的程序自动放行权。提高风险的方向是安全的（只读→unknown 只是
// 多问一次人），降低风险必须有明确规则支撑。
//
// # 规则表结构
//
// 判定是"有序规则表 + 首个命中"：危险规则 → 条件的 unknown 规则 → 只读规则 →
// 写入规则。每行是一个 commandRule{ID, Reason, Risk, Match}，ID 是稳定契约
// （审计日志与测试按它断言，例如 "rm.recursive-root"），Reason 是面向人的中文
// 短句。新增一条规则 = 在对应构造函数里加一行 + 在 command_risk_test.go 里加
// 用例，不需要改判定引擎。
//
// # 平台语义
//
// 程序名规范化复用 command.go 的既有实现（normalizeCommandName）：Windows 上
// 忽略大小写与 ".exe"，其他平台按字面比较。脚本级判定（ClassifyShellCommand）
// 固定用 POSIX 语义，因为它对应 /bin/sh -c。
//
// # 与 OS 沙箱的关系
//
// 本判定是进程内策略，不是安全边界：它只能约束"配合的模型发起的、形态可识别"
// 的命令。混淆（base64 解码后执行、动态拼装、编译后运行）一律无法识别，因此
// 边界清单见 SECURITY.md 的“已知边界”一节。
//
// 其中 git 控制面（`write.git.control`）是一条**刻意加在命令层**的防线：Landlock
// 后端的并集语义无法表达"根可写但 .git/config、.git/hooks 只读"，只能把 .git
// 整根授予 rw 才能让 `git commit` 工作；而 .git/hooks/** 是 git 会直接执行的
// 代码。进程内围栏（write_file/apply_patch）看不到 shell 重定向，所以这条规则是
// 唯一能拦住 `echo evil > .git/hooks/pre-commit` 的地方。
// 依然需要 OS 沙箱（sandbox/osrunner）或容器作为真正的边界。

import (
	"runtime"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// CommandRisk 是一条命令的副作用等级。取值随严重度递增，调用方可以安全地用
// "取最大值"的方式合并多条命令的判定（管道、列表、子 shell）。
type CommandRisk int

const (
	// RiskUnknown 表示无法静态判定，调用方应按"需要审批"处理。它是零值，
	// 因此任何零值评估都失败即保守。
	RiskUnknown CommandRisk = iota
	// RiskReadOnly 表示没有持久副作用：只读文件系统、打印或查询状态。
	RiskReadOnly
	// RiskMutating 表示会写文件或改变状态（但仍可能是正常开发操作）。
	RiskMutating
	// RiskDangerous 表示明确破坏性：会造成不可逆的数据丢失、系统损坏或
	// 下载即执行。
	RiskDangerous
)

// String 返回风险等级的名字，用于日志与门禁文案。
func (r CommandRisk) String() string {
	switch r {
	case RiskReadOnly:
		return "read-only"
	case RiskMutating:
		return "mutating"
	case RiskDangerous:
		return "dangerous"
	default:
		return "unknown"
	}
}

// CommandAssessment 是一次命令判定的完整结果。
type CommandAssessment struct {
	// Risk 是判定出的风险等级。
	Risk CommandRisk
	// Program 是 argv[0] 的基名（去掉目录部分）；按平台语义规范化
	// （Windows 上小写并去掉 .exe）。
	Program string
	// Argv 是解析后的 argv（调用方切片的副本）；无法解析时为 nil。
	Argv []string
	// Reason 是面向人的中文短句，例如 "rm 递归删除指向根目录"。
	Reason string
	// Detail 是命中的具体规则 id，例如 "rm.recursive-root"；它比 Reason 稳定，
	// 适合审计日志与统计。
	Detail string
}

// ClassifyCommand 判定一条已分词命令（argv）的风险。
//
// Windows 上按 Windows 程序名语义判定，其他平台按字面判定。argv 为空、程序名是
// 动态展开、或程序未收录时返回 RiskUnknown（调用方应按需要审批处理）。
func ClassifyCommand(argv []string) CommandAssessment {
	return classifyCommand(argv, runtime.GOOS == "windows")
}

// maxClassifyDepth 限制递归判定（bash -c、xargs、find -exec、函数调用）的深度，
// 避免构造出的深层嵌套消耗栈或绕过判定。
const maxClassifyDepth = 4

// 解析前的输入闸门。两条独立上限，缺一不可：
//
//   - maxShellScriptBytes：Linux 的 MAX_ARG_STRLEN 是 128 KiB，再长的脚本根本
//     无法作为单个 argv 交给 `sh -c`，分析它没有语义价值；
//   - maxShellNesting：mvdan.cc/sh 的 bash 方言解析器是递归下降的，
//     `strings.Repeat("(", 150000)` 这类输入会耗尽 goroutine 栈并触发
//     `fatal error: stack overflow`——**那是无法 recover 的进程级崩溃**，会把
//     宿主整个带走。按嵌套字符数先挡一道，与栈帧大小无关地封住深度。
const (
	maxShellScriptBytes = 16 << 10
	maxShellNesting     = 4096
)

// shellScriptTooComplex 在解析前挡掉"过长/过深"的脚本，返回命中的 detail。
//
// 只读判定会变成门禁自动放行，而这类输入既不可执行也不可分析，一律按
// RiskUnknown（需要审批）处理——保守方向。
func shellScriptTooComplex(script string) (string, bool) {
	if len(script) > maxShellScriptBytes {
		return "shell.too-large", true
	}
	nesting := 0
	for index := 0; index < len(script); index++ {
		switch script[index] {
		case '(', '[', '{':
			nesting++
			if nesting > maxShellNesting {
				return "shell.too-nested", true
			}
		}
	}
	return "", false
}

// classifyCommand 是 ClassifyCommand 的平台参数化实现：windows 决定程序名比较
// 语义与 Windows 专属规则（format/diskpart/rd /s /q 等）。它让 Windows 分支能在
// 任意宿主上被测试，与 command.go 的 checkCommand 做法一致。
func classifyCommand(argv []string, windows bool) CommandAssessment {
	return classifyCommandDepth(argv, windows, 0)
}

// classifyCommandDepth 是带递归深度的判定实现。
func classifyCommandDepth(argv []string, windows bool, depth int) CommandAssessment {
	if depth > maxClassifyDepth {
		return CommandAssessment{
			Risk:   RiskUnknown,
			Reason: "命令嵌套过深，无法静态判定",
			Detail: "recursion.depth",
		}
	}
	if len(argv) == 0 {
		return CommandAssessment{
			Risk:   RiskUnknown,
			Reason: "命令为空，无法判定副作用",
			Detail: "argv.empty",
		}
	}
	copied := append([]string(nil), argv...)
	program := commandBaseName(copied[0], windows)
	if program == "" {
		return CommandAssessment{
			Risk:   RiskUnknown,
			Argv:   copied,
			Reason: "命令名为空，无法判定副作用",
			Detail: "argv.empty",
		}
	}
	// 动态命令名（$CMD、`whoami`）无法静态判定。
	if strings.ContainsAny(program, "$`") {
		return CommandAssessment{
			Risk:    RiskUnknown,
			Program: program,
			Argv:    copied,
			Reason:  "命令名包含动态展开，无法静态判定",
			Detail:  "argv.dynamic-name",
		}
	}

	view := &commandView{argv: copied, prog: program, args: copied[1:], windows: windows, depth: depth}
	// 透明包装器（bash -c、xargs、find -exec、eval、sudo、env …）先递归判定其
	// 内部命令，这样 `bash -c 'rm -rf /'` 与直接 `rm -rf /` 得到同样的结论。
	if inner, ok := wrappedAssessment(view); ok {
		return inner
	}
	for _, rule := range commandRiskRules {
		if rule.Match != nil && rule.Match(view) {
			return CommandAssessment{
				Risk:    rule.Risk,
				Program: program,
				Argv:    copied,
				Reason:  rule.Reason,
				Detail:  rule.ID,
			}
		}
	}
	return CommandAssessment{
		Risk:    RiskUnknown,
		Program: program,
		Argv:    copied,
		Reason:  "未收录的命令 " + program + "：无法判定它是否会写文件或改状态",
		Detail:  "unknown.program",
	}
}

// commandView 是一次判定的规范化视图。
type commandView struct {
	argv    []string
	prog    string   // 规范化后的程序名
	args    []string // argv[1:]
	windows bool
	depth   int
}

// isWin 报告该命令是否是指定的 Windows 程序（仅在 windows 平台语义下）。
func (c *commandView) isWin(program string) bool {
	return c.windows && c.prog == program
}

// commandRule 是规则表的一行。
type commandRule struct {
	// ID 是稳定规则 id（审计/测试契约）。
	ID string
	// Reason 是面向人的中文短句。
	Reason string
	// Risk 是命中后的风险等级。
	Risk CommandRisk
	// Match 报告该规则是否命中。
	Match func(*commandView) bool
}

// commandRiskRules 是有序规则表：首个命中即生效。危险规则在前，因此
// `rm -rf /etc` 得到 rm.recursive-root 而不是 write.system-path。
var commandRiskRules = buildCommandRiskRules()

// buildCommandRiskRules 按"危险 → 条件 unknown → 只读 → 写入"的顺序拼装规则表。
// 新增规则只需要在对应的构造函数里加一行。
func buildCommandRiskRules() []commandRule {
	rules := make([]commandRule, 0, 160)
	rules = append(rules, dangerousCommandRules()...)
	rules = append(rules, conditionalUnknownCommandRules()...)
	rules = append(rules, readOnlyCommandRules()...)
	rules = append(rules, mutatingCommandRules()...)
	return rules
}

// --- git 控制面（命令层逃逸防护）------------------------------------------

// gitControlRuleID 是"写 git 控制面"的审计规则 id。
const gitControlRuleID = "write.git.control"

// gitControlModeRuleID 是"改 git 控制面权限位"的审计规则 id（chmod/chattr 不写
// 内容，但能让一个已存在的 hook 变成可执行，同样是逃逸）。
const gitControlModeRuleID = "write.git.control-mode"

// gitControlReason 是模型可见的理由（英文：工具的 Description/错误文案不翻译）。
const gitControlReason = "git hooks are executed by git; git config can redirect hooks/pagers/urls " +
	"(command execution or credential exfiltration). Rewrite history with git commands instead, " +
	"and ask the user to change repository configuration."

// gitControlModeReason 是权限位改动的理由：额外说明"不写内容也算"。
const gitControlModeReason = gitControlReason + " Changing a hook's mode is the same escape: " +
	"chmod +x only makes an already-present hook executable, and git runs it."

// isGitControlPath 报告一个路径参数是否落在 git 控制面上。
func isGitControlPath(path string) bool { return gitControlPlaneKind(path) != "" }

// gitControlPlaneKind 把路径归一化后判定它是否属于 git 控制面，返回类别
// （hooks / config / info / modules）。
//
// 匹配的是"任意仓库"的 <any>/.git/<控制面>：不要求绝对路径，`./.git/hooks/x`、
// `$PWD/.git/hooks/x`、`~/repo/.git/hooks/x` 都算。`only 字面量`：动态拼出的路径
// 识别不了（见 SECURITY.md 的“已知边界”一节）。
func gitControlPlaneKind(rawPath string) string {
	normalized := normalizeGitControlPath(rawPath)
	if normalized == "" {
		return ""
	}
	// .git 段按大小写不敏感匹配：Windows/macOS 上 .GIT 仍是同一个仓库目录，
	// 而 POSIX 上极少有人真的建一个叫 .GIT 的普通目录——宁可多拦。
	lowered := strings.ToLower(normalized)
	index := strings.Index(lowered, ".git/")
	if index < 0 {
		return ""
	}
	if index > 0 && normalized[index-1] != '/' {
		return ""
	}
	rest := lowered[index+len(".git/"):]
	switch {
	case rest == "hooks" || strings.HasPrefix(rest, "hooks/"):
		return "hooks"
	case rest == "config" || rest == "config.worktree":
		return "config"
	case rest == "info" || strings.HasPrefix(rest, "info/"):
		return "info"
	case rest == "modules/config" || strings.HasPrefix(rest, "modules/") && strings.HasSuffix(rest, "/config"):
		return "modules"
	}
	return ""
}

// normalizeGitControlPath 把可能的 shell 片段规范成用于路径比较的形式：
// 去外层引号、反斜杠转正斜杠、去掉 $PWD/${PWD}/$HOME/~ 前缀与 ./ 段。
func normalizeGitControlPath(rawPath string) string {
	s := stripOuterQuotes(strings.TrimSpace(rawPath))
	if s == "" {
		return ""
	}
	s = strings.ReplaceAll(s, `\`, "/")
	for _, prefix := range []string{"${PWD}/", "$PWD/", "${HOME}/", "$HOME/", "~/", "$HOME"} {
		s = strings.TrimPrefix(s, prefix)
	}
	for strings.HasPrefix(s, "./") {
		s = s[2:]
	}
	for strings.Contains(s, "/./") {
		s = strings.ReplaceAll(s, "/./", "/")
	}
	for strings.Contains(s, "//") {
		s = strings.ReplaceAll(s, "//", "/")
	}
	return s
}

// gitControlPathInText 在一段文本（解释器 -c/-e 的内联代码）里找字面量的 git
// 控制面路径；找不到返回空串。
func gitControlPathInText(text string) string {
	fields := strings.FieldsFunc(text, func(r rune) bool {
		switch r {
		case ' ', '\t', '\n', '\r', '"', '\'', '(', ')', '[', ']', '{', '}', ',', ';', ':', '=', '+', '|', '&', '<', '>':
			return true
		}
		return false
	})
	for _, field := range fields {
		if kind := gitControlPlaneKind(field); kind != "" {
			return field
		}
	}
	return ""
}

// interpreterPrograms 是"内联代码里写字面量路径"的解释器。
var interpreterPrograms = map[string]bool{
	"python": true, "python3": true, "node": true, "nodejs": true,
	"perl": true, "ruby": true, "php": true, "deno": true, "bun": true,
}

// interpreterWritesGitControl 识别 `python -c 'open(".git/hooks/x","w")'` /
// `node -e 'writeFileSync(".git/config")'` / `perl -pi -e …` 这类内联写文件。
//
// 只覆盖字面量路径 + 明确的写标记；动态拼装识别不了（KNOWN-BOUNDARIES）。
func interpreterWritesGitControl(c *commandView) bool {
	if !interpreterPrograms[c.prog] {
		return false
	}
	joined := strings.Join(c.args, " ")
	if gitControlPathInText(joined) == "" {
		return false
	}
	lowered := strings.ToLower(joined)
	for _, marker := range []string{
		"open(", "writefile", "writefilesync", "appendfile", "createwritestream",
		"syswrite", "file.write", "'w'", `"w"`, "'a'", `"a"`, "-i", "-pi", "-i.bak",
		"truncate", "unlink", "rename", "chmod",
	} {
		if strings.Contains(lowered, marker) {
			return true
		}
	}
	return false
}

// gitConfigWrite 报告是否是"写 git 配置"（git config 的非只读形态）。
//
// `git config --get/--list/…` 是只读，交给 read.git 规则；其余（`git config
// user.name X`、`--unset`、`--add`、`-e`、`core.hooksPath …`）都会改控制面。
func gitConfigWrite(c *commandView) bool {
	if c.prog != "git" {
		return false
	}
	positional := nonFlagArgs(c.args)
	if len(positional) == 0 || positional[0] != "config" {
		return false
	}
	return !gitReadOnly(c.args)
}

// gitExecutionConfigKeys 是 `git -c key=value` 里能导致"执行代码或把请求/凭据
// 引到别处"的配置键（小写比较，前缀匹配用 HasPrefix）。
var gitExecutionConfigKeys = []string{
	"core.hookspath", "core.pager", "core.sshcommand", "core.fsmonitor",
	"core.askpass", "credential.helper", "diff.external", "alias.",
	"url.", "gpg.program", "sequence.editor",
}

// gitDashCExecutes 报告 `git -c key=value …` 是否在用一次性配置改变执行面。
//
// 这不是"写 config 文件"，但效果一样：`git -c core.hooksPath=/tmp/h commit` 会让
// git 执行 /tmp/h 里的脚本（/tmp 可写）。`git -c user.email=…` 这类正常用法不受
// 影响。
func gitDashCExecutes(c *commandView) bool {
	if c.prog != "git" {
		return false
	}
	for index := 0; index < len(c.args); index++ {
		arg := c.args[index]
		value := ""
		switch {
		case arg == "-c":
			if index+1 >= len(c.args) {
				continue
			}
			value = c.args[index+1]
		case strings.HasPrefix(arg, "-c") && len(arg) > 2:
			value = arg[2:]
		default:
			continue
		}
		value = strings.ToLower(stripOuterQuotes(value))
		for _, key := range gitExecutionConfigKeys {
			if strings.HasPrefix(value, key) {
				return true
			}
		}
	}
	return false
}

// targetDirectoryArg 返回 cp/mv/install/ln 的 -t/--target-directory 值。
func targetDirectoryArg(args []string) string {
	for index, arg := range args {
		switch {
		case arg == "-t" || arg == "--target-directory":
			if index+1 < len(args) {
				return args[index+1]
			}
		case strings.HasPrefix(arg, "--target-directory="):
			return strings.TrimPrefix(arg, "--target-directory=")
		}
	}
	return ""
}

// gitControlWriteTargets 返回一条命令会写入的目标路径（只覆盖"写文件"的真实写法：
// 重定向由 classifyRedirectTarget 负责，这里管命令参数）。
//
// 不追求完备：`patch`、`tar -x`、`git checkout -- .git/...` 之类写哪些文件由输入
// 决定，识别不了，见 SECURITY.md 的“已知边界”一节。
func gitControlWriteTargets(c *commandView) []string {
	positional := nonFlagArgs(c.args)
	switch c.prog {
	case "cp", "mv", "ln", "install", "rsync", "scp", "unlink":
		targets := make([]string, 0, 2)
		if dir := targetDirectoryArg(c.args); dir != "" {
			targets = append(targets, dir)
		}
		if len(positional) > 0 {
			targets = append(targets, positional[len(positional)-1])
		}
		return targets
	case "dd":
		out := make([]string, 0, 1)
		for _, arg := range c.args {
			if strings.HasPrefix(arg, "of=") {
				out = append(out, strings.TrimPrefix(arg, "of="))
			}
		}
		return out
	case "tee", "truncate", "shred", "touch":
		return positional
	case "sed":
		if !sedInPlace(c.args) || len(positional) == 0 {
			return nil
		}
		return positional[len(positional)-1:]
	case "chmod", "chown", "chgrp":
		return permissionTargets(c.args)
	case "chattr":
		out := make([]string, 0, len(positional))
		for _, arg := range positional {
			if strings.HasPrefix(arg, "-") || strings.HasPrefix(arg, "+") || strings.HasPrefix(arg, "=") {
				continue
			}
			out = append(out, arg)
		}
		return out
	}
	return nil
}

// dangerousCommandRules 返回明确破坏性的规则。
func dangerousCommandRules() []commandRule {
	return []commandRule{
		{
			ID:     "rm.recursive-root",
			Risk:   RiskDangerous,
			Reason: "rm 递归删除指向根目录、家目录或工作区外路径",
			Match: func(c *commandView) bool {
				return c.prog == "rm" && rmIsRecursive(c.args) && anyArg(c.args, isDangerousDeleteTarget)
			},
		},
		{
			ID:     "mv.root",
			Risk:   RiskDangerous,
			Reason: "mv 的源是根目录或根目录通配",
			Match: func(c *commandView) bool {
				return c.prog == "mv" && anyArg(c.args, isRootLikeSource)
			},
		},
		{
			ID:     "dd.device",
			Risk:   RiskDangerous,
			Reason: "dd 直接写入块设备",
			Match: func(c *commandView) bool {
				return c.prog == "dd" && anyArg(c.args, ddWritesDevice)
			},
		},
		{
			ID:     "mkfs",
			Risk:   RiskDangerous,
			Reason: "mkfs 会重建文件系统",
			Match: func(c *commandView) bool {
				return strings.HasPrefix(c.prog, "mkfs")
			},
		},
		{
			ID:     "system.power",
			Risk:   RiskDangerous,
			Reason: "关机/重启会中断所有会话与进程",
			Match: func(c *commandView) bool {
				switch c.prog {
				case "shutdown", "reboot", "halt", "poweroff", "init", "kexec":
					return true
				case "systemctl":
					return len(c.args) > 0 && powerVerb(c.args[0])
				}
				return false
			},
		},
		{
			ID:     "chmod.recursive-root",
			Risk:   RiskDangerous,
			Reason: "chmod -R 作用于根目录或系统目录",
			Match: func(c *commandView) bool {
				if c.prog != "chmod" || !rmIsRecursive(c.args) {
					return false
				}
				return anyArg(permissionTargets(c.args), isDangerousWriteTarget)
			},
		},
		{
			ID:     "chown.recursive-root",
			Risk:   RiskDangerous,
			Reason: "chown -R 作用于根目录或系统目录",
			Match: func(c *commandView) bool {
				if c.prog != "chown" && c.prog != "chgrp" || !rmIsRecursive(c.args) {
					return false
				}
				return anyArg(permissionTargets(c.args), isDangerousWriteTarget)
			},
		},
		{
			ID:     "git.push-force-protected",
			Risk:   RiskDangerous,
			Reason: "git 强制推送到受保护分支会覆盖远端历史",
			Match:  gitForcePushToProtected,
		},
		{
			// 命令层拦住"往 git 控制面写"。
			//
			// 为什么必须在命令层做：Landlock 后端无法表达"根可写但 .git/config、
			// .git/hooks 只读"（并集语义），只能把 .git 整根授予 rw 才能让
			// `git commit` 工作；于是 `bash -c 'echo evil > .git/hooks/pre-commit'`
			// 在 OS 层会成功，而 .git/hooks/** 是 git 会直接执行的代码。进程内围栏
			// （write_file/apply_patch）看不到 shell 重定向，所以这里是唯一能拦的地方。
			ID:     gitControlRuleID,
			Risk:   RiskDangerous,
			Reason: gitControlReason,
			Match: func(c *commandView) bool {
				if gitConfigWrite(c) || gitDashCExecutes(c) {
					return true
				}
				if interpreterWritesGitControl(c) {
					return true
				}
				return anyArg(gitControlWriteTargets(c), isGitControlPath)
			},
		},
		{
			// 权限位不是"写内容"，但 `chmod +x .git/hooks/pre-commit` 会让一个
			// 已经存在的 hook 变成可执行——同样是"让 git 去跑仓库自带代码"的逃逸。
			ID:     gitControlModeRuleID,
			Risk:   RiskDangerous,
			Reason: gitControlModeReason,
			Match: func(c *commandView) bool {
				switch c.prog {
				case "chmod", "chown", "chgrp", "chattr":
				default:
					return false
				}
				return anyArg(gitControlWriteTargets(c), isGitControlPath)
			},
		},
		{
			ID:     "write.system-path",
			Risk:   RiskDangerous,
			Reason: "写系统目录会破坏宿主环境",
			Match: func(c *commandView) bool {
				return anyArg(writeTargets(c), isDangerousWriteTarget)
			},
		},
		{
			ID:     "win.format",
			Risk:   RiskDangerous,
			Reason: "format 会格式化磁盘卷",
			Match:  func(c *commandView) bool { return c.isWin("format") },
		},
		{
			ID:     "win.diskpart",
			Risk:   RiskDangerous,
			Reason: "diskpart 会改写磁盘分区",
			Match:  func(c *commandView) bool { return c.isWin("diskpart") },
		},
		{
			ID:     "win.rd-root",
			Risk:   RiskDangerous,
			Reason: "rd /s /q 删除整个磁盘卷",
			Match: func(c *commandView) bool {
				if !c.windows || c.prog != "rd" && c.prog != "rmdir" && c.prog != "deltree" {
					return false
				}
				return hasWindowsSwitch(c.args, "/s") && anyArg(c.args, isWindowsDriveRoot)
			},
		},
		{
			ID:     "win.del-root",
			Risk:   RiskDangerous,
			Reason: "del /s /q 递归删除系统目录或整个卷",
			Match: func(c *commandView) bool {
				if !c.windows || c.prog != "del" && c.prog != "erase" {
					return false
				}
				return anyArg(c.args, func(target string) bool {
					trimmed := strings.TrimSuffix(target, "*")
					return isWindowsDriveRoot(trimmed) || isWindowsSystemPath(trimmed)
				})
			},
		},
		{
			ID:     "win.reg-delete",
			Risk:   RiskDangerous,
			Reason: "reg delete 改写系统注册表",
			Match: func(c *commandView) bool {
				if !c.windows || c.prog != "reg" || len(c.args) == 0 {
					return false
				}
				if !strings.EqualFold(c.args[0], "delete") {
					return false
				}
				return anyArg(c.args[1:], isRegistryRoot)
			},
		},
		{
			ID:     "win.remove-item-root",
			Risk:   RiskDangerous,
			Reason: "Remove-Item -Recurse 作用于卷根",
			Match: func(c *commandView) bool {
				if !c.windows || c.prog != "remove-item" && c.prog != "ri" {
					return false
				}
				if !hasWindowsSwitch(c.args, "-recurse") {
					return false
				}
				return anyArg(c.args, isWindowsDriveRoot)
			},
		},
	}
}

// conditionalUnknownCommandRules 返回"看起来正常但无法判定"的规则；它们比只读
// 规则先求值，因此把可执行的副作用挡在自动放行之外。
func conditionalUnknownCommandRules() []commandRule {
	return []commandRule{
		{
			ID:     "awk.side-effect",
			Risk:   RiskUnknown,
			Reason: "awk 脚本含 system() 或输出重定向，副作用无法静态判定",
			Match: func(c *commandView) bool {
				return isAwkProgram(c.prog) && awkHasSideEffect(c.args)
			},
		},
		{
			ID:     "shell.source",
			Risk:   RiskUnknown,
			Reason: "source/. 会执行外部脚本，内容无法静态判定",
			Match: func(c *commandView) bool {
				return c.prog == "source" || c.prog == "."
			},
		},
		{
			// 只读程序的"会执行外部命令"的选项：sort 的 --compress-program 用来
			// 压缩临时文件、rg 的 --pre 用来预处理文件，两者都会真的 spawn 一个
			// 程序（且参数完全由调用方控制）。
			ID:     "option.exec",
			Risk:   RiskUnknown,
			Reason: "选项会调用外部命令（sort --compress-program / rg --pre），无法静态判定",
			Match: func(c *commandView) bool {
				switch c.prog {
				case "sort":
					return hasLongFlag(c.args, "--compress-program")
				case "rg":
					return hasLongFlag(c.args, "--pre")
				}
				return false
			},
		},
		{
			// sed 的只读性不能只看 -i：`e` 命令会在脚本里执行 shell，
			// `s///e` 标志把替换结果交给 shell，`w`/`W` 直接写文件，
			// `-f 脚本文件` 的内容则完全看不到。任一条成立都必须退出"自动放行"。
			ID:     "sed.embedded-command",
			Risk:   RiskUnknown,
			Reason: "sed 脚本含 e/w 命令或 s///e 标志：会执行命令或写文件，无法静态判定",
			Match: func(c *commandView) bool {
				return c.prog == "sed" && sedScriptUnsafe(c.args)
			},
		},
	}
}

// readOnlyCommandRules 返回只读规则。带参数的只读程序（sed/awk/find/sort/date/
// env/git/go/kubectl/npm/reg）用谓词收窄到"确实没有副作用"的形态。
func readOnlyCommandRules() []commandRule {
	rules := make([]commandRule, 0, 96)
	for _, prog := range posixReadOnlyPrograms {
		program := prog
		rules = append(rules, commandRule{
			ID:     "read." + program,
			Risk:   RiskReadOnly,
			Reason: "只读命令：不写文件、不改状态",
			Match:  func(c *commandView) bool { return c.prog == program },
		})
	}
	for _, prog := range windowsReadOnlyPrograms {
		program := prog
		rules = append(rules, commandRule{
			ID:     "read." + program,
			Risk:   RiskReadOnly,
			Reason: "只读命令：不写文件、不改状态",
			Match:  func(c *commandView) bool { return c.windows && c.prog == program },
		})
	}
	return append(rules,
		commandRule{
			ID:     "read.sed",
			Risk:   RiskReadOnly,
			Reason: "sed 只读输出（无 -i）",
			Match:  func(c *commandView) bool { return c.prog == "sed" && !sedInPlace(c.args) },
		},
		commandRule{
			ID:     "read.awk",
			Risk:   RiskReadOnly,
			Reason: "awk 只读输出（无 system/重定向）",
			Match:  func(c *commandView) bool { return isAwkProgram(c.prog) && !awkHasSideEffect(c.args) },
		},
		commandRule{
			ID:     "read.find",
			Risk:   RiskReadOnly,
			Reason: "find 只遍历（无 -delete/-exec/-fprint）",
			Match:  func(c *commandView) bool { return c.prog == "find" && !findHasAction(c.args) },
		},
		commandRule{
			ID:     "read.sort",
			Risk:   RiskReadOnly,
			Reason: "sort 只输出（无 -o）",
			Match: func(c *commandView) bool {
				return c.prog == "sort" && !hasShortFlag(c.args, 'o') && !hasLongFlag(c.args, "--output")
			},
		},
		commandRule{
			ID:     "read.date",
			Risk:   RiskReadOnly,
			Reason: "date 只打印（无 -s/--set）",
			Match: func(c *commandView) bool {
				return c.prog == "date" && !dateSetsClock(c.args)
			},
		},
		commandRule{
			ID:     "read.env",
			Risk:   RiskReadOnly,
			Reason: "env 只打印环境变量",
			Match:  func(c *commandView) bool { return c.prog == "env" && len(envInnerArgs(c.args)) == 0 },
		},
		commandRule{
			ID:     "read.command-query",
			Risk:   RiskReadOnly,
			Reason: "command -v/-V 只查询命令位置",
			Match: func(c *commandView) bool {
				return c.prog == "command" && (hasShortFlag(c.args, 'v') || hasShortFlag(c.args, 'V'))
			},
		},
		commandRule{
			ID:     "read.git",
			Risk:   RiskReadOnly,
			Reason: "git 只读子命令",
			Match:  func(c *commandView) bool { return c.prog == "git" && gitReadOnly(c.args) },
		},
		commandRule{
			ID:     "read.go",
			Risk:   RiskReadOnly,
			Reason: "go 只读子命令",
			Match:  func(c *commandView) bool { return c.prog == "go" && goReadOnly(c.args) },
		},
		commandRule{
			ID:     "read.kubectl",
			Risk:   RiskReadOnly,
			Reason: "kubectl 只读子命令",
			Match:  func(c *commandView) bool { return c.prog == "kubectl" && kubectlReadOnly(c.args) },
		},
		commandRule{
			ID:     "read.npm",
			Risk:   RiskReadOnly,
			Reason: "npm 只读子命令",
			Match:  func(c *commandView) bool { return c.prog == "npm" && npmReadOnly(c.args) },
		},
		commandRule{
			ID:     "read.hostname",
			Risk:   RiskReadOnly,
			Reason: "hostname 只打印主机名（无参数）",
			Match:  func(c *commandView) bool { return c.prog == "hostname" && len(nonFlagArgs(c.args)) == 0 },
		},
		commandRule{
			ID:     "read.file",
			Risk:   RiskReadOnly,
			Reason: "file 只识别文件类型（无 -C/--compile）",
			Match: func(c *commandView) bool {
				return c.prog == "file" && !hasShortFlag(c.args, 'C') && !hasLongFlag(c.args, "--compile")
			},
		},
		commandRule{
			ID:     "read.reg-query",
			Risk:   RiskReadOnly,
			Reason: "reg query 只读",
			Match: func(c *commandView) bool {
				return c.windows && c.prog == "reg" && len(c.args) > 0 && strings.EqualFold(c.args[0], "query")
			},
		},
	)
}

// mutatingCommandRules 返回"至少会写文件/改状态"的规则。
func mutatingCommandRules() []commandRule {
	rules := make([]commandRule, 0, 96)
	for _, prog := range mutatingPrograms {
		program := prog
		rules = append(rules, commandRule{
			ID:     "write." + program,
			Risk:   RiskMutating,
			Reason: "会写文件或改变状态",
			Match:  func(c *commandView) bool { return c.prog == program },
		})
	}
	for _, prog := range windowsMutatingPrograms {
		program := prog
		rules = append(rules, commandRule{
			ID:     "write." + program,
			Risk:   RiskMutating,
			Reason: "会写文件或改变状态",
			Match:  func(c *commandView) bool { return c.windows && c.prog == program },
		})
	}
	return append(rules,
		commandRule{
			ID:     "write.curl",
			Risk:   RiskMutating,
			Reason: "curl/wget 带输出文件参数会写文件",
			Match: func(c *commandView) bool {
				return (c.prog == "curl" || c.prog == "wget") && downloadWritesFile(c.args)
			},
		},
	)
}

// posixReadOnlyPrograms 是"无论参数都没有持久副作用"的程序名（POSIX/GNU 常见
// 工具）。带副作用的参数形态（sed -i、sort -o、date -s、find -delete 等）由上面
// 的条件规则单独处理。
var posixReadOnlyPrograms = []string{
	"ls", "cat", "head", "tail", "less", "more", "wc", "stat", "du", "df",
	"pwd", "echo", "printf", "which", "type", "whereis", "id", "whoami", "uname",
	"printenv", "grep", "egrep", "fgrep", "rg", "ag", "jq", "uniq", "cut", "tr",
	"diff", "cmp", "tree", "sha256sum", "sha1sum", "md5sum", "basename", "dirname",
	"realpath", "readlink", "test", "[", "[[", "true", "false", "sleep",
	"nproc", "uptime", "free", "ps", "clear", "column", "tac", "rev", "seq", "expr",
	// shell 内建：只影响当前 shell 的进程内状态，不产生持久副作用。
	"cd", "export", "unset", "shift", "wait", "jobs", "read", "builtin", "hash",
}

// windowsReadOnlyPrograms 是 Windows/PowerShell 的只读程序（仅在 windows 平台
// 语义下生效，避免在 POSIX 上把同名程序误判为安全）。
var windowsReadOnlyPrograms = []string{
	"dir", "findstr", "where", "ver", "tasklist", "systeminfo", "ipconfig",
	"get-childitem", "get-content", "get-item", "get-location", "select-string",
	"get-process", "get-service", "resolve-path", "test-path", "measure-object",
	"sort-object", "format-table", "get-command", "get-help", "write-output",
	"select-object",
}

// mutatingPrograms 是"默认会写文件/改状态"的程序名。判定表把它们放在只读规则
// 之后，因此 sed/sort/date/find/env 的只读形态仍会先命中只读规则。
var mutatingPrograms = []string{
	"cp", "mv", "rm", "rmdir", "mkdir", "touch", "chmod", "chown", "chgrp", "ln",
	"tee", "truncate", "install", "shred", "dd", "rsync", "tar", "unzip", "zip",
	"gzip", "gunzip", "bzip2", "xz", "patch", "sed", "sort", "date", "find", "env",
	"git", "go", "kubectl", "npm", "yarn", "pnpm", "pip", "pip3", "cargo",
	"make", "cmake", "ninja",
	"hostname", "file",
	"systemctl", "service", "kill", "pkill", "killall", "ssh", "scp", "crontab",
	"at", "mount", "umount", "swapon", "swapoff", "useradd", "userdel", "usermod",
	"groupadd", "passwd", "visudo", "modprobe", "insmod", "rmmod", "sysctl",
	"helm", "terraform", "ansible", "ansible-playbook",
}

// windowsMutatingPrograms 是 Windows/PowerShell 的写入类程序。
var windowsMutatingPrograms = []string{
	"copy", "xcopy", "robocopy", "move", "ren", "rename", "del", "erase", "rd",
	"deltree", "attrib", "icacls", "takeown", "reg", "regedit", "sc", "net",
	"set-content", "add-content", "out-file", "new-item", "remove-item",
	"move-item", "copy-item", "rename-item", "set-itemproperty", "start-process",
	"stop-process", "invoke-webrequest", "invoke-restmethod", "invoke-expression",
	"iex", "ni", "ri", "mkdir", "md", "mklink", "fsutil",
}

// ---------- 程序名与参数工具 ----------

// commandBaseName 返回程序名的基名并按平台语义规范化。它同时接受 POSIX 与
// Windows 的目录分隔符，因此在 Linux 上也能正确判定 `C:\Windows\x.exe`。
func commandBaseName(name string, windows bool) string {
	name = strings.TrimSpace(name)
	if index := strings.LastIndexAny(name, `/\`); index >= 0 {
		name = name[index+1:]
	}
	return normalizeCommandName(name, windows)
}

// shortFlags 返回单个短选项参数里的字母集合（"-rf" → "rf"）；长选项返回空。
func shortFlags(arg string) string {
	if len(arg) < 2 || arg[0] != '-' || arg[1] == '-' {
		return ""
	}
	return arg[1:]
}

// hasShortFlag 报告是否出现某个短选项字母（支持合并写法 -rf、-fr）。
func hasShortFlag(args []string, letter rune) bool {
	for _, arg := range args {
		if strings.ContainsRune(shortFlags(arg), letter) {
			return true
		}
	}
	return false
}

// hasLongFlag 报告是否出现某个长选项（含 --flag=value 形态）。
func hasLongFlag(args []string, names ...string) bool {
	for _, arg := range args {
		for _, name := range names {
			if arg == name || strings.HasPrefix(arg, name+"=") {
				return true
			}
		}
	}
	return false
}

// hasWindowsSwitch 报告是否出现某个 Windows 风格开关（大小写不敏感）。
func hasWindowsSwitch(args []string, name string) bool {
	loweredName := strings.ToLower(name)
	for _, arg := range args {
		lowered := strings.ToLower(arg)
		if lowered == loweredName || strings.HasPrefix(lowered, loweredName+":") {
			return true
		}
	}
	return false
}

// nonFlagArgs 返回不以 '-' 开头的参数（近似"位置参数"）。
func nonFlagArgs(args []string) []string {
	out := make([]string, 0, len(args))
	for _, arg := range args {
		if arg == "" || strings.HasPrefix(arg, "-") {
			continue
		}
		out = append(out, arg)
	}
	return out
}

// anyArg 报告是否至少有一个参数满足 pred（参数会先去掉外层引号）。
func anyArg(args []string, pred func(string) bool) bool {
	for _, arg := range args {
		if pred(stripOuterQuotes(strings.TrimSpace(arg))) {
			return true
		}
	}
	return false
}

// stripOuterQuotes 去掉成对的单引号/双引号。shell 解析对非静态词返回源码文本，
// 因此 `rm -rf "$HOME"` 的目标带引号，需要先归一化再匹配。
func stripOuterQuotes(value string) string {
	for len(value) >= 2 {
		first, last := value[0], value[len(value)-1]
		if first != last || first != '"' && first != '\'' {
			break
		}
		value = value[1 : len(value)-1]
	}
	return value
}

// ---------- 目标判定 ----------

// isRootLikeTarget 报告该目标是否指向根目录、家目录、当前/上级目录等"删除即
// 灾难"的位置。注意它不把 "*" 与 "./*" 算进来：工作区内的 `rm -rf *` 属于常规
// 清理，交给人工审批（mutating）而不是直接拒绝。
func isRootLikeTarget(target string) bool {
	switch target {
	case "/", "/*", "//", "/.", "~", "~/", "~/*", "..", "../", "../..", "../*",
		".", "$HOME", "${HOME}", "$HOME/", "${HOME}/", "$HOME/*", "${HOME}/*":
		return true
	}
	return false
}

// isRootLikeSource 比 isRootLikeTarget 更严：mv 的源是根或根通配时最危险。
func isRootLikeSource(target string) bool {
	switch target {
	case "/", "/*", "//", "~", "~/*", "$HOME", "${HOME}", "$HOME/*", "${HOME}/*":
		return true
	}
	return false
}

// isDangerousDeleteTarget 报告递归删除的目标是否危险：根/家目录/上级目录、
// 系统目录，或任何绝对路径（判定看不到工作区边界，因此保守地按"工作区外"处理）
// 与以 "../" 开头的路径（删到当前目录之外）。
func isDangerousDeleteTarget(target string) bool {
	if isRootLikeTarget(target) || isSystemPath(target) {
		return true
	}
	if strings.HasPrefix(target, "/") || strings.HasPrefix(target, "../") {
		return true
	}
	return isWindowsDriveRoot(target)
}

// isDangerousWriteTarget 报告写目标是否位于根目录/家目录/系统目录。
// "." 不算危险写目标：`touch .`、`chmod -R 777 .` 都是工作区内的常规操作，按
// mutating 交给人工审批，而不是直接拒绝。
func isDangerousWriteTarget(target string) bool {
	if target == "." {
		return false
	}
	return isRootLikeTarget(target) || isSystemPath(target)
}

// ddWritesDevice 报告一个 dd 参数是否是写入块设备的 of=。
func ddWritesDevice(arg string) bool {
	if !strings.HasPrefix(arg, "of=") {
		return false
	}
	return isDevicePath(strings.TrimPrefix(arg, "of="))
}

// isDevicePath 报告该路径是否是块/字符设备（写入即可能毁盘）。
func isDevicePath(path string) bool {
	prefixes := []string{
		"/dev/sd", "/dev/nvme", "/dev/hd", "/dev/vd", "/dev/xvd", "/dev/mmcblk",
		"/dev/dm-", "/dev/md", "/dev/disk", "/dev/rdisk", "/dev/loop",
	}
	for _, prefix := range prefixes {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

// isSystemPath 报告该路径是否位于系统目录（写入会破坏宿主环境）。
func isSystemPath(path string) bool {
	if path == "" {
		return false
	}
	posix := []string{
		"/etc", "/usr", "/bin", "/sbin", "/lib", "/lib32", "/lib64", "/boot",
		"/var/lib", "/var/spool", "/System", "/Library", "/Applications",
		"/private/etc", "/private/var/db",
	}
	for _, prefix := range posix {
		if path == prefix || strings.HasPrefix(path, prefix+"/") {
			return true
		}
	}
	return isWindowsSystemPath(path)
}

// isWindowsSystemPath 报告该路径是否位于 Windows 系统目录（大小写不敏感）。
func isWindowsSystemPath(path string) bool {
	normalized := strings.ToLower(strings.ReplaceAll(path, "/", `\`))
	prefixes := []string{
		`c:\windows`, `c:\program files`, `c:\program files (x86)`, `c:\programdata`,
		`%systemroot%`, `%windir%`,
	}
	for _, prefix := range prefixes {
		if normalized == prefix || strings.HasPrefix(normalized, prefix+`\`) {
			return true
		}
	}
	return false
}

// isWindowsDriveRoot 报告该路径是否是一个卷根（C:、C:\、C:\*）。
func isWindowsDriveRoot(path string) bool {
	trimmed := strings.TrimSpace(path)
	trimmed = strings.TrimSuffix(trimmed, "*")
	trimmed = strings.TrimSuffix(trimmed, `\`)
	trimmed = strings.TrimSuffix(trimmed, `/`)
	trimmed = strings.TrimSpace(trimmed)
	if len(trimmed) != 2 || trimmed[1] != ':' {
		return false
	}
	return trimmed[0] >= 'a' && trimmed[0] <= 'z' || trimmed[0] >= 'A' && trimmed[0] <= 'Z'
}

// isRegistryRoot 报告该参数是否指向注册表根键（HKLM/HKEY_LOCAL_MACHINE 等）。
func isRegistryRoot(arg string) bool {
	lowered := strings.ToLower(arg)
	roots := []string{"hklm", "hkey_local_machine", "hku", "hkey_users"}
	for _, root := range roots {
		if lowered == root || strings.HasPrefix(lowered, root+`\`) {
			return true
		}
	}
	return false
}

// powerVerb 报告 systemctl 子命令是否会关机/重启。
func powerVerb(verb string) bool {
	switch verb {
	case "reboot", "poweroff", "halt", "shutdown", "isolate", "kexec", "suspend", "hibernate":
		return true
	}
	return false
}

// ---------- 各程序的参数形态 ----------

// rmIsRecursive 报告是否递归（-r/-R/--recursive，支持 -rf 合并写法）。
func rmIsRecursive(args []string) bool {
	return hasShortFlag(args, 'r') || hasShortFlag(args, 'R') || hasLongFlag(args, "--recursive")
}

// permissionTargets 返回 chmod/chown 的实际目标（跳过模式/属主参数）。
func permissionTargets(args []string) []string {
	positional := nonFlagArgs(args)
	if len(positional) <= 1 {
		return nil
	}
	return positional[1:]
}

// writeTargets 返回一条命令会写入的路径（用于系统目录检测）。无法可靠判定时
// 返回 nil（交给其它规则处理）。
func writeTargets(c *commandView) []string {
	positional := nonFlagArgs(c.args)
	switch c.prog {
	case "cp", "mv", "ln", "install", "rsync", "scp":
		if len(positional) == 0 {
			return nil
		}
		return positional[len(positional)-1:]
	case "chmod", "chown", "chgrp":
		return permissionTargets(c.args)
	case "sed":
		if len(positional) == 0 {
			return nil
		}
		return positional[len(positional)-1:]
	case "tee", "touch", "mkdir", "rmdir", "rm", "truncate", "shred":
		return positional
	case "dd":
		out := make([]string, 0, 1)
		for _, arg := range c.args {
			if strings.HasPrefix(arg, "of=") {
				out = append(out, strings.TrimPrefix(arg, "of="))
			}
		}
		return out
	case "curl", "wget":
		return downloadOutputTargets(c.args)
	}
	return nil
}

// dateSetsClock 报告 date 是否在设置系统时间。只有独立的 -s / -s<值> / --set
// 才算：`date -Is` 这类"选项里含 s"的只读用法必须保持只读。
func dateSetsClock(args []string) bool {
	if hasLongFlag(args, "--set") {
		return true
	}
	// -s 可以与其它开关合并（`date -us 2020-01-01` 等价于 -u -s），所以按短选项
	// 字母扫描；但 -I[FMT] / -dDATE / -fFILE / -rSECONDS 的值是粘在同一参数里的，
	// 遇到它们就停止扫描本参数（否则 `date -Is`、`date -Iseconds` 会被误判）。
	for _, arg := range args {
		flags := shortFlags(arg)
		for index := 0; index < len(flags); index++ {
			switch flags[index] {
			case 's':
				return true
			case 'I', 'd', 'f', 'r':
				index = len(flags)
			}
		}
	}
	return false
}

// sedInPlace 报告 sed 是否原地修改（-i / -i.bak / -Ei / --in-place）。
func sedInPlace(args []string) bool {
	for _, arg := range args {
		if arg == "--in-place" || strings.HasPrefix(arg, "--in-place=") {
			return true
		}
		if strings.ContainsRune(shortFlags(arg), 'i') {
			return true
		}
	}
	return false
}

// isAwkProgram 报告程序名是否是 awk 家族。
func isAwkProgram(program string) bool {
	switch program {
	case "awk", "gawk", "mawk", "nawk":
		return true
	}
	return false
}

// awkHasSideEffect 报告 awk 程序文本里是否有 system()/getline 管道/输出重定向。
func awkHasSideEffect(args []string) bool {
	for _, arg := range args {
		// awk 的管道 `print x | "cmd"` 与 `"cmd" | getline` 都会执行 shell 命令，
		// 而且空格可有可无（`|"sh"`、`|getline`），因此先压掉空白再匹配。
		squashed := strings.ReplaceAll(strings.ToLower(arg), " ", "")
		squashed = strings.ReplaceAll(squashed, "\t", "")
		if strings.Contains(squashed, "system(") ||
			strings.Contains(squashed, "|getline") ||
			strings.Contains(squashed, "|\"") ||
			strings.Contains(squashed, "print>") ||
			strings.Contains(squashed, "printf>") ||
			strings.Contains(squashed, ">\"") {
			return true
		}
	}
	return false
}

// findHasAction 报告 find 是否带写/执行动作。
func findHasAction(args []string) bool {
	for _, arg := range args {
		switch arg {
		case "-delete", "-exec", "-execdir", "-ok", "-okdir", "-fls":
			return true
		}
		if strings.HasPrefix(arg, "-fprint") {
			return true
		}
	}
	return false
}

// findExecCommand 返回 find 的 -exec/-execdir/-ok/-okdir 内部命令参数（到 ";" 或
// "+" 为止）。
func findExecCommand(args []string) []string {
	for index, arg := range args {
		switch arg {
		case "-exec", "-execdir", "-ok", "-okdir":
			out := make([]string, 0, 4)
			for _, rest := range args[index+1:] {
				if rest == ";" || rest == "+" {
					break
				}
				out = append(out, rest)
			}
			return out
		}
	}
	return nil
}

// findSearchRoots 返回 find 的搜索起点（第一个谓词之前的位置参数）。
func findSearchRoots(args []string) []string {
	out := make([]string, 0, 2)
	for _, arg := range args {
		if strings.HasPrefix(arg, "-") || arg == "(" || arg == "!" {
			break
		}
		out = append(out, arg)
	}
	return out
}

// downloadWritesFile 报告 curl/wget 是否写文件。
func downloadWritesFile(args []string) bool {
	if len(downloadOutputTargets(args)) > 0 {
		return true
	}
	// curl -O / wget -O（远端文件名）也是写文件。
	return hasShortFlag(args, 'O') || hasShortFlag(args, 'J')
}

// downloadOutputTargets 返回 curl/wget 的输出文件参数值。
func downloadOutputTargets(args []string) []string {
	out := make([]string, 0, 2)
	for index := 0; index < len(args); index++ {
		arg := args[index]
		switch arg {
		case "-o", "--output", "-O", "--output-document", "-P", "--directory-prefix",
			"--output-dir":
			if index+1 < len(args) {
				out = append(out, args[index+1])
				index++
			}
		default:
			for _, prefix := range []string{"--output=", "--output-document=", "--output-dir="} {
				if value, ok := strings.CutPrefix(arg, prefix); ok {
					out = append(out, value)
				}
			}
			if strings.HasPrefix(arg, "-o") && len(arg) > 2 {
				out = append(out, arg[2:])
			}
		}
	}
	return out
}

// envInnerArgs 返回 env 后面的内部命令（跳过选项与环境变量赋值）。
func envInnerArgs(args []string) []string {
	index := 0
	for index < len(args) {
		arg := args[index]
		switch {
		case arg == "-i" || arg == "--ignore-environment" || arg == "-0" || arg == "--null":
			index++
		case arg == "-u" || arg == "--unset":
			index += 2
		case strings.HasPrefix(arg, "--unset="):
			index++
		case strings.HasPrefix(arg, "-"):
			index++
		case isEnvAssignment(arg):
			index++
		default:
			return args[index:]
		}
	}
	return nil
}

// riskyEnvName 报告环境变量名是否会影响命令解析/加载（可能把只读程序变成任意
// 执行）。
func riskyEnvName(name string) bool {
	switch strings.ToUpper(name) {
	case "PATH", "LD_PRELOAD", "LD_LIBRARY_PATH", "LD_AUDIT", "IFS", "BASH_ENV",
		"ENV", "SHELLOPTS", "PS4", "GIT_SSH", "GIT_SSH_COMMAND",
		"GIT_EXTERNAL_DIFF", "GIT_ASKPASS", "SSH_ASKPASS", "PERL5OPT",
		"PYTHONSTARTUP", "PYTHONPATH", "NODE_OPTIONS":
		return true
	}
	return false
}

// ---------- 子命令判定 ----------

// gitReadOnly 报告 git 调用是否是只读子命令。
func gitReadOnly(args []string) bool {
	positional := nonFlagArgs(args)
	if len(positional) == 0 {
		return hasLongFlag(args, "--version") || hasShortFlag(args, 'v')
	}
	switch positional[0] {
	case "status", "diff", "log", "show", "describe", "blame", "grep", "ls-files",
		"ls-tree", "cat-file", "shortlog", "show-ref", "merge-base", "rev-list",
		"rev-parse", "whatchanged", "count-objects", "check-ignore", "name-rev",
		"var", "help", "version":
		return true
	case "branch", "remote":
		// 只有"列出"形态是只读：没有位置参数。
		return len(positional) == 1
	case "config":
		return hasLongFlag(args, "--get", "--get-all", "--get-regexp", "--list") ||
			hasShortFlag(args, 'l')
	case "tag":
		// `git tag` 单独执行是列出；带 -l/-n 时位置参数是匹配模式，仍然是列出。
		return len(positional) == 1 || hasLongFlag(args, "--list") ||
			hasShortFlag(args, 'l') || hasShortFlag(args, 'n')
	case "stash":
		return len(positional) > 1 && positional[1] == "list"
	case "worktree":
		return len(positional) > 1 && positional[1] == "list"
	case "submodule":
		return len(positional) > 1 && positional[1] == "status"
	}
	return false
}

// gitForcePushToProtected 报告是否是强制推送到受保护分支。
func gitForcePushToProtected(c *commandView) bool {
	if c.prog != "git" {
		return false
	}
	positional := nonFlagArgs(c.args)
	if len(positional) == 0 || positional[0] != "push" {
		return false
	}
	force := hasLongFlag(c.args, "--force", "--force-with-lease", "--force-if-includes") ||
		hasShortFlag(c.args, 'f')
	if !force {
		return false
	}
	for _, arg := range c.args {
		ref := stripOuterQuotes(arg)
		ref = strings.TrimPrefix(ref, "+")
		for _, part := range strings.SplitN(ref, ":", 2) {
			if isProtectedBranch(part) {
				return true
			}
		}
	}
	return false
}

// isProtectedBranch 报告 refspec/分支名是否指向受保护分支。
func isProtectedBranch(ref string) bool {
	ref = strings.TrimSpace(ref)
	if index := strings.LastIndex(ref, "/"); index >= 0 {
		ref = ref[index+1:]
	}
	switch ref {
	case "main", "master", "trunk":
		return true
	}
	return false
}

// goReadOnly 报告 go 子命令是否只读。
func goReadOnly(args []string) bool {
	positional := nonFlagArgs(args)
	if len(positional) == 0 {
		return hasLongFlag(args, "--version")
	}
	switch positional[0] {
	case "version", "list", "doc", "which", "bug":
		return true
	case "env":
		// `go env -w/-u` 会改写 go env 配置文件。
		return !hasShortFlag(args, 'w') && !hasShortFlag(args, 'u') &&
			!hasLongFlag(args, "-w", "-u")
	}
	return false
}

// kubectlReadOnly 报告 kubectl 子命令是否只读。
func kubectlReadOnly(args []string) bool {
	positional := nonFlagArgs(args)
	if len(positional) == 0 {
		return hasLongFlag(args, "--version")
	}
	switch positional[0] {
	case "get", "describe", "logs", "version", "explain", "api-resources",
		"api-versions", "cluster-info", "top", "diff", "auth":
		return true
	}
	return false
}

// npmReadOnly 报告 npm 子命令是否只读。
func npmReadOnly(args []string) bool {
	positional := nonFlagArgs(args)
	if len(positional) == 0 {
		return hasLongFlag(args, "--version") || hasShortFlag(args, 'v')
	}
	switch positional[0] {
	case "ls", "list", "ll", "la", "view", "info", "show", "help", "outdated",
		"why", "explain", "pkg", "search", "whoami":
		return true
	}
	return false
}

// ---------- 透明包装器递归 ----------

// xargsValueOptions 是 xargs 取值的短选项（字母）。
var xargsValueOptions = map[string]bool{
	"n": true, "I": true, "d": true, "P": true, "L": true, "s": true, "E": true,
	"a": true,
}

// sudoValueOptions 是 sudo/doas 取值的短选项。
var sudoValueOptions = map[string]bool{
	"u": true, "g": true, "p": true, "C": true, "h": true, "U": true, "r": true,
	"t": true, "T": true, "D": true, "R": true, "c": true,
}

// niceValueOptions 是 nice/timeout/stdbuf 取值的短选项。
var niceValueOptions = map[string]bool{
	"n": true, "s": true, "k": true, "p": true, "e": true, "i": true,
}

// wrappedAssessment 递归判定透明包装器内部命令，返回内部命令的评估。
// ok=false 表示不是包装器（或内部命令无法提取），调用方继续走规则表。
func wrappedAssessment(c *commandView) (CommandAssessment, bool) {
	switch c.prog {
	case "bash", "sh", "zsh", "dash", "ksh", "ash", "mksh", "fish", "yash", "pwsh":
		script, ok := shellCommandArgument(c.args)
		if !ok {
			return CommandAssessment{}, false
		}
		return classifyShellScript(script, c.depth+1, c.windows), true
	case "eval":
		if len(c.args) == 0 {
			return CommandAssessment{}, false
		}
		return classifyShellScript(strings.Join(c.args, " "), c.depth+1, c.windows), true
	case "xargs":
		inner := stripLeadingOptions(c.args, xargsValueOptions)
		if len(inner) == 0 {
			return CommandAssessment{
				Risk:    RiskReadOnly,
				Program: "xargs",
				Argv:    append([]string(nil), c.argv...),
				Reason:  "xargs 没有内部命令时默认执行 echo，只打印",
				Detail:  "read.xargs",
			}, true
		}
		return classifyCommandDepth(inner, c.windows, c.depth+1), true
	case "find":
		inner := findExecCommand(c.args)
		if len(inner) == 0 {
			return CommandAssessment{}, false
		}
		value := classifyCommandDepth(inner, c.windows, c.depth+1)
		if value.Risk != RiskDangerous &&
			isRecursiveDelete(inner, c.windows) && anyArg(findSearchRoots(c.args), isRootLikeSource) {
			return CommandAssessment{
				Risk:    RiskDangerous,
				Program: "find",
				Argv:    append(append([]string(nil), c.argv...), inner...),
				Reason:  "find 在根目录上执行递归删除",
				Detail:  "rm.recursive-root",
			}, true
		}
		return value, true
	case "exec":
		inner := stripLeadingOptions(c.args, nil)
		if len(inner) == 0 {
			return CommandAssessment{}, false
		}
		return classifyCommandDepth(inner, c.windows, c.depth+1), true
	case "env":
		inner := envInnerArgs(c.args)
		if len(inner) == 0 {
			return CommandAssessment{}, false
		}
		return classifyCommandDepth(inner, c.windows, c.depth+1), true
	case "sudo", "doas":
		inner := stripLeadingOptions(c.args, sudoValueOptions)
		if len(inner) == 0 {
			return CommandAssessment{}, false
		}
		return classifyCommandDepth(inner, c.windows, c.depth+1), true
	case "timeout":
		// timeout [选项] DURATION COMMAND...：先去掉选项，再跳过第一个位置参数
		// （时长），剩下的才是要判定的命令。
		inner := stripLeadingOptions(c.args, niceValueOptions)
		if len(inner) < 2 {
			return CommandAssessment{}, false
		}
		return classifyCommandDepth(inner[1:], c.windows, c.depth+1), true
	case "nohup", "time", "command", "builtin", "stdbuf", "nice", "ionice":
		inner := stripLeadingOptions(c.args, niceValueOptions)
		if len(inner) == 0 {
			return CommandAssessment{}, false
		}
		return classifyCommandDepth(inner, c.windows, c.depth+1), true
	case "cmd":
		// cmd /c <argv>：按 Windows 语义递归判定剩余 argv。
		for index, arg := range c.args {
			lowered := strings.ToLower(arg)
			if lowered == "/c" || lowered == "/k" {
				if index+1 < len(c.args) {
					return classifyCommandDepth(c.args[index+1:], true, c.depth+1), true
				}
			}
		}
		return CommandAssessment{}, false
	}
	return CommandAssessment{}, false
}

// isRecursiveDelete 报告 argv 是否是递归 rm。
func isRecursiveDelete(argv []string, windows bool) bool {
	if len(argv) == 0 {
		return false
	}
	program := commandBaseName(argv[0], windows)
	return program == "rm" && rmIsRecursive(argv[1:])
}

// shellCommandArgument 返回 `bash -c SCRIPT` 里的 SCRIPT。
func shellCommandArgument(args []string) (string, bool) {
	for index, arg := range args {
		if arg == "--" {
			return "", false
		}
		if arg == "-c" || arg == "--command" {
			if index+1 < len(args) {
				return args[index+1], true
			}
			return "", false
		}
		if strings.ContainsRune(shortFlags(arg), 'c') {
			if index+1 < len(args) {
				return args[index+1], true
			}
			return "", false
		}
	}
	return "", false
}

// stripLeadingOptions 去掉前导选项（含取值的选项与 -- 分隔符），返回剩余 argv。
func stripLeadingOptions(args []string, valueOptions map[string]bool) []string {
	index := 0
	for index < len(args) {
		arg := args[index]
		switch {
		case arg == "--":
			index++
		case strings.HasPrefix(arg, "--"):
			name := arg
			inline := false
			if cut := strings.Index(arg, "="); cut >= 0 {
				name, inline = arg[:cut], true
			}
			index++
			if !inline && takesLongValue(name) {
				index++
			}
		case strings.HasPrefix(arg, "-") && arg != "-":
			takesValue := false
			flags := arg[1:]
			for _, letter := range flags {
				if valueOptions != nil && valueOptions[string(letter)] {
					takesValue = true
					break
				}
			}
			index++
			if takesValue && len(flags) == 1 {
				index++
			}
		default:
			return args[index:]
		}
	}
	return nil
}

// takesLongValue 报告某个长选项是否单独取值（简化表，覆盖常见包装器）。
func takesLongValue(name string) bool {
	switch name {
	case "--max-args", "--max-procs", "--delimiter", "--arg-file", "--replace",
		"--max-lines", "--max-chars", "--eof", "--user", "--group", "--prompt",
		"--signal", "--kill-after", "--preserve-environment":
		return true
	}
	return false
}

// ---------- 脚本级判定 ----------

// ClassifyShellCommand 解析一段 /bin/sh -c 脚本并判定其风险。
//
// 判定是"取最严"：管道、`&&`/`||`/`;`、子 shell、控制结构、命令替换、函数
// 定义中的所有命令都会被判定，取最高风险；因此 `ls && rm -rf /` 是
// RiskDangerous。重定向单独判定（`>`/`>>` 写非 /dev/null 目标至少 mutating，
// 写系统目录或块设备是 dangerous）。解析失败、空脚本、动态命令名返回
// RiskUnknown。返回值里的 Argv/Program 取自"最严的那条命令"，便于把命中的具体
// 命令回传给调用方。
func ClassifyShellCommand(script string) CommandAssessment {
	return ClassifyShellCommandOnPlatform(script, runtime.GOOS == "windows")
}

// ClassifyShellCommandOnPlatform 是 ClassifyShellCommand 的平台参数化版本：
// windows 决定程序名比较语义与 Windows 专属规则（format/diskpart/rd /s /q …）。
//
// 它对"命令文本来自 PowerShell 或 cmd 工具"的宿主有用：同一段 `format C:` 在
// POSIX 语义下只是一个未知程序，在 Windows 语义下是危险命令。工具名到平台的映射
// 由宿主决定（gate.CommandRiskGate 对 pwsh/powershell/cmd 传 true）。
func ClassifyShellCommandOnPlatform(script string, windows bool) CommandAssessment {
	return classifyShellScript(script, 0, windows)
}

// classifyShellScript 是带递归深度与平台语义的脚本级判定。
func classifyShellScript(script string, depth int, windows bool) CommandAssessment {
	if depth > maxClassifyDepth {
		return CommandAssessment{
			Risk:   RiskUnknown,
			Reason: "命令嵌套过深，无法静态判定",
			Detail: "recursion.depth",
		}
	}
	if detail, tooComplex := shellScriptTooComplex(script); tooComplex {
		return CommandAssessment{
			Risk:   RiskUnknown,
			Reason: "命令文本过长或嵌套过深，不做静态分析（按需要审批处理）",
			Detail: detail,
		}
	}
	normalized := stripShellLineContinuations(script)
	file, err := parseShell(normalized)
	if err != nil {
		return CommandAssessment{
			Risk:   RiskUnknown,
			Reason: "shell 命令无法解析，无法判定副作用",
			Detail: "shell.parse-error",
		}
	}
	analyzer := &shellAnalyzer{
		source:    normalized,
		depth:     depth,
		windows:   windows,
		funcs:     make(map[string]*syntax.FuncDecl),
		seenFuncs: make(map[string]bool),
		seenPipes: make(map[*syntax.BinaryCmd]bool),
	}
	analyzer.analyzeFile(file)
	if !analyzer.seen {
		return CommandAssessment{
			Risk:   RiskUnknown,
			Reason: "脚本里没有可判定的命令",
			Detail: "shell.empty",
		}
	}
	if analyzer.worst.Reason == "" {
		analyzer.worst.Reason = "无法判定命令副作用"
	}
	return analyzer.worst
}

// parseShell 先按 POSIX 解析；失败时再按 bash 方言解析一次。bash/mksh/zsh 专有
// 语法（`&>`、`>()`、冒号函数名等）在 /bin/sh 下会直接报错，但既然用户可能把
// 脚本交给 bash，判定就按 bash 语义识别它们，而不是一律判成"无法解析"。
func parseShell(source string) (*syntax.File, error) {
	file, err := syntax.NewParser(syntax.Variant(syntax.LangPOSIX)).Parse(strings.NewReader(source), "")
	if err == nil {
		return file, nil
	}
	bashFile, bashErr := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(source), "")
	if bashErr != nil {
		return nil, err
	}
	return bashFile, nil
}

// shellAnalyzer 累计脚本级判定的最严结果。
type shellAnalyzer struct {
	source    string
	depth     int
	windows   bool
	funcs     map[string]*syntax.FuncDecl
	seenFuncs map[string]bool
	// seenPipes 记录已经分析过的管道节点。syntax.Walk 是前序遍历（父节点先于
	// 子节点），因此在最外层管道里把整条链的节点都标记掉，后续访问内层节点时
	// 直接跳过——否则 n 级管道会被分析 n 次，退化成 O(n²)（4000 级 ≈ 2.8s）。
	seenPipes map[*syntax.BinaryCmd]bool
	seen      bool
	worst     CommandAssessment
}

// analyzeFile 先收集函数定义（支持 `f() { ... }; f` 的递归判定），再遍历脚本。
func (a *shellAnalyzer) analyzeFile(file *syntax.File) {
	syntax.Walk(file, func(node syntax.Node) bool {
		if fn, ok := node.(*syntax.FuncDecl); ok && fn.Name != nil {
			a.funcs[fn.Name.Value] = fn
		}
		return true
	})
	syntax.Walk(file, a.visit)
}

// visit 是语法树访问器。
func (a *shellAnalyzer) visit(node syntax.Node) bool {
	switch typed := node.(type) {
	case *syntax.Stmt:
		a.analyzeRedirs(typed.Redirs)
	case *syntax.CallExpr:
		a.analyzeCall(typed)
	case *syntax.BinaryCmd:
		if typed.Op == syntax.Pipe {
			a.analyzePipeline(typed)
		}
	case *syntax.FuncDecl:
		if isForkBomb(typed) {
			a.report(CommandAssessment{
				Risk:    RiskDangerous,
				Program: typed.Name.Value,
				Reason:  "递归 fork 炸弹会耗尽进程与 CPU",
				Detail:  "shell.fork-bomb",
			})
		}
	}
	return true
}

// analyzeCall 判定一条简单命令，并处理包装器递归与函数调用。
func (a *shellAnalyzer) analyzeCall(call *syntax.CallExpr) {
	if call == nil {
		return
	}
	for _, assign := range call.Assigns {
		if assign == nil || assign.Name == nil {
			continue
		}
		if riskyEnvName(assign.Name.Value) {
			a.report(CommandAssessment{
				Risk:   RiskUnknown,
				Reason: "前置环境变量赋值会改变命令解析（PATH/LD_PRELOAD 等），无法判定",
				Detail: "shell.risky-assignment",
			})
		}
	}
	if len(call.Args) == 0 {
		return
	}
	argv := make([]string, 0, len(call.Args))
	dynamicName := false
	for index, word := range call.Args {
		value, static := decodeShellWord(word, a.source)
		if index == 0 && !static {
			dynamicName = true
		}
		argv = append(argv, value)
	}
	if dynamicName {
		a.report(CommandAssessment{
			Risk:   RiskUnknown,
			Argv:   argv,
			Reason: "命令名包含动态展开（命令替换/变量），无法静态判定",
			Detail: "argv.dynamic-name",
		})
		return
	}
	name := commandBaseName(argv[0], a.windows)
	// 脚本内定义的函数：判定其函数体（带递归保护）。
	if fn, ok := a.funcs[name]; ok {
		if a.seenFuncs[name] {
			return
		}
		a.seenFuncs[name] = true
		if fn.Body != nil {
			syntax.Walk(fn.Body, a.visit)
		}
		return
	}
	view := &commandView{
		argv: argv, prog: name, args: argv[1:],
		windows: a.windows, depth: a.depth + 1,
	}
	if inner, ok := wrappedAssessment(view); ok {
		a.report(inner)
		return
	}
	a.report(classifyCommandDepth(argv, a.windows, a.depth+1))
}

// analyzeRedirs 判定一条语句的重定向。
func (a *shellAnalyzer) analyzeRedirs(redirs []*syntax.Redirect) {
	for _, redir := range redirs {
		if redir == nil {
			continue
		}
		switch redir.Op {
		case syntax.DplIn, syntax.DplOut, syntax.RdrIn, syntax.Hdoc,
			syntax.DashHdoc, syntax.WordHdoc:
			// 复制 fd（2>&1）、读入（<、here-doc）都不是持久写入。
			continue
		}
		target, static := decodeShellWord(redir.Word, a.source)
		// 先判 git 控制面：`> $PWD/.git/hooks/x` 虽然"动态"，但字面量里已经
		// 写明目标是 hooks —— 这正是最需要拦住的形态，不能退化成 unknown。
		if assessment := classifyRedirectTarget(target); assessment.Detail == gitControlRuleID {
			a.report(assessment)
			continue
		}
		if !static {
			a.report(CommandAssessment{
				Risk:   RiskUnknown,
				Reason: "重定向目标包含动态展开，无法判定写到哪里",
				Detail: "redirect.dynamic",
			})
			continue
		}
		a.report(classifyRedirectTarget(target))
	}
}

// classifyRedirectTarget 判定一次"写重定向"的目标。
func classifyRedirectTarget(target string) CommandAssessment {
	path := stripOuterQuotes(strings.TrimSpace(target))
	if isGitControlPath(path) {
		return CommandAssessment{
			Risk:   RiskDangerous,
			Reason: gitControlReason,
			Detail: gitControlRuleID,
		}
	}
	switch path {
	case "", "/dev/null", "/dev/stdout", "/dev/stderr", "/dev/tty":
		return CommandAssessment{
			Risk:   RiskReadOnly,
			Reason: "重定向到空设备或标准流，不落盘",
			Detail: "redirect.null",
		}
	}
	if strings.HasPrefix(path, "/dev/fd/") || strings.HasPrefix(path, "/proc/self/fd/") {
		return CommandAssessment{
			Risk:   RiskReadOnly,
			Reason: "重定向到文件描述符，不落盘",
			Detail: "redirect.null",
		}
	}
	if isDevicePath(path) {
		return CommandAssessment{
			Risk:   RiskDangerous,
			Reason: "重定向写到块设备，会破坏磁盘数据",
			Detail: "redirect.device",
		}
	}
	if isSystemPath(path) {
		return CommandAssessment{
			Risk:   RiskDangerous,
			Reason: "重定向写系统目录，会破坏宿主环境",
			Detail: "redirect.system-path",
		}
	}
	return CommandAssessment{
		Risk:   RiskMutating,
		Reason: "重定向写文件",
		Detail: "redirect.write",
	}
}

// analyzePipeline 判定 `a | b | c`：取所有阶段的最严结果，并识别"下载即执行"。
func (a *shellAnalyzer) analyzePipeline(node *syntax.BinaryCmd) {
	if a.seenPipes[node] {
		// 已经作为外层管道的一部分分析过（前序遍历保证外层先到）。
		return
	}
	programs := pipePrograms(node, a.seenPipes)
	if len(programs) == 0 {
		return
	}
	downloader := ""
	executor := ""
	for _, program := range programs {
		if downloader == "" && downloaderPrograms[program] {
			downloader = program
		}
		if executor == "" && executorPrograms[program] {
			executor = program
		}
	}
	if downloader == "" || executor == "" {
		return
	}
	a.report(CommandAssessment{
		Risk:    RiskDangerous,
		Program: executor,
		Argv:    append([]string(nil), programs...),
		Reason:  "把下载的内容直接交给解释器执行",
		Detail:  "shell.download-exec",
	})
}

// pipePrograms 按出现顺序返回一条管道里各阶段（含嵌套管道）的程序名。
//
// visited 非 nil 时，沿途每个管道节点都会被标记，使调用方能在后续访问内层节点
// 时跳过重复分析（见 analyzePipeline）。
func pipePrograms(node *syntax.BinaryCmd, visited map[*syntax.BinaryCmd]bool) []string {
	programs := make([]string, 0, 4)
	var walk func(candidate *syntax.BinaryCmd)
	walk = func(candidate *syntax.BinaryCmd) {
		if candidate == nil {
			return
		}
		if visited != nil {
			visited[candidate] = true
		}
		for _, side := range []*syntax.Stmt{candidate.X, candidate.Y} {
			if side == nil {
				continue
			}
			if nested, ok := side.Cmd.(*syntax.BinaryCmd); ok && nested.Op == syntax.Pipe {
				walk(nested)
				continue
			}
			if program := statementProgram(side, false); program != "" {
				programs = append(programs, program)
			}
		}
	}
	walk(node)
	return programs
}

// statementProgram 返回一条语句的程序名（不是简单命令时返回空串）。
func statementProgram(stmt *syntax.Stmt, windows bool) string {
	if stmt == nil {
		return ""
	}
	call, ok := stmt.Cmd.(*syntax.CallExpr)
	if !ok || len(call.Args) == 0 {
		return ""
	}
	for _, part := range call.Args[0].Parts {
		if lit, ok := part.(*syntax.Lit); ok {
			return commandBaseName(lit.Value, windows)
		}
	}
	return ""
}

// downloaderPrograms 是"把远端内容取回本地"的程序。
var downloaderPrograms = map[string]bool{
	"curl": true, "wget": true, "fetch": true, "aria2c": true,
}

// executorPrograms 是"执行 stdin 内容"的解释器。
var executorPrograms = map[string]bool{
	"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true, "ash": true,
	"mksh": true, "fish": true, "yash": true, "python": true, "python2": true,
	"python3": true, "perl": true, "ruby": true, "node": true, "php": true,
	"pwsh": true, "powershell": true,
}

// isForkBomb 报告函数体是否是递归 fork 炸弹（`f() { f|f& }; f`）。
func isForkBomb(fn *syntax.FuncDecl) bool {
	if fn == nil || fn.Name == nil || fn.Body == nil {
		return false
	}
	self := fn.Name.Value
	if self == "" {
		return false
	}
	selfPipe := false
	background := false
	syntax.Walk(fn.Body, func(node syntax.Node) bool {
		switch typed := node.(type) {
		case *syntax.Stmt:
			if typed.Background {
				background = true
			}
		case *syntax.BinaryCmd:
			if typed.Op != syntax.Pipe {
				return true
			}
			if statementProgram(typed.X, false) == self && statementProgram(typed.Y, false) == self {
				selfPipe = true
			}
		}
		return true
	})
	return selfPipe && background
}

// riskSeverity 是合并多条命令时使用的严重度。RiskUnknown 的数值最小（它是零值，
// 便于"零值失败即保守"），但它高于 RiskReadOnly：一条"不知道会不会写"的命令
// 绝不能被同批的只读命令掩盖成可自动放行。
func riskSeverity(risk CommandRisk) int {
	switch risk {
	case RiskReadOnly:
		return 1
	case RiskUnknown:
		return 2
	case RiskMutating:
		return 3
	case RiskDangerous:
		return 4
	}
	return 0
}

// report 累计最严结果；严重度相同时保留先出现的一条（确定性）。
func (a *shellAnalyzer) report(assessment CommandAssessment) {
	if !a.seen {
		a.seen = true
		a.worst = assessment
		return
	}
	if riskSeverity(assessment.Risk) > riskSeverity(a.worst.Risk) {
		a.worst = assessment
	}
}

// sedScriptUnsafe 报告一次 sed 调用是否会执行命令或写文件。
//
// 覆盖四种"看起来只读"的形态：
//   - `e` 命令（GNU sed 扩展，脚本里执行 shell）；
//   - `s///e` 标志（把替换结果交给 shell 执行）；
//   - `w` / `W` 命令（把缓冲区写进文件）；
//   - `-f 脚本文件`（脚本内容不在 argv 里，这里看不到）。
//
// 只读判定会直接变成"门禁自动放行"，所以这里一律保守：解析不确定就当作不安全。
func sedScriptUnsafe(args []string) bool {
	scripts, fromFile := sedScripts(args)
	if fromFile {
		return true
	}
	for _, script := range scripts {
		if sedScriptHasUnsafeCommand(script) {
			return true
		}
	}
	return false
}

// sedScripts 取出 sed 的脚本片段（-e/--expression 或第一个位置参数），
// 并报告是否使用了 -f/--file（脚本来自文件，内容不可见）。
func sedScripts(args []string) (scripts []string, fromFile bool) {
	explicit := false
	onlyPositional := false
	for index := 0; index < len(args); index++ {
		arg := args[index]
		if onlyPositional {
			if !explicit {
				scripts = append(scripts, arg)
				explicit = true
			}
			continue
		}
		switch {
		case arg == "--":
			onlyPositional = true
		case arg == "-e" || arg == "--expression":
			explicit = true
			if index+1 < len(args) {
				scripts = append(scripts, args[index+1])
				index++
			}
		case strings.HasPrefix(arg, "--expression="):
			explicit = true
			scripts = append(scripts, strings.TrimPrefix(arg, "--expression="))
		case arg == "-f" || arg == "--file" || strings.HasPrefix(arg, "--file="):
			return scripts, true
		case strings.HasPrefix(arg, "-f") && len(arg) > 2:
			return scripts, true
		case strings.HasPrefix(arg, "-e") && len(arg) > 2:
			explicit = true
			scripts = append(scripts, arg[2:])
		case strings.HasPrefix(arg, "-") && arg != "-":
			// 其它选项（-n/-E/-r/-s/-u/-z/-i[SUFFIX] …）。
		default:
			if !explicit {
				scripts = append(scripts, arg)
				explicit = true
			}
		}
	}
	return scripts, false
}

// sedScriptHasUnsafeCommand 扫描一段 sed 脚本，报告是否出现 e/w/W 命令或
// s///e|w 标志。
//
// 它只做"够用且保守"的扫描：跳过地址前缀后取命令字母；遇到不认识的命令字母
// 当作不安全（宁可多问一次审批，也不能自动放行）。
func sedScriptHasUnsafeCommand(script string) bool {
	index, length := 0, len(script)
	for index < length {
		for index < length && isSedSeparator(script[index]) {
			index++
		}
		if index >= length {
			return false
		}
		if script[index] == '#' {
			for index < length && script[index] != '\n' {
				index++
			}
			continue
		}
		index = skipSedAddresses(script, index)
		if index >= length {
			return false
		}
		command := script[index]
		index++
		switch command {
		case 'e', 'E', 'w', 'W':
			return true
		case 's':
			next, ok := skipSedSubstitution(script, index)
			if !ok {
				return true
			}
			index = next
		case 'y':
			next, ok := skipSedTwoSegments(script, index)
			if !ok {
				return true
			}
			index = next
		case 'b', 't', 'T', ':', 'r', 'R', 'v':
			// 标签 / 读文件 / 版本：参数到行尾或分号，不执行命令。
			for index < length && script[index] != ';' && script[index] != '\n' {
				index++
			}
		case 'a', 'i', 'c':
			// 追加/插入/替换文本：文本内容任意，跳到行尾（含续行）。
			for index < length && script[index] != '\n' {
				if script[index] == '\\' && index+1 < length && script[index+1] == '\n' {
					index += 2
					continue
				}
				index++
			}
		case 'l', 'p', 'P', 'd', 'D', 'g', 'G', 'h', 'H', 'x', 'n', 'N',
			'q', 'Q', 'z', '=', 'F', 'Z':
			// 无参数、无副作用。
		default:
			return true
		}
	}
	return false
}

// isSedSeparator 报告该字节是否分隔 sed 命令。
func isSedSeparator(ch byte) bool {
	switch ch {
	case ';', '\n', ' ', '\t', '{', '}':
		return true
	}
	return false
}

// skipSedAddresses 跳过命令前的地址（数字、$、/regex/、\cREc、以及 ! 取反）。
func skipSedAddresses(script string, index int) int {
	length := len(script)
	for index < length {
		switch {
		case script[index] >= '0' && script[index] <= '9',
			script[index] == '$', script[index] == '.', script[index] == ',',
			script[index] == '+', script[index] == '~':
			index++
		case script[index] == '/', script[index] == '\\':
			index = skipSedDelimited(script, index)
		case script[index] == '!':
			index++
		default:
			return index
		}
	}
	return index
}

// skipSedDelimited 跳过一段以 delim 包裹的内容（delim 由首个字节给出；若首个
// 字节是反斜杠，则用下一个字节作分隔符）。返回闭合后的位置。
func skipSedDelimited(script string, index int) int {
	length := len(script)
	delim := script[index]
	if delim == '\\' {
		index++
		if index >= length {
			return index
		}
		delim = script[index]
	}
	index++
	for index < length {
		if script[index] == '\\' && index+1 < length {
			index += 2
			continue
		}
		if script[index] == delim {
			return index + 1
		}
		if script[index] == '\n' {
			return index
		}
		index++
	}
	return index
}

// skipSedTwoSegments 跳过 y 命令的两段（y/src/dst/）。
func skipSedTwoSegments(script string, index int) (int, bool) {
	length := len(script)
	if index >= length || script[index] == '\n' || script[index] == '\\' {
		return index, false
	}
	delim := script[index]
	index++
	for pass := 0; pass < 2; pass++ {
		closed, next := scanSedSegment(script, index, delim)
		if !closed {
			return next, false
		}
		index = next
	}
	return index, true
}

// skipSedSubstitution 跳过 s 命令的三段，并检查 flags 里的 e/w。
func skipSedSubstitution(script string, index int) (int, bool) {
	length := len(script)
	if index >= length || script[index] == '\n' || script[index] == '\\' {
		return index, false
	}
	delim := script[index]
	index++
	for pass := 0; pass < 2; pass++ {
		closed, next := scanSedSegment(script, index, delim)
		if !closed {
			return next, false
		}
		index = next
	}
	for index < length {
		switch ch := script[index]; {
		case ch == 'e' || ch == 'E' || ch == 'w' || ch == 'W':
			return index, false
		case ch == 'g' || ch == 'p' || ch == 'i' || ch == 'I' || ch == 'm' || ch == 'M',
			ch >= '0' && ch <= '9':
			index++
		default:
			return index, true
		}
	}
	return index, true
}

// scanSedSegment 从 index 起读到未转义的分隔符，报告是否闭合与下一位置。
func scanSedSegment(script string, index int, delim byte) (bool, int) {
	length := len(script)
	for index < length {
		if script[index] == '\\' && index+1 < length {
			index += 2
			continue
		}
		if script[index] == delim {
			return true, index + 1
		}
		if script[index] == '\n' {
			return false, index
		}
		index++
	}
	return false, index
}
