package kit_test

// present 的内容产物与 goal/plan 注入：这两组工具以前在 kit 里"恒缺注入入口"
// （cfgUnavailableTools 无条件摘掉），宿主只能自建 present 工具与 goal 工具。
//
// 这里钉住"给了就要留、并且真的用注入的实现"。

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/kit"
	"github.com/zeokeel/zeokeel-rig/tools/hosttools"
)

type stubArtifactPresenter struct {
	mu    sync.Mutex
	calls []struct{ title, body string }
}

func (s *stubArtifactPresenter) Present(_ context.Context, title, body string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, struct{ title, body string }{title, body})
	return "a-42", nil
}

type stubPlan struct{}

func (p *stubPlan) State() string { return "idle" }
func (p *stubPlan) Review(context.Context, string) (string, error) {
	return "approved", nil
}
func (p *stubPlan) Exit(context.Context, string) (string, error) { return "exited:approved", nil }

// TestKitKeepsGoalToolsWhenGoalsInjected：注入 GoalStore 后 get_goal 等工具必须保留。
func TestKitKeepsGoalToolsWhenGoalsInjected(t *testing.T) {
	host := hosttools.NewMemoryHost()
	if _, err := host.Set("把构建修好"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	agent, err := kit.New(kit.Config{
		Model:         "test-model",
		Root:          t.TempDir(),
		Goals:         host,
		GoalRevisions: host,
	})
	if err != nil {
		t.Fatalf("kit.New: %v", err)
	}
	defer func() { _ = agent.Shutdown(context.Background()) }()

	tool, ok := agent.Tool("get_goal")
	if !ok {
		t.Fatal("注入 Goals 后 get_goal 仍然被摘掉了")
	}
	result, err := tool.Invoke(context.Background(), rig.ToolCall{ID: "g", Name: "get_goal", Arguments: `{}`})
	if err != nil || result.IsError {
		t.Fatalf("get_goal: err=%v content=%q", err, result.Content)
	}
	if !strings.Contains(result.Content, "把构建修好") {
		t.Fatalf("get_goal 没有读注入的 GoalStore：\n%s", result.Content)
	}
}

// TestKitKeepsPlanToolsWhenPlanInjected：注入 PlanController 后 plan_review 必须保留。
func TestKitKeepsPlanToolsWhenPlanInjected(t *testing.T) {
	agent, err := kit.New(kit.Config{Model: "test-model", Root: t.TempDir(), PlanController: &stubPlan{}})
	if err != nil {
		t.Fatalf("kit.New: %v", err)
	}
	defer func() { _ = agent.Shutdown(context.Background()) }()

	if _, ok := agent.Tool("plan_review"); !ok {
		t.Fatal("注入 PlanController 后 plan_review 仍然被摘掉了")
	}
}

// TestKitPresentArtifactMode：注入 ArtifactPresenter 后 present 的 body 形态可用。
func TestKitPresentArtifactMode(t *testing.T) {
	presenter := &stubArtifactPresenter{}
	agent, err := kit.New(kit.Config{Model: "test-model", Root: t.TempDir(), ArtifactPresenter: presenter})
	if err != nil {
		t.Fatalf("kit.New: %v", err)
	}
	defer func() { _ = agent.Shutdown(context.Background()) }()

	tool, ok := agent.Tool("present")
	if !ok {
		t.Fatal("注入 ArtifactPresenter 后 present 仍然被摘掉了")
	}
	result, err := tool.Invoke(context.Background(), rig.ToolCall{
		ID: "p", Name: "present", Arguments: `{"title":"报告","body":"# 结论"}`,
	})
	if err != nil {
		t.Fatalf("present.Invoke: %v", err)
	}
	if result.IsError {
		t.Fatalf("present 失败: %q", result.Content)
	}
	if len(presenter.calls) != 1 {
		t.Fatalf("ArtifactPresenter 被调用 %d 次, want 1", len(presenter.calls))
	}
	if presenter.calls[0].title != "报告" {
		t.Fatalf("title 没传对: %+v", presenter.calls[0])
	}
}
