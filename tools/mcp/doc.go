// Package mcp 实现一个零依赖的 Model Context Protocol（MCP）客户端，支持两种传输：
//
//   - stdio：与子进程之间以换行分隔的 JSON-RPC 2.0 消息（Config.Command）；
//   - Streamable HTTP：把 JSON-RPC 消息 POST 到远端端点，响应为单个 JSON 或 SSE 流，
//     会话/协议状态放在 Mcp-Session-Id、MCP-Protocol-Version 头中
//     （Config.Transport = TransportHTTP + Config.URL，或 StartHTTP）。
//
// 已实现的子集是工具型 agent 所需的部分：
//
//   - initialize / notifications/initialized 握手，依据 SupportedProtocolVersions 协商
//     协议版本并校验 server-info；
//   - tools/list，支持 cursor 分页与跨页去重；
//   - tools/call，映射 text、image、audio、resource 内容；
//   - resources/list + resources/read + resources/templates/list（cursor 分页、text/blob
//     内容、URI 模板）；
//   - prompts/list + prompts/get（参数、消息渲染）；
//   - ping；
//   - HTTP 服务端通知流（P2-7）：initialize 之后可用 Client.OnNotification
//     打开 GET + Accept: text/event-stream 的 SSE 流，解析 event:/data:/id:
//     帧并分发 JSON-RPC 通知（如 resources/list_changed、
//     tools/list_changed、prompts/list_changed）；断线后带 Last-Event-ID
//     重连恢复；Close 取消通知流并尽力发送 Mcp-Session-Id 的 HTTP DELETE；
//   - 并发调用、上下文取消、单请求超时与幂等 Close。
//
// # 模型可见工具
//
// 动态工具：Tools/NewTools 把一个 client 的每个工具适配为 rig.Tool，命名为
// <prefix><server>__<tool>（默认前缀 "mcp__"），因此 MCP server 的工具与 Rig 其他工具
// 行为一致：
//
//	client, err := mcp.Start(ctx, mcp.Config{Command: "my-mcp-server"})
//	if err != nil { return err }
//	defer client.Close()
//	agentSpec.Tools = append(agentSpec.Tools, mcp.Tools(client, "")...)
//
// 资源/提示工具：ResourceTools 把 resources/ 与 prompts/ 适配为五个模型可见工具（前缀原样
// 前置于每个名称）：
//
//	mcp_list_resources           {}                -> 每行一个资源
//	mcp_read_resource            {uri}             -> 资源文本；二进制内容只报告类型与大小
//	mcp_list_resource_templates  {}                -> 每个 URI 模板一项
//	mcp_list_prompts             {}                -> 每个 prompt 一项
//	mcp_get_prompt               {name, arguments?} -> prompt 消息渲染为文本
//
// 这些工具每次调用时才懒加载列表（ResourceTools 不做 I/O），nil client 会在调用时报告
// 配置错误。工具级失败通过 CallResult.IsError 返回；协议级失败返回 error（服务端错误为
// *RPCError）。
//
// # 远端 server
//
// 同一个 client 也支持远端：
//
//	client, err := mcp.StartHTTP(ctx, mcp.HTTPConfig{
//		URL:     "https://mcp.example.com/mcp",
//		Headers: map[string]string{"Authorization": "Bearer " + token},
//	})
//
// # 限制与边界
//
// 单请求默认超时 DefaultTimeout=30s；单条 JSON-RPC 消息上限 MaxMessageBytes=8 MiB；
// cursor 分页上限 maxListPages=1000 页；stdio Close 最多等待 closeGrace=2s 让子进程退出，
// 之后强杀。工具结果中 image/audio/resource 等非文本内容会渲染为占位说明，blob 字节不会
// 发送给模型。
//
// stdio 传输只实现客户端发起请求：服务端通知被忽略。HTTP 传输支持 GET SSE 通知流
// （见下），但未实现 OAuth；subscriptions、sampling/elicitation/roots 与服务端 MCP 也不
// 在范围内（通知流中的服务端→客户端请求会被忽略）。客户端可安全并发调用；Close 不可与
// 自身并发。
//
// # 通知流
//
// 远端传输在 initialize 之后按需打开通知流；未注册回调时不发 GET 请求，因此不支持
// 订阅的服务端保持兼容：
//
//	client.OnNotificationError(func(err error) { log.Printf("mcp notifications: %v", err) })
//	client.OnNotification(func(n mcp.Notification) {
//		switch n.Method {
//		case mcp.NotificationToolsListChanged:
//			// 使工具缓存失效
//		}
//	})
//
// 通知回调在通知流 goroutine 上同步执行，必须快速返回。GET 返回 405/501 表示服务端
// 不支持通知流：客户端报告一次错误后停止重连，普通请求/响应不受影响。连接中断后客户端
// 使用最近事件的 id 作为 Last-Event-ID 退避重连。
//
// # 权限
//
// stdio 传输会以 exec.Command 规则启动子进程，HTTP 传输会发起网络请求；两者都不经过
// sandbox.Policy（网络/进程访问由宿主部署与 rig.Gate 在工具层控制），只应在受信配置下
// 连接 MCP server。tools/full.All 只在传入 MCPClients 时才加入这些工具。
package mcp
