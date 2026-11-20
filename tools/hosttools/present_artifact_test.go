package hosttools_test

// present 的两种交付形态：
//   - 文件：模型给出已存在的普通文件 → rig.FilePresenter（既有行为，一个字节不改）；
//   - 内容产物：模型给出 title + body → rig.Presenter（宿主渲染成卡片/文档）。
//
// 以前只有文件形态（`rig.Presenter` 被标废弃、没有任何工具调用它），于是"内容
// 产物"这条产品语义只能由宿主自建一套 present 工具。

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/tools/hosttools"
)

type stubArtifact struct {
	mu    sync.Mutex
	calls []struct{ title, body string }
}

func (s *stubArtifact) Present(_ context.Context, title, body string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, struct{ title, body string }{title, body})
	return "artifact-1", nil
}

func (s *stubArtifact) snapshot() []struct{ title, body string } {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]struct{ title, body string }(nil), s.calls...)
}

// presentTool 取出一个 present 工具。
func presentTool(t *testing.T, opts hosttools.Options) rig.Tool {
	t.Helper()
	for _, tool := range hosttools.Tools(opts) {
		if tool.Name() == "present" {
			return tool
		}
	}
	t.Fatal("没有 present 工具")
	return nil
}

// TestPresentBodyUsesArtifactPresenter：title+body 形态走 rig.Presenter。
func TestPresentBodyUsesArtifactPresenter(t *testing.T) {
	artifact := &stubArtifact{}
	tool := presentTool(t, hosttools.Options{Artifact: artifact})
	payload, err := json.Marshal(map[string]any{"title": "构建报告", "body": "# 结论\n全部通过"})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	result, err := tool.Invoke(context.Background(), rig.ToolCall{ID: "p", Name: "present", Arguments: string(payload)})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if result.IsError {
		t.Fatalf("present 失败: %q", result.Content)
	}
	calls := artifact.snapshot()
	if len(calls) != 1 {
		t.Fatalf("ArtifactPresenter 被调用 %d 次, want 1", len(calls))
	}
	if calls[0].title != "构建报告" || !strings.Contains(calls[0].body, "全部通过") {
		t.Fatalf("title/body 没传对: %+v", calls[0])
	}
	if !strings.Contains(result.Content, "artifact-1") {
		t.Fatalf("结果里应带宿主返回的产物标识，实际 %q", result.Content)
	}
}

// TestPresentBodyWithoutArtifactPresenterFails：没有实现时给可读错误，而不是假装成功。
func TestPresentBodyWithoutArtifactPresenterFails(t *testing.T) {
	tool := presentTool(t, hosttools.Options{})
	result, err := tool.Invoke(context.Background(), rig.ToolCall{ID: "p", Name: "present", Arguments: `{"title":"t","body":"b"}`})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !result.IsError {
		t.Fatalf("没有 ArtifactPresenter 时应当报错，实际: %q", result.Content)
	}
	if !strings.Contains(strings.ToLower(result.Content), "artifact") {
		t.Fatalf("错误应说明缺的是 ArtifactPresenter，实际 %q", result.Content)
	}
}

// TestPresentRejectsBothModesAtOnce：files 与 body 二选一，不能同时给。
func TestPresentRejectsBothModesAtOnce(t *testing.T) {
	artifact := &stubArtifact{}
	tool := presentTool(t, hosttools.Options{Artifact: artifact})
	result, err := tool.Invoke(context.Background(), rig.ToolCall{
		ID: "p", Name: "present",
		Arguments: `{"title":"t","body":"b","files":[{"path":"nope.txt"}]}`,
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !result.IsError {
		t.Fatal("files 与 body 同时给出应当报错")
	}
	if len(artifact.snapshot()) != 0 {
		t.Fatal("拒绝的调用不该到达 ArtifactPresenter")
	}
}
