package sandbox

import (
	"fmt"
	"regexp"
	"strings"
)

// 结构化沙箱拒绝。
//
// 归因行原先只有一句"疑似被沙箱拒绝"，模型无法据此行动。Denial 把一次拒绝拆成
// 可读的字段（被拒路径、操作、模式、当前可写面、下一步），让 agent 能自己判断
// "是该把工具状态重定向进工作区，还是该申请一个具体的可写目录"。
//
// 路径提取是启发式的：只从常见错误文案里取绝对路径，取不到就留空——绝不猜测。
// 本包不硬编码任何具体工具的目录；通用性来自"路径 + 原因"这一结构。

// DenialKind 是被拒绝资源的类别。
//
// 只有 DenialPath 能用"申请可写目录"救济：注册表与凭据库不在文件系统里，路径
// 授权对它们没有任何作用。这个区分存在的唯一目的，就是别让模型在注册表被拒时
// 反复去申请目录、或者去试 `DWS_DISABLE_KEYCHAIN=1` 这类偏方。
type DenialKind string

const (
	// DenialPath 是文件系统路径（默认，也是唯一可被可写根救济的类别）。
	DenialPath DenialKind = "path"
	// DenialRegistry 是 Windows 注册表（HKCU/HKLM/HKEY_*、reg add/import、.reg、
	// RegistryKey/RegSetValue 等 API）。
	DenialRegistry DenialKind = "registry"
	// DenialCredentialStore 是操作系统凭据库（DPAPI、macOS keychain、libsecret/
	// secret-tool、Windows Credential Manager/wincred、CredWrite 等）。
	DenialCredentialStore DenialKind = "credential-store"
)

// Denial 是一次已识别的沙箱拒绝。
type Denial struct {
	// Matched 是命中的拒绝方言。
	Matched string
	// Kind 是被拒绝资源的类别：path（默认）、registry、credential-store。
	Kind DenialKind
	// Resource 是非路径类资源的具体标识（注册表键、凭据库名字）；路径类为空。
	Resource string
	// Mode 是当时的权限模式。
	Mode Mode
	// Enforcement 是 runner 的实际覆盖程度。
	Enforcement Enforcement
	// Path 是从输出里提取到的被拒绝对路径（提取不到时为空）。
	Path string
	// Op 是推断的操作类别：write / create / delete / rename / metadata / unknown。
	Op string
	// Output 是匹配到的行（裁剪后），供用户核对。
	Output string
	// ExitCode 是子进程退出码；-1 表示调用方没有提供（例如 PTY 会话）。
	ExitCode int
	// Signal 是判定为信号终止时的信号名（目前只有 Linux seccomp 的 SIGSYS）。
	Signal string
	// WorkspaceRoots 是策略的 workspace 根（相对路径解析基准、默认可写）。
	WorkspaceRoots []string
	// WritableRoots 是显式追加的可写根（启动时配置 + 运行时授权），不含
	// workspace 根与平台临时目录。
	WritableRoots []string
	// ProtectedPaths 是当时策略的保护路径。
	ProtectedPaths []string
	// Grants 是当时已批准的运行期授权。
	Grants []Grant
	// Budget 是剩余可申请次数。
	Budget int
	// Options 描述**调用方装配里实际可用**的处置手段。它由调用方注入（工具知道
	// 自己有没有提权通道，宿主知道有没有开授权工具），渲染建议时据此取舍：
	// 绝不建议模型去调一个当前组合里不存在的工具。
	Options Options
	// hasPolicy 记录归因时是否带上了策略（nil 策略时不应凭空渲染可写面）。
	hasPolicy bool
}

// Options 描述一次拒绝归因时，调用方装配里**实际可用**的处置手段。
//
// 为什么要注入而不是写死：同一个 sandbox 包会被不同组合使用——零配置 kit 没有
// 授权工具、也没接提权通道；装了 grants 的组合有 request_sandbox_access；接了
// Escalator 的组合才能用 sandbox_permissions:"require_escalated"。建议里点名一个
// 不存在的工具，模型就会在死路上反复重试（真实用户日志里正是如此）。
type Options struct {
	// HasGrantTool 表示组合里存在 request_sandbox_access（可写根授权工具）。
	HasGrantTool bool
	// HasEscalation 表示组合里支持 sandbox_permissions:"require_escalated"
	// （逐命令提权，需人工审批）。
	HasEscalation bool
}

// denialPathPatterns 是常见的"路径 + 拒绝原因"文案。顺序即优先级。
var denialPathPatterns = []*regexp.Regexp{
	// /bin/sh: 1: cannot create /a/b: Read-only file system
	regexp.MustCompile(`(?i)cannot (?:create|open|remove|mkdir|touch|move|stat|write)(?: \w+){0,3} '?([^'\s]+)'?`),
	// mkdir: cannot create directory '/a/b': Permission denied
	regexp.MustCompile(`(?i)(?:open|mkdir|rename|unlink|chmod|chown|utime|lstat)\s+'([^']+)'`),
	// '/a/b': Permission denied / Access is denied
	regexp.MustCompile(`(?i)'((?:/|[A-Za-z]:[\\/])[^']+)':\s*(?:permission denied|read-only file system|access is denied|operation not permitted)`),
	// error: /a/b: read-only file system
	regexp.MustCompile(`(?i)((?:/|[A-Za-z]:[\\/])[^:\s"']+):\s*(?:permission denied|read-only file system|access is denied|operation not permitted)`),
	// Windows 把原因写在前面：Access is denied. (os error 5): C:\Users\me\.dws\.data.lock
	regexp.MustCompile(`(?i)(?:access is denied|permission denied|拒绝访问)[^:\n]*:\s*((?:[A-Za-z]:[\\/]|/)[^\s"']+)`),
}

// denialOpHints 把输出里的动词映射为操作类别。
var denialOpHints = []struct {
	op      string
	markers []string
}{
	{"create", []string{"cannot create", "mkdir", "make_dir", "create_dir"}},
	{"delete", []string{"cannot remove", "unlink", "rmdir", "delete"}},
	{"rename", []string{"cannot move", "rename", "cross-device"}},
	{"metadata", []string{"chmod", "chown", "utime", "setxattr"}},
}

// denialResourceSignals 识别"非文件系统资源"。
//
// 它们**只在已经命中拒绝方言之后**参与分类：输出里出现 "HKCU" 或 "keychain"
// 本身不算拒绝（很多正常输出会提到它们）。顺序即优先级——注册表在前，因为它的
// 标记更具体（HKCU\\...、reg add），凭据库在后。
var denialResourceSignals = []struct {
	kind    DenialKind
	name    string
	pattern *regexp.Regexp
}{
	// --- 注册表 -------------------------------------------------------------
	{DenialRegistry, "HKEY root", regexp.MustCompile(`(?i)\bHKEY_(?:LOCAL_MACHINE|CURRENT_USER|CLASSES_ROOT|USERS|CURRENT_CONFIG)\b`)},
	{DenialRegistry, "HK abbreviation", regexp.MustCompile(`(?i)\bHK(?:CU|LM|CR|U|CC)[\\:]`)},
	{DenialRegistry, "reg subcommand", regexp.MustCompile(`(?i)\breg(?:\.exe)?\s+(?:add|import|delete|load|restore|copy|save)\b`)},
	{DenialRegistry, "Set-ItemProperty", regexp.MustCompile(`(?i)\bSet-ItemProperty\b[^\n]*(?:HKCU|HKLM|HKEY_)`)},
	{DenialRegistry, "registry API", regexp.MustCompile(`(?i)\b(?:RegistryKey|Registry\.(?:LocalMachine|CurrentUser|ClassesRoot|Users|CurrentConfig))\b`)},
	{DenialRegistry, "Reg* API", regexp.MustCompile(`(?i)\bReg(?:SetValue|GetValue|QueryValue|CreateKey|OpenKey|SetKeySecurity|DeleteKey)(?:Ex)?[AW]?\b`)},
	{DenialRegistry, ".reg file", regexp.MustCompile(`(?i)\.reg\b[^\n]*(?:denied|denies|access|read-only|read only|拒绝)`)},
	{DenialRegistry, "0x5 with registry", regexp.MustCompile(`(?i)(?:0x0*5\b[^\n]*registry|registry[^\n]*0x0*5\b)`)},
	// --- 凭据库 -------------------------------------------------------------
	{DenialCredentialStore, "DPAPI", regexp.MustCompile(`(?i)\bDPAPI\b`)},
	{DenialCredentialStore, "CryptProtectData", regexp.MustCompile(`(?i)\bCrypt(?:Protect|Unprotect)Data\b`)},
	{DenialCredentialStore, "keychain", regexp.MustCompile(`(?i)\bkeychain\b`)},
	{DenialCredentialStore, "libsecret", regexp.MustCompile(`(?i)\b(?:libsecret|secret-tool)\b`)},
	{DenialCredentialStore, "security CLI", regexp.MustCompile(`(?i)\bsecurity\s+add-(?:generic|internet)-password\b`)},
	{DenialCredentialStore, "Windows Credential Manager", regexp.MustCompile(`(?i)\b(?:Windows\s+Credential\s+Manager|Credential\s+Manager|wincred)\b`)},
	{DenialCredentialStore, "CredWrite API", regexp.MustCompile(`(?i)\bCred(?:Write|Read|Delete|Enumerate)[AW]?\b`)},
}

// registryKeyPattern 提取具体的注册表键（HKCU\\Software\\X 或 HKCU:\\Software\\X）。
var registryKeyPattern = regexp.MustCompile(`(?i)\b(?:HKEY_[A-Za-z_]+|HK(?:CU|LM|CR|U|CC))[\\:][^\s"',;)\]]*`)

// classifyDenialResource 判定一次拒绝属于哪类资源，并返回资源标识。
func classifyDenialResource(output string) (DenialKind, string) {
	if output == "" {
		return DenialPath, ""
	}
	for _, signal := range denialResourceSignals {
		if !signal.pattern.MatchString(output) {
			continue
		}
		if signal.kind != DenialRegistry {
			return signal.kind, signal.name
		}
		return signal.kind, extractRegistryKey(output)
	}
	return DenialPath, ""
}

// extractRegistryKey 从输出里取第一个注册表键；取不到返回空（绝不猜）。
func extractRegistryKey(output string) string {
	match := registryKeyPattern.FindString(output)
	if match == "" {
		return ""
	}
	return strings.TrimRight(match, ".,;:)")
}

// DetectDenial 在命令输出里查找沙箱拒绝方言，命中时返回结构化拒绝；未命中返回
// nil。signatures 为空或 policy 为 nil 时不做归因（保持成功路径逐字节不变）。
//
// 与旧行为的关键差异：**不要求退出码非零**。很多 CLI 会把落盘失败包成自己的错误
// 并退出 0；此时仍然给出归因，只是文案标注"命令自称成功但输出含沙箱拒绝"。
func DetectDenial(output string, signatures []string, policy *Policy, enforcement Enforcement) *Denial {
	return detectDenial(output, -1, signatures, policy, enforcement)
}

// quickRejectExitCodes 是"命令根本没跑起来"的退出码：POSIX shell 用 126（不可执行）、
// 127（找不到命令），2 常被 shell/参数解析用来表示用法错误。这些退出码下的
// "permission denied" 之类文本来自 shell 自己，不是沙箱拒绝——同类实现的
// denial.rs 同样对 {2,126,127} 做快速拒绝。
var quickRejectExitCodes = map[int]bool{2: true, 126: true, 127: true}

// sigsysExitCode 是进程被 SIGSYS 终止时 shell 报告的退出码（128+31）。
// Seccomp 的拒绝没有文件文案，只能靠这个信号识别。
const sigsysExitCode = 128 + 31

// DetectDenialWithExit 是 DetectDenial 的带退出码版本。
//
// 相比只看文本，它多做两件事：
//   - **抑制误报**：退出码 2/126/127 表示命令没跑起来，输出里的 "permission
//     denied" 来自 shell 而非沙箱；
//   - **补漏报**：被杀于 SIGSYS（退出码 159）时即使没有任何文案也判定为拒绝，
//     并标注 signal=SIGSYS（Linux seccomp 路径）。
//
// exitCode < 0 表示调用方没有退出码（例如 PTY 会话），此时行为与 DetectDenial 一致。
func DetectDenialWithExit(output string, exitCode int, signatures []string, policy *Policy, enforcement Enforcement) *Denial {
	if exitCode >= 0 && quickRejectExitCodes[exitCode] {
		return nil
	}
	denial := detectDenial(output, exitCode, signatures, policy, enforcement)
	if denial == nil && exitCode == sigsysExitCode {
		denial = &Denial{
			Matched:     "signal SIGSYS",
			Kind:        DenialPath,
			Op:          "unknown",
			ExitCode:    exitCode,
			Signal:      "SIGSYS",
			Enforcement: enforcement,
		}
		if policy != nil {
			denial.hasPolicy = true
			denial.Options = policy.DenialOptions()
			denial.Mode = policy.Mode()
			denial.WorkspaceRoots = policy.Roots()
			denial.WritableRoots = policy.AdditionalWritableRoots()
			denial.ProtectedPaths = policy.ProtectedPaths()
			denial.Grants = policy.Grants()
			if budget := policy.GrantBudget(); budget > len(denial.Grants) {
				denial.Budget = budget - len(denial.Grants)
			}
		}
	}
	return denial
}

// detectDenial 是两条入口共用的实现。
func detectDenial(output string, exitCode int, signatures []string, policy *Policy, enforcement Enforcement) *Denial {
	if output == "" || len(signatures) == 0 {
		return nil
	}
	lowered := strings.ToLower(output)
	matched := ""
	for _, signature := range signatures {
		signature = strings.TrimSpace(signature)
		if signature == "" {
			continue
		}
		if strings.Contains(lowered, strings.ToLower(signature)) {
			matched = signature
			break
		}
	}
	if matched == "" {
		return nil
	}
	denial := &Denial{
		Matched:     matched,
		Enforcement: enforcement,
		Op:          "unknown",
		ExitCode:    exitCode,
	}
	if policy != nil {
		denial.hasPolicy = true
		denial.Options = policy.DenialOptions()
		denial.Mode = policy.Mode()
		denial.WorkspaceRoots = policy.Roots()
		denial.WritableRoots = policy.AdditionalWritableRoots()
		denial.ProtectedPaths = policy.ProtectedPaths()
		denial.Grants = policy.Grants()
		if budget := policy.GrantBudget(); budget > len(denial.Grants) {
			denial.Budget = budget - len(denial.Grants)
		}
	}
	denial.Path = extractDenialPath(output)
	// 非路径资源（注册表/凭据库）不给"被拒路径"：给出路径会让模型以为申请一个
	// 可写目录就能解决，而这两类根本不在文件系统里。
	denial.Kind, denial.Resource = classifyDenialResource(output)
	if denial.Kind != DenialPath {
		denial.Path = ""
	}
	for _, hint := range denialOpHints {
		for _, marker := range hint.markers {
			if strings.Contains(lowered, marker) {
				denial.Op = hint.op
				break
			}
		}
		if denial.Op != "unknown" {
			break
		}
	}
	if denial.Op == "unknown" && (denial.Kind == DenialRegistry || denial.Kind == DenialCredentialStore) {
		// 注册表/凭据库的拒绝在本包里几乎总是"写"（reg add、reg import、
		// CredWrite、keychain add…）；标注 write 比 unknown 更有指导性。
		denial.Op = "write"
	}
	if line := matchedLine(output, matched); line != "" {
		denial.Output = line
	}
	return denial
}

// extractDenialPath 从输出中提取第一个看起来像绝对路径的 token。
//
// 除了"像绝对路径"，还要求它前面是边界字符（行首、空白、引号、括号、冒号或等号），
// 否则 "./build: Permission denied" 里的 "/build" 会被误当成绝对路径。
func extractDenialPath(output string) string {
	for _, pattern := range denialPathPatterns {
		for _, match := range pattern.FindAllStringSubmatchIndex(output, -1) {
			if len(match) < 4 {
				continue
			}
			start, end := match[2], match[3]
			if start < 0 || end <= start || end > len(output) {
				continue
			}
			if start > 0 && !isPathBoundary(output[start-1]) {
				continue
			}
			candidate := strings.TrimRight(strings.TrimSpace(output[start:end]), ".,;:)")
			if !looksAbsolute(candidate) {
				continue
			}
			return candidate
		}
	}
	return ""
}

// isPathBoundary 报告某个字节是否可以把一个绝对路径 token 与前面隔开。
func isPathBoundary(b byte) bool {
	switch b {
	case ' ', '\t', '\n', '\r', '\'', '"', '(', '[', '{', ':', '=', ',', ';':
		return true
	}
	return false
}

// looksAbsolute 判断 token 是否像 POSIX 绝对路径或 Windows 盘符路径。
func looksAbsolute(token string) bool {
	if strings.HasPrefix(token, "/") && len(token) > 1 {
		return true
	}
	if len(token) >= 3 && token[1] == ':' && (token[2] == '\\' || token[2] == '/') {
		return true
	}
	return false
}

// matchedLine 返回输出中包含该方言的第一行（裁剪到合理长度）。
func matchedLine(output, signature string) string {
	loweredSignature := strings.ToLower(signature)
	for _, line := range strings.Split(output, "\n") {
		if strings.Contains(strings.ToLower(line), loweredSignature) {
			trimmed := strings.TrimSpace(line)
			if len(trimmed) > 300 {
				trimmed = trimmed[:300] + "…"
			}
			return trimmed
		}
	}
	return ""
}

// Summary 返回单行结构化归因，形如：
//
//	[sandbox: denied write to /home/me/.foo/creds.json (matched "read-only file system"; mode workspace-write, enforcement full)]
func (d *Denial) Summary() string {
	if d == nil {
		return ""
	}
	modeText := string(d.Mode)
	if modeText == "" {
		modeText = "unknown"
	}
	enforcementText := string(d.Enforcement)
	if enforcementText == "" {
		enforcementText = "unknown"
	}
	target := d.Path
	switch d.Kind {
	case DenialRegistry:
		if d.Resource != "" {
			target = "the Windows registry (" + d.Resource + ")"
		} else {
			target = "the Windows registry"
		}
	case DenialCredentialStore:
		if d.Resource != "" {
			target = "an OS credential store (" + d.Resource + ")"
		} else {
			target = "an OS credential store"
		}
	default:
		if target == "" {
			target = "a path outside the writable roots"
		}
	}
	extra := ""
	if d.Signal != "" {
		extra = fmt.Sprintf(", signal %s", d.Signal)
	} else if d.ExitCode >= 0 {
		extra = fmt.Sprintf(", exit %d", d.ExitCode)
	}
	return fmt.Sprintf("[sandbox: denied %s to %s (matched %q; mode %s, enforcement %s%s)]",
		d.Op, target, d.Matched, modeText, enforcementText, extra)
}

// NextSteps 返回面向模型的通用处置顺序（不含任何具体工具名）。
//
// 两条顺序是刻意相反的：
//   - 路径类：先想办法把状态重定向进工作区（不需要授权），实在不行才申请可写根；
//   - 非路径类（注册表/凭据库）：先以不受沙箱限制的方式重跑，再考虑重定向——因为
//     "申请一个可写目录"对它们根本没有作用，先提重定向只会让模型白试一轮。
func (d *Denial) NextSteps() []string {
	if d == nil {
		return nil
	}
	var steps []string
	if d.Kind == DenialRegistry || d.Kind == DenialCredentialStore {
		// 非路径资源：可写目录完全无效，只能靠"提权一整条命令"或改工具的存储位置。
		if d.Options.HasEscalation {
			steps = append(steps, `This is not a path, so a writable root cannot help: retry this one command with sandbox_permissions:"require_escalated" and a justification, and let the user approve it (escalation skips the OS sandbox for that one command; policy rules and dangerous-command checks still apply).`)
		} else {
			steps = append(steps, "This is not a path, so a writable root cannot help, and this composition has no per-command escalation: ask the user to enable command escalation in the app settings, or to run the command themselves.")
		}
		steps = append(steps,
			"If the tool can keep that state inside the workspace instead (config-dir flag, XDG_* / HOME override, or a tool-specific env var), prefer that and retry: the sandbox then stays in force.",
			"Do not repeat the same failing command unchanged, and do not guess undocumented env vars to disable a credential store: an unsupported workaround can corrupt the stored secret.",
		)
	} else {
		steps = append(steps, "Prefer redirecting the tool's own state into the workspace (config-dir flag, XDG_* / HOME override, or a tool-specific env var) and retry; no grant is needed for that.")
		switch {
		case d.Options.HasGrantTool:
			steps = append(steps, "If the tool only writes to a fixed absolute path, request a writable root for exactly that directory with request_sandbox_access(path, reason) and explain why the tool needs it.")
		case d.Options.HasEscalation:
			steps = append(steps, `If the tool only writes to a fixed absolute path and no grant tool is available, retry this one command with sandbox_permissions:"require_escalated" and a justification; the user must approve it.`)
		default:
			steps = append(steps, "This composition can neither grant a writable root nor escalate a single command: ask the user to enable sandbox grants or command escalation in the app settings before retrying.")
		}
		if d.Options.HasEscalation && d.Options.HasGrantTool {
			steps = append(steps, `If the denial is about a non-path resource (Windows registry, DPAPI/keychain credentials), a grant cannot help: retry this one command with sandbox_permissions:"require_escalated" and a justification.`)
		}
		steps = append(steps, "Do not repeat the same failing command unchanged; a denial is deterministic under the same policy.")
	}
	if d.hasPolicy && d.Budget <= 0 {
		steps = append(steps, "The grant budget for this session is exhausted; ask the operator to change the sandbox configuration.")
	}
	return steps
}

// Note 渲染面向模型的多行说明：结构化摘要 + 可写面 + 下一步。它替代原先只有
// 一句"疑似被拒绝"的归因行，让 agent 能自主决策而不是反复重试。
func (d *Denial) Note() string {
	if d == nil {
		return ""
	}
	var builder strings.Builder
	builder.WriteString(d.Summary())
	if d.Kind == DenialRegistry || d.Kind == DenialCredentialStore {
		// 这两类刻意**不**渲染工作区/可写根清单：它们只会把模型引向"申请目录"
		// 这条无效路径（真实日志里模型正是这么被绕进去的）。
		builder.WriteString("\nThis denial is about a NON-FILESYSTEM resource (")
		builder.WriteString(string(d.Kind))
		if d.Resource != "" {
			builder.WriteString(": ")
			builder.WriteString(d.Resource)
		}
		builder.WriteString("): granting a writable directory cannot help — there is no path to allow. " +
			"Registry keys and OS credential stores are governed by the user's security context, not by the sandbox's writable roots.")
		for index, step := range d.NextSteps() {
			builder.WriteString(fmt.Sprintf("\n%d. %s", index+1, step))
		}
		builder.WriteString("\nThis is an OS sandbox denial of a non-filesystem resource: no writable-root configuration can lift it.")
		return builder.String()
	}
	if d.hasPolicy {
		builder.WriteString("\nworkspace roots: ")
		if len(d.WorkspaceRoots) == 0 {
			builder.WriteString("(none)")
		} else {
			builder.WriteString(strings.Join(d.WorkspaceRoots, ", "))
		}
		builder.WriteString("\nadditional writable roots: ")
		if len(d.WritableRoots) == 0 {
			builder.WriteString("(none; only the workspace roots and platform temporary areas are writable)")
		} else {
			builder.WriteString(strings.Join(d.WritableRoots, ", "))
		}
	}
	if len(d.ProtectedPaths) > 0 {
		builder.WriteString("\nprotected (read-only) paths: ")
		builder.WriteString(strings.Join(d.ProtectedPaths, ", "))
	}
	if len(d.Grants) > 0 {
		paths := make([]string, 0, len(d.Grants))
		for _, grant := range d.Grants {
			paths = append(paths, grant.Path)
		}
		builder.WriteString("\nalready granted this session: ")
		builder.WriteString(strings.Join(paths, ", "))
	}
	for index, step := range d.NextSteps() {
		builder.WriteString(fmt.Sprintf("\n%d. %s", index+1, step))
	}
	builder.WriteString("\nThis is an OS sandbox denial, not a tool bug: file effects are confined to the roots above.")
	return builder.String()
}
