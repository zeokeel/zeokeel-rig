package sqlite_test

// 记忆必须按工作区分区：同一个库被多个仓库共用时，A 仓的记忆不能出现在 B 仓。
// 文件后端（internal/memwiki）早就按工作区落目录，数据库后端此前是个全局命名空间，
// 于是"串味"是必然的。这里的用例把 Index/Read/Write/Append/Delete/Search 逐条钉住。

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig/memory"
	sqlitestore "github.com/zeokeel/zeokeel-rig/session/sqlite"
)

// openWorkspace 打开同一个库文件上的一个工作区视图。
func openWorkspace(t *testing.T, path, workspace string) *sqlitestore.Store {
	t.Helper()
	store, err := sqlitestore.Open(sqlitestore.Options{Path: path, Workspace: workspace})
	if err != nil {
		t.Fatalf("Open(workspace=%q): %v", workspace, err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// TestMemoryIsPartitionedByWorkspace 是跨仓串味的回归用例：两个工作区都有一页
// 同名的 "build"（真实现场：每个仓库都有构建笔记），彼此的 Index/Read/Append/
// Delete/Search 都不能碰到对方的行。
func TestMemoryIsPartitionedByWorkspace(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "shared.sqlite")
	repoA := openWorkspace(t, path, "repo-a")
	repoB := openWorkspace(t, path, "repo-b")

	if err := repoA.Memory().Write(ctx, memory.Page{
		Name: "build", Summary: "A 仓的构建记录", Content: "A 的正文",
	}); err != nil {
		t.Fatalf("A Write: %v", err)
	}
	if err := repoB.Memory().Write(ctx, memory.Page{
		Name: "build", Summary: "B 仓的构建记录", Content: "B 的正文",
	}); err != nil {
		t.Fatalf("B Write（同名页不能撞主键）: %v", err)
	}

	pageA, err := repoA.Memory().Read(ctx, "build")
	if err != nil {
		t.Fatalf("A Read: %v", err)
	}
	pageB, err := repoB.Memory().Read(ctx, "build")
	if err != nil {
		t.Fatalf("B Read: %v", err)
	}
	if pageA.Content != "A 的正文" || pageA.Summary != "A 仓的构建记录" {
		t.Fatalf("A 读到的是 %+v", pageA)
	}
	if pageB.Content != "B 的正文" || pageB.Summary != "B 仓的构建记录" {
		t.Fatalf("B 读到的是 %+v", pageB)
	}

	// Index 只看自己的工作区。
	if err := repoA.Memory().Write(ctx, memory.Page{Name: "deploy", Summary: "A 仓的部署"}); err != nil {
		t.Fatalf("A Write(deploy): %v", err)
	}
	metasA, err := repoA.Memory().Index(ctx)
	if err != nil {
		t.Fatalf("A Index: %v", err)
	}
	if len(metasA) != 2 {
		t.Fatalf("A Index = %d 页，want 2（%+v）", len(metasA), metasA)
	}
	metasB, err := repoB.Memory().Index(ctx)
	if err != nil {
		t.Fatalf("B Index: %v", err)
	}
	if len(metasB) != 1 || metasB[0].Name != "build" {
		t.Fatalf("B Index = %+v，want 只有 build", metasB)
	}
	// 逻辑页名不能把内部作用域前缀漏给模型/用户。
	if metasA[0].Name != "build" && metasA[0].Name != "deploy" {
		t.Fatalf("Index 里的页名被污染了：%q", metasA[0].Name)
	}

	// Append 只改自己的页。
	if err := repoA.Memory().Append(ctx, "build", "A 追加的一条"); err != nil {
		t.Fatalf("A Append: %v", err)
	}
	pageB, err = repoB.Memory().Read(ctx, "build")
	if err != nil {
		t.Fatalf("B Read(append 之后): %v", err)
	}
	if pageB.Content != "B 的正文" {
		t.Fatalf("A 的 Append 改到了 B 的页：%q", pageB.Content)
	}

	// Search 只在自己的工作区里命中。
	found, err := repoA.Memory().Search(ctx, "A 仓的构建")
	if err != nil {
		t.Fatalf("A Search: %v", err)
	}
	if len(found) != 1 {
		t.Fatalf("A Search = %d 条，want 1", len(found))
	}
	crossed, err := repoB.Memory().Search(ctx, "A 仓的构建")
	if err != nil {
		t.Fatalf("B Search: %v", err)
	}
	if len(crossed) != 0 {
		t.Fatalf("B 搜到了 A 的页：%+v", crossed)
	}

	// Delete 只删自己的页。
	if err := repoA.Memory().Delete(ctx, "build"); err != nil {
		t.Fatalf("A Delete: %v", err)
	}
	if _, err := repoB.Memory().Read(ctx, "build"); err != nil {
		t.Fatalf("A 的 Delete 连 B 的页一起删了：%v", err)
	}
	if _, err := repoA.Memory().Read(ctx, "build"); !errors.Is(err, memory.ErrNotFound) {
		t.Fatalf("A 删完还能读到自己的页：%v", err)
	}
}

// TestMemoryEmptyWorkspaceSeesEverything：空 Workspace 保持既有行为（不按工作区
// 过滤，全部可见），老行（workspace_id 为空串）也仍然读得出来。
func TestMemoryEmptyWorkspaceSeesEverything(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "shared.sqlite")
	// 先按老行为（空工作区）写一页：这就是升级前的老行。
	legacy := openWorkspace(t, path, "")
	if err := legacy.Memory().Write(ctx, memory.Page{
		Name: "old-notes", Summary: "升级前的页", Content: "老正文",
	}); err != nil {
		t.Fatalf("老库 Write: %v", err)
	}

	repo := openWorkspace(t, path, "repo-a")
	// 升级连续性优先于"完全隔离"：老页（workspace_id='' 的共享桶）在工作区视图里
	// 仍然读得到，否则宿主一旦开始传 Workspace，用户升级前的记忆会静默消失。
	if _, err := repo.Memory().Read(ctx, "old-notes"); err != nil {
		t.Fatalf("工作区视图应当仍能读到老页（升级连续性）：%v", err)
	}
	metas, err := repo.Memory().Index(ctx)
	if err != nil {
		t.Fatalf("工作区 Index: %v", err)
	}
	if len(metas) != 1 || metas[0].Name != "old-notes" {
		t.Fatalf("工作区 Index = %+v，want 只含老页", metas)
	}

	// 空工作区仍能读到老行（升级不丢记忆）。
	page, err := legacy.Memory().Read(ctx, "old-notes")
	if err != nil {
		t.Fatalf("空工作区 Read(老行): %v", err)
	}
	if page.Content != "老正文" || page.Summary != "升级前的页" {
		t.Fatalf("老行内容 = %+v", page)
	}
	if all, err := legacy.Memory().Index(ctx); err != nil || len(all) != 1 {
		t.Fatalf("空工作区 Index = %+v / %v，want 1 页", all, err)
	}
}

// TestMemoryUnscopedViewResolvesPagesFromOtherWorkspaces 钉住"列表里能打开的，读也要
// 打得开"。
//
// 不分区视图（空 Workspace = 既有行为"全部可见"）的 Index 会列出所有工作区的页
// （逻辑名），但 Read/Append/Delete 曾按逻辑名精确查物理主键，于是分区写入的页
// "列得出来、打不开"。同名页出现在多个工作区时必须要求宿主指定工作区，而不是
// 随机挑一行。
func TestMemoryUnscopedViewResolvesPagesFromOtherWorkspaces(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mem.db")
	ctx := context.Background()

	scoped, err := sqlitestore.Open(sqlitestore.Options{Path: path, Workspace: "repo-a"})
	if err != nil {
		t.Fatalf("Open scoped: %v", err)
	}
	if err := scoped.Memory().Write(ctx, memory.Page{Name: "build", Summary: "repo-a 的构建笔记", Content: "repo-a 的构建笔记正文"}); err != nil {
		t.Fatalf("Write: %v", err)
	}

	unscoped, err := sqlitestore.Open(sqlitestore.Options{Path: path})
	if err != nil {
		t.Fatalf("Open unscoped: %v", err)
	}
	defer func() { _ = unscoped.Close() }()
	defer func() { _ = scoped.Close() }()

	metas, err := unscoped.Memory().Index(ctx)
	if err != nil {
		t.Fatalf("Index: %v", err)
	}
	if len(metas) != 1 || metas[0].Name != "build" {
		t.Fatalf("不分区视图应列出一页 build，实际 %+v", metas)
	}
	page, err := unscoped.Memory().Read(ctx, "build")
	if err != nil {
		t.Fatalf("列表里能看到的页必须读得到: %v", err)
	}
	if !strings.Contains(page.Content, "repo-a 的构建笔记正文") {
		t.Fatalf("读到的正文不对: %q", page.Content)
	}

	// 第二个工作区有同名页之后，不分区视图的精确读必须报"有歧义"。
	other, err := sqlitestore.Open(sqlitestore.Options{Path: path, Workspace: "repo-b"})
	if err != nil {
		t.Fatalf("Open other: %v", err)
	}
	defer func() { _ = other.Close() }()
	if err := other.Memory().Write(ctx, memory.Page{Name: "build", Summary: "repo-b 的构建笔记", Content: "repo-b 的构建笔记正文"}); err != nil {
		t.Fatalf("Write other: %v", err)
	}
	if _, err := unscoped.Memory().Read(ctx, "build"); err == nil {
		t.Fatal("多个工作区有同名页时，不分区视图的读应当报错要求指定工作区")
	}
}

// TestMemoryScopedViewStillSeesLegacyPages 钉住升级连续性：宿主一旦传入 Workspace，
// 升级前的老页（workspace_id=”）不能凭空消失。
func TestMemoryScopedViewStillSeesLegacyPages(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy-mem.db")
	ctx := context.Background()

	legacy, err := sqlitestore.Open(sqlitestore.Options{Path: path})
	if err != nil {
		t.Fatalf("Open legacy: %v", err)
	}
	if err := legacy.Memory().Write(ctx, memory.Page{Name: "notes", Summary: "升级前的记忆", Content: "升级前的记忆正文"}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	scoped, err := sqlitestore.Open(sqlitestore.Options{Path: path, Workspace: "repo-a"})
	if err != nil {
		t.Fatalf("Open scoped: %v", err)
	}
	defer func() { _ = scoped.Close() }()

	metas, err := scoped.Memory().Index(ctx)
	if err != nil {
		t.Fatalf("Index: %v", err)
	}
	if len(metas) != 1 || metas[0].Name != "notes" {
		t.Fatalf("分区视图应仍能看到老页，实际 %+v", metas)
	}
	page, err := scoped.Memory().Read(ctx, "notes")
	if err != nil {
		t.Fatalf("老页必须读得到: %v", err)
	}
	if page.Content != "升级前的记忆正文" {
		t.Fatalf("老页正文不对: %q", page.Content)
	}
}
