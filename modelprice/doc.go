// Package modelprice 内置一张"每百万 token 的美元价"表，并提供一个保守的查找函数，
// 用来给会话成本估值。
//
// 它的定位是**UI 估算**，不是账单：个人用户自己付钱，所以"这次会话大概花了多少"
// 必须有数；但价格表天然会过时，因此：
//
//   - 表里的云端条目一律标 Estimated=true，输出时必须能标注"估算"；
//   - 表里查不到的模型返回 ok=false，**绝不猜价**——调用方可以按 0 处理，或用自己的
//     配置覆盖（见 costmeter.WithPricer）；
//   - 本地推理（Ollama/LM Studio/vLLM 等）没有 API 计费，价格是事实上的 0，因此
//     Estimated=false（"0 不是估算，是事实"）。
//
// # 查找规则
//
// Lookup 按以下顺序判定（全部大小写不敏感、去首尾空白）：
//
//  1. 本地 provider 前缀（ollama/、lmstudio/、vllm/、llamacpp/、llama.cpp/、mlx/、
//     local/）→ 0 价；
//  2. 精确命中表内条目；
//  3. 前缀命中表内条目，但**必须在分隔符处断开**（`-`、`.`、`:`、`@`）：
//     `gpt-4o-2024-08-06` 命中 gpt-4o，而 `gpt-4oz` 不会命中 gpt-4；最长前缀优先，
//     因此 `gpt-4o-mini-…` 命中 mini 而不是 4o；
//  4. `provider/model` 形式：剥掉第一段再重试（`openai/gpt-4o`、`anthropic/…`），
//     并把 provider 记进结果；多段（`vendor/org/model`）会逐段剥离；
//  5. Ollama 风格的 `name:tag`（`llama3.1:8b`）且 family 是已知开源权重家族 → 0 价；
//  6. 其余 → ok=false。
//
// # 加条目的硬规则：便宜/昂贵兄弟型号必须显式建条目
//
// 前缀匹配只保证"在分隔符处断开"，它**不能**区分同族的兄弟型号。典型事故：
// `o3-mini`（$1.10/$4.40）会命中 `o3` 的前缀（$2/$8），金额高估约 1.8 倍——审计
// 2F 实测过。因此：
//
//   - 新增任何子型号（`-mini`、`-nano`、`-flash`、`-pro`、带日期后缀除外）时，
//     **必须同时加一条精确条目**，不能指望前缀兜底；
//   - 只带**日期/版本后缀**的别名（`gpt-4o-2024-08-06`）可以靠前缀命中，因为价格
//     与父型号相同；
//   - 表条目数有下限守护（`TestTableSizeAtLeast30`），删表会让测试红。
//
// # 未收录但已知在售的模型（待核实，绝不编价）
//
// 下面这些名字可能在用户配置里出现，但本包**没有可信出处**，因此刻意不收录——
// Lookup 会返回 ok=false，costmeter 会把它记成未定价（成本 0 + Estimated=true +
// 进 UnpricedModels），UI 应当提示"部分模型未计价"。补条目时必须先拿到官方价目页：
//
//   - Anthropic：`claude-haiku-4-5`、`claude-sonnet-4-5`、`claude-opus-4-1`
//   - Google：`gemini-2.5-pro`、`gemini-2.5-flash`
//   - xAI：`grok-4`
//   - Moonshot：`kimi-k2`（官方已公告 2026-12 底退市）、`kimi-k3`、`kimi-k3.1-pro`
//
// 另外两个已知维度问题（不是 bug，但会影响精度）：
//
//   - **峰谷/地域/批量价**：DeepSeek 分峰谷（谷时半价）、阿里云分地域与批量折扣。
//     表里只能存单值，统一取**较高**的公开档（宁高不低），并在 Note 里写明取哪一档；
//   - **cached 价未公布**的条目（如 qwen3.8-max）留 0，costmeter 会按输入价计，
//     同样是宁高不低。
//
// # 价格表来源与更新策略
//
// 数字取自各厂商公开的 list price（USD / 1M tokens），采集时间见 Price.Updated
// 字段与包变量 TableUpdated。价格会变，**不随版本发布自动更新**：改这张表等于改
// 一个对外承诺，必须由人显式改、并在 CHANGELOG 里记一行。因此表内的所有云端价
// 都带 Estimated=true，UI 应当据此显示"≈"。
//
// 缓存价（CachedInputPerMillion）为 0 表示"该模型没有公开的缓存折扣"：costmeter
// 在这种情况会把 cached token 按普通输入价计费（宁高不低，避免把成本算少）。
package modelprice
