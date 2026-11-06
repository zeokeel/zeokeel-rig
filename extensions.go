package rig

import (
	"context"
	"encoding/json"
	"time"
)

// Host capability interfaces used by the tools that need a UI, a store or
// process state. Products implement them; the Rig tools stay headless and only
// depend on these contracts (nil implementations produce readable tool errors).
type (
	// Asker asks the human a question and returns the answer.
	Asker interface {
		Ask(ctx context.Context, question string, options []string) (answer string, err error)
	}
	// Presenter shows an artifact (document, diff, table) to the human and
	// returns its identifier.
	Presenter interface {
		Present(ctx context.Context, title, body string) (artifactID string, err error)
	}
	// PlanController owns plan-mode review/exit state.
	PlanController interface {
		State() string
		Review(ctx context.Context, content string) (verdict string, err error)
		Exit(ctx context.Context, action string) (string, error)
	}
	// GoalStore owns the session's goal state.
	GoalStore interface {
		Set(text string) (id string, err error)
		Get() (id, text, status string, ok bool)
		Clear() bool
	}
)

// Memory is one stored memory entry.
type Memory struct {
	ID      string    `json:"id"`
	Text    string    `json:"text"`
	Scope   string    `json:"scope,omitempty"`
	Created time.Time `json:"created,omitempty"`
}

// MemoryStore stores agent memories (workspace or global scope).
type MemoryStore interface {
	List(ctx context.Context) ([]Memory, error)
	Add(ctx context.Context, memory Memory) (Memory, error)
	Remove(ctx context.Context, id string) bool
	Search(ctx context.Context, query string) ([]Memory, error)
}

// Skill is one skill document loaded from the filesystem.
type Skill struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Path        string `json:"path,omitempty"`
	Body        string `json:"body,omitempty"`
}

// SkillStore lists and reads available skills.
type SkillStore interface {
	List(ctx context.Context) ([]Skill, error)
	Read(ctx context.Context, name string) (Skill, error)
}

// ScheduledTask is one scheduled automation task.
type ScheduledTask struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Schedule string    `json:"schedule,omitempty"`
	Prompt   string    `json:"prompt"`
	Enabled  bool      `json:"enabled"`
	NextRun  time.Time `json:"nextRun,omitempty"`
	Created  time.Time `json:"created,omitempty"`
}

// Scheduler manages scheduled tasks (cron/interval evaluation belongs to the
// host; the tools only describe and mutate tasks).
type Scheduler interface {
	Create(ctx context.Context, task ScheduledTask) (ScheduledTask, error)
	List(ctx context.Context) ([]ScheduledTask, error)
	Update(ctx context.Context, task ScheduledTask) (ScheduledTask, error)
	Delete(ctx context.Context, id string) error
}

// SessionEvent is one event of a session log (read model for inspect/report and
// session_query tools).
type SessionEvent struct {
	Seq       int64           `json:"seq"`
	SessionID string          `json:"sessionId"`
	Type      string          `json:"type"`
	Time      time.Time       `json:"time,omitempty"`
	Payload   json.RawMessage `json:"payload,omitempty"`
}

// SessionLog reads a session's event log.
type SessionLog interface {
	// Events returns events with from <= seq <= to; to <= 0 means "until the end".
	Events(ctx context.Context, sessionID string, from, to int64) ([]SessionEvent, error)
	// Search returns matching events up to limit.
	Search(ctx context.Context, query string, limit int) ([]SessionEvent, error)
	// Last returns the most recent events of one session.
	Last(ctx context.Context, sessionID string, limit int) ([]SessionEvent, error)
}

// ReviewRequest asks a reviewer to evaluate code or a plan.
type ReviewRequest struct {
	// Kind is "code", "plan", "diff" or a host-defined value.
	Kind string
	// Target is a path, diff identifier or session id.
	Target string
	// Prompt is the reviewer instruction.
	Prompt string
	// Content is the material to review (optional; a reviewer may load it).
	Content string
}

// ReviewFinding is one review result item.
type ReviewFinding struct {
	Severity string `json:"severity"`
	File     string `json:"file,omitempty"`
	Line     int    `json:"line,omitempty"`
	Message  string `json:"message"`
}

// ReviewResult is the reviewer's verdict.
type ReviewResult struct {
	Verdict  string          `json:"verdict"`
	Summary  string          `json:"summary,omitempty"`
	Findings []ReviewFinding `json:"findings,omitempty"`
}

// Reviewer reviews code/plans (hosts wire it to a model or a human).
type Reviewer interface {
	Review(ctx context.Context, request ReviewRequest) (ReviewResult, error)
}

// GoalUpdateAction is a lifecycle transition for a session goal.
type GoalUpdateAction string

const (
	// GoalComplete marks the goal completed.
	GoalComplete GoalUpdateAction = "complete"
	// GoalBlocked marks the goal blocked (reason required).
	GoalBlocked GoalUpdateAction = "blocked"
	// GoalPause pauses the goal.
	GoalPause GoalUpdateAction = "pause"
	// GoalResume resumes a paused goal.
	GoalResume GoalUpdateAction = "resume"
	// GoalEdit replaces the goal objective.
	GoalEdit GoalUpdateAction = "edit"
)

// GoalUpdater is the optional lifecycle half of a GoalStore. The goal tool
// accepts action=update when its store implements it; hosts that only keep a
// text goal can implement GoalStore alone.
type GoalUpdater interface {
	// Update applies action (complete/blocked/pause/resume/edit) and returns
	// the resulting status. text carries the reason for blocked/ or the new
	// objective for edit/.
	Update(ctx context.Context, action GoalUpdateAction, text string) (status string, err error)
}

// TeamTask is one card on the shared team task board.
type TeamTask struct {
	ID          string    `json:"id"`
	Subject     string    `json:"subject"`
	Description string    `json:"description,omitempty"`
	Status      string    `json:"status"` // pending, in_progress, completed
	Owner       string    `json:"owner,omitempty"`
	BlockedBy   []string  `json:"blockedBy,omitempty"`
	WriteScopes []string  `json:"writeScopes,omitempty"`
	Created     time.Time `json:"created,omitempty"`
	Updated     time.Time `json:"updated,omitempty"`
}

// TeamTaskFilter selects tasks for List.
type TeamTaskFilter struct {
	Status string
	Owner  string
}

// TeamTaskUpdate is a partial update for one task. Nil fields are unchanged.
type TeamTaskUpdate struct {
	Subject     *string
	Description *string
	Status      *string
	Owner       *string
	AddBlocked  []string
	DelBlocked  []string
	WriteScopes *[]string
}

// TeamTaskManager is the shared task board behind the team_task_* tools
// (create/list/get/update). The SDK ships an in-memory implementation; hosts
// can back it with a database.
type TeamTaskManager interface {
	Create(ctx context.Context, task TeamTask) (TeamTask, error)
	List(ctx context.Context, filter TeamTaskFilter) ([]TeamTask, error)
	Get(ctx context.Context, id string) (TeamTask, error)
	Update(ctx context.Context, id string, update TeamTaskUpdate) (TeamTask, error)
}

// ReviewRequest/ReviewResult/Reviewer live above; the review tools use
// Reviewer with a model- or human-backed implementation.
