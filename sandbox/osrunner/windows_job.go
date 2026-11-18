package osrunner

// 本文件是 Windows 受限运行的**进程树**纯逻辑：结果模型、退出码映射、可识别诊断
// 文案、环境块编码、spawn 前置条件（fail closed 策略）。它不带 build tag，因此
// 所有能在 Linux 上断言的决策都在这里，Win32 调用在 windows_job_object.go
// （//go:build windows）。
//
// # 为什么需要 Job（kill-on-close）
//
// helper 只等待**直接子进程**。`cmd /c start /b …`、`pwsh -Command "Start-Process …"`
// 这类命令会把后代脱离父进程存活，于是 helper 退出、cleanup 恢复 per-run 的
// Everyone 拒绝与 Low 标签之后，后代仍在裸跑——而且是在"脚下"失去授权（Low 标签
// 被撤 → NO_WRITE_UP 拒绝），私有 temp 也已经被删掉。同类实现的注释明确写着
// "descendants may outlive the launcher"，他们用 Job 管整棵树。
//
// 本后端的顺序约定：**先让整棵进程树消失，再恢复 per-run 形状**。Job 句柄带
// JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE，helper 进程死亡（含被宿主 ctx 超时强杀、
// 崩溃）时内核关闭句柄 → 整棵树被终止。这是唯一能覆盖"helper 被强杀"的机制。

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf16"
)

const (
	// winTreeTerminatedExitCode 是 helper **主动终止整棵进程树**时使用的退出码
	// （与 GNU timeout 的约定一致）。它刻意不伪装成命令自己的退出码：调用方看到
	// 124 就知道"这不是命令的业务结果，而是沙箱把树收了"。
	winTreeTerminatedExitCode = 124
	// winTreeTerminationNoticePrefix 是"进程树被沙箱终止"的固定可识别文本前缀。
	winTreeTerminationNoticePrefix = "windows-acl: process tree terminated by sandbox"
	// winTreeTerminationWait 是终止整棵树之后等待其真正消失的上限。超过这个上限
	// 仍在的进程会被逐字记进 stderr（几乎不可能：TerminateProcess 是可靠的）。
	winTreeTerminationWait = 5 * time.Second
)

// winTreeKillResult 是"收掉整棵进程树"的结果。
type winTreeKillResult struct {
	// Terminated 是顶层命令之外被终止的进程数（0 表示本来就没有后代）。
	Terminated int
	// Remaining 是等待上限之后仍存活的进程数（正常为 0）。
	Remaining int
}

// terminated 报告是否真的收掉过进程。
func (result winTreeKillResult) terminated() bool { return result.Terminated > 0 }

// winProcessOutcome 是一次受限运行的完整结果。
type winProcessOutcome struct {
	// ExitCode 是顶层命令的退出码（超时路径下是 winTreeTerminatedExitCode）。
	ExitCode int
	// TimedOut 表示触发了 helper 侧的看门狗时限（不是命令自己失败）。
	TimedOut bool
	// Timeout 是触发看门狗时使用的时限（用于诊断文案）。
	Timeout time.Duration
	// Tree 是收树结果（没有后代时 Terminated/Remaining 都是 0）。
	Tree winTreeKillResult
}

// winSpawnPrecondition 报告一次受限 spawn 的前置条件；不满足时返回 fail closed 错误。
//
// 策略选择：**没有 Job 就不 spawn**（fail closed），不提供"没有 Job 也照跑"的
// 降级。理由：降级 path 会回到旧语义——后代可以活过 helper，而 helper 退出时会
// 撤掉 per-run 的 Everyone 拒绝与 Low 标签（并删掉私有 temp），于是那些后代在
// 自己的脚下失去授权，表现为"随机失败的命令/写不进去的半截文件"，同时沙箱的
// 边界描述也不再成立。宁可让命令明确跑不起来（helper 以 127 + 可读错误退出，
// 而且功能探测在 New() 阶段就会失败），也不静默放宽。
func winSpawnPrecondition(jobAvailable, layoutOK bool) error {
	if !layoutOK {
		return errors.New("windows-acl-run: the sandbox job object struct layout does not match 64-bit Windows")
	}
	if !jobAvailable {
		return errors.New("windows-acl-run: no sandbox job object (refusing to spawn outside a kill-on-close job)")
	}
	return nil
}

// winJobWaitMilliseconds 把 helper 侧看门狗时限映射成 WaitForSingleObject 的毫秒参数。
//
// 0 或负值 = INFINITE（没有看门狗）；正数按毫秒向上取整，至少 1ms——0 在
// WaitForSingleObject 里表示"立刻返回"，不能用来表达"极短超时"。
func winJobWaitMilliseconds(timeout time.Duration) uint32 {
	if timeout <= 0 {
		return 0xFFFFFFFF // INFINITE
	}
	if timeout <= time.Millisecond {
		return 1
	}
	milliseconds := int64(timeout / time.Millisecond)
	if timeout%time.Millisecond != 0 {
		milliseconds++
	}
	if milliseconds >= int64(0xFFFFFFFE) {
		return 0xFFFFFFFE
	}
	return uint32(milliseconds)
}

// winShouldTerminateTree 报告是否需要主动终止进程树（仍有活跃进程）。
func winShouldTerminateTree(activeProcesses int) bool {
	return activeProcesses > 0
}

// winOutcomeExitCode 返回 helper 应返回的退出码。
//
// 超时（树被沙箱收掉）用固定码 124，绝不把"被终止"伪装成命令自己的退出码；
// 其它情况沿用顶层命令的退出码。
func winOutcomeExitCode(outcome winProcessOutcome) int {
	if outcome.TimedOut {
		return winTreeTerminatedExitCode
	}
	return outcome.ExitCode
}

// winTreeTerminationNotice 构造"进程树被沙箱终止"的诊断行；没有终止任何东西时
// 返回空串（调用方据此不打印噪声）。
func winTreeTerminationNotice(outcome winProcessOutcome) string {
	if outcome.TimedOut {
		text := fmt.Sprintf("%s (timeout after %s", winTreeTerminationNoticePrefix, outcome.Timeout)
		if outcome.Tree.Terminated > 0 {
			text += fmt.Sprintf(", %d descendant process(es) killed", outcome.Tree.Terminated)
		}
		text += ")"
		return appendRemainingNotice(text, outcome.Tree)
	}
	if !outcome.Tree.terminated() {
		return ""
	}
	text := fmt.Sprintf("%s (%d descendant process(es) killed after the top-level command exited)",
		winTreeTerminationNoticePrefix, outcome.Tree.Terminated)
	return appendRemainingNotice(text, outcome.Tree)
}

// appendRemainingNotice 把"等待上限后仍有进程存活"的异常情况补进诊断行。
func appendRemainingNotice(text string, result winTreeKillResult) string {
	if result.Remaining <= 0 {
		return text
	}
	return fmt.Sprintf("%s; %d process(es) still alive after %s", text, result.Remaining, winTreeTerminationWait)
}

// winEnvironmentBlockFor 返回要传给 CreateProcess 的环境块。
//
// nil 输入表示"继承 helper 的环境"：返回 (nil, nil)，调用方必须传 NULL 指针给
// CreateProcess（传一个只有结尾 NUL 的空块会得到**空环境**——PATH 都没了，命令
// 会莫名其妙跑不起来）。非 nil（哪怕是空切片）才编码成替换用的环境块。
func winEnvironmentBlockFor(environment []string) ([]uint16, error) {
	if environment == nil {
		return nil, nil
	}
	return winBuildEnvironmentBlock(environment)
}

// winBuildEnvironmentBlock 把环境变量编码成 CreateProcess 需要的 UTF-16 环境块：
// 若干 NUL 结尾的 "NAME=VALUE" 字符串 + 末尾额外的 NUL。
//
// 与 syscall 内部实现一致的三条要求：按变量名（ASCII 大写）排序、任何含 NUL 的
// 条目都是错误、空块也要有结尾 NUL。额外的防御：跳过空字符串条目（空条目会在块
// 中间插入提前终止符，让后面的变量静默丢失）。
func winBuildEnvironmentBlock(environment []string) ([]uint16, error) {
	entries := make([]string, 0, len(environment))
	for _, entry := range environment {
		if entry == "" {
			continue
		}
		if strings.ContainsRune(entry, 0) {
			return nil, errors.New("windows-acl: environment entry contains a NUL byte")
		}
		entries = append(entries, entry)
	}
	if len(entries) == 0 {
		return []uint16{0, 0}, nil
	}
	// 稳定排序：同名变量（大小写不同）保持输入顺序，行为可预测。
	sort.SliceStable(entries, func(left, right int) bool {
		return winEnvironmentKey(entries[left]) < winEnvironmentKey(entries[right])
	})
	block := make([]uint16, 0, len(entries)*8)
	for _, entry := range entries {
		for _, char := range entry {
			block = utf16.AppendRune(block, char)
		}
		block = utf16.AppendRune(block, 0)
	}
	return utf16.AppendRune(block, 0), nil
}

// winEnvironmentKey 返回环境条目的排序键（变量名的 ASCII 大写形式）。
//
// 只折叠 ASCII：Windows 的环境变量名在实践中都是 ASCII，而且系统文档只要求
// "按名字字母序"，不要求真正的 Unicode 排序。
func winEnvironmentKey(entry string) string {
	name, _, found := strings.Cut(entry, "=")
	if !found {
		return ""
	}
	upper := []byte(name)
	for index, char := range upper {
		if 'a' <= char && char <= 'z' {
			upper[index] = char - ('a' - 'A')
		}
	}
	return string(upper)
}
