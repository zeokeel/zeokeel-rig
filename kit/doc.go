// Package kit 是 ZeoKeel Rig 的“一次导入、开箱即用”门面包：New 把兼容 OpenAI 的
// provider、完整的沙箱工具集、安全的只读默认审批门禁以及内存会话历史装配成一个
// 可直接运行的 agent。
//
// # 一分钟上手
//
// 一次 import 即可跑完整 agent：
//
//	import (
//		"context"
//		"fmt"
//		"log"
//		"time"
//
//		"github.com/zeokeel/zeokeel-rig/kit"
//	)
//
//	a, err := kit.New(kit.Config{Model: "gpt-4o-mini", Root: "."})
//	if err != nil {
//		log.Fatal(err)
//	}
//	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
//	defer cancel()
//	_, _, err = a.RunText(ctx, "session-1", "列出这个仓库里的文件",
//		func(ev kit.Event) {
//			if ev.Type == kit.EventTextDelta {
//				fmt.Print(ev.Text)
//			}
//		})
//
// # 主要类型
//
//   - Config：装配参数。只有 Model 必填；Mode 选择权限模式，Approval 选择审批
//     预设，PTC 控制是否追加 run_code 工具，其余字段都有文档化的默认值。
//   - Agent：装配好的 agent，实现 rig.Agent；Run 执行一轮，RunText 是“一个 prompt
//     → 聚合文本”的便捷封装。
//   - Mode/ModeReadOnly/ModeWorkspaceWrite/ModeDangerFullAccess：sandbox 权限模式的
//     别名；ApprovalAsk/ApprovalAuto/ApprovalNever：内置审批预设。
//   - 其余类型（Provider、Tool、Gate、History、Message、Event、各常量等）都是根包
//     rig 对应类型的别名，见 kit/aliases.go。
//
// # 默认值
//
// Config 只有 Model 必填；零值 Config 加一个模型就是可用且刻意保守的 agent。
// Config 的“默认值”文档逐项说明了每个选择：tools/full 的完整工具集、以 Root 为
// 根且不带 shell 允许列表/不联网的沙箱策略、只放行 read_file/list_dir/glob/grep
// 的审批门禁，以及全新的内存历史。Approval=auto 会放行全部工具调用，应只在与
// 可信部署搭配时使用。
//
// # 逃生通道
//
// kit 是薄门面而不是框架：这里的每个类型都是 github.com/zeokeel/zeokeel-rig 中
// 对应类型的别名（见 kit/aliases.go），因此当默认装配不够用时，可以保留相同的值
// 并直接 import 专门包（agent、tools/full、gate、sandbox、session、provider/openai
// 等）。*kit.Agent 本身实现 rig.Agent。需要 Host/Runner/MCP/LSP/Searcher 等完整
// 能力时，请改用 tools/full.All 显式装配（参见 docs/zh/AI-SDK.md）。
//
// # 不变量与安全说明
//
//   - kit.New 不暴露 Host、Runner、MCPClient、Searcher 等宿主能力；依赖它们的工具
//     会返回“host 未配置”一类可读错误，而不会 panic。
//   - 默认 ApprovalAsk 只放行只读工具，写文件与 shell 执行需要显式改用 ApprovalAuto
//     或传入自定义 Gate。
//   - 权限模式与审批是工具实现层的围栏，不是内核级沙箱；不可信模型应放进容器或
//     VM。danger-full-access 会移除工作区/允许列表围栏，仅应与显式或自动审批搭配。
//   - PTC（run_code）默认关闭；打开后模型编写的 Python/Node 程序通过系统已安装的
//     运行时执行，内层工具调用仍走同一个 Gate。
//   - 默认 History 是进程内 session.Memory，按 SessionID 隔离；进程退出即丢失。
//
// 与 agent 循环/工具装配的关系：kit.New 内部等价于构造 sandbox.Policy →
// tools/full.All → gate → session.NewMemory → agent.New，因此它只是把常见装配
// 收敛成一次调用；需要精细控制时可以按同样步骤手动组装。
package kit
