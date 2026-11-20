package kit_test

// 装配入口的注入契约：宿主给 kit 的能力必须真的到达工具。
//
// 这几个字段以前只有 `full.AllOptions` 有、kit 没有：接 MCP/技能库/共享任务板/
// 调度器/评审器的宿主只能 `Config.Tools = full.All(...)` 自建整套装配，而那条路径
// 会跳过 `full.NewRuntime`（丢懒加载、体检、grant 工具）。这里逐个钉住"给了就要用"。

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/kit"
)

// --- 桩 ---------------------------------------------------------------------

type stubSkills struct {
	mu     sync.Mutex
	skills []rig.Skill
}

func (s *stubSkills) List(context.Context) ([]rig.Skill, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]rig.Skill(nil), s.skills...), nil
}

func (s *stubSkills) Read(_ context.Context, name string) (rig.Skill, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, skill := range s.skills {
		if skill.Name == name {
			return skill, nil
		}
	}
	return rig.Skill{}, errNotFound
}

type stubScheduler struct {
	mu    sync.Mutex
	tasks []rig.ScheduledTask
}

func (s *stubScheduler) Create(_ context.Context, task rig.ScheduledTask) (rig.ScheduledTask, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tasks = append(s.tasks, task)
	return task, nil
}

func (s *stubScheduler) List(context.Context) ([]rig.ScheduledTask, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]rig.ScheduledTask(nil), s.tasks...), nil
}

func (s *stubScheduler) Update(_ context.Context, task rig.ScheduledTask) (rig.ScheduledTask, error) {
	return task, nil
}

func (s *stubScheduler) Delete(context.Context, string) error { return nil }

type stubTeamTasks struct {
	mu    sync.Mutex
	tasks []rig.TeamTask
}

func (s *stubTeamTasks) Create(_ context.Context, task rig.TeamTask) (rig.TeamTask, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tasks = append(s.tasks, task)
	return task, nil
}

func (s *stubTeamTasks) List(context.Context, rig.TeamTaskFilter) ([]rig.TeamTask, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]rig.TeamTask(nil), s.tasks...), nil
}

func (s *stubTeamTasks) Get(context.Context, string) (rig.TeamTask, error) {
	return rig.TeamTask{}, errNotFound
}

func (s *stubTeamTasks) Update(_ context.Context, id string, _ rig.TeamTaskUpdate) (rig.TeamTask, error) {
	return rig.TeamTask{ID: id}, nil
}

type stubError string

func (e stubError) Error() string { return string(e) }

const errNotFound = stubError("not found")

// --- 用例 -------------------------------------------------------------------

// TestKitForwardsSkillStore：注入的技能库必须就是 skill 工具读的那个。
func TestKitForwardsSkillStore(t *testing.T) {
	agent, err := kit.New(kit.Config{
		Model:  "test-model",
		Root:   t.TempDir(),
		Skills: &stubSkills{skills: []rig.Skill{{Name: "demo-skill", Description: "from the host store", Body: "# demo"}}},
	})
	if err != nil {
		t.Fatalf("kit.New: %v", err)
	}
	defer func() { _ = agent.Shutdown(context.Background()) }()

	tool, ok := agent.Tool("skill")
	if !ok {
		t.Fatal("kit 没有装配 skill 工具")
	}
	result, err := tool.Invoke(context.Background(), rig.ToolCall{ID: "s", Name: "skill", Arguments: `{"action":"list"}`})
	if err != nil || result.IsError {
		t.Fatalf("skill.Invoke: err=%v content=%q", err, result.Content)
	}
	if !strings.Contains(result.Content, "demo-skill") {
		t.Fatalf("skill 工具没有用注入的存储：\n%s", result.Content)
	}
}

// TestKitForwardsScheduler：注入的调度器必须就是 schedule_* 用的那个。
func TestKitForwardsScheduler(t *testing.T) {
	scheduler := &stubScheduler{tasks: []rig.ScheduledTask{{ID: "t1", Name: "nightly", Prompt: "run tests", Enabled: true, Created: time.Now()}}}
	agent, err := kit.New(kit.Config{Model: "test-model", Root: t.TempDir(), Scheduler: scheduler})
	if err != nil {
		t.Fatalf("kit.New: %v", err)
	}
	defer func() { _ = agent.Shutdown(context.Background()) }()

	tool, ok := agent.Tool("schedule_list")
	if !ok {
		t.Fatal("kit 没有装配 schedule_list（注入 Scheduler 后它应当出现）")
	}
	result, err := tool.Invoke(context.Background(), rig.ToolCall{ID: "l", Name: "schedule_list", Arguments: `{}`})
	if err != nil || result.IsError {
		t.Fatalf("schedule_list.Invoke: err=%v content=%q", err, result.Content)
	}
	if !strings.Contains(result.Content, "nightly") {
		t.Fatalf("schedule_list 没有读注入的调度器：\n%s", result.Content)
	}
}

// TestKitForwardsTeamTasks：注入的共享任务板必须就是 team_task_* 用的那个。
func TestKitForwardsTeamTasks(t *testing.T) {
	board := &stubTeamTasks{tasks: []rig.TeamTask{{ID: "a1", Subject: "共享任务板里的任务", Status: "pending"}}}
	// team_task_* 属于多智能体编排工具：默认的 personal 预设会把它们过滤掉，
	// 这里显式要全量工具集。
	agent, err := kit.New(kit.Config{
		Model:      "test-model",
		Root:       t.TempDir(),
		TeamTasks:  board,
		ToolPreset: "all",
	})
	if err != nil {
		t.Fatalf("kit.New: %v", err)
	}
	defer func() { _ = agent.Shutdown(context.Background()) }()

	tool, ok := agent.Tool("team_task_list")
	if !ok {
		t.Fatal("kit 没有装配 team_task_list（注入 TeamTasks 后它应当出现）")
	}
	result, err := tool.Invoke(context.Background(), rig.ToolCall{ID: "t", Name: "team_task_list", Arguments: `{}`})
	if err != nil || result.IsError {
		t.Fatalf("team_task_list.Invoke: err=%v content=%q", err, result.Content)
	}
	if !strings.Contains(result.Content, "共享任务板里的任务") {
		t.Fatalf("team_task_list 没有读注入的任务板：\n%s", result.Content)
	}
}
