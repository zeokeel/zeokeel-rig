// Package costmeter 做**会话级**的 token 与金额计量：把每次模型调用的
// rig.Usage 累加成一个可给 UI 直接渲染的 Summary，并支持预算告警。
//
// 定位是个人 agent 客户端（用户自己付钱）："这个会话花了多少、还有多少额度、
// 上下文用了多少 token"是产品面刚需，而不是运维指标。因此本包刻意保持**纯计算**：
//
//   - 只依赖 rig.Usage（不 import provider，也不碰 agent，因此可以在 UI 线程、
//     测试与回放脚本里自由使用）；
//   - 价格来自 modelprice 的内置表，也可以由宿主注入（WithPricer）；
//   - 金额用**整数纳美元**累加，逐次相加不产生浮点漂移。
//
// # 为什么是纳美元而不是微美元（**不要"优化"回去**）
//
// 内部累加器是 `int64` 纳美元（1e-9 USD）；Snapshot 里也存纳米字段
// `cost_nano_usd`。这不是过度设计，而是**唯一能算准**的单位：
//
//   - 便宜模型的单次调用远小于 1 微美元：deepseek-chat 输入 $0.27/M，1 token 只有
//     0.27 微美元。按微美元逐次四舍五入 → 每次得 0，**一千次 1-token 调用误差
//     100%**（有测试 TestCheapModelPrecision 钉住）。
//   - 纳美元仍然是整数：逐次相加没有浮点漂移，10 万次累加后依然精确到 1e-9 美元。
//
// 因此：
//
//   - 对外的 `Summary.CostUSD`（JSON `cost_usd`）是 `float64` 的**展示值**——
//     UI 要的是能直接打印的金额，不是整数；
//   - 持久化用 `Snapshot.CostNanoUSD`（`cost_nano_usd`）——**不要**换成微美元字段，
//     那是精度损失而不是简化；
//   - 需要更细粒度时只能加更细的整数单位，绝不能把累加器改成 float64。
//
// # 两个视图：ByModel 与 Models
//
// Summary 同时给出两种分组，回答两个不同的问题：
//
//   - `ByModel`（键 = 价目表规范名）：**"这个模型花了多少"**。`gpt-4o`、
//     `gpt-4o-2024-08-06`、`openai/gpt-4o` 会归并到 `gpt-4o`——UI 的"按模型
//     排行"就该用这个。
//   - `Models []ModelUsage`（键 = **调用时的原始名**）：**"这次会话具体用了哪个
//     版本、各几次"**。每项带 `Resolved`（解析到的规范名，未定价时为空）与
//     `Priced`。按 `Model` 字典序排列，序列化逐字节确定（见 TestSummaryGolden）。
//
// 两者都是派生视图，底层只有一份"原始名 → 累加器 + 规范名"的状态，明细求和恒等于
// 总计（有测试钉住）。
//
// UI 只想显示"这次花了多少"时，**只读一个字段**：`Summary.CostUSD`（JSON
// `cost_usd`）。`Estimated` 为 true 时在数字前加 "≈"，`UnpricedModels` 非空时
// 追加"部分模型未计价"。
//
// # 未定价模型
//
// 表里查不到的模型**不编造价格**：它照样计 token 与请求数，但成本记 0，并出现在
// Summary.UnpricedModels 里；只要出现过一个未定价模型，Summary.Estimated 就是
// true（总额不可信，UI 应当显示"≈ 且部分模型未计价"）。宿主想算准，就注入自己的
// 价目（WithPricer）。
//
// # 预算
//
// WithBudget(usd) 之后：
//
//   - Warned() 在成本达到预算的 WarnAt（默认 80%）时变 true，并**只**触发一次
//     WithWarnFunc 回调（Reset 之后可以再次触发）；
//   - Exceeded() 在成本 ≥ 预算（100%）时为 true。
//
// 预算只是一个**信号**，Meter 不会自己去中断 Run：什么时候停（还是继续但提示用户）
// 是宿主的产品决策。
//
// # 并发与持久化
//
// Meter 的方法都可以并发调用（内部一把 mutex）。Snapshot/Restore 用于跟随会话
// 持久化：Snapshot 是纯数据（JSON 友好），Restore 会先校验再整体替换，脏快照不会
// 破坏当前状态。
//
// 零值 Meter 可用（无预算、用内置价目表）；需要配置时用 New。
package costmeter
