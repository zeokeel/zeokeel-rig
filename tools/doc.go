// Package tools 是 ZeoKeel Rig 的工具注册表：它把一组实现 rig.Tool 契约的工具
// 组织成 agent 调用的注册表（按名字查找、为模型生成确定顺序的工具规格、并发安全的
// 快照接口）。
//
// 工具实现分散在子包中，使用方只链接自己用到的工具：
//
//	tools/builtin   沙箱策略下的文件与 shell 工具
//	tools/editor    str_replace_editor（view/create/str_replace/insert）
//	tools/image     read_image（通过 ToolResult.Parts 返回多模态附件）
//	tools/terminal  有状态的持久 shell 会话（terminal_*）
//	tools/jobs      后台进程（job_*）
//	tools/web       web_fetch 与可插拔的 web_search
//	tools/mcp       MCP stdio 客户端以及 MCP tools/resources/prompts
//	tools/lsp       LSP stdio 客户端以及 lsp_* 工具
//	tools/misc      working_directory 与 todo_write
//	tools/full      一次性聚合全部静态工具的 All(opts)
//
// # 主要类型
//
//   - Registry：按名字索引、并发安全的工具集合，支持 Register/Unregister、
//     Invoke、Tools、Specs、Len。
//   - rig.Tool：单个工具契约（Name、Description、Schema、Invoke）。
//   - rig.ToolSpec：暴露给模型的工具规格；Registry.Specs 按名字排序后返回，
//     保证同一工具集合的请求内容稳定、可复现。
//   - rig.ToolResult：工具返回值，含 Content、IsError 以及可选的非文本附件 Parts。
//
// # 典型用法
//
// 注册表是“工具集合 → agent 可用工具”的组装点；可以直接构造，也可以交给
// tools/full 等聚合器：
//
//	import (
//		"context"
//
//		"github.com/zeokeel/zeokeel-rig"
//		"github.com/zeokeel/zeokeel-rig/agent"
//		"github.com/zeokeel/zeokeel-rig/tools"
//		"github.com/zeokeel/zeokeel-rig/tools/builtin"
//	)
//
//	registry := tools.NewRegistry(builtin.NewWithPolicy(policy)...)
//
//	// 把注册表里的工具交给 agent：agent 会用 Specs() 生成请求中的 tools 字段。
//	a, err := agent.New(rig.AgentSpec{
//		Provider: provider,
//		Model:    "gpt-4o-mini",
//		Tools:    registry.Tools(),
//	})
//	if err != nil {
//		return err
//	}
//
//	// 也可以不经过 agent，直接调用某个工具（参数是模型产出的原始 JSON 字符串）。
//	result, err := registry.Invoke(context.Background(), rig.ToolCall{
//		ID:        "call-1",
//		Name:      "read_file",
//		Arguments: `{"path":"README.md"}`,
//	})
//
// # 不变量与错误语义
//
//   - 注册表是编译期集合（工具被链接进二进制），除工具值本身外不持有状态，因此
//     Invoke/Specs/Register 等操作并发安全；工具自身要么无状态、要么内部同步
//     （见 rig.Tool）。
//   - NewRegistry 在工具为 nil、名字为空或重名时会 panic：在编译期注册路径上这
//     属于编程错误；动态组装的工具集合应改用 Register 并处理返回的错误。
//   - Invoke 把模型的 Arguments 原样传给工具（不重新编码）；未知工具名返回包装
//     rig.ErrToolNotFound 的错误。
//   - Specs/Tools 返回快照并按名字排序；返回值中的 schema map 是工具自身的值，
//     调用方不得修改（注册表是共享的）。
//
// 与 agent 循环的关系：agent 不依赖 Registry 的具体实现，只接收 []rig.Tool；
// Registry 负责把它们整理成模型请求里的工具声明，并在模型请求调用时按名字路由。
package tools
