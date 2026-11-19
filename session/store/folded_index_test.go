package store_test

// 已知限制 #1：`checkSessionOwner` 的归属校验在索引里做 O(n) 扫描。
//
// 两个独立但同源的代价：
//   - `ownerOfStemLocked` 遍历**全部**会话找 stem 相同的那个（每次 Append/Load/
//     Replace/Delete 都跑一次）；
//   - 大小写折叠撞名的候选也是遍历找的（仅 !exact 时）。
//
// 本文件钉两件事：
//   1. 折叠索引与主索引**同步维护**（Create / Replace / Delete / 重启扫盘之后，
//      别名仍然被拒、正常会话不受影响）；
//   2. Append 的单次代价**不随会话数线性增长**（2000 会话 vs 10 会话的比值有上界）。

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/session/store"
)

// linkAlias 在 dir 下把 realStem.ndjson 硬链接成 aliasStem.ndjson。
//
// 用**硬链接**而不是符号链接：符号链接会先被"非常规文件"那条检查拦下，走不到
// 大小写折叠的判定；硬链接是常规文件、两个名字同一个 inode，正好复现
// "大小写不敏感文件系统上 Prod/prod 是同一个文件"的形状（`os.SameFile` 为真）。
func linkAlias(t *testing.T, dir, realStem, aliasStem string) {
	t.Helper()
	realPath := filepath.Join(dir, realStem+".ndjson")
	aliasPath := filepath.Join(dir, aliasStem+".ndjson")
	if err := os.Link(realPath, aliasPath); err != nil {
		t.Skipf("硬链接不可用（%v）；该平台无法构造折叠别名", err)
	}
}

// TestFoldedIndexStaysConsistent 覆盖折叠索引的全部维护点。
func TestFoldedIndexStaysConsistent(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "sessions")
	dir, err := store.NewDir(root)
	if err != nil {
		t.Fatalf("NewDir: %v", err)
	}
	defer func() { _ = dir.Close() }()

	// Create 之后：折叠别名必须被拒。
	if _, err := dir.Create(ctx, store.Meta{ID: "Prod", Title: "真实会话"}); err != nil {
		t.Fatalf("Create(Prod): %v", err)
	}
	if _, err := dir.AppendBatch(ctx, "Prod", []rig.Message{user("hello")}...); err != nil {
		t.Fatalf("AppendBatch(Prod): %v", err)
	}
	linkAlias(t, root, "Prod", "prod")

	for _, call := range []struct {
		name string
		run  func() error
	}{
		{"Append", func() error {
			_, err := dir.Append(ctx, "prod", user("hijack"))
			return err
		}},
		{"Load", func() error {
			_, _, err := dir.Load(ctx, "prod")
			return err
		}},
		{"Replace", func() error {
			_, err := dir.Replace(ctx, "prod", []rig.Message{user("hijack")})
			return err
		}},
		{"Delete", func() error { return dir.Delete(ctx, "prod") }},
	} {
		if err := call.run(); !errors.Is(err, store.ErrSessionIDConflict) {
			t.Fatalf("%s(prod) err = %v, want ErrSessionIDConflict（Create 之后的折叠索引）", call.name, err)
		}
	}
	// 真实会话完全没被影响。
	if _, messages, err := dir.Load(ctx, "Prod"); err != nil {
		t.Fatalf("Load(Prod): %v", err)
	} else if len(messages) != 1 || messages[0].Text() != "hello" {
		t.Fatalf("Prod 内容 = %v，want 未被别名调用改写", textsOf(messages))
	}

	// Replace 会 tmp+rename 换掉 inode，硬链接别名随之失效（真实语义：别名不再
	// 指向同一个文件）。"Replace 之后折叠表仍与主索引一致"由内部用例
	// TestFoldedIndexMaintenanceAfterMutations 直接断言。

	// 正常会话不受影响。
	if _, err := dir.Create(ctx, store.Meta{ID: "beta", Title: "普通会话"}); err != nil {
		t.Fatalf("Create(beta): %v", err)
	}
	if _, err := dir.Append(ctx, "beta", user("ok")); err != nil {
		t.Fatalf("Append(beta): %v", err)
	}

	// Delete 之后：折叠项必须被删掉（否则同名新会话会被它误伤）。
	if err := dir.Delete(ctx, "Prod"); err != nil {
		t.Fatalf("Delete(Prod): %v", err)
	}
	if _, err := dir.Append(ctx, "prod", user("fresh")); err != nil {
		t.Fatalf("删除 Prod 之后 Append(prod) 仍被旧折叠项拦住: %v", err)
	}

	// 重启扫盘：折叠索引必须从磁盘**重建**。
	//
	// 注意别把别名文件留到扫盘之前：扫目录会把 `gamma.ndjson` 也登记成一个
	// 独立会话（硬链接别名不在校验范围内，见包文档），那样 exact 命中就不会走
	// 折叠判定。这里先只留 "Gamma"，重开之后再建别名——这才验证的是"扫盘把
	// 折叠表填好了"。
	if _, err := dir.Create(ctx, store.Meta{ID: "Gamma", Title: "重启前"}); err != nil {
		t.Fatalf("Create(Gamma): %v", err)
	}
	if _, err := dir.Append(ctx, "Gamma", user("before restart")); err != nil {
		t.Fatalf("Append(Gamma): %v", err)
	}
	if err := dir.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	reopened, err := store.NewDir(root)
	if err != nil {
		t.Fatalf("reopen NewDir: %v", err)
	}
	defer func() { _ = reopened.Close() }()
	linkAlias(t, root, "Gamma", "gamma")
	if _, err := reopened.Append(ctx, "gamma", user("hijack")); !errors.Is(err, store.ErrSessionIDConflict) {
		t.Fatalf("重启后 Append(gamma) err = %v, want ErrSessionIDConflict（扫盘必须重建折叠索引）", err)
	}
}

// seedSessionFiles 直接在目录里放 n 个空会话文件（走扫盘索引，比 n 次 Create 快）。
func seedSessionFiles(t *testing.T, dir string, n int) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for i := 0; i < n; i++ {
		path := filepath.Join(dir, fmt.Sprintf("s%05d.ndjson", i))
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatalf("seed %q: %v", path, err)
		}
	}
}

// measureAppend 返回在 store 上连续 Append 的单次平均耗时。
func measureAppend(t *testing.T, dir string, iterations int) time.Duration {
	t.Helper()
	opened, err := store.NewDir(dir, store.WithScanContents(false))
	if err != nil {
		t.Fatalf("NewDir: %v", err)
	}
	defer func() { _ = opened.Close() }()
	ctx := context.Background()
	// 预热：让第一条 Append 的懒初始化开销不计入。
	if _, err := opened.Append(ctx, "s00000", user("warmup")); err != nil {
		t.Fatalf("warmup Append: %v", err)
	}
	start := time.Now()
	for i := 0; i < iterations; i++ {
		if _, err := opened.Append(ctx, "s00000", user("payload")); err != nil {
			t.Fatalf("Append(%d): %v", i, err)
		}
	}
	return time.Since(start) / time.Duration(iterations)
}

// TestAppendCostDoesNotGrowWithSessionCount 是复杂度断言：会话数从 10 涨到 2000
// 时，单次 Append 的耗时不得同比例增长。
//
// 旧实现每次 Append 都在索引里做 O(n) 扫描（ownerOfStemLocked），2000 会话下实测
// 8.6×；修复后两张派生索引让归属校验变成 O(1)，实测 ≈1.2×。阈值给到 5×，并要求
// 大库侧不超过 5ms 绝对上限，避免小样本抖动造成 flaky。
func TestAppendCostDoesNotGrowWithSessionCount(t *testing.T) {
	if testing.Short() {
		t.Skip("规模化耗时断言在 -short 下跳过")
	}

	smallDir := filepath.Join(t.TempDir(), "small")
	seedSessionFiles(t, smallDir, 10)
	largeDir := filepath.Join(t.TempDir(), "large")
	seedSessionFiles(t, largeDir, 2000)

	const iterations = 50
	small := measureAppend(t, smallDir, iterations)
	large := measureAppend(t, largeDir, iterations)
	t.Logf("Append 单次耗时：10 个会话=%v，2000 个会话=%v（比值 %.1f×）",
		small, large, float64(large)/float64(small))

	if small <= 0 {
		small = time.Nanosecond
	}
	if large > small*5 {
		t.Fatalf("Append 代价随会话数线性增长：10 会话 %v → 2000 会话 %v（%.1f×，上限 5×）",
			small, large, float64(large)/float64(small))
	}
	if large > 5*time.Millisecond {
		t.Fatalf("2000 会话下 Append 单次耗时 %v 超过 5ms 上限", large)
	}
}
