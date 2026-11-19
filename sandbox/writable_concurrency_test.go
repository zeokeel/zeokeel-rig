package sandbox_test

// 运行期声明 API（DeclareWritableRoot / DeclareDegradableWritableRoot /
// AddReadOnlySubpaths）在 DSH 对齐后仍被 osrunner 与 gitpolicy 在**会话中途**
// 调用（例如 `git init` 之后补登记 .git 的只读子路径）。这些方法直接写
// `policy.readOnlySubpaths` / `extraWritableRoots` / `degradableRoots`，而读侧
// （WritableRoots / AllowsWrite / CheckWritablePath）在工具 goroutine 里并发读同一
// 批 map —— 没有锁时这是 Go 的 `fatal error: concurrent map read and map write`
// （不可 recover，直接带走宿主进程），`-race` 下则是数据竞争。
//
// 本文件先钉住"并发读写不得炸、且语义不变"（修复前 -race 红），修完实现后转绿。

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"github.com/zeokeel/zeokeel-rig/sandbox"
)

// TestDeclareWritableRootConcurrentReads 用 4 个读者 + 1 个写者压同一份 Policy。
//
// 子路径名必须**唯一**：addReadOnlySubpaths 有"已存在就跳过"的快路径，重复用
// 同一个名字会让写侧几乎不落笔，掩盖竞争。
func TestDeclareWritableRootConcurrentReads(t *testing.T) {
	root := t.TempDir()
	policy := sandbox.New(
		sandbox.WithRoots(root),
		sandbox.WithMode(sandbox.ModeWorkspaceWrite),
	)

	const iterations = 200
	var wg sync.WaitGroup
	start := make(chan struct{})
	for reader := 0; reader < 4; reader++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for i := 0; i < iterations; i++ {
				_ = policy.WritableRoots()
				_ = policy.AllowsWrite(filepath.Join(root, "f.txt"))
				_ = policy.AdditionalWritableRoots()
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		for i := 0; i < iterations; i++ {
			policy.DeclareWritableRoot(root, fmt.Sprintf("ro/%d", i))
		}
	}()
	close(start)
	wg.Wait()

	// 语义不变的断言：最后一条声明必须被登记，且全部 200 条都在。
	roots := policy.WritableRoots()
	found := 0
	for _, writable := range roots {
		found += len(writable.ReadOnlySubpaths)
	}
	if found != iterations {
		t.Fatalf("只读子路径数量 = %d, want %d", found, iterations)
	}
	if policy.AllowsWrite(filepath.Join(root, "ro", "199", "x")) {
		t.Fatal("AllowsWrite 放行了只读子路径")
	}
	if !policy.AllowsWrite(filepath.Join(root, "ordinary.txt")) {
		t.Fatal("AllowsWrite 拒绝了普通路径")
	}
}

// TestDeclareWritableRootPublishesToConcurrentReaders 是同一竞争的另一个切面：
// 读者先看到"没有只读子路径"，写者随后声明；最终读者必须看到它（可见性 + 不崩）。
func TestDeclareWritableRootPublishesToConcurrentReaders(t *testing.T) {
	root := t.TempDir()
	policy := sandbox.New(sandbox.WithRoots(root), sandbox.WithMode(sandbox.ModeWorkspaceWrite))

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for reader := 0; reader < 3; reader++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = policy.WritableRoots()
					_ = policy.CheckWritablePath(filepath.Join(root, "vault", "x"))
				}
			}
		}()
	}
	for i := 0; i < 50; i++ {
		policy.DeclareDegradableWritableRoot(root, fmt.Sprintf("vault/%d", i))
	}
	close(stop)
	wg.Wait()

	if !policy.AllowsWrite(filepath.Join(root, "vault", "deadbeef")) {
		t.Fatal("降级可写根本身的 AllowsWrite 应当仍为 true（子路径不该匹配）")
	}
	if policy.AllowsWrite(filepath.Join(root, "vault", "7", "secret")) {
		t.Fatal("AllowsWrite 放行了只读子路径")
	}
}
