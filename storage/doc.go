// Package storage 是「一个库装一份会话状态」的**聚合契约**：宿主只注入一个值，
// 就能把会话历史、检查点、记忆与溢出结果四类状态一次交给 SDK。
//
// # 为什么必须是新包
//
// 聚合契约不能放根包 `rig`：根包受 stdlib-only 门禁约束
// （internal/rigdoc/modularity_test.go 的 TestRootPackageStaysStdlibOnly 硬拦），
// 而本包要引用 `rig` 的契约与 `memory.Wiki`。反过来，本包也不能把 `session/store`
// 拉进来——那是 layer 2，见下面的分层铁律。
//
// # 分层铁律
//
//	layer 0  根包 rig                        stdlib only，最小契约叶子
//	layer 1  storage（本包）                 stdlib + 本仓 rig / memory，聚合契约
//	layer 2  session/store、session、internal/memwiki   零依赖实现
//	layer 3  子模块 session/sqlite           modernc.org/sqlite，参考实现
//	layer 4  宿主                            只写业务逻辑
//
// 由此推出本包两条不可越过的约束，且都由测试守住：
//
//   - 本包**不得** import `session/store` 或 `session/sqlite`（layer 1 依赖 layer 2
//     会成环，并把只想拿契约的宿主拖进整棵子树）。见 TestStorageDoesNotImportSessionStore。
//   - 本包**不得**被根包 `rig` 引用（否则根包立刻违反 stdlib-only）。这一点由
//     internal/rigdoc 的守护测试间接保证：本包不在根包的依赖图里。
//
// 因此「把 *store.Dir 包成本契约」的适配器**不放这里**，它归 `session/store` 侧
// （store 不 import storage，方向正确、无环）。
//
// # 边界：什么归 SDK、什么归宿主
//
// 判据是 PLATFORM §0 的那一句：**「这份数据丢了，agent 这个回合还能不能正确继续？」**
//
//   - 不能 → 必需：会话/消息、检查点、溢出结果。SDK 必须提供存储与恢复路径。
//   - 能，只是体验差 → 可选：事件、花费、记忆、检索。记忆下一回合可以重建，
//     事件写失败按设计不该打断 Run。
//
// 这个区分直接决定接口形状：必需能力进主接口 Storage（实现方必须全给），
// 可选能力**将来以独立的小接口出现**（例如事件源、花费源、会话列表、租约），
// 由实现方按需满足，宿主按需类型断言。不要把可选能力塞进主接口——那样第二个
// 实现（目录后端、内存后端）就永远不达标。
//
// # 当前范围：只读五方法起步
//
// 主接口只声明「已有实现已经具备」的方法，否则验收永远不可能绿：
//
//	History() rig.History            会话消息读写
//	Checkpoints() rig.CheckpointStore 检查点（崩溃恢复）
//	Memory() memory.Wiki             长期记忆 wiki（可选能力，但两实现都已有）
//	Spill() rig.SpillStore           溢出的大工具结果
//	Close() error                    收尾
//
// 已知**尚未**进入契约的三处缺口（PLATFORM §3，不要在后续批次之外自行补方法）：
//
//   - SessionLog：各实现自有接口的方法集与 rig.SessionLog 不一致，需先统一返回类型；
//   - 事件写入：目前只有 kit.Config.EventWriter（NDJSON 导出通道），还没定义
//     「事件写失败算不算 turn error」——语义未定前不进契约；
//   - 追加签名：两实现都只有单条 Append，契约里一律用已有的
//     AppendBatch(ctx, id, ...rig.Message)，**不要**再定义一个变参 Append。
//
// # 为什么不用 embedding 拼契约
//
// 现有契约里 Load 出现 4 次、Save/Append 各 3 次、Search/Delete 各 2 次，
// 同名不同签名的方法无法在同一个接口里共存。所以 Storage 只能是**显式写出来的
// 五个访问器**，逐项声明归属，不能靠嵌入已有接口凑出来。
//
// # 使用方式
//
// 契约是「得到句柄」，不是「由 SDK 构造」。宿主拿到实现后把四个视图分别注入
// 运行时（agent.AgentSpec 的 History / CheckpointStore / Spill、工具层的记忆 wiki），
// 并在进程收尾时调用一次 Close：
//
//	st, err := sqlite.Open(sqlite.Options{Path: statePath}) // 子模块，参考实现
//	if err != nil {
//		return err
//	}
//	// …… 把 st.History() / st.Checkpoints() / st.Spill() / st.Memory() 注入运行时
//
// 注意：**先收尾回合、后台任务与事件，再 Close**。正在跑的回合若在 Close 之后
// 继续写库，会拿到「已关闭」错误并把一个正常回合判成失败（用户看到「回答到一半
// 消失」）。面向宿主的统一关停入口（Shutdown）是后续批次的内容。
package storage
