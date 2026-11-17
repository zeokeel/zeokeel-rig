// Package local 是"OpenAI 兼容但不完全一样"的本地模型服务的共享基座。
//
// # 为什么需要它
//
// provider/openai 面向托管服务：默认 https、API key、严格的流终止标记。本地模型
// 服务（llama.cpp server、vLLM、Ollama、LM Studio、各种网关）说的是同一条协议，
// 但细节到处不同：
//
//   - 有的直接关连接，永远不发 `data: [DONE]`；
//   - 有的把 `finish_reason` 写成 `tool_call` / `max_tokens` / `eos_token`；
//   - 有的把 `tool_calls[].function.arguments` 发成 JSON **对象**而不是字符串，
//     或者只在最后一个事件里给整条 `message`；
//   - 有的把 `content` 发成 `[{"type":"text","text":"..."}]` 分片数组；
//   - 很多服务不需要也不应该收到 Authorization 头。
//
// 本包把这些差异收进 Quirks 与弹性解码里，对 agent 暴露的永远是一样的 rig.Chunk。
// 厂商封装（provider/ollama、provider/lmstudio）只负责默认地址、模型列表路径和
// 用户提示，不重复实现重试与解析。
//
//	// 通用本地服务（llama.cpp server 默认 8080）
//	p, err := local.New(local.Config{DefaultModel: "qwen2.5-coder"})
//
// # 与 provider/openai 的关系（同一份协议实现）
//
// **本包是协议本体**：请求体构造（BuildChatRequest）、带重试的 HTTP 传输
// （Transport）、SSE 分帧与终止判定（NewStream）、用量解析都在这里。provider/openai
// 复用它们，只保留托管服务特有的默认值与错误文案（见 openai 包文档）。
//
// 两者的解析差异用 Quirks 表达，而不是各写一份：
//
//	openai: Quirks{RequireDone: <Config.RequireCompleteStream>,
//	               StrictFinishReasons: true, StrictDeltas: true}
//	local : 零值（宽松：容忍缺失终止标记、finish_reason 变体归一化、message 回退、
//	        对象型 arguments、旧式 function_call）
//
// 四条差异有跨 provider 契约用例钉住（provider/openai/audit_cross_provider_test.go）。
//
// # 鉴权默认值
//
// 本地服务通常没有 key，因此本包**默认不发 Authorization**；APIKey 为空时读
// APIKeyEnv（默认 LOCAL_API_KEY，刻意不是 OPENAI_API_KEY：把用户的真实托管 key
// 发给 127.0.0.1 是凭据泄漏）。需要鉴权时显式配置 APIKey，或用 DisableAuth 强制
// 关闭。
//
// # 模型列表
//
// Models 用 GET {BaseURL}{ModelsPath} 拉列表，并把响应归一化成 []ModelInfo；
// ModelLister 与 Endpoint 是给宿主的"provider 选择 + 模型预设"用的钩子。
//
// # 已知边界
//
//   - 只支持流式（stream: true）；非 SSE 的 2xx 响应会直接报错。
//   - 工具调用依赖模型本身支持 function calling；不支持时用
//     Quirks.DisableTools 关掉 tools，避免服务端 400。
//   - 不做 token 计数与上下文裁剪：上下文窗口由服务端/模型决定，超限是服务端错误。
package local
