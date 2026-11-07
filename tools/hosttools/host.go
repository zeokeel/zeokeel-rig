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

// Compile-time proof that the in-memory host satisfies every host interface.
var (
	_ rig.Asker          = (*Host)(nil)
	_ rig.Presenter      = (*Host)(nil)
	_ rig.FilePresenter  = (*Host)(nil)
	_ rig.PlanController = (*Host)(nil)
	_ rig.GoalStore      = (*Host)(nil)
	_ rig.GoalUpdater    = (*Host)(nil)
)

// Host is the SDK's in-memory host: it implements rig.Asker, rig.Presenter,
// rig.PlanController, rig.GoalStore and rig.GoalUpdater — plus the DSH revision
// half of the goal domain (hosttools.GoalRevisionStore) — with
// concurrency-safe, process-local state.
//
// It exists so a standalone agent (or a test) can use the host tools without a
// product UI: answers can be scripted with SetAnswers, presented artifacts are
// retained for inspection, and plan/goal state follows a small deterministic
// state machine. A product replaces it with its real implementations by passing
// them in Options.
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

// Question is one recorded ask_user call.
type Question struct {
	Question string
	Options  []string
	Answer   string
}

// Artifact is one recorded present call.
type Artifact struct {
	ID    string
	Title string
	Body  string
}

// NewMemoryHost creates an empty in-memory host in the "inactive" plan state.
func NewMemoryHost() *Host {
	return &Host{planState: "inactive"}
}

// SetAnswers scripts the answers returned by subsequent Ask calls (in order).
// When the queue is empty Ask returns an empty answer and no error.
func (h *Host) SetAnswers(answers ...string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.answers = slices.Clone(answers)
}

// Questions returns a snapshot of the recorded ask_user calls.
func (h *Host) Questions() []Question {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.questions)
}

// Artifacts returns a snapshot of the presented artifacts.
func (h *Host) Artifacts() []Artifact {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.artifacts)
}

// Ask implements rig.Asker.
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

// Present implements rig.Presenter.
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

// PresentFiles implements rig.FilePresenter.
func (h *Host) PresentFiles(ctx context.Context, files []rig.PresentedFile) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.deliverables = append(h.deliverables, slices.Clone(files)...)
	return nil
}

// Deliverables returns a snapshot of the present file declarations.
func (h *Host) Deliverables() []rig.PresentedFile {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.deliverables)
}

// State implements rig.PlanController.
func (h *Host) State() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.planState
}

// Review implements rig.PlanController: it records the plan and enters the
// "reviewing" state. The in-memory host cannot judge a plan, so the verdict is
// "pending" (a product reviewer returns a real verdict).
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

// Exit implements rig.PlanController. approve records "approved", cancel
// discards the plan and returns to "inactive"; status reports the state.
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

// PlanContent returns the last reviewed plan text (empty when none).
func (h *Host) PlanContent() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.planContent
}

// Set implements rig.GoalStore: it stores (or replaces) the goal and returns a
// new id.
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

// Get implements rig.GoalStore.
func (h *Host) Get() (id, text, status string, ok bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.goalID == "" {
		return "", "", "", false
	}
	return h.goalID, h.goalText, h.goalStatus, true
}

// Clear implements rig.GoalStore.
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

// GoalReason returns the reason recorded by the last blocked transition (empty
// otherwise).
func (h *Host) GoalReason() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.goalReason
}

// Update implements rig.GoalUpdater with a small deterministic state machine:
//
//	active  --pause-->  paused
//	paused  --resume--> active
//	active, paused --complete--> completed
//	active, paused --blocked(reason)--> blocked
//	active, paused, blocked --edit(text)--> same status, new text
//
// Illegal transitions (for example completing a blocked goal, pausing a
// completed one, resuming an active one) return a readable error and leave the
// goal unchanged.
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
