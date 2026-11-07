// Package lsp 是仅依赖标准库的 Language Server Protocol 客户端，以及构建在其上的模型可见
// 工具（lsp_hover、lsp_definition、lsp_references、lsp_implementation、lsp_diagnostics、
// lsp_symbols）与旧版合并工具 lsp。
//
// # 传输
//
// 客户端使用 LSP 的 stdio 传输：语言服务器作为子进程启动，JSON-RPC 消息以 Content-Length
// 头分帧：
//
//	Content-Length: 42\r\n
//	\r\n
//	{"jsonrpc":"2.0",...}
//
// 一个 goroutine 读取帧并按数字 ID 完成挂起请求；服务端通知（只消费
// textDocument/publishDiagnostics）被保存，服务端发起的请求会得到通用 JSON-RPC 回复。
// 入站消息上限 16 MiB；消息头在分配前以 64 KiB 字节预算读取（单行未终止的头会被拒绝，而
// 不是缓冲），因此恶意服务端无法用超大头部耗尽宿主内存。
//
// # 生命周期
//
//	client, err := lsp.Start(ctx, lsp.Config{Command: "gopls"})
//	defer client.Close()
//	if err := client.Initialize(ctx); err != nil { ... }
//	if err := client.DidOpen(ctx, uri, text); err != nil { ... }
//	hover, err := client.Hover(ctx, uri, line, character)           // 0 基的 LSP 位置
//	refs, err := client.References(ctx, uri, line, character)       // 引用
//	impls, err := client.Implementation(ctx, uri, line, character)  // 实现
//
// Initialize 完成 initialize 请求/响应握手并发送 initialized 通知，同时解析服务端在
// initialize 结果中声明的 referencesProvider/implementationProvider 能力；客户端只读取静态
// 能力声明，不支持动态注册。服务端未声明某个新能力时，对应请求会返回可读错误而不是被转发
// 出去。Shutdown、Exit 与 Close 幂等：Close 先做有界优雅尝试（shutdown、exit、等待进程
// 退出），之后才强杀子进程。取消传给 Start 的 ctx 会终止服务器并让挂起请求失败。
//
// # 模型可见工具
//
//   - lsp_hover {path, line, character}：符号的类型/文档信息。
//   - lsp_definition {path, line, character}：符号定义位置（一个或多个 path:line:column）。
//   - lsp_references {path, line, character}：符号的引用位置；请求包含声明本身
//     （includeDeclaration=true）。
//   - lsp_implementation {path, line, character}：接口方法或符号的实现位置。
//   - lsp_diagnostics {path}：语言服务器为该文件发布的诊断（错误/警告/提示），格式
//     "severity message (line:column)"；发布是异步的，因此是尽力而为（最多等待
//     DiagnosticsWait=500ms，可能暂时为空）。
//   - lsp_symbols {path}：文件的缩进符号树，格式 "kind name (line:column)"。
//   - lsp（旧版合并工具）{action: hover|definition|references|implementation|diagnostics|
//     symbols, path, line?, character?}：与 lsp_* 相同的客户端方法与格式化，供从旧工具面
//     迁移的宿主保持稳定工具名；action 还接受 DSH 风格别名（goToDefinition、
//     findReferences、goToImplementation）与下划线拼写。
//
// # 位置校验
//
// 位置类工具参数是 1 基，character 按 UTF-16 code unit 计数（LSP 原生位置是 0 基）。工具
// 会用 UTF16Len/ValidatePosition 按文档文本精确校验行号与列宽：行号必须在文档行数内；
// character 的合法范围是 1..UTF16Len(该行)+1（等于最后值时表示行尾）；BMP 之外的字符
// （emoji、部分 CJK 扩展）按两个 code unit 计。越界坐标返回可读的模型可见错误，且不会把
// 查询发给语言服务器。CRLF、CR 与 LF 都视为换行，文件末尾换行产生一个可定位的空行。
//
// 这组工具是只读的：不修改文件，只读取路径并通过 didOpen/didChange 同步文本。
//
// # 宿主接口、权限与限制
//
// 通过 Tools(client, filePathToURI, opts...) 构造六个 lsp_* 工具；LegacyTool 构造旧版合并
// 工具。client 为 nil 时调用返回配置错误。PathPolicy/WithPathPolicy 可在工具读取文件前校验
// 模型提供的路径（例如接入 sandbox.Policy.CheckPath），拒绝会作为模型可见工具错误返回；
// tools/full.All 在传入 Policy 时默认用 Policy.CheckPath 作为路径策略。客户端用
// exec.Command 启动语言服务器，不经过 OS 沙箱；审批在 agent 层由 rig.Gate 控制。
//
// 已实现：initialize/initialized、hover、definition（Location、Location[]、LocationLink）、
// references、implementation、documentSymbols（层级 DocumentSymbol 与扁平
// SymbolInformation）、publishDiagnostics、didOpen/didChange（全文同步）、shutdown/exit。
//
// 未实现（只读工具集的边界之外）：completion、rename、code actions、formatting、workspace
// folders/configuration、动态能力注册、其他传输（TCP、pipes）与重试。
package lsp
