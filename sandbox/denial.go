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

// Denial 是一次已识别的沙箱拒绝。
type Denial struct {
	// Matched 是命中的拒绝方言。
	Matched string
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
	// hasPolicy 记录归因时是否带上了策略（nil 策略时不应凭空渲染可写面）。
	hasPolicy bool
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
// "permission denied" 之类文本来自 shell 自己，不是沙箱拒绝——Codex 的
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
			Op:          "unknown",
			ExitCode:    exitCode,
			Signal:      "SIGSYS",
			Enforcement: enforcement,
		}
		if policy != nil {
			denial.hasPolicy = true
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
	if target == "" {
		target = "a path outside the writable roots"
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
func (d *Denial) NextSteps() []string {
	if d == nil {
		return nil
	}
	steps := []string{
		"Prefer redirecting the tool's own state into the workspace (config-dir flag, XDG_* / HOME override, or a tool-specific env var) and retry; no grant is needed for that.",
		"If the tool only writes to a fixed absolute path, request a writable root for exactly that directory with request_sandbox_access(path, reason) and explain why the tool needs it.",
		"Do not repeat the same failing command unchanged; a denial is deterministic under the same policy.",
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
