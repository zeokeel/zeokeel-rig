package escalate

// 本文件负责把一次提权结果渲染成模型可见的文本。
//
// 三态必须互不混淆（它们是模型据以决策的唯一信息）：
//
//   - 成功：命令**执行了**（含非零退出码）。第一行固定是 `exit_code: N`。
//   - 策略拒绝：命令**没有**执行。`[escalation denied: <原因>]`
//   - 通道故障/超时：不知道命令有没有跑完。`[escalation failed: <错误>]`
//   - 宿主繁忙：容量不足（排队已满），不是拒绝，可以稍后重试。
//     `[escalation busy: <原因>; ...]`
//
// 文案是稳定接口：测试与宿主 UI 都按前缀识别，改动需同步文档与用例。

import (
	"fmt"
	"strings"
)

const (
	// DeniedPrefix 标识"策略拒绝、命令未执行"。
	DeniedPrefix = "[escalation denied: "
	// FailedPrefix 标识"提权通道故障/超时"。
	FailedPrefix = "[escalation failed: "
	// BusyPrefix 标识"宿主繁忙、可稍后重试"（不是策略拒绝）。
	BusyPrefix = "[escalation busy: "
	// truncatedNotice 是输出被截断时追加的标注。
	truncatedNotice = "[truncated: output exceeded the host limit]"
	// emptyBodyNotice 表示某一路输出为空（保持三态格式稳定，便于模型解析）。
	emptyBodyNotice = "(empty)"
)

// renderSuccess 渲染一次"命令已执行"的结果。limit 是 stdout/stderr 各自的字节上限
// （<=0 表示不截断）；截断时追加 truncatedNotice。
func renderSuccess(exitCode int, stdout, stderr string, truncated bool, limit int) string {
	stdout, stdoutCut := truncateUTF8(stdout, limit)
	stderr, stderrCut := truncateUTF8(stderr, limit)
	var builder strings.Builder
	fmt.Fprintf(&builder, "exit_code: %d", exitCode)
	builder.WriteString("\nstdout:\n")
	if stdout == "" {
		builder.WriteString(emptyBodyNotice)
	} else {
		builder.WriteString(stdout)
	}
	builder.WriteString("\nstderr:\n")
	if stderr == "" {
		builder.WriteString(emptyBodyNotice)
	} else {
		builder.WriteString(stderr)
	}
	if truncated || stdoutCut || stderrCut {
		builder.WriteString("\n")
		builder.WriteString(truncatedNotice)
	}
	return builder.String()
}

// renderDenied 渲染"策略拒绝、命令未执行"。
func renderDenied(reason string) string {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "no reason given by the host policy"
	}
	return DeniedPrefix + reason + "]"
}

// renderFailed 渲染"提权通道故障/超时"。
func renderFailed(err error) string {
	message := "unknown error"
	if err != nil {
		message = strings.TrimSpace(err.Error())
		if message == "" {
			message = "unknown error"
		}
	}
	return FailedPrefix + message + "]"
}

// renderBusy 渲染"宿主繁忙、可稍后重试"。
//
// 必须与策略拒绝区分：模型看到 denied 会放弃，看到 busy 应该退避重试。
func renderBusy(reason string) string {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "the host is at capacity"
	}
	return BusyPrefix + reason + "; retry the same command later — this is a capacity limit, not a policy denial]"
}

// truncateUTF8 把 s 截到最多 limit 字节且不切断多字节字符；limit<=0 表示不截断。
func truncateUTF8(s string, limit int) (string, bool) {
	if limit <= 0 || len(s) <= limit {
		return s, false
	}
	cut := limit
	for cut > 0 && !utf8RuneStart(s[cut]) {
		cut--
	}
	return s[:cut], true
}
