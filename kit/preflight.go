package kit

import (
	"fmt"

	"github.com/zeokeel/zeokeel-rig/sandbox"
)

// 沙箱状态的"零思考装配"。
//
// 本文件只做两件事：
//
//  1. 把 Config.Sandbox 的三态翻译成"用不用宿主注入的 runner"，并且保证**任何
//     环境问题都不让 kit.New 失败**——装不上系统沙箱不该让应用打不开；
//  2. 给 UI 一个一行拿状态的方法（Agent.Preflight），措辞与人话都来自
//     sandbox.Preflight，避免 UI 与模型看到两套说法。

// resolveSandbox 按 Config.Sandbox 的三态决定实际使用的 runner。
//
//	nil   （默认）= 尽力而为：宿主注入了 runner 就用，没有就退回进程内围栏；
//	true         = 希望启用：仍然只用宿主注入的 runner（kit 不 import 平台代码），
//	               不可用时**不报错**，由 Preflight 如实告知；
//	false        = 明确不要：忽略 runner。
//
// 唯一会返回错误的是"自相矛盾的配置"（显式关掉沙箱却又注入 runner）——那是
// 配置写错了，不是环境问题，按注入即失效（KN-18）的口径当场报错。
func resolveSandbox(cfg Config) (sandbox.Runner, bool, error) {
	// 第三个返回值只在"用户显式要求了隔离"（Sandbox = true）时为 true：
	// nil（默认尽力而为）不制造"你要求了却没给"的落差提示。
	explicit := cfg.Sandbox != nil && *cfg.Sandbox
	if cfg.Sandbox != nil && !*cfg.Sandbox {
		if cfg.SandboxRunner != nil {
			return nil, false, fmt.Errorf("kit: Config.Sandbox=false 与 Config.SandboxRunner 不能同时设置——" +
				"一个说不要系统沙箱、一个注入了它；请删掉其中一个")
		}
		return nil, false, nil
	}
	return cfg.SandboxRunner, explicit, nil
}

// Preflight 返回当前沙箱状态的一屏说明（UI 可直接展示）。
//
// 它回答三件事：我安全吗（Summaries）、我需要知道什么（Warnings）、被挡住了点
// 哪里（NextSteps）。措辞与工具里"被拒绝"的建议同源（sandbox.Preflight），所以
// UI 与模型不会各说一套。
func (a *Agent) Preflight() sandbox.PreflightReport {
	if a == nil {
		return sandbox.Preflight(nil, nil, false)
	}
	reason := a.sandboxUnavailableReason
	if a.runner != nil {
		// 有 runner 时"为什么没有沙箱"这句话就没有意义了（哪怕宿主填过）。
		reason = ""
	}
	report := sandbox.Preflight(a.policy, a.runner, a.askerAvailable, reason)
	if a.sandboxRequested && !report.Enforced {
		// 用户明确要求了隔离却没得到：把期望与事实的落差说在最前面，而不是让人
		// 以为"没报错就是隔离了"。
		report.Summaries = append([]string{"你要求了系统沙箱，但当前组合里没有可用的后端；应用照常运行，只是没有系统级围栏。"},
			report.Summaries...)
	}
	if a.autoDegraded {
		// 选了 auto 但没配 ToolReviewer：门禁退化为纯 ask。这是"配置少了一半"，
		// 必须让用户知道，否则他会以为每次调用都经过了自动预审。
		report.Warnings = append([]string{"权限预设 auto 需要 Config.ToolReviewer（自动预审）；当前没有配置，按 approval=ask 运行：需要审批的调用仍会问你（没有审批通道时会被拒绝）。"},
			report.Warnings...)
	}
	return report
}
