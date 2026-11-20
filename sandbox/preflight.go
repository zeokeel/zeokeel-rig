package sandbox

import (
	"fmt"
	"strings"
)

// 沙箱状态的一屏说明（给**用户**看，不是给机器看）。
//
// 用户不该需要知道 read-only / workspace-write / danger-full-access、osrunner、
// bubblewrap、Landlock、Seatbelt、ACL 这些词。他只想知道三件事：
//
//  1. 我现在安全吗？（Summaries）
//  2. 我能让它做什么？（Summaries + Warnings）
//  3. 它被挡住了，我该点哪里？（NextSteps）
//
// 因此本文件只做"把策略与 runner 的自报事实翻译成人话"，不含任何平台代码：
// 后端名字、覆盖程度、网络治理范围全部来自 Runner 的自报（Reporter /
// NetworkReporter），没有 runner 时就是"没有"，绝不猜测。

// PreflightReport 是一次预检的结果。
type PreflightReport struct {
	// Mode 是实际生效的权限模式。
	Mode Mode
	// Enforced 表示有没有**真的** OS 围栏（false = 只有应用层的进程内校验）。
	Enforced bool
	// Backend 是人类可读的后端名（"bubblewrap" / "Landlock" / "Seatbelt" /
	// "Windows ACL"）；没有后端时为空。
	Backend string
	// Enforcement 是后端对策略承诺的文件效果的实际覆盖程度。
	Enforcement Enforcement
	// Network 是网络的实际状态："off"（完全不能联网）、"loopback"（只有本地
	// 回环）、"full"（可以联网）、"not-governed"（没有系统级网络围栏）。
	Network string
	// Summaries 是一两句人话，按重要性排序。
	Summaries []string
	// Warnings 是"你需要知道但不会挡住你"的事实（例如读不受限）。
	Warnings []string
	// NextSteps 是"被挡住时该点哪里"。
	NextSteps []string
}

// 网络状态的取值（PreflightReport.Network）。
const (
	NetworkOff         = "off"
	NetworkLoopback    = "loopback"
	NetworkFull        = "full"
	NetworkNotGoverned = "not-governed"
)

// Preflight 汇总"现在的隔离状态"，用一两句人话回答"我安全吗 / 能做什么 / 被挡了怎么办"。
//
// 参数：
//   - policy 是当前生效的策略（nil 按默认 workspace-write 处理）；
//   - runner 是 OS 级执行器（nil 表示这台机器上没有可用的系统沙箱）；
//   - askerAvailable 表示当前组合里"有人可以被问"（决定被挡住时是不是真的能点
//     「允许这一次」）；
//   - unavailableReason 是可选的一句"为什么没有沙箱"（由调用方从 osrunner.New 的
//     错误里取，例如 "bubblewrap 未安装"）。本包不探测平台，只负责展示。
func Preflight(policy *Policy, runner Runner, askerAvailable bool, unavailableReason ...string) PreflightReport {
	report := PreflightReport{
		Mode:        policy.Mode(),
		Enforcement: EnforcementNone,
		Network:     NetworkNotGoverned,
	}
	if policy != nil && len(policy.Roots()) > 0 {
		report.Summaries = nil // 下面按状态填
	}

	backendName, enforcement, limits := "", EnforcementNone, ""
	if runner != nil {
		backendName, enforcement, limits = RunnerStatus(runner)
	}
	report.Backend = humanBackend(backendName)
	report.Enforcement = enforcement

	isolated := runner != nil && report.Mode != ModeDangerFullAccess && enforcement != EnforcementNone
	report.Enforced = isolated
	report.Network = networkState(policy, runner, isolated)

	workspace := "工作区"
	if policy != nil {
		if roots := policy.Roots(); len(roots) == 1 {
			workspace = roots[0]
		} else if len(roots) > 1 {
			workspace = strings.Join(roots, "、")
		}
	}

	// 私有临时目录（A4）属于"真实可写面"的一部分，必须出现在下面这句话里；
	// 建不出来时更要说出来，否则命令会在运行期以 ENOENT/EACCES 零散失败。
	tempNote := "（以及系统临时目录）"
	if policy != nil && policy.Mode() == ModeReadOnly {
		if scratch := policy.ScratchDir(); scratch != "" {
			tempNote = "（以及私有临时目录 " + scratch + "）"
		} else {
			tempNote = "（没有可写的临时目录）"
		}
	}
	if policy != nil {
		if err := policy.ScratchDirError(); err != nil {
			report.Warnings = append(report.Warnings,
				"私有临时目录没能准备好："+err.Error()+"。需要临时文件的命令可能会失败。")
		}
	}

	switch {
	case report.Mode == ModeDangerFullAccess:
		report.Summaries = append(report.Summaries,
			"未隔离：agent 可以直接改你机器上的任何文件、执行任何命令。危险命令仍会被拦，但这不等于安全。")
		report.Warnings = append(report.Warnings,
			"这一档是「我相信它」的意思，不是「它更听话」：文件围栏与命令白名单都不生效，只有破坏性命令的拦截还在。")
		report.Warnings = append(report.Warnings, "要在能干活与安全之间取平衡，把档位改成「只让它改工作区」这一档。")
	case !isolated && runner == nil:
		report.Summaries = append(report.Summaries,
			"未隔离：这台机器上没有可用的系统沙箱（只有应用层校验）。")
		if len(unavailableReason) > 0 && strings.TrimSpace(unavailableReason[0]) != "" {
			report.Summaries = append(report.Summaries, "原因："+strings.TrimSpace(unavailableReason[0])+"。")
		}
		report.Warnings = append(report.Warnings,
			"应用层校验只约束配合的模型：它能让 agent 不去碰不该碰的文件，挡不住它真的去碰。")
		report.Warnings = append(report.Warnings,
			"要真正隔离：Linux 上装一个系统沙箱（发行版包名通常是 bubblewrap）；macOS 与 Windows 自带，不需要装。不装也能用，只是没有系统级围栏。")
	case !isolated:
		report.Summaries = append(report.Summaries,
			"未隔离：这台机器上检测到了系统沙箱，但它没能覆盖当前模式（只有应用层校验）。")
	case enforcement == EnforcementFull:
		report.Summaries = append(report.Summaries,
			fmt.Sprintf("已隔离：这个会话只能改 %s%s。", workspace, tempNote))
	default:
		report.Summaries = append(report.Summaries,
			fmt.Sprintf("部分隔离：只能改 %s%s，越界的写入与删除会被挡住；但网络与读取没有限制（别把凭据放在 agent 能读的地方）。", workspace, tempNote))
	}

	// 网络单列一句：不能联网是最容易被误解的状态（"为什么我的 dev server 连不上"）。
	switch report.Network {
	case NetworkOff:
		report.Summaries = append(report.Summaries, "不能联网（本地 dev server 也连不上）。")
	case NetworkLoopback:
		report.Summaries = append(report.Summaries, "只能连本机（localhost），不能访问外网。")
	case NetworkFull:
		report.Summaries = append(report.Summaries, "可以联网（小心它把你的文件或凭据发出去）。")
	case NetworkNotGoverned:
		if report.Enforced && enforcement == EnforcementFull {
			report.Warnings = append(report.Warnings, "这个后端没有系统级网络围栏：agent 能不能联网取决于应用设置，而不是系统沙箱。")
		}
	}

	// 后端自报的 limits 刻意不在这里展示：它是实现者视角的细节（哪些系统调用、
	// 哪些 socket 类型没被治理），对"我现在安全吗"没有增量信息，而且通常是长句
	// 英文。要看细节请用 sandbox_status 或 rigdoctor 的默认子命令。
	_ = limits
	if isolated && enforcement == EnforcementFull {
		// 现在所有后端都**不治理读**，所以直接说事实，不用"通常"这种模糊词。
		// 若将来有后端开始治理读，就在 Reporter 里加一项读覆盖度并按它分支。
		report.Warnings = append(report.Warnings,
			"这个后端不管读：agent 能读到的凭据（~/.ssh、~/.aws、浏览器 cookie）都算它读得到。")
	}

	// 被挡住时的下一步：这里说一遍，和工具里的拒绝建议是同一套口径。
	if isolated {
		if askerAvailable {
			report.NextSteps = append(report.NextSteps,
				"被挡住时：在界面上点「允许这一次」，这一条命令就会在沙箱外重跑一次；只对当前这条命令有效，不会放宽整个会话。")
		} else {
			report.NextSteps = append(report.NextSteps,
				"被挡住时：当前没有可以问人的审批通道，被沙箱挡住的命令会直接被拒绝；请先在设置里接入审批。")
		}
		report.NextSteps = append(report.NextSteps,
			"要长期改，就在设置里给它加一个可写目录；没被授权的地方它永远改不了。")
	} else if report.Mode == ModeDangerFullAccess {
		// 后端是好的，是档位把它关掉了——别让人去装东西。
		report.NextSteps = append(report.NextSteps,
			"想恢复隔离：把档位改成「只让它改工作区」。这一档下没有任何围栏，也就没有「被挡住」这回事。")
	} else {
		report.NextSteps = append(report.NextSteps,
			"现在没有系统级围栏：不会有「被沙箱挡住」这回事，挡住你的只有应用层校验。")
		report.NextSteps = append(report.NextSteps,
			"想让它少被挡：在设置里给它加一个可写目录；想真正隔离：装好后端后重新打开应用即可，不需要改配置。")
	}
	return report
}

// humanBackend 把 runner 的稳定标识翻译成人能读的名字；未知标识原样返回。
func humanBackend(name string) string {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "":
		return ""
	case "bwrap", "bubblewrap":
		return "bubblewrap"
	case "landlock":
		return "Landlock"
	case "seatbelt", "sandbox-exec":
		return "Seatbelt"
	case "windows-acl", "windows_acl", "acl":
		return "Windows ACL"
	}
	return name
}

// networkState 把"策略要不要联网 + 后端的实际覆盖"翻译成一个状态。
func networkState(policy *Policy, runner Runner, isolated bool) string {
	if !isolated {
		return NetworkNotGoverned
	}
	if policy != nil && policy.NetworkAllowed() {
		return NetworkFull
	}
	switch RunnerNetworkCoverage(runner) {
	case "full":
		return NetworkOff
	case "partial", "loopback":
		return NetworkLoopback
	}
	return NetworkNotGoverned
}

// Text 把整个预检结果拼成一段可以直接塞进 UI 的多行文本（Summaries/Warnings/
// NextSteps 各一段，带小标题）。命令行与 UI 共用一份口径，避免两处措辞漂移。
func (r PreflightReport) Text() string {
	var builder strings.Builder
	for _, summary := range r.Summaries {
		builder.WriteString(summary)
		builder.WriteString("\n")
	}
	writeSection := func(title string, items []string) {
		if len(items) == 0 {
			return
		}
		builder.WriteString("\n" + title + "：\n")
		for _, item := range items {
			builder.WriteString("  - " + item + "\n")
		}
	}
	writeSection("需要知道", r.Warnings)
	writeSection("下一步", r.NextSteps)
	return strings.TrimRight(builder.String(), "\n")
}
