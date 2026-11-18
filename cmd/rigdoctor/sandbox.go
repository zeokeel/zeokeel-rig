// rigdoctor sandbox：把"我现在安全吗"打印成一屏人话。
//
// 与 `rigdoctor`（默认子命令，输出每个后端的 profile 细节）的分工：
//
//	rigdoctor            给**实现者**看：展开了哪些路径、哪条规则没有生效；
//	rigdoctor sandbox    给**用户**看：我安全吗 / 能做什么 / 被挡了怎么办。
//
// 两者都只有一句话是重叠的："现在实际用的是哪个后端"。
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"runtime"
	"strings"

	"github.com/zeokeel/zeokeel-rig/sandbox"
	"github.com/zeokeel/zeokeel-rig/sandbox/osrunner"
)

// sandboxJSON 是 `rigdoctor sandbox -json` 的输出形状（机器可读的那一份）。
type sandboxJSON struct {
	Platform    string   `json:"platform"`
	Backend     string   `json:"backend"`
	RunnerName  string   `json:"runner"`
	Enforced    bool     `json:"enforced"`
	Mode        string   `json:"mode"`
	Enforcement string   `json:"enforcement"`
	Network     string   `json:"network"`
	Roots       []string `json:"roots,omitempty"`
	RunnerError string   `json:"runnerError,omitempty"`
	Summaries   []string `json:"summaries"`
	Warnings    []string `json:"warnings,omitempty"`
	NextSteps   []string `json:"nextSteps,omitempty"`
}

// runSandboxCommand 执行 `rigdoctor sandbox`，返回进程退出码。
//
// 退出码刻意**总是 0**：这是一个"告知"命令，不是检查门禁。没有系统沙箱是常见的
// 正常状态（大多数开发机默认没装 bubblewrap），把它变成非零退出会让人以为"坏了"。
func runSandboxCommand(args []string) int {
	flags := flag.NewFlagSet("rigdoctor sandbox", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	root := flags.String("root", ".", "workspace root（绝对路径或相对于当前目录）")
	mode := flags.String("mode", "", "只看某一档（read-only|workspace-write|danger-full-access）；默认取策略的当前值")
	network := flags.Bool("network", false, "假设允许联网（默认按“不联网”报告）")
	asker := flags.String("asker", "auto", "应用里有没有审批通道：auto（默认，尽量探测；探测不出来按保守假设「没有」并标注）| yes | no")
	asJSON := flags.Bool("json", false, "输出 JSON（给脚本用；默认是给人看的一屏）")
	if err := flags.Parse(args); err != nil {
		return 2
	}

	options := []sandbox.Option{
		sandbox.WithRoots(*root),
		sandbox.WithNetwork(*network),
	}
	if *mode != "" {
		options = append(options, sandbox.WithMode(sandbox.Mode(*mode)))
	}
	policy := sandbox.New(options...)

	askerAvailable, askerNote := resolveAsker(*asker)

	runner, runnerErr := osrunner.New(osrunner.Config{})
	unavailable := ""
	if runnerErr != nil {
		unavailable = runnerErr.Error()
	}
	report := sandbox.Preflight(policy, runner, askerAvailable, unavailable)

	if *asJSON {
		runnerName, _, _ := sandbox.RunnerStatus(runner)
		payload := sandboxJSON{
			Platform:    runtime.GOOS,
			Backend:     report.Backend,
			RunnerName:  runnerName,
			Enforced:    report.Enforced,
			Mode:        string(report.Mode),
			Enforcement: string(report.Enforcement),
			Network:     report.Network,
			Roots:       policy.Roots(),
			Summaries:   report.Summaries,
			Warnings:    report.Warnings,
			NextSteps:   report.NextSteps,
		}
		if runnerErr != nil {
			payload.RunnerError = runnerErr.Error()
		}
		encoded, encodeErr := json.MarshalIndent(payload, "", "  ")
		if encodeErr != nil {
			fmt.Fprintln(os.Stderr, "rigdoctor sandbox:", encodeErr)
			return 1
		}
		fmt.Println(string(encoded))
		return 0
	}

	fmt.Println(report.Text())
	if askerNote != "" {
		fmt.Println()
		fmt.Println(askerNote)
	}
	fmt.Println()
	fmt.Printf("检测到的后端：%s\n", describeBackend(report, runnerErr))
	return 0
}

// resolveAsker 解析 -asker 三态。
//
//	auto（默认）：尽量探测。当前没有可靠的探测手段（是否连了终端、有没有某个环境
//	             变量都不能证明"应用里有审批通道"），因此探测不出来时**按保守假设
//	             处理**，并把"这是假设"明说出来——比悄悄给个结论好。
//	yes / no：用它。
func resolveAsker(mode string) (available bool, note string) {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "yes", "true", "1":
		return true, ""
	case "no", "false", "0":
		return false, ""
	case "", "auto":
		if value := strings.TrimSpace(os.Getenv("RIGDOCTOR_ASKER")); value != "" {
			switch strings.ToLower(value) {
			case "yes", "true", "1":
				return true, "（审批通道：按环境变量 RIGDOCTOR_ASKER=" + value + " 假设「有」）"
			case "no", "false", "0":
				return false, "（审批通道：按环境变量 RIGDOCTOR_ASKER=" + value + " 假设「没有」）"
			}
		}
		return false, "（审批通道：未指定，按保守假设「没有」——如果你的应用接了审批，用 -asker=yes 看另一种说法）"
	}
	return false, "（未识别的 -asker=" + mode + "，按「没有」处理）"
}

// describeBackend 拼出"实际用的是哪个后端、它管到哪"这一行（人话，不堆术语）。
func describeBackend(report sandbox.PreflightReport, runnerErr error) string {
	if runnerErr != nil {
		return "无 —— " + firstLine(runnerErr.Error())
	}
	if !report.Enforced {
		if report.Backend == "" {
			return "无 —— 这台机器上没有可用的系统沙箱"
		}
		return report.Backend + "（当前这一档没有启用它）"
	}
	switch report.Enforcement {
	case sandbox.EnforcementFull:
		return report.Backend + "（写入、删除与网络都按设置执行）"
	case sandbox.EnforcementPartial:
		return report.Backend + "（只挡住了写入与删除，网络与读取另说）"
	}
	return report.Backend
}

// firstLine 取错误信息的第一行（bubblewrap 之类的错误常带多行 usage 提示）。
func firstLine(text string) string {
	for index := 0; index < len(text); index++ {
		if text[index] == '\n' {
			return text[:index]
		}
	}
	return text
}
