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
// 行为一致。工具**身份**是"连接 + 原始工具名"：同一条连接里同一个原始名重复列出只包装
// 一次；两条不同的连接即使声明同一个 ServerName 也各自保留，名字冲突用确定性后缀
// `__2`、`__3` 消歧（多连接聚合见 AggregateTools），因此名称冲突不会让 agent.New 因为
// duplicate tool name 而起不来：
//
//	client, err := mcp.Start(ctx, mcp.Config{Command: "my-mcp-server"})
//	if err != nil { return err }
//	defer client.Close()
//	agentSpec.Tools = append(agentSpec.Tools, mcp.Tools(client, "")...)
//
// 资源/提示工具：ResourceTools 把 resources/ 与 prompts/ 适配为五个模型可见工具，名字
// 与普通工具同一条规则 —— <prefix><server>__<base>（默认前缀 "mcp__"，例如
// `mcp__srv_a__list_resources`；server 来自 client.ServerName()，清洗规则与 Tools 一致）：
//
//	list_resources           {}                -> 每行一个资源
//	read_resource            {uri}             -> 资源文本；二进制内容只报告类型与大小
//	list_resource_templates  {}                -> 每个 URI 模板一项
//	list_prompts             {}                -> 每个 prompt 一项
//	get_prompt               {name, arguments?} -> prompt 消息渲染为文本
//
// 名字里的 server 维度是必需的：旧名（`mcp_list_resources` 等）配两个 server 时逐字
// 重复，宿主只能靠上层改名兜底，同名 server 的两个连接更是无法区分。旧名**不保留**。
// 名字可以用 ResourceToolName / ResourceToolNames 直接算出来；多连接聚合用
// AggregateTools（ServerTools.Resources = true 会把资源工具并入同一个名字空间并消歧）。
//
// 这些工具每次调用时才懒加载列表（ResourceTools 不做 I/O），nil client 会在调用时报告
// 配置错误。工具级失败通过 CallResult.IsError 返回；协议级失败返回 error（服务端错误为
// *RPCError）。
//
// # 宿主侧命名与旧名迁移
//
// 宿主若要复刻模型可见工具名（disabledTools 白名单、持久化允许清单、UI 展示），用
// SanitizeName 而不是自己写清洗规则：它就是 Tools/ResourceTools 生成名字时用的那条
// 规则（[A-Za-z0-9_-]，其余字符变下划线、首尾下划线去掉、空回退 "tool"）。
//
// v0.24.0 之前的资源/提示工具名（`mcp_list_resources` 等）没有 server 维度，配两个
// server 时逐字重复，因此被改名为 `mcp__<server>__<base>` 且旧名不保留。旧配置用
// MigrateLegacyNames(names, server) 迁移：给定 server 时五个旧名映射成新名，
// server 为空或名字无法识别时原样进 needsUser，由宿主提示用户补 server 或重选，
// 不会静默丢配置。
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
// # OAuth 授权（远端 server）
//
// 需要 OAuth 2.1 的远端 server 用 NewOAuthClient 构造授权客户端（协议实现见
// 本包 oauth*.go）：
//
//	oauth, err := mcp.NewOAuthClient(mcp.OAuthConfig{
//		ServerName: "files",                       // TokenStore 的稳定 key
//		Resource:   "https://mcp.example.com/mcp", // canonical resource URI
//		TokenStore: store,                         // 宿主持久化（rig 不写文件）
//		OpenBrowser: func(url string) error {       // 宿主 UI 线程打开浏览器
//			return openURL(url)                     // 宿主实现（rig 不依赖 UI）
//		},
//	})
//	client, err := mcp.StartHTTP(ctx, mcp.HTTPConfig{URL: "https://mcp.example.com/mcp", OAuth: oauth})
//
// 交互式授权是两阶段的（宿主需要控制浏览器打开时机与 UI 状态）：
//
//	flow, err := oauth.Prepare(ctx)   // 发现 + 注册 + PKCE + 回环监听（阻塞，放 goroutine）
//	// 在 UI 主线程打开 flow.AuthURL；展示“等待浏览器授权”
//	status, err := oauth.Wait(ctx, flow) // 等回调 + 换 token + 持久化（阻塞，放 goroutine）
//
// oauth.Status() 是只读快照（无 I/O），oauth.Authorize(ctx) 是
// Prepare+OpenBrowser+Wait 的 CLI 便捷入口；refresh/revoke 分别对应
// oauth.Refresh/oauth.Revoke。HTTP 传输的每个请求由 OAuth 客户端注入 Bearer，
// 401 时单飞刷新并重试一次。
//
// 安全边界：PKCE 必须 S256（AS metadata 未声明时拒绝）；本地只监听 127.0.0.1；
// 校验 Protected Resource Metadata 的 resource 与 AS metadata 的 issuer，回调校验
// state 与 RFC 9207 的 iss；远程端点必须 https（回环地址仅为本地测试/开发放行）。
// 令牌记录由本包序列化并经 TokenStore 持久化；TokenStore 必须按机密数据处理
// （0600、不得进入日志/诊断/导出），错误与日志不回显令牌。MCP 请求只跟随同源
// 重定向，绝不把 Bearer 发往其它 origin；403 insufficient_scope 只记录为错误码，
// 不自动做 step-up 授权。CIMD（Client ID Metadata Documents）尚未实现：桌面
// 客户端使用预注册 client_id 或 RFC 7591 动态注册。
//
// # 限制与边界
//
// 单请求默认超时 DefaultTimeout=30s；单条 JSON-RPC 消息上限 MaxMessageBytes=8 MiB；
// cursor 分页上限 maxListPages=1000 页；stdio Close 最多等待 closeGrace=2s 让子进程退出，
// 之后强杀。工具结果中 image/audio/resource 等非文本内容会渲染为占位说明，blob 字节不会
// 发送给模型。
//
// stdio 传输只实现客户端发起请求：服务端通知被忽略。HTTP 传输支持 GET SSE 通知流
// （见下）与 OAuth 2.1 授权（见上）；subscriptions、sampling/elicitation/roots 与服务端
// MCP 仍不在范围内（通知流中的服务端→客户端请求会被忽略）。客户端与 OAuthClient 可安全
// 并发调用；Close 不可与自身并发。
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
