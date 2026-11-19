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
// ⚠️ 上面的默认配置（Approval 缺省 = "ask" 且没有 Asker）**只能读、不能写**：
// 写文件与可变命令（含 `go test`、`go build`）都会因为没有审批通道而被拒绝，模型
// 收到的是"没有可用的审批通道"的工具错误。要跑"改代码 → 跑测试 → 修好"这条闭环，
// 二选一：
//
//	// ① 有人看着：接上 Asker，写操作与可变命令会弹给你确认
//	a, err = kit.New(kit.Config{Model: "gpt-4o-mini", Root: ".", Asker: myAsker})
//
//	// ② 不想被打断：auto（**不等于** AllowAll——rm -rf /、mkfs、curl|sh 仍会被拒）
//	a, err = kit.New(kit.Config{Model: "gpt-4o-mini", Root: ".", Approval: kit.ApprovalAuto})
//
// # 主要类型
//
//   - Config：装配参数。只有 Model 必填；Mode 选择权限模式，Approval 选择审批
//     预设，PTC 控制是否追加 run_code 工具，SandboxGrants 控制是否注册
//     sandbox_status/request_sandbox_access（默认开，nil = 开；只注册、不进初始
//     active 集），DisableProtection 关闭默认保护路径，其余字段都有文档化的默认值。
//   - Agent：装配好的 agent，实现 rig.Agent；Run 执行一轮，RunText 是“一个 prompt
//     → 聚合文本”的便捷封装。
//   - Mode/ModeReadOnly/ModeWorkspaceWrite/ModeDangerFullAccess：sandbox 权限模式的
//     别名；ApprovalAsk/ApprovalAuto/ApprovalNever：内置审批预设。
//   - 其余类型（Provider、Tool、Gate、History、Message、Event、各常量等）都是根包
//     rig 对应类型的别名，见 kit/aliases.go。
//
// # 默认值
//
// Config 只有 Model 必填；零值 Config 加一个模型就是**可用但只读**的 agent
// （Ask 档 + 无 Asker：只读工具放行，其余——写文件、bash/pwsh——都被拒，见
// "一分钟上手"的两种改法）。Config 的“默认值”文档逐项说明了每个选择：tools/full 的完整
// 工具集（再经 personal 预设与零配置体检裁剪）、以 Root 为根且不带 shell 允许
// 列表/不联网的沙箱策略、两层审批门禁（只读免问 + 其余问人，默认不超时），以及
// 全新的内存历史。Approval=auto 不问人，
// 且全部放行（等于 AllowAll；与 DSH 的 danger-full-access 档同义），应只在与可信
// 部署或已开启的 OS 沙箱搭配时使用。需要"危险命令硬拦"请显式叠加
// gate.CommandRiskGate（默认预设不再使用它）。
//
// # 逃生通道
//
// kit 是薄门面而不是框架：这里的每个类型都是 github.com/zeokeel/zeokeel-rig 中
// 对应类型的别名（见 kit/aliases.go），因此当默认装配不够用时，可以保留相同的值
// 并直接 import 专门包（agent、tools/full、gate、sandbox、session、provider/openai
// 等）。*kit.Agent 本身实现 rig.Agent。需要 Host/Runner/MCP/LSP/Searcher 等完整
// 能力时，请改用 tools/full.All 显式装配（参见 docs/AI-SDK.md）。
//
// # 不变量与安全说明
//
//   - kit.New 不暴露 Host、子 agent Runner、MCP/LSP 客户端等宿主能力（Searcher 与
//     SessionLog 是可注入的，见 Config）；依赖它们的工具会返回“host 未配置”一类
//     可读错误，而不会 panic。
//   - 默认 ApprovalAsk 是两层门禁：只读工具（gate.SandboxReadOnlyTools）免问；
//     其余——写文件、bash/pwsh（**包括危险命令**）——交给 Config.Asker 人工审批，
//     默认**不超时**（一直等，只有取消本轮才撤回）。没有 Asker 时退到
//     PersonalAskWithoutAsker：只读放行、其余拒绝并说明“没有可用的审批通道”。
//     默认门禁**不做**命令风险分类；要硬拦危险命令请显式叠加 gate.CommandRiskGate。
//     要无人值守改用 ApprovalAuto（不问人、全部放行）或传入自定义 Gate。
//   - 权限模式与审批是工具实现层的围栏，不是内核级沙箱；不可信模型应放进容器或
//     VM。danger-full-access 会移除工作区/允许列表围栏，仅应与显式或自动审批搭配。
//   - PTC（run_code）默认关闭；打开后模型编写的 Python/Node 程序通过系统已安装的
//     运行时执行，内层工具调用仍走同一个 Gate。
//   - **系统沙箱的 runner 由宿主注入**（Config.SandboxRunner，见 sandbox/osrunner）。
//     Config.Sandbox 只是三态意愿开关：nil 尽力而为、true 表示"我想要隔离"、
//     false 表示明确不要；kit **不会**自己探测、安装或选择后端——即
//     `Config.Sandbox=true` 也不能让一台没装沙箱的机器凭空多出围栏。想知道现在
//     到底有没有隔离、以及没有的话为什么没有，用 Agent.Preflight()（宿主可在
//     Config.SandboxUnavailableReason 里补上"为什么没有"的那句话）。
//   - SandboxGrants 默认**开启**（nil = 开）：默认工具集会注册
//     request_sandbox_access（模型为某个工具自己的凭据/缓存目录申请会话级可写授权）
//     与只读的 sandbox_status（报告当前可写面、已批准授权与剩余预算），但两者都是
//     discoverable 的、不进初始 active 集。默认门禁里 sandbox_status 是只读诊断、
//     自动放行；request_sandbox_access 不在只读白名单里，会被拒绝——放宽沙箱必须由
//     宿主显式配置审批（ApprovalAuto 或 Ask/Review/自定义 Gate），模型只能“申请”，
//     是否放行由人或宿主决定。关掉用 kit.Bool(false)。
//   - 默认 History 是进程内 session.Memory，按 SessionID 隔离；进程退出即丢失。
//
// # 个人客户端的两个默认（与"防自己人"无关）
//
// 本 SDK 的定位是**个人 agent 客户端**：单用户，所有数据都是用户自己的。因此默认
// 装配不做"防自己人"的出口清洗，而是把默认值放在"不弄坏用户自己的东西"上：
//
//   - **输出脱敏默认关闭**（Config.Redactor 为零值 nil）。工具结果、模型文本明文
//     直通，零开销。凭据脱敏只服务一条路径：用户**主动导出**日志/会话（见
//     secrets.RedactForExport 与 secrets 包文档）。显式设置 Config.Redactor 仍会
//     打开 agent 层的脱敏（能力保留，不是默认）。
//
//   - **保护路径默认开启**：kit 自建策略时（Config.Policy 为 nil）给每个根追加
//     workspace 元数据（.git 含 worktree gitdir 指针、.aws、.codex），并追加主目录
//     下**存在**的凭据位置（~/.ssh、~/.aws、~/.config/gh、~/.netrc）。命中保护
//     路径的写入会被文件工具/apply_patch 以及（挂接 sandbox/osrunner 时的）OS
//     profile 拒绝。要关掉用 Config.DisableProtection=true。
//
//     这是**防误伤**，不是沙箱：它不改变进程权限，也拦不住用户自己用外部编辑器
//     改这些文件。宿主显式提供 Config.Policy 时保护策略归宿主所有，kit 不追加。
//
//     迁移指引（v0.19.0 →）：`Config.ProtectWorkspaceMetadata` 已废弃且不再改变
//     行为——老代码继续设 true 不会有额外效果（保护本来就开），想**关掉**保护必须
//     改用 `Config.DisableProtection = true`；设 false 不会关闭任何东西。
//
// 与 agent 循环/工具装配的关系：kit.New 内部等价于构造 sandbox.Policy →
// tools/full.All → gate → session.NewMemory → agent.New，因此它只是把常见装配
// 收敛成一次调用；需要精细控制时可以按同样步骤手动组装。
package kit
