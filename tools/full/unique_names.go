package full

import (
	"strconv"

	"github.com/zeokeel/zeokeel-rig"
)

// uniquifyToolNames 让一组工具的名字在**这一组内**唯一：首次出现的名字保持原样，
// 后续同名项改成 `<name>__2`、`<name>__3`…（确定性：只取决于输入顺序）。
//
// 为什么要在 full 这个边界收敛：agent.New 对重名是硬错误（宿主应用直接起不来），
// 而 server 绑定工具的名字不是 full 能完全控制的：
//
//   - 两个 MCP client 可以声明同一个 ServerName（同一个 server 配两次，或两个 server
//     恰好同名）—— 它们各自是独立连接，工具也确实都要暴露；
//   - MCP 的资源/提示工具（mcp_list_resources、mcp_read_resource、
//     mcp_list_resource_templates、mcp_list_prompts、mcp_get_prompt）名字里**完全
//     不含 server 身份**，多 client 时必然逐字重复。
//
// mcp.AggregateTools 只能解决"server 名不同、清洗后相同"的情况，而且会把"两个 client
// 声明同一个 server 名"当成同一个工具丢掉其中一个；它也管不到资源/提示工具。所以在
// 装配边界统一消歧一次，宿主拿到的工具集一定可注册，且名字在同样的输入下总是相同
// （对 prompt 前缀缓存友好）。
//
// 只用于 server 绑定工具：本地工具重名是装配 bug，应该让测试/agent.New 直接报出来，
// 不该被静默改名。
func uniquifyToolNames(tools []rig.Tool) []rig.Tool {
	if len(tools) == 0 {
		return tools
	}
	taken := make(map[string]struct{}, len(tools))
	out := make([]rig.Tool, 0, len(tools))
	for _, tool := range tools {
		if tool == nil || tool.Name() == "" {
			continue
		}
		name := tool.Name()
		if _, exists := taken[name]; !exists {
			taken[name] = struct{}{}
			out = append(out, tool)
			continue
		}
		for suffix := 2; ; suffix++ {
			candidate := name + "__" + strconv.Itoa(suffix)
			if _, exists := taken[candidate]; exists {
				continue
			}
			taken[candidate] = struct{}{}
			out = append(out, &renamedTool{Tool: tool, name: candidate})
			break
		}
	}
	return out
}

// renamedTool 是"只换名字"的工具包装：只用 Name() 返回消歧后的名字，Description/
// Schema/Invoke 全部由嵌入的 rig.Tool 转发。
//
// 注意：它不转发可选的附加接口（rig.NoticeSource 等）——类型断言对 *renamedTool 不
// 成立。当前只用于 MCP 工具，它们不实现这些接口；将来若有 server 绑定工具实现
// NoticeSource，需要在这里补齐转发。
type renamedTool struct {
	rig.Tool
	name string
}

// Name 实现 rig.Tool。
func (t *renamedTool) Name() string { return t.name }
