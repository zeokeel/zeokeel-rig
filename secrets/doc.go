// Package secrets 对文本、JSON、字节流与流式 I/O 做凭据脱敏（redaction）。
//
// agent 的工具输出、日志与审计事件里出现 API key、Bearer token、私钥是常态：
// 模型会把它们读进上下文、宿主会把它们写进日志。本包提供一条廉价的出口清洗：
// 用一个并发安全的 Redactor 把已知明文与常见凭据形态替换成 [REDACTED] 或
// [REDACTED:NAME]。
//
// # 只做模式匹配，不做高熵检测
//
// 本包**刻意不做**通用"高熵检测"（entropy heuristics）：不做信息熵/字符分布打分、
// 不做"看起来像随机串就脱敏"。原因是高熵启发式在真实输出上误杀率很高（base64
// 图片、哈希、UUID、压缩数据、中文 base64 都会中招），而且成本随输入长度上升。
// 这里只有两类判定：
//
//   - 已知明文：宿主通过 New / Add / AddNamed 登记的当前凭据（例如正在使用的
//     API key），按字面量替换，与长度无关（≥8 字节才登记，8..15 字节也照样替换）。
//   - 内置模式：一组稳定的凭据前缀/形态正则（sk-、ghp_、AKIA、JWT、PEM/PGP 私钥块、
//     Bearer/Basic、URL userinfo、key=value 等），见包内 rules 表。Bearer/Basic 这类
//     "关键字 + 任意词"的模式会先做形态判定（Basic 要求 base64 的 "user:password"，
//     Bearer 要求含数字/符号或足够长），因此不会把 "basic configuration" 这种散文
//     抹掉。
//
// 代价是"从未见过的自定义凭据形态"可能漏判；配方是：宿主把当前凭据登记为已知
// 明文，模式表负责常见第三方凭据。
//
// # 典型用法
//
//	r := secrets.New(os.Getenv("OPENAI_API_KEY"))
//	r.AddNamed("github", os.Getenv("GITHUB_TOKEN"))
//
//	cleaned := r.Redact(toolOutput)              // 文本
//	payload, err := r.RedactJSON(rawJSON)        // JSON（结构保留，只改敏感字符串）
//	io.Copy(dst, r.Reader(src))                  // 流式读取
//	w := r.Writer(dst)                           // 流式写入
//	w.Write(chunk1); w.Write(chunk2)
//	w.(secrets.Flusher).Flush()                  // 输出被扣留的尾巴
//
// 零值 Redactor 可用。Redactor 自身的方法（Redact/RedactBytes/RedactJSON/Add/
// AddNamed/Empty/Writer/Reader）都可以并发调用，Add 之后立即对所有正在进行的读取
// 生效。注意 Writer/Reader **返回的流对象**带可变状态，按 io.Writer/io.Reader 的
// 惯例不是并发安全的：多个 goroutine 写同一个 Writer 必须由调用方串行化。
//
// # JSON 语义
//
// RedactJSON 只改字符串**值**：键名、空白、键序、数字/布尔/null 逐字节保留。敏感
// 键（password/token/api_key/secret/pwd/pass/pin 等）的整个值都会被置为占位符，
// 包括数组里的每个元素与嵌套对象里的每个字符串值；已经是占位符的值与 `${VAR}` 这类
// 模板引用保持原样（幂等、不破坏配置模板）。
//
// # 已知边界
//
// 下面这些是刻意的取舍，接线方必须知道：
//
//   - 内置模式全部是**单行**规则（流式层靠这一点保证 token 不会被行边界切开），
//     因此 YAML 的 `password:` 与缩进值分居两行、或跨行拼接的凭据不会被匹配；
//   - 已知明文若是字面量（含换行）只能被非流式 Redact/RedactJSON 完整匹配；流式
//     Writer/Reader 逐块处理，跨块的已知明文可能不被替换；
//   - 流式路径为未决缓冲设了 MaxBuffered（64 KiB）上限：超过它的单条凭据可能被强制
//     冲刷切开（非流式路径没有这个限制）。**副作用是延迟**：一条没有换行、远超
//     64 KiB 的输出不会实时出现，而是攒到上限后成块吐出（尾部窗口仍会扣留）。因此
//     处理超大输出时的建议是"**先整体 Redact，再写入**"：
//     `dst.Write([]byte(r.Redact(hugeText)))` 没有分块边界问题，也没有延迟。
//   - **数字型秘密不会被脱敏**：敏感键下的数字/布尔/null 一律保持原值。这是刻意
//     取舍——把 `{"pin":123456}` 改写成字符串 "[REDACTED]" 会破坏调用方的 JSON 类型
//     契约（下游按 schema 解析直接失败），改写成 0 又会改变语义。**因此数字型凭据
//     请改用字符串承载**，或在上游序列化前换成占位符；`Add` 只对**文本/字符串**形态
//     生效（它按字面量匹配文本，不会进入 JSON 数字字面量），登记数字没有效果。
//   - 不做熵检测：URL 编码、base64 包装、零宽字符插字、以及未列入模式表的前缀
//     （glpat-、hf_、pypi- 等）不会被识别，需要宿主 Add 登记。
//
// # 性能
//
// 成本与输入长度成线性，常数由"命中多少个模式"决定（正则引擎是主要开销）。本机
// （Xeon 5220R）实测：1 MiB 无秘密文本约 35ms；1 MiB "每 24 字节一个 token=" 的
// 高命中文本约 355ms；16 MiB 同形态约 3.6s——16 倍输入约 10 倍时间，确认不是
// O(n²)，但常数不小。无秘密文本的 Redact 是 0 分配（每条规则都带字面量前置检查，
// 没命中就不进正则），因此把它放在每条工具输出的出口是可行的；对 >8 MiB 的输出
// 建议流式处理或先截断，避免单次调用吃掉数百毫秒。
//
// # 流式语义
//
// Writer/Reader 只把"完整行"立刻输出，扣留最后一个未完成行；内置模式全部只在
// 单行内匹配，因此一条被切成多个 chunk 的 token 不会被切断泄露（私钥块是唯一跨行
// 的模式，另外按 BEGIN/END 整块扣留）。没有换行的超长单行按 MaxBuffered（64 KiB）
// 强制冲刷并保留同样大小的尾部窗口，因此未决缓冲上界就是 64 KiB，且不超过 64 KiB
// 的凭据依然不会被切断。写完最后一块后必须 Flush（或 Close）才能看到被扣留的尾巴。
package secrets
