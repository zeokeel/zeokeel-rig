package sandbox_test

// D2 第 1 条：`Policy.roots` 的读写必须配对。
//
// `p.roots` 原本只在 `New`（WithRoots）里写一次，所以读侧（ResolvePath /
// CheckPath / OpenPath / containingRoot / resolveFullAccessPath）不加锁也是安全的。
// 加进 `RemoveWritableRoot` 之后这个前提没了：它的写侧在 `p.mu.Lock()` 下执行
// `p.roots = kept`，而 `Runtime.RemoveRoot` 天生就是"**跑着工具时**撤销一个根"，
// 于是同一个切片出现"写者持锁写 / 读者无锁读"的配对缺失。可见后果不只是
// `-race` 报竞争：ResolvePath 在一次解析里读三次 `p.roots`（空判定、`roots[0]`
// 作相对基准、包含性判定），中途被撤销一个根就会拼出"撤销前的基准 + 撤销后的
// 围栏"。
//
// 本用例有两个阶段，覆盖两个写点：
//  1. 逐个撤销大量根——每次撤销都写 `p.roots`，读者同时反复解析/判定；
//  2. 反复撤销/重加同一个根——每次都写追加可写根集合，读者同时读取可写面。
//
// 不变量（确定性断言）：任何一次读取都必须落在某个**完整快照**上——要么含某个
// 根、要么不含，不允许出现半更新的混合集合（重复项、长度与集合不符、乱序）。
//
// 说明：本环境门禁强制 CGO_ENABLED=0，`-race` 不进 CI；这条竞争的"红"用
// `CGO_ENABLED=1 go test -race` 单独跑出来（见交付说明）。日常无 -race 时这里靠
// 上面的快照不变量守住"读到半更新集合"，并保证整个过程不 panic。

import (
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"github.com/zeokeel/zeokeel-rig/sandbox"
)

// TestPolicyRootsSnapshotIsRaceFree：一个 goroutine 反复移除/追加根，另一批
// goroutine 反复做路径判定与可写面读取；2000 轮之后每次读取都是一个完整快照。
func TestPolicyRootsSnapshotIsRaceFree(t *testing.T) {
	const (
		// toggledRoots 是"逐个撤销"阶段要撤掉的根数量：每个根对应一次
		// `p.roots` 的运行期写。
		toggledRoots = 300
		// iterations 是读者与第二阶段写者的轮数。
		iterations = 2000
		readers    = 3
	)

	roots := make([]string, 0, toggledRoots+2)
	roots = append(roots, t.TempDir(), t.TempDir())
	for i := 0; i < toggledRoots; i++ {
		roots = append(roots, t.TempDir())
	}
	policy := sandbox.New(
		sandbox.WithRoots(roots...),
		sandbox.WithMode(sandbox.ModeWorkspaceWrite),
	)

	// 用策略自己规范化之后的写法做基准（macOS 上 t.TempDir 是 /var，策略里会
	// 变成 /private/var），避免手写字符串比较。顺序与传入顺序一致（保序去重）。
	original := policy.Roots()
	if len(original) != len(roots) {
		t.Fatalf("构造期根数量 = %d, want %d", len(original), len(roots))
	}
	primaryRoot := original[0]
	// 第二阶段反复撤销/重加的那个根：取最后一个，使两个阶段的"完整快照"可以
	// 用同一条规则表达（详见下面的 completeSnapshot）。
	toggledRoot := original[len(original)-1]

	// completeSnapshot 报告 set 是否是某个**完整快照**。
	//
	// 撤销只会从可写面里**保序**删项（withoutPath），第二阶段的重加则把根追加到
	// 末尾。所以任何合法快照都是"主根与稳定根 + 构造期列表的一个后缀"
	// `original[:2] + original[k:]`：k=2 是全量，k=len(original) 时只剩两个策略根。
	// 长度对、元素对、顺序对，才算完整快照——半更新的混合集合会在这里露馅。
	completeSnapshot := func(set []string) bool {
		if len(set) < 2 || len(set) > len(original) {
			return false
		}
		for i := 0; i < 2; i++ {
			if set[i] != original[i] {
				return false
			}
		}
		suffixFrom := len(original) - (len(set) - 2)
		for i := 2; i < len(set); i++ {
			if set[i] != original[suffixFrom+(i-2)] {
				return false
			}
		}
		return true
	}

	writableSnapshot := func() []string {
		writable := policy.WritableRoots()
		out := make([]string, 0, len(writable))
		for _, root := range writable {
			out = append(out, root.Root)
		}
		return out
	}

	// 读者可能在任意一步发现不一致，用带缓冲的 channel 收集而不是在 goroutine
	// 里 t.Fatalf（FailNow 只能在测试主 goroutine 里调用）。
	failures := make(chan error, readers+1)
	fail := func(format string, args ...any) {
		select {
		case failures <- fmt.Errorf(format, args...):
		default:
		}
	}

	var readersReady sync.WaitGroup
	readersReady.Add(readers)
	var wg sync.WaitGroup
	start := make(chan struct{})

	for reader := 0; reader < readers; reader++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			<-start
			readersReady.Done()
			for i := 0; i < iterations; i++ {
				// 1) 根集合必须是完整快照：半更新的集合长度/顺序都会露馅。
				if got := policy.Roots(); !completeSnapshot(got) {
					fail("Roots() 长度 = %d, 不是构造期根列表（长度 %d）的完整快照（半更新的集合）", len(got), len(original))
					return
				}
				// 2) 可写面同理（WritableRoots 还要读追加可写根与授权）。
				if got := writableSnapshot(); !completeSnapshot(got) {
					fail("WritableRoots() 根数量 = %d, 不是构造期根列表（长度 %d）的完整快照（半更新的集合）", len(got), len(original))
					return
				}
				// 3) 判定只有"允许 / 拒绝"两种结果，拒绝必须是策略拒绝。
				target := filepath.Join(original[2+i%toggledRoots], "out.txt")
				if err := policy.CheckWritablePath(target); err != nil && !errors.Is(err, sandbox.ErrDenied) {
					fail("CheckWritablePath(%q) = %v, want nil 或 ErrDenied", target, err)
					return
				}
				// 4) 相对路径始终按主根解析：成功时落点必须在主根内。
				resolved, err := policy.ResolvePath("out.txt")
				if err != nil {
					if !errors.Is(err, sandbox.ErrDenied) {
						fail("ResolvePath = %v, want nil 或 ErrDenied", err)
						return
					}
					continue
				}
				if filepath.Dir(resolved) != primaryRoot {
					fail("ResolvePath(%q) = %q, 不在主根 %q 内（读到半更新的基准）", "out.txt", resolved, primaryRoot)
					return
				}
			}
		}(reader)
	}

	// 写者：先逐个撤销（每次写 `p.roots`），再反复撤销/重加同一个根（每次写
	// 追加可写根集合），让两个写点都与读者的读取竞争。
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		readersReady.Wait()
		for _, root := range original[2:] {
			if err := policy.RemoveWritableRoot(root); err != nil {
				fail("RemoveWritableRoot(%q) = %v", root, err)
				return
			}
		}
		for i := 0; i < iterations; i++ {
			// 第一阶段已经把 toggledRoot 从 `p.roots` 里撤掉了，所以这里"重加"
			// 落在追加可写根上（可写面变回长集合），再撤销它（回到短集合）。
			policy.DeclareWritableRoot(toggledRoot)
			if err := policy.RemoveWritableRoot(toggledRoot); err != nil {
				fail("第 %d 轮 RemoveWritableRoot(%q) = %v", i, toggledRoot, err)
				return
			}
		}
	}()

	close(start)
	wg.Wait()
	close(failures)
	for err := range failures {
		t.Error(err)
	}
}
