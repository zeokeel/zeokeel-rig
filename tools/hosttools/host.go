package hosttools

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/zeokeel/zeokeel-rig"
)

// 编译期证明：进程内 host 满足每一个宿主接口。
var (
	_ rig.Asker          = (*Host)(nil)
	_ rig.Presenter      = (*Host)(nil)
	_ rig.FilePresenter  = (*Host)(nil)
	_ rig.PlanController = (*Host)(nil)
	_ rig.GoalStore      = (*Host)(nil)
	_ rig.GoalUpdater    = (*Host)(nil)
)

// Host 是 SDK 的进程内 host：以并发安全、进程本地状态实现 rig.Asker、rig.Presenter、
// rig.PlanController、rig.GoalStore 与 rig.GoalUpdater，以及目标域中 DSH 修订那一半
// （hosttools.GoalRevisionStore）。
//
// 它让独立 agent（或测试）无需产品 UI 就能使用宿主工具：可以通过 SetAnswers 预置回答，
// 展示过的产物会被保留以供检查，plan/目标状态遵循一个小型确定性状态机。产品通过 Options
// 传入自己的实现来替换它。
type Host struct {
	mu sync.Mutex

	answers      []string
	questions    []Question
	artifacts    []Artifact
	nextArtifact int
	deliverables []rig.PresentedFile

	planState   string
	planContent string
	planVerdict string

	goalID        string
	goalText      string
	goalStatus    string
	goalReason    string
	goalRevision  int
	goalRounds    int
	goalMaxRounds int
	nextGoal      int
}

// Question 是一次被记录的 ask_user 调用。
type Question struct {
	Question string
	Options  []string
	Answer   string
}

// Artifact 是一次被记录的 present 调用。
type Artifact struct {
	ID    string
	Title string
	Body  string
}

// NewMemoryHost 创建一个处于 "inactive" plan 状态的空进程内 host。
func NewMemoryHost() *Host {
	return &Host{planState: "inactive"}
}

// SetAnswers 按顺序预置后续 Ask 调用返回的答案。队列为空时 Ask 返回空答案且不报错。
func (h *Host) SetAnswers(answers ...string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.answers = slices.Clone(answers)
}

// Questions 返回已记录的 ask_user 调用快照。
func (h *Host) Questions() []Question {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.questions)
}

// Artifacts 返回已展示产物的快照。
func (h *Host) Artifacts() []Artifact {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.artifacts)
}

// Ask 实现 rig.Asker。
func (h *Host) Ask(ctx context.Context, question string, options []string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	answer := ""
	if len(h.answers) > 0 {
		answer = h.answers[0]
		h.answers = h.answers[1:]
	}
	h.questions = append(h.questions, Question{
		Question: question,
		Options:  slices.Clone(options),
		Answer:   answer,
	})
	return answer, nil
}

// Present 实现 rig.Presenter。
func (h *Host) Present(ctx context.Context, title, body string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.nextArtifact++
	id := fmt.Sprintf("artifact-%d", h.nextArtifact)
	h.artifacts = append(h.artifacts, Artifact{ID: id, Title: title, Body: body})
	return id, nil
}

// PresentFiles 实现 rig.FilePresenter。
func (h *Host) PresentFiles(ctx context.Context, files []rig.PresentedFile) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.deliverables = append(h.deliverables, slices.Clone(files)...)
	return nil
}

// Deliverables 返回 present 文件声明的快照。
func (h *Host) Deliverables() []rig.PresentedFile {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.deliverables)
}

// State 实现 rig.PlanController。
func (h *Host) State() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.planState
}

// Review 实现 rig.PlanController：记录计划并进入 "reviewing" 状态。进程内 host 无法
// 评判计划，因此 verdict 为 "pending"（产品评审者会返回真实 verdict）。
func (h *Host) Review(ctx context.Context, content string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if strings.TrimSpace(content) == "" {
		return "", errors.New("hosttools: plan content is required")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.planContent = content
	h.planState = "reviewing"
	h.planVerdict = "pending"
	return h.planVerdict, nil
}

// Exit 实现 rig.PlanController：approve 记录 "approved"，cancel 丢弃计划并回到
// "inactive"；status 报告当前状态。
func (h *Host) Exit(ctx context.Context, action string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	action = strings.ToLower(strings.TrimSpace(action))
	h.mu.Lock()
	defer h.mu.Unlock()
	switch action {
	case "approve":
		h.planState = "approved"
		return "plan mode: approved", nil
	case "cancel":
		h.planState = "inactive"
		h.planContent = ""
		h.planVerdict = ""
		return "plan mode: cancelled", nil
	case "status":
		return h.planState, nil
	default:
		return "", fmt.Errorf("hosttools: unknown plan action %q (want approve, cancel or status)", action)
	}
}

// PlanContent 返回最近一次评审的计划文本（没有时为空）。
func (h *Host) PlanContent() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.planContent
}

// Set 实现 rig.GoalStore：保存（或替换）目标并返回新 id。
func (h *Host) Set(text string) (string, error) {
	if strings.TrimSpace(text) == "" {
		return "", errors.New("hosttools: goal text is required")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.nextGoal++
	h.goalID = fmt.Sprintf("goal-%d", h.nextGoal)
	h.goalText = text
	h.goalStatus = "active"
	h.goalReason = ""
	h.goalRevision = 1
	h.goalRounds = 0
	h.goalMaxRounds = DefaultMaxGoalRounds
	return h.goalID, nil
}

// Get 实现 rig.GoalStore。
func (h *Host) Get() (id, text, status string, ok bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.goalID == "" {
		return "", "", "", false
	}
	return h.goalID, h.goalText, h.goalStatus, true
}

// Clear 实现 rig.GoalStore。
func (h *Host) Clear() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.goalID == "" {
		return false
	}
	h.goalID = ""
	h.goalText = ""
	h.goalStatus = ""
	h.goalReason = ""
	h.goalRevision = 0
	h.goalRounds = 0
	h.goalMaxRounds = 0
	return true
}

// GoalReason 返回最后一次 blocked 迁移记录的原因（其他情况为空）。
func (h *Host) GoalReason() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.goalReason
}

// Update 用一个小型确定性状态机实现 rig.GoalUpdater：
//
//	active  --pause-->  paused
//	paused  --resume--> active
//	active, paused --complete--> completed
//	active, paused --blocked(reason)--> blocked
//	active, paused, blocked --edit(text)--> 状态不变，文本更新
//
// 非法迁移（例如完成一个 blocked 目标、暂停已完成目标、恢复活跃目标）会返回可读错误，
// 并保持目标不变。
func (h *Host) Update(ctx context.Context, action rig.GoalUpdateAction, text string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.goalID == "" {
		return "", errors.New("hosttools: no goal is set")
	}
	switch action {
	case rig.GoalComplete:
		if h.goalStatus != "active" && h.goalStatus != "paused" {
			return "", fmt.Errorf("hosttools: cannot complete a %s goal", h.goalStatus)
		}
		h.goalStatus = "completed"
		h.goalReason = ""
	case rig.GoalBlocked:
		reason := strings.TrimSpace(text)
		if reason == "" {
			return "", errors.New("hosttools: blocked action requires a reason")
		}
		if h.goalStatus != "active" && h.goalStatus != "paused" {
			return "", fmt.Errorf("hosttools: cannot block a %s goal", h.goalStatus)
		}
		h.goalStatus = "blocked"
		h.goalReason = reason
	case rig.GoalPause:
		if h.goalStatus != "active" {
			return "", fmt.Errorf("hosttools: cannot pause a %s goal", h.goalStatus)
		}
		h.goalStatus = "paused"
		h.goalReason = ""
	case rig.GoalResume:
		if h.goalStatus != "paused" {
			return "", fmt.Errorf("hosttools: cannot resume a %s goal", h.goalStatus)
		}
		h.goalStatus = "active"
		h.goalReason = ""
	case rig.GoalEdit:
		if strings.TrimSpace(text) == "" {
			return "", errors.New("hosttools: edit action requires new goal text")
		}
		if h.goalStatus == "completed" {
			return "", fmt.Errorf("hosttools: cannot edit a %s goal", h.goalStatus)
		}
		h.goalText = text
	default:
		return "", fmt.Errorf("hosttools: unknown goal update %q", action)
	}
	h.bumpGoalRevisionLocked()
	return h.goalStatus, nil
}
