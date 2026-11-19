// Package storagetest 是 storage.Storage 的共享契约基线套件：一份用例，所有
// 实现各跑一遍。
//
// 一个后端只在自己的测试里写几行：
//
//	func TestStorageBaseline(t *testing.T) {
//		dirCaps := storagetest.Capability{History: true}
//		storagetest.Run(t, "dir", func(t *testing.T) storage.Storage {
//			d, err := store.NewDir(t.TempDir())
//			if err != nil {
//				t.Fatalf("NewDir: %v", err)
//			}
//			t.Cleanup(func() { _ = d.Close() })
//			return store.NewDirStorage(d)
//		}, dirCaps)
//	}
//
// # 为什么必须有这一层
//
// 同一份契约有两个实现（目录 NDJSON / SQLite）时，"两边各写一遍测试"必然分叉：
// 一边补了并发用例、另一边没补，缺陷就只在没补的那边活着，而两边的测试都是绿的。
// 把阈值、边界与坑固定在套件里之后，实现只能选择"支持/不支持"（能力位），
// **不能**选择"少被测一条"。
//
// # 能力位：不猜、不伪造
//
// storage.Storage 只有五个方法，其中 Checkpoints/Memory/Spill 是可选的：不支持的
// 后端对相应访问器返回 nil。调用方用 Capability 声明自己支持什么：
//
//   - 声明为 false 的用例会 skip，并打印 `skip: backend does not support <能力>`；
//   - 声明为 true 但访问器返回 nil 视为**撒谎**，直接判失败——能力位是给宿主看
//     的契约，不能只有注释里的分量；
//   - 会话列表与删除（storage 契约刻意没有收编的方法）通过可选接口 SessionAdmin
//     发现，没实现就 skip 并打印原因。
//
// # 依赖
//
// 只依赖标准库与本仓的 rig/storage/memory 契约包（**没有第三方依赖**），因此任何
// 实现了 storage.Storage 的包——包括仓外的宿主——都能 import 它。
package storagetest

import (
	"testing"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/memory"
	"github.com/zeokeel/zeokeel-rig/storage"
)

// Factory 返回一份**全新的、空的**存储实现。
//
// 套件为每个用例调用一次工厂（用例之间不共享状态），因此实现可以在 Factory 里
// 用 t.TempDir() 建目录、用 t.Cleanup 注册关闭。返回 nil 会被判为失败。
type Factory func(t *testing.T) storage.Storage

// Capability 是某个后端声明的可选能力位，与 storage.Storage 的访问器一一对应。
//
// 每一位的含义是"这个访问器会返回可用的实现"，而不是"文档里写了支持"：
// 声明 true 就必须真的返回非 nil，否则套件判失败。
//
// 声明 false 时，"不支持"有两种合法表达：访问器返回 nil，或者返回一个**每个方法
// 都报错的桩**（storage.Storage 的接口文档要求后者：nil 会让调用方在离原因很远的
// 地方 panic）。两种都由套件 skip 并打印原因；桩还会额外在关闭用例里被验证"关闭
// 后仍然报错"。
type Capability struct {
	// History 是否支持会话消息读写。所有实现都应为 true；置 false 只用于
	// "只提供记忆/检查点"的极端后端，此时会话相关用例全部 skip。
	History bool
	// Checkpoints 是否支持回合检查点（rig.CheckpointStore）。
	Checkpoints bool
	// Memory 是否支持长期记忆 wiki（memory.Wiki）。
	Memory bool
	// Spill 是否支持溢出结果存储（rig.SpillStore）。
	Spill bool
}

// Capabilities 是"实现名 → 能力位"的登记表。
//
// 用登记表而不是让每个调用方自己写 Capability 字面量，是为了让"这个后端支持
// 什么"集中在一处、可被审查：同一个实现名不允许在一处声明支持检查点、在另一处
// 悄悄不测检查点。
type Capabilities map[string]Capability

// Lookup 返回 name 登记的能力位。未登记时返回零值（全部不支持）与 false。
func (c Capabilities) Lookup(name string) (Capability, bool) {
	caps, ok := c[name]
	return caps, ok
}

// Run 用登记表里 name 对应的能力位跑一遍基线套件。
//
// 与包级 Run 的区别：未登记的实现名直接失败，而不是按"什么都不支持"跳过全部
// 用例——"忘了登记"和"确实不支持"在结果上必须能区分开。
func (c Capabilities) Run(t *testing.T, name string, newStorage Factory) {
	t.Helper()
	caps, ok := c.Lookup(name)
	if !ok {
		t.Fatalf("storagetest: 后端 %q 没有在 Capabilities 里登记能力位（不要靠零值蒙混：跳过全部用例也是绿）", name)
	}
	Run(t, name, newStorage, caps)
}

// Run 跑完整的契约基线套件。
//
// name 是后端名（子测试名，也是失败信息里的标识）。每个用例都会重新调用
// newStorage 拿一份空实现，用例之间互不影响。
func Run(t *testing.T, name string, newStorage Factory, caps Capability) {
	t.Helper()
	if newStorage == nil {
		t.Fatalf("storagetest: 后端 %q 的 Factory 是 nil", name)
	}
	t.Run(name, func(t *testing.T) {
		runHistoryCases(t, caps, newStorage)
		runSessionAdminCases(t, caps, newStorage)
		runCloseCases(t, caps, newStorage)
		runCheckpointCases(t, caps, newStorage)
		runMemoryCases(t, caps, newStorage)
		runSpillCases(t, caps, newStorage)
		runConcurrencyCases(t, caps, newStorage)
	})
}

// newStore 拿一份新实现，并在用例结束时关闭。
//
// Close 必须幂等（套件有专门用例），所以这里无条件再关一次是安全的；忽略返回值
// 是因为用例自己的断言才是失败来源，收尾的 Close 报错不该掩盖它。
func newStore(t *testing.T, newStorage Factory) storage.Storage {
	t.Helper()
	s := newStorage(t)
	if s == nil {
		t.Fatalf("storagetest: Factory 返回了 nil storage.Storage")
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// skipNoCapability 打印**可归因**的跳过原因。
//
// 必须带原因：一个静默 skip 的套件看起来全绿，读日志的人却不知道"这个后端到底
// 测没测检查点"。文案固定成 `skip: backend does not support <x>`，便于 CI 里 grep。
func skipNoCapability(t *testing.T, capability string) {
	t.Helper()
	t.Skipf("skip: backend does not support %s", capability)
}

// requireHistory 取 History()，顺带守住"能力位不得撒谎"。
func requireHistory(t *testing.T, s storage.Storage, caps Capability) rig.History {
	t.Helper()
	if !caps.History {
		skipNoCapability(t, "history")
	}
	history := s.History()
	if history == nil {
		t.Fatalf("能力位声明支持 history，但 History() 返回 nil：不要伪造能力" +
			"（要么给出实现，要么把 Capability.History 置 false 让用例 skip）")
	}
	return history
}

// requireCheckpoints 取 Checkpoints()，顺带守住"能力位不得撒谎"。
func requireCheckpoints(t *testing.T, s storage.Storage, caps Capability) rig.CheckpointStore {
	t.Helper()
	if !caps.Checkpoints {
		skipNoCapability(t, "checkpoints")
	}
	checkpoints := s.Checkpoints()
	if checkpoints == nil {
		t.Fatalf("能力位声明支持 checkpoints，但 Checkpoints() 返回 nil：不要伪造能力")
	}
	return checkpoints
}

// requireMemory 取 Memory()，顺带守住"能力位不得撒谎"。
func requireMemory(t *testing.T, s storage.Storage, caps Capability) memory.Wiki {
	t.Helper()
	if !caps.Memory {
		skipNoCapability(t, "memory")
	}
	wiki := s.Memory()
	if wiki == nil {
		t.Fatalf("能力位声明支持 memory，但 Memory() 返回 nil：不要伪造能力")
	}
	return wiki
}

// requireSpill 取 Spill()，顺带守住"能力位不得撒谎"。
func requireSpill(t *testing.T, s storage.Storage, caps Capability) rig.SpillStore {
	t.Helper()
	if !caps.Spill {
		skipNoCapability(t, "spill")
	}
	spill := s.Spill()
	if spill == nil {
		t.Fatalf("能力位声明支持 spill，但 Spill() 返回 nil：不要伪造能力")
	}
	return spill
}
