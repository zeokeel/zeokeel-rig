package store

// 折叠索引 / stem 索引与主索引的**同步维护**（内部视角）。
//
// 外部黑盒用例只能用"硬链接别名必须被拒"来间接观察折叠表；但 Replace 会
// tmp+rename 换掉 inode，硬链接别名随之失效，黑盒无法覆盖"Replace 之后折叠表
// 仍然有效"。这里直接读三张表，逐个变更点断言。

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
)

func foldedEntry(t *testing.T, d *Dir, stem string) (string, bool) {
	t.Helper()
	d.indexMu.RLock()
	defer d.indexMu.RUnlock()
	id, ok := d.folded[foldStem(stem)]
	return id, ok
}

func stemsEntry(t *testing.T, d *Dir, stem string) (string, bool) {
	t.Helper()
	d.indexMu.RLock()
	defer d.indexMu.RUnlock()
	id, ok := d.stems[stem]
	return id, ok
}

// TestFoldedIndexMaintenanceAfterMutations 覆盖 7 个索引变更点里的
// Create / commit(Append) / Replace(commitReplace) / Delete / 扫盘重建。
func TestFoldedIndexMaintenanceAfterMutations(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "sessions")
	d, err := NewDir(root)
	if err != nil {
		t.Fatalf("NewDir: %v", err)
	}

	// Create：两张派生表都要有。
	if _, err := d.Create(ctx, Meta{ID: "Prod", Title: "t"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if id, ok := stemsEntry(t, d, "Prod"); !ok || id != "Prod" {
		t.Fatalf("Create 之后 stems[Prod] = %q ok=%v", id, ok)
	}
	if id, ok := foldedEntry(t, d, "prod"); !ok || id != "Prod" {
		t.Fatalf("Create 之后 folded[prod] = %q ok=%v", id, ok)
	}

	// Append（commit）：不得把派生表写丢。
	if _, err := d.AppendBatch(ctx, "Prod", []rig.Message{rig.NewMessage(rig.RoleUser, "hello")}...); err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}
	if id, ok := foldedEntry(t, d, "prod"); !ok || id != "Prod" {
		t.Fatalf("Append 之后 folded[prod] = %q ok=%v", id, ok)
	}

	// Replace（commitReplace）：同样不得写丢。
	if _, err := d.Replace(ctx, "Prod", []rig.Message{rig.NewMessage(rig.RoleUser, "rewritten")}); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	if id, ok := foldedEntry(t, d, "prod"); !ok || id != "Prod" {
		t.Fatalf("Replace 之后 folded[prod] = %q ok=%v", id, ok)
	}
	if id, ok := stemsEntry(t, d, "Prod"); !ok || id != "Prod" {
		t.Fatalf("Replace 之后 stems[Prod] = %q ok=%v", id, ok)
	}

	// 大小写敏感平台上 Prod/prod 是两个独立会话：两张表要能同时容纳。
	if _, err := d.Create(ctx, Meta{ID: "prod2", Title: "t2"}); err != nil {
		t.Fatalf("Create(prod2): %v", err)
	}
	if id, ok := stemsEntry(t, d, "prod2"); !ok || id != "prod2" {
		t.Fatalf("stems[prod2] = %q ok=%v", id, ok)
	}

	// Delete：两张表都要删干净。
	if err := d.Delete(ctx, "Prod"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if id, ok := stemsEntry(t, d, "Prod"); ok {
		t.Fatalf("Delete 之后 stems[Prod] 仍指向 %q", id)
	}
	if id, ok := foldedEntry(t, d, "prod"); ok && id == "Prod" {
		t.Fatalf("Delete 之后 folded[prod] 仍指向已删除的 %q", id)
	}

	// dropSession（文件被外部删除后的兜底）：也要同步清理。
	if _, err := d.Create(ctx, Meta{ID: "Dropped", Title: "t"}); err != nil {
		t.Fatalf("Create(Dropped): %v", err)
	}
	d.dropSession("Dropped", "测试")
	if id, ok := stemsEntry(t, d, "Dropped"); ok {
		t.Fatalf("dropSession 之后 stems[Dropped] 仍指向 %q", id)
	}

	// 扫盘重建：新开一个 Store，两张表必须从磁盘填回来。
	if _, err := d.Create(ctx, Meta{ID: "Gamma", Title: "g"}); err != nil {
		t.Fatalf("Create(Gamma): %v", err)
	}
	reopened, err := NewDir(root)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = reopened.Close() }()
	if id, ok := stemsEntry(t, reopened, "Gamma"); !ok || id != "Gamma" {
		t.Fatalf("扫盘后 stems[Gamma] = %q ok=%v", id, ok)
	}
	if id, ok := foldedEntry(t, reopened, "gamma"); !ok || id != "Gamma" {
		t.Fatalf("扫盘后 folded[gamma] = %q ok=%v", id, ok)
	}
	if id, ok := stemsEntry(t, reopened, "prod2"); !ok || id != "prod2" {
		t.Fatalf("扫盘后 stems[prod2] = %q ok=%v", id, ok)
	}
}
