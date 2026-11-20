// Package events 定义 ZeoKeel Rig 的**版本化事件线格式**（NDJSON）。
//
// rig 的 agent 运行时通过 rig.Event 在进程内广播可观察步骤；那是 Go 结构体，
// 没有线格式也没有版本号，第三方前端、日志管道与回放工具无法稳定消费它。
// 本包把"事件长什么样"从 Go 类型里剥离出来，成为一份可以独立演进、可被任何
// 语言实现的契约：
//
//	{"version":1,"seq":1,"time":"2026-11-17T12:00:00Z","kind":"message","payload":{...}}
//
// # 线格式
//
// 一行一条事件（NDJSON，UTF-8），行以 \n 结束：
//
//   - version：线格式版本，见 Version。不兼容变更必须 +1。
//   - seq：单 emitter 内从 1 开始单调递增，可用于检测丢失与排序。
//   - time：UTC，RFC3339Nano（秒以下有值时保留，无值时省略）。
//   - kind：稳定的事件类型字符串，见 Kind* 常量。
//   - payload：事件体，原始 JSON。未知 kind 时消费方应原样透传而不是报错。
//
// 字段顺序、零值省略规则与时间格式都由字节级快照（testdata/stream.ndjson）
// 钉死；同一输入序列化后字节确定（payload 都是结构体，不依赖 map 顺序）。
//
// # 兼容性承诺
//
//   - 新增 kind 是兼容变更：旧消费方忽略未知 kind 即可（Decoder 不会因此报错）。
//   - 新增字段是兼容变更：消费方必须忽略不认识的字段（本包解码时即如此）。
//   - 改名、删字段、改变语义、改变 payload 编码是不兼容变更，必须 Version+1。
//   - 读到比本包更新的 version 时 Decoder 返回 ErrUnsupportedVersion，绝不
//     "尽力解释"——猜错字段比拒绝服务更糟。
//
// # 内置 payload
//
// 每种内置 kind 的 payload 形状定义在 events.go（字段名与零值省略规则是线格式
// 契约，见该文件的类型注释）：
//
//   - KindRunStart → RunStart；KindRunEnd → RunEnd
//   - KindMessage → Message；KindMessageDelta/KindReasoning → MsgDelta
//   - KindToolCall → ToolCall
//   - KindToolResult → ToolResult：Content 是模型看到的文本，Data 是宿主的
//     结构化结果原样转发的 JSON（json.RawMessage，可选——回放/UI 不必再解析
//     Content 的人读文本）
//   - KindToolActivated → ToolActivated
//   - KindApprovalRequest → ApprovalRequest；KindApprovalResponse → ApprovalResponse
//   - KindDiff → Diff；KindError → Error
//   - KindNotice/KindSandboxEscalation → 自由 payload（kit 写 {"message": ...}
//     与提权摘要），本包不定义具体类型，消费方按 kind 自行解释
//
// # 边界
//
// 本包只负责**序列化与解析**：不提供订阅、缓冲、过滤、背压、脱敏，也不依赖
// rig 的任何类型（便于第三方单独引入）。写到不可靠的 io.Writer 上时由调用方
// 负责重试与持久化；Emitter 只保证"一行原子写出"与"写失败后不再假装成功"。
package events
