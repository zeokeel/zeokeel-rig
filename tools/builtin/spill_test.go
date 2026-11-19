package builtin_test

// spill_read：溢出结果的**读回**入口。
//
// 没有它，宿主把 Store 接上之后模型只看到 `ref: spill-…`，而 SDK 里没有任何工具
// 能把这个 id 读回来——"结果太大，已存起来"就变成了"结果永久丢失"。

import (
	"context"
	"errors"
	"strings"
	"testing"

	rig "github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/tools/builtin"
)

// fakeSpill 是最小的 SpillStore 实现。
type fakeSpill struct {
	content map[string]string
	loads   int
}

func (f *fakeSpill) Save(context.Context, rig.ToolCall, string) (rig.SpillRef, error) {
	return rig.SpillRef{}, nil
}

func (f *fakeSpill) Load(_ context.Context, id string) (string, error) {
	f.loads++
	content, ok := f.content[id]
	if !ok {
		return "", rig.ErrSpillNotFound
	}
	return content, nil
}

func (f *fakeSpill) GC(context.Context, int) (int, error) { return 0, nil }

func TestNewSpillReadIsAbsentWithoutAStore(t *testing.T) {
	if tool := builtin.NewSpillRead(nil); tool != nil {
		t.Fatalf("没有 Store 时不该注册 spill_read，得到 %v", tool.Name())
	}
}

func TestSpillReadReturnsTheStoredContent(t *testing.T) {
	store := &fakeSpill{content: map[string]string{"spill-1": "完整输出"}}
	tool := builtin.NewSpillRead(store)
	if tool == nil {
		t.Fatal("有 Store 时应注册 spill_read")
	}
	if tool.Name() != builtin.ToolSpillRead {
		t.Fatalf("Name = %q", tool.Name())
	}
	result, err := tool.Invoke(context.Background(), rig.ToolCall{Name: tool.Name(), Arguments: `{"id":"spill-1"}`})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if result.Content != "完整输出" || result.IsError {
		t.Fatalf("result = %+v", result)
	}
	if store.loads != 1 {
		t.Fatalf("Load 调用 = %d, want 1", store.loads)
	}
}

func TestSpillReadReportsAnUnknownHandle(t *testing.T) {
	tool := builtin.NewSpillRead(&fakeSpill{content: map[string]string{}})
	_, err := tool.Invoke(context.Background(), rig.ToolCall{Arguments: `{"id":"nope"}`})
	if err == nil {
		t.Fatal("未知 id 应返回错误")
	}
	if !strings.Contains(err.Error(), "no stored result") {
		t.Fatalf("错误文案应说明这个 id 不存在：%v", err)
	}
	if !errors.Is(err, rig.ErrSpillNotFound) {
		// 工具层把哨兵错误包了一层文案，但仍要能被 errors.Is 认出，便于宿主分支。
		t.Fatalf("错误没有保留 ErrSpillNotFound 语义：%v", err)
	}
}

func TestSpillReadRequiresAnID(t *testing.T) {
	tool := builtin.NewSpillRead(&fakeSpill{content: map[string]string{}})
	if _, err := tool.Invoke(context.Background(), rig.ToolCall{Arguments: `{}`}); err == nil {
		t.Fatal("缺 id 应报错")
	}
	if _, err := tool.Invoke(context.Background(), rig.ToolCall{Arguments: `not json`}); err == nil {
		t.Fatal("非法 JSON 应报错")
	}
}

// TestSpillReadTruncatesOnACharacterBoundary 断言 8 KiB 上限切在 UTF-8 边界上：
// 切出半个汉字会让模型读到乱码。
func TestSpillReadTruncatesOnACharacterBoundary(t *testing.T) {
	body := strings.Repeat("长", 4000) // 12000 字节 > 8192
	tool := builtin.NewSpillRead(&fakeSpill{content: map[string]string{"big": body}})
	result, err := tool.Invoke(context.Background(), rig.ToolCall{Arguments: `{"id":"big"}`})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !strings.Contains(result.Content, "truncated") {
		t.Fatalf("超过上限时应说明被截断：%q", result.Content)
	}
	cut := strings.SplitN(result.Content, "\n\n[truncated", 2)[0]
	if strings.ContainsRune(cut, '\uFFFD') {
		t.Fatal("截断点把一个汉字劈成了半个（出现替换字符）")
	}
	if !strings.HasSuffix(cut, "长") {
		t.Fatalf("截断结果应以完整字符结尾：%q", cut[len(cut)-6:])
	}
}
