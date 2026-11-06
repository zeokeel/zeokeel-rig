package hosttools

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	v1 "github.com/zeokeel/zeokeel-rig/contracts/v1"
)

// Compile-time proof that the in-memory host satisfies every host interface.
var (
	_ v1.Asker          = (*Host)(nil)
	_ v1.Presenter      = (*Host)(nil)
	_ v1.PlanController = (*Host)(nil)
	_ v1.GoalStore      = (*Host)(nil)
)

// Host is the SDK's in-memory host: it implements v1.Asker, v1.Presenter,
// v1.PlanController and v1.GoalStore with concurrency-safe, process-local state.
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

	planState   string
	planContent string
	planVerdict string

	goalID     string
	goalText   string
	goalStatus string
	nextGoal   int
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
	h.answers = append([]string(nil), answers...)
}

// Questions returns a snapshot of the recorded ask_user calls.
func (h *Host) Questions() []Question {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]Question(nil), h.questions...)
}

// Artifacts returns a snapshot of the presented artifacts.
func (h *Host) Artifacts() []Artifact {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]Artifact(nil), h.artifacts...)
}

// Ask implements v1.Asker.
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
		Options:  append([]string(nil), options...),
		Answer:   answer,
	})
	return answer, nil
}

// Present implements v1.Presenter.
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

// State implements v1.PlanController.
func (h *Host) State() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.planState
}

// Review implements v1.PlanController: it records the plan and enters the
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

// Exit implements v1.PlanController. approve records "approved", cancel
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

// Set implements v1.GoalStore: it stores (or replaces) the goal and returns a
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
	return h.goalID, nil
}

// Get implements v1.GoalStore.
func (h *Host) Get() (id, text, status string, ok bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.goalID == "" {
		return "", "", "", false
	}
	return h.goalID, h.goalText, h.goalStatus, true
}

// Clear implements v1.GoalStore.
func (h *Host) Clear() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.goalID == "" {
		return false
	}
	h.goalID = ""
	h.goalText = ""
	h.goalStatus = ""
	return true
}
