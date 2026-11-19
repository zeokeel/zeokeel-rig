package storagetest

import (
	"context"
	"errors"
	"testing"

	"github.com/zeokeel/zeokeel-rig/memory"
)

// runMemoryCases 守长期记忆 wiki 的最小契约（可选能力）。
//
// 这里只覆盖"记忆作为 storage 的一部分还活着"这条底线：写→读→索引→搜索→删。
// memory.Wiki 本身还有六条更细的规则（名称校验、摘要必填、Append 不自建、
// 改写留痕、Index 全量、Search 排序），它们与 storage 无关，应该由一份独立的
// memorytest 共享套件守（见交付说明里的"还该进基线但没写"）。
func runMemoryCases(t *testing.T, caps Capability, newStorage Factory) {
	t.Run("MemoryPageRoundTrip", func(t *testing.T) {
		s := newStore(t, newStorage)
		wiki := requireMemory(t, s, caps)
		ctx := context.Background()
		page := memory.Page{
			Name:    "build-notes",
			Title:   "构建笔记",
			Summary: "how to run the build",
			Content: "# 构建\n\nmake test",
		}
		if err := wiki.Write(ctx, page); err != nil {
			t.Fatalf("Write: %v", err)
		}
		got, err := wiki.Read(ctx, page.Name)
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		if got.Name != page.Name || got.Content != page.Content || got.Summary != page.Summary {
			t.Fatalf("Read = %+v，want Name/Summary/Content 与写入一致（%+v）", got, page)
		}

		index, err := wiki.Index(ctx)
		if err != nil {
			t.Fatalf("Index: %v", err)
		}
		found := false
		for _, meta := range index {
			if meta.Name == page.Name {
				found = true
			}
		}
		if !found {
			t.Fatalf("Index = %+v，want 含 %q（索引是模型发现记忆的唯一入口）", index, page.Name)
		}

		// 搜索只返回元数据，但必须能找到这页。
		hits, err := wiki.Search(ctx, "build")
		if err != nil {
			t.Fatalf("Search: %v", err)
		}
		found = false
		for _, meta := range hits {
			if meta.Name == page.Name {
				found = true
			}
		}
		if !found {
			t.Fatalf("Search(\"build\") = %+v，want 含 %q", hits, page.Name)
		}
	})

	t.Run("MemoryDeleteRemovesPage", func(t *testing.T) {
		s := newStore(t, newStorage)
		wiki := requireMemory(t, s, caps)
		ctx := context.Background()
		page := memory.Page{Name: "gone", Summary: "会被删掉", Content: "x"}
		if err := wiki.Write(ctx, page); err != nil {
			t.Fatalf("Write: %v", err)
		}
		if err := wiki.Delete(ctx, page.Name); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		if _, err := wiki.Read(ctx, page.Name); !errors.Is(err, memory.ErrNotFound) {
			t.Fatalf("删除后 Read = %v，want memory.ErrNotFound", err)
		}
	})
}
