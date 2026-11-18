package gate

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/zeokeel/zeokeel-rig"
)

// 本文件提供"沙箱工具"的安全默认组合。
//
// 沙箱策略把可写面压到工作区与临时目录之后，agent 迟早会遇到"某个工具必须把
// 凭据/缓存写到固定路径"的情况。SDK 提供的两个工具分工明确：
//
//   - sandbox_status 只读，可以自动放行；
//   - request_sandbox_access 会扩大可写面，**必须由人批准**。
//
// 因此推荐的门禁组合是"只读自动放行 + 危险命令直接拒 + 其余询问"，也就是
// gate.PersonalAsk（见 defaults.go）：
//
//	hostGate := gate.PersonalAsk(gate.Ask(
//		host.Asker,
//		gate.WithAskTimeout(10*time.Minute),
//		gate.WithAskPrompt(gate.SandboxGrantPrompt),
//	))
//
// 这样 status 与只读命令不会被无意义的弹窗打断，每一次放宽可写面都会展示**具体
// 路径与理由**给人的审批界面，而 `rm -rf /` 这类危险命令在弹窗之前就被拒绝（并把
// 命中的规则 id 作为工具错误回给模型）。

// SandboxReadOnlyTools 是零配置默认档下**免问**的工具名。
//
// **判据是"是否触及用户资源（文件 / 进程 / 网络）"，不是"是否只读"**：memory 会写
// 字节、todo_write 会改 agent 状态，但它们写的是**我们自己的状态**，不是用户的文件
// 与进程——逐条弹窗只会让常驻工具变成不可用（"常驻但不可用"就是欺骗用户）。
// 反过来，任何修改用户文件（write_file/edit_file/apply_patch/str_replace_editor）、
// 执行命令（bash/pwsh/terminal_*/job_*）、长期改变行为（schedule_*）或产生用户可见
// 交付（present）的工具都必须问人。
//
// 宿主可以在此基础上增删（例如加入自己实现的只读工具）。
var SandboxReadOnlyTools = []string{
	// 文件系统只读
	"read_file", "list_dir", "glob", "grep", "read_image",
	// 检索/诊断（只读）
	"search", "inspect", "sandbox_status", "working_directory",
	// 元数据/发现（只读）：tool_search 只列名字与一句话摘要，skill 只读取技能清单与
	// 说明——两者都不碰文件、不执行命令。它们必须在白名单里：动态工具加载的默认
	// 档（ask）下，tool_search 被拒就等于整个发现机制失效（无 Asker 的宿主更是
	// 永远激活不了任何工具）。
	"tool_search", "skill",
	// 本地状态（写，但不触及用户资源）：
	//   - memory 写的是我们自己的 app 状态目录（记忆库），不是用户文件，也没有 exec
	//     与网络；它是**常驻工具**，逐条审批等于不可用；
	//   - todo_write 改的是 agent 自己的待办列表，等同于模型的思考记录。
	// 删除/清空由宿主 UI 提供，不靠审批拦。
	"memory", "todo_write",
	// 会话日志只读
	"session_search", "session_trace",
	"session_event_read", "session_event_search", "session_event_trace",
	// 后台任务/终端的只读视图（open/send/signal/close/start/kill 不在此列）
	"job_list", "job_output", "job_wait",
	"terminal_read", "terminal_list",
}

// AllowThenAsk 返回组合门禁：名字在 allow 里的工具直接放行，其余交给 inner。
//
// 它不改变 inner 的任何语义（超时、allow-always、错误传播都原样保留），只是把
// "显然安全"的只读调用从审批流里摘出去。inner 为 nil 时返回的门禁对未允许的
// 调用报配置错误（fail closed）。名字按 tool.Name() 精确匹配（tool 为 nil 时
// 退回 call.Name），与 AllowTools 一致；空名单等价于直接使用 inner。
func AllowThenAsk(allow []string, inner rig.Gate) rig.Gate {
	allowed := make(map[string]struct{}, len(allow))
	for _, name := range allow {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		allowed[name] = struct{}{}
	}
	innerExplain := explainFrom(inner)
	return funcGate{
		fn: func(ctx context.Context, tool rig.Tool, call rig.ToolCall) (rig.Decision, error) {
			if name := toolCallName(tool, call); name != "" {
				if _, ok := allowed[name]; ok {
					return rig.DecisionAllow, nil
				}
			}
			if inner == nil {
				// 非白名单 + 没有 inner：deny + 理由（不是 error，见包文档"失败模式"）。
				return rig.DecisionDeny, nil
			}
			return inner.Approve(ctx, tool, call)
		},
		// 拒绝理由必须逐层委托：组合门禁自己没有理由时，用户想知道的恰恰是内层
		// （人工审批/自动审查）说了什么。白名单内的工具永远不会被拒，因此不委托。
		explain: func(tool rig.Tool, call rig.ToolCall) string {
			if name := toolCallName(tool, call); name != "" {
				if _, ok := allowed[name]; ok {
					return ""
				}
			}
			if innerExplain != nil {
				return innerExplain(tool, call)
			}
			if inner == nil {
				return NoApprovalChannelToolReason
			}
			return ""
		},
	}
}

// SandboxGrantPrompt 为 request_sandbox_access 生成审批文案：路径 + 理由 + 当前
// 请求的动作，让审批人一眼判断"这是不是该工具真正的凭据目录"。
//
// 其他工具回退为空串，调用方（WithAskPrompt）会使用默认文案。参数解析失败时也
// 返回空串——审批文案不应该因为参数畸形而消失。
func SandboxGrantPrompt(tool rig.Tool, call rig.ToolCall) string {
	if toolCallName(tool, call) != "request_sandbox_access" {
		return ""
	}
	var args struct {
		Path   string `json:"path"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(call.Arguments), &args); err != nil {
		return ""
	}
	path := strings.TrimSpace(args.Path)
	reason := strings.TrimSpace(args.Reason)
	if path == "" {
		return ""
	}
	var builder strings.Builder
	builder.WriteString("Allow this session to write to ")
	builder.WriteString(path)
	builder.WriteString("?")
	if reason != "" {
		builder.WriteString("\nReason: ")
		builder.WriteString(reason)
	}
	builder.WriteString("\nThis widens the sandbox's writable surface for the rest of the session; approve only if that directory is where this tool legitimately keeps its own credentials or cache.")
	return builder.String()
}
