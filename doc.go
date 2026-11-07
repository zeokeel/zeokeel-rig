// Package rig 是 ZeoKeel Rig 稳定的扩展契约面，也是本模块的根包。
//
// 第三方模块、模型 provider、工具、宿主或持久化存储所需的一切接口都在这里：
// 消息与流式分片、模型 provider 接口、工具接口、审批门禁、会话历史、agent 事件、
// agent 接口，以及宿主/存储契约（宿主交互、通知、记忆、技能、调度、会话日志、
// 评审、目标、团队任务板等，见 extensions.go）。本包刻意只依赖 Go 标准库：它是
// 导入图的叶子，无头的 Rig 二进制不会因为契约而引入 UI 框架、数据库驱动或 agent
// 框架（CI 会校验“根包仅标准库、且不导入本模块其他包”）。本模块的绝大多数包都
// 导入它；sandbox、tools/catalog、tools/audit 是刻意不导入它的叶子。
//
// # 主要类型与接口
//
//   - 消息与用量：Message、Role、ContentPart、Usage、ToolCall、ToolSpec、
//     NewMessage、TextPart、ImagePart。Message 支持 system/user/assistant/tool
//     四种角色、多模态内容、assistant 的工具调用，以及只用于展示、不参与回放的
//     reasoning 文本。
//   - 模型侧契约：Provider 是模型后端接口；Request/Stream/Chunk/ToolCallDelta
//     是流式传输契约（当前契约代只支持流式，确保任何地方都能观察部分输出）。
//   - 工具侧契约：Tool（Name/Description/Schema/Invoke）、ToolResult（Content/
//     IsError/Parts）、ToolLister。
//   - 审批：Gate 与 Decision（allow / allow-always / deny）。
//   - 会话：History（按 sessionID 的 Load/Append）。
//   - agent：Agent、RunRequest、RunResult、Event/EventType、AgentSpec、Logger、
//     Settings。
//   - 子 agent 编排：Subagent、SubagentRunner、SubagentSpec、SubagentResult。
//   - 宿主/存储扩展（extensions.go）：Asker、Presenter、FilePresenter、
//     PlanController、GoalStore/GoalUpdater、NoticeSource、MemoryStore、SkillStore、
//     Scheduler、SessionLog、Reviewer、TeamTaskManager。
//
// # 典型用法
//
// 根包只定义契约，不包含具体实现；组装 agent 时通常从 agent、provider/openai、
// tools/full 等包取值，再把它们当作 rig 类型使用：
//
//	import (
//		"context"
//
//		"github.com/zeokeel/zeokeel-rig"
//		"github.com/zeokeel/zeokeel-rig/agent"
//		"github.com/zeokeel/zeokeel-rig/provider/openai"
//		"github.com/zeokeel/zeokeel-rig/tools/full"
//	)
//
//	provider, err := openai.New(openai.Config{DefaultModel: "gpt-4o-mini"})
//	if err != nil {
//		return err
//	}
//	a, err := agent.New(rig.AgentSpec{
//		Provider: provider,
//		Model:    "gpt-4o-mini",
//		Tools:    full.All(full.AllOptions{Root: "."}),
//	})
//	if err != nil {
//		return err
//	}
//	result, err := a.Run(context.Background(), rig.RunRequest{
//		Input: []rig.Message{rig.NewMessage(rig.RoleUser, "你好")},
//	})
//	_ = result.Messages
//
// 自定义工具只需实现 rig.Tool；自定义模型后端只需实现 rig.Provider；持久化会话
// 只需实现 rig.History。三者都不要求导入本模块的其他包。
//
// # 与 agent 循环和工具装配的关系
//
// agent 循环（agent 包）只消费本包的接口：Provider 提供模型补全流，Tool 提供工具
// 声明与执行，History 保存会话消息，Gate 决定审批；装配器（tools/full 等）把各
// 工具包的实现整理成 []Tool 交给 AgentSpec.Tools，把各宿主/存储实现交给对应工具。
// 因此扩展能力不需要修改 agent 循环：实现对应契约并在装配层注册即可。
//
// # 不变量与语义
//
//   - 根包只依赖标准库，且不导入本模块其他包：任何层都可以安全地导入它。
//   - agent 可能并发执行同一批工具调用，因此 Tool 实现必须支持并发调用；
//     History 实现必须并发安全。
//   - Provider 可被并发调用；返回的 Stream 归调用方所有，Recv 必须保证同一时刻
//     只有一个 goroutine 调用，并在返回错误或 io.EOF 时释放资源。
//   - Gate 在 agent 运行循环的 goroutine 上、工具执行之前被调用，可以阻塞等待
//     用户输入；nil Gate 表示使用 agent 配置的默认策略（对受信工具通常是全部
//     放行）。
//   - 哨兵错误（见 errors.go：ErrProviderRequired、ErrModelRequired、
//     ErrToolNotFound、ErrMaxIterations）可用 errors.Is 判断；实现方可以包装，
//     但不得替换。
//
// # 版本与兼容性
//
// 模块遵循语义导入版本（semantic import versioning）。根包是当前契约代，在同一
// 模块大版本内只做加法式变更；v1.0.0 之后的破坏性契约变更通过导入路径上的新大
// 版本实现（例如 github.com/zeokeel/zeokeel-rig/v2），而不是在本模块内建立带
// 版本号的子目录。v1.0.0 之前，次要版本之间仍可能发生破坏性变更。Version 常量
// 给出当前模块版本，并与 release tag 保持同步（见 scripts/sync-version.sh）；
// 详细规则见 docs/VERSIONING.md、docs/ARCHITECTURE.md 与 COMPATIBILITY.md。
package rig
