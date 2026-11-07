package rig

import (
	"context"
	"encoding/json"
	"time"
)

// 工具所需的宿主能力接口：需要 UI、存储或进程状态的工具依赖它们。产品层实现
// 这些接口；Rig 的工具保持无头，只依赖契约（nil 实现会让工具返回可读的错误）。
type (
	// Asker 向人类提问并返回答案。
	Asker interface {
		Ask(ctx context.Context, question string, options []string) (answer string, err error)
	}
	// Presenter 把产物（文档、diff、表格）展示给人类并返回其标识。它支撑
	// present_artifact 工具。
	Presenter interface {
		Present(ctx context.Context, title, body string) (artifactID string, err error)
	}
	// FilePresenter 记录通过 DSH 兼容的 present 工具声明的最终交付文件；
	// 宿主可以在自己的 UI 中打开它们。
	FilePresenter interface {
		PresentFiles(ctx context.Context, files []PresentedFile) error
	}
	// PlanController 管理 plan 模式的评审与退出状态。
	PlanController interface {
		State() string
		Review(ctx context.Context, content string) (verdict string, err error)
		Exit(ctx context.Context, action string) (string, error)
	}
	// GoalStore 管理会话的目标状态。
	GoalStore interface {
		Set(text string) (id string, err error)
		Get() (id, text, status string, ok bool)
		Clear() bool
	}
	// NoticeSource 是可选的工具能力。每个工具轮次结束后，agent 会从每个实现了
	// 它的工具取走通知，并作为一条 user 角色消息追加给下一次模型调用
	// （DSH 风格的完成通知）。
	NoticeSource interface {
		PendingNotices() []string
	}
)

// PresentedFile 是通过 present 工具声明的一个最终交付文件。
type PresentedFile struct {
	Path        string `json:"path"`
	Description string `json:"description,omitempty"`
}

// Memory 是一条已存储的记忆条目。
type Memory struct {
	ID      string    `json:"id"`
	Text    string    `json:"text"`
	Scope   string    `json:"scope,omitempty"`
	Created time.Time `json:"created,omitempty"`
}

// MemoryStore 存储 agent 记忆（工作区级或全局级）。
type MemoryStore interface {
	List(ctx context.Context) ([]Memory, error)
	Add(ctx context.Context, memory Memory) (Memory, error)
	Remove(ctx context.Context, id string) bool
	Search(ctx context.Context, query string) ([]Memory, error)
}

// Skill 是一份从文件系统加载的技能文档。
type Skill struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Path        string `json:"path,omitempty"`
	Body        string `json:"body,omitempty"`
}

// SkillStore 列出并读取可用技能。
type SkillStore interface {
	List(ctx context.Context) ([]Skill, error)
	Read(ctx context.Context, name string) (Skill, error)
}

// ScheduledTask 是一条定时自动化任务。
type ScheduledTask struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Schedule string    `json:"schedule,omitempty"`
	Prompt   string    `json:"prompt"`
	Enabled  bool      `json:"enabled"`
	NextRun  time.Time `json:"nextRun,omitempty"`
	Created  time.Time `json:"created,omitempty"`
}

// Scheduler 管理定时任务（cron/间隔求值属于宿主；工具只负责描述和修改任务）。
type Scheduler interface {
	Create(ctx context.Context, task ScheduledTask) (ScheduledTask, error)
	List(ctx context.Context) ([]ScheduledTask, error)
	Update(ctx context.Context, task ScheduledTask) (ScheduledTask, error)
	Delete(ctx context.Context, id string) error
}

// SessionEvent 是会话日志中的一条事件（inspect/report 与 session_* 工具的读模型）。
type SessionEvent struct {
	Seq       int64           `json:"seq"`
	SessionID string          `json:"sessionId"`
	Type      string          `json:"type"`
	Time      time.Time       `json:"time,omitempty"`
	Payload   json.RawMessage `json:"payload,omitempty"`
}

// SessionLog 读取会话的事件日志。
type SessionLog interface {
	// Events 返回 from <= seq <= to 的事件；to <= 0 表示直到末尾。
	Events(ctx context.Context, sessionID string, from, to int64) ([]SessionEvent, error)
	// Search 返回最多 limit 条匹配事件。
	Search(ctx context.Context, query string, limit int) ([]SessionEvent, error)
	// Last 返回某个会话最近的若干事件。
	Last(ctx context.Context, sessionID string, limit int) ([]SessionEvent, error)
}

// ReviewRequest 请求评审者评估代码或计划。
type ReviewRequest struct {
	// Kind 是 "code"、"plan"、"diff" 或宿主定义的值。
	Kind string
	// Target 是路径、diff 标识或会话 id。
	Target string
	// Prompt 是评审指令。
	Prompt string
	// Content 是待评审材料（可选；评审者也可以自行加载）。
	Content string
}

// ReviewFinding 是一条评审结果项。
type ReviewFinding struct {
	Severity string `json:"severity"`
	File     string `json:"file,omitempty"`
	Line     int    `json:"line,omitempty"`
	Message  string `json:"message"`
}

// ReviewResult 是评审者的结论。
type ReviewResult struct {
	Verdict  string          `json:"verdict"`
	Summary  string          `json:"summary,omitempty"`
	Findings []ReviewFinding `json:"findings,omitempty"`
}

// Reviewer 评审代码/计划（宿主可接模型或人类）。
type Reviewer interface {
	Review(ctx context.Context, request ReviewRequest) (ReviewResult, error)
}

// GoalUpdateAction 是会话目标的一次生命周期状态转换。
type GoalUpdateAction string

const (
	// GoalComplete 把目标标记为完成。
	GoalComplete GoalUpdateAction = "complete"
	// GoalBlocked 把目标标记为受阻（必须给出原因）。
	GoalBlocked GoalUpdateAction = "blocked"
	// GoalPause 暂停目标。
	GoalPause GoalUpdateAction = "pause"
	// GoalResume 恢复暂停的目标。
	GoalResume GoalUpdateAction = "resume"
	// GoalEdit 替换目标内容。
	GoalEdit GoalUpdateAction = "edit"
)

// GoalUpdater 是 GoalStore 可选的生命周期扩展。当存储实现了它时，goal 工具会
// 接受 action=update；只维护文本目标的宿主可以只实现 GoalStore。
type GoalUpdater interface {
	// Update 应用 action（complete/blocked/pause/resume/edit）并返回结果状态。
	// blocked 时 text 携带原因，edit 时 text 是新的目标内容。
	Update(ctx context.Context, action GoalUpdateAction, text string) (status string, err error)
}

// TeamTask 是共享团队任务板上的一张任务卡。
type TeamTask struct {
	ID          string    `json:"id"`
	Subject     string    `json:"subject"`
	Description string    `json:"description,omitempty"`
	Status      string    `json:"status"` // pending、in_progress、completed
	Owner       string    `json:"owner,omitempty"`
	BlockedBy   []string  `json:"blockedBy,omitempty"`
	WriteScopes []string  `json:"writeScopes,omitempty"`
	Created     time.Time `json:"created,omitempty"`
	Updated     time.Time `json:"updated,omitempty"`
}

// TeamTaskFilter 用于筛选 List 返回的任务。
type TeamTaskFilter struct {
	Status string
	Owner  string
}

// TeamTaskUpdate 是单个任务的部分更新。nil 字段表示保持不变。
type TeamTaskUpdate struct {
	Subject     *string
	Description *string
	Status      *string
	Owner       *string
	AddBlocked  []string
	DelBlocked  []string
	WriteScopes *[]string
}

// TeamTaskManager 是 team_task_* 工具（create/list/get/update）背后的共享任务板。
// SDK 自带内存实现；宿主可以接入数据库。
type TeamTaskManager interface {
	Create(ctx context.Context, task TeamTask) (TeamTask, error)
	List(ctx context.Context, filter TeamTaskFilter) ([]TeamTask, error)
	Get(ctx context.Context, id string) (TeamTask, error)
	Update(ctx context.Context, id string, update TeamTaskUpdate) (TeamTask, error)
}
