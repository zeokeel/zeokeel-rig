package storage

import (
	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/memory"
)

// Storage 是「一个库装一份会话状态」的最小必需面：宿主只注入这一个值，就能把
// 会话历史、检查点、记忆与溢出结果四类状态一次交给 SDK。
//
// # 形状从哪来
//
// 五个方法都已在现有实现里存在（用编译期断言实测）：`*sqlite.Store`
// 直接满足；目录后端 `*store.Dir` 只有会话落盘能力，需要一个**适配器**把它包成本
// 契约，而适配器归属 `session/store` 侧——本包不得反向依赖 layer 2。
//
// 契约刻意只声明**只读五方法**起步：四个访问器各自返回一类状态的句柄（「由谁实现」
// 交给返回的接口去管），唯一的写操作是 Close。写路径（事件入库、花费结算、租约）
// 要等语义定清（例如「事件写失败算不算 turn error」）再以独立能力接口加入。
//
// 主接口**不放可选能力**。事件、花费、会话列表、租约这些东西是「宿主付了配置成本
// 才拿到」的，应该做成独立的小接口让实现方按需满足；一旦塞进主接口，第二个实现
// （目录后端、内存后端）就永远不达标。
//
// # 实现者必须保证
//
//   - 四个访问器**并发安全**，并**原样返回**注入的同一个实例：宿主侧栏、诊断与
//     「我传进去的那个后端就是被用的那个」这一直觉都依赖它。
//   - 访问器**不返回 nil**。某类能力确实不存在时返回空实现（例如空 wiki、空 Spill），
//     而不是 nil——nil 会让调用方在上游某处 panic，错误现场离真正原因很远。
//   - Close 幂等，且把后端的真实错误透出（宿主据此判断收尾是否成功）。
type Storage interface {
	// History 返回会话消息的读写入口（rig.History：Load 读取、Append 追加）。
	//
	// 能力类别：**必需**（判据：消息是回合的上下文，丢了本回合就无法
	// 正确继续）。
	//
	// 归属：layer 2 的会话实现（内存 session.Memory、目录 store.Dir、session/sqlite）。
	// Agent 每轮会自行 Load/Append，宿主**不需要**替它搬运消息；拿这个句柄是为了读
	// 历史（渲染、导出）或做诊断。
	History() rig.History

	// Checkpoints 返回检查点存储（rig.CheckpointStore：Load / Save / Clear）。
	//
	// 能力类别：**必需**——检查点是崩溃恢复的唯一依据：回合中途进程没了，下一轮
	// 要靠它把「已经发出的工具调用」补回来，否则历史会留下一个永远等不到结果的空洞。
	//
	// 归属：agent 侧只读依赖 Load；Save 由 agent 在 requested / completed /
	// tool_pending 三个位置调用，所以实现方必须同时给读与写。
	Checkpoints() rig.CheckpointStore

	// Memory 返回长期记忆的 wiki（memory.Wiki：Index / Read / Write / Append /
	// Delete / Search）。
	//
	// 能力类别：**可选**（判据：记忆下一回合可以重建，丢了只是体验差）。
	// 它仍在主接口里，是因为两个现成后端都已经具备这份能力，声明它不会让任何实现
	// 达不到契约；**不要**把这当成「以后可选能力都可以往主接口塞」的先例。
	//
	// 归属：internal/memwiki（文件 / 进程内）与 session/sqlite（同库）。宿主不需要
	// 记忆时返回一个空实现（满足六个方法、返回「不存在」）即可，不要返回 nil。
	Memory() memory.Wiki

	// Spill 返回溢出结果的存储（rig.SpillStore：Save / Load / GC）。
	//
	// 能力类别：**必需**——大到放不进模型上下文的工具结果会被存起来，模型只看到
	// 摘要与句柄。「存了但读不回来」等于永久丢失，所以 Save、Load 与 GC 必须同属
	// 一个后端，不能拆开给两家。
	//
	// 归属：文件落盘实现见 agent 侧默认 SpillPolicy；数据库实现见 session/sqlite。
	Spill() rig.SpillStore

	// Close 收尾并释放后端资源。
	//
	// 能力类别：**必需**（生命周期）——这是宿主关停路径的最后一环，返回错误表示
	// 收尾没成功（例如 WAL 没 checkpoint、连接没关干净）。
	//
	// **调用顺序**：先取消 / 收尾正在跑的回合与后台任务、排空事件，再 Close。反过来
	// 做会让运行中的回合拿到「已关闭」错误，把一次正常回答判成失败。实现必须幂等：
	// 重复 Close 不报错、不 panic；关闭之后访问器仍返回句柄，句柄自身的方法返回
	// 「已关闭」类错误。
	Close() error
}
