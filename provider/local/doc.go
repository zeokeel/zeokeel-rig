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
// # 与 provider/openai 的关系
//
// 两者是并行实现，说同一条协议。openai 保留给托管服务（它有意严格：非 SSE 直接
// 报错、必须 finish_reason 或 [DONE] 才算完整）；local 面向本地服务（宽容：缺失
// 终止标记视为正常结束、未知字段忽略、工具调用写法归一化）。两者的 rig.Stream
// 契约一致：Recv 返回分片，最后返回 io.EOF；错误用 errors.Is 可分类。
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
