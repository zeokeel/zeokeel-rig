package secrets

// 导出用的便捷入口（个人客户端定位）。
//
// 本包**不是**运行时默认能力：个人 agent 客户端是单用户、数据都是用户自己的，
// 模型看工具输出、宿主写本地日志都不需要"防自己人"。本包只服务一条路径——用户
// **主动**把会话/日志导出、上报或分享给别人时，洗掉里面的凭据。
//
// 因此 kit 的默认装配不再调用本包（工具输出明文直通）；需要时调用方自己调
// RedactForExport 或显式注入 Redactor。

// RedactForExport 清洗一段**要离开本机**的文本：把内置模式能识别的凭据形态
// （sk-、ghp_、AKIA、JWT、PEM/PGP 私钥块、Bearer/Basic、URL userinfo、key=value
// 等）与调用方给的已知明文（known，例如当前会话用到的 key）替换成占位符。
//
// 语义与 Redactor 完全一致（同样的规则表、同样不做熵检测），只是省掉了"先建实例
// 再登记"的样板：每次调用新建一个一次性 Redactor，因此不需要考虑并发与状态残留。
// 代价是每次调用都会重新走一遍规则表，**不要**把它放进逐 token 的热路径；导出是
// 低频操作，这也正是本包现在唯一的定位。
//
// 空文本返回空串；known 里空串、短于 8 字节、与占位符重叠的值被忽略（同 Add）。
//
// 已知边界（与 Redactor 相同，导出前请记住）：
//   - 只做模式匹配，不做高熵检测：没进规则表、也没被 known 登记的自定义凭据会漏；
//   - 内置模式全是单行规则：跨行拼接的凭据不会被识别；
//   - 数字型凭据（JSON 里的数字字面量）不会被改写。
func RedactForExport(text string, known ...string) string {
	if text == "" {
		return text
	}
	return New(known...).Redact(text)
}
