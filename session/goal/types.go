package goal

import (
	"context"
	"errors"
	"time"

	"github.com/zeokeel/zeokeel-rig"
)

// Phase is the persistent lifecycle phase of a session goal.
type Phase string

const (
	PhaseActive   Phase = "active"
	PhasePaused   Phase = "paused"
	PhaseBlocked  Phase = "blocked"
	PhaseComplete Phase = "complete"
)

// Activation is the live continuation state (not persisted).
type Activation string

const (
	ActivationArmed    Activation = "armed"
	ActivationDisarmed Activation = "disarmed"
)

// BlockedReason is attached to a blocked goal.
type BlockedReason struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

const (
	BlockedCodeRoundLimit    = "round-limit"
	BlockedCodeModelReported = "model-reported"

	DefaultMaxGoalRounds = 256
	MinGoalBlockedRounds = 3
	RoundLimitMessage    = "the goal reached its automatic continuation round limit"
)

// Action is one goal lifecycle mutation.
type Action string

const (
	ActionEdit     Action = "edit"
	ActionPause    Action = "pause"
	ActionResume   Action = "resume"
	ActionComplete Action = "complete"
	ActionBlocked  Action = "blocked"
)

// Goal is one session's durable goal snapshot.
type Goal struct {
	SessionID     string         `json:"sessionId,omitempty"`
	ID            string         `json:"id"`
	Revision      int            `json:"revision"`
	Objective     string         `json:"objective,omitempty"`
	Phase         Phase          `json:"phase"`
	RoundsStarted int            `json:"roundsStarted"`
	MaxGoalRounds int            `json:"maxGoalRounds"`
	BlockedReason *BlockedReason `json:"blockedReason,omitempty"`
	Activation    Activation     `json:"activation,omitempty"`
	UpdatedAt     time.Time      `json:"updatedAt,omitempty"`
}

// Ref is the compare-and-set identity.
type Ref struct {
	ID       string
	Revision int
}

// Update is one validated mutation (zero values mean "omitted").
type Update struct {
	Action        Action
	Objective     string
	MaxGoalRounds int
	BlockedReason string
}

// Errors are the classification sentinels.
var (
	ErrGoalNotFound            = errors.New("goal: no goal for this session")
	ErrGoalStaleRef            = errors.New("goal: stale goal reference")
	ErrGoalInvalidTransition   = errors.New("goal: invalid goal transition")
	ErrGoalRoundLimit          = errors.New("goal: goal round limit reached")
	ErrGoalConflict            = errors.New("goal: a goal already exists for this session")
	ErrGoalDirectHumanRequired = errors.New("goal: this action requires a direct human turn")
)

// EventLog is the narrow durable session-log view the service needs.
type EventLog interface {
	Events(ctx context.Context, sessionID string, from, to int64) ([]rig.SessionEvent, error)
	AppendEvent(ctx context.Context, event rig.SessionEvent) error
}

// SessionExistsFunc reports whether a session still exists; nil skips the check.
type SessionExistsFunc func(ctx context.Context, sessionID string) (bool, error)

// Compatibility aliases matching the product contract names.
type (
	GoalPhase         = Phase
	GoalActivation    = Activation
	GoalBlockedReason = BlockedReason
	GoalRef           = Ref
	GoalUpdate        = Update
	GoalAction        = Action
)

const (
	GoalPhaseActive   = PhaseActive
	GoalPhasePaused   = PhasePaused
	GoalPhaseBlocked  = PhaseBlocked
	GoalPhaseComplete = PhaseComplete

	GoalActivationArmed    = ActivationArmed
	GoalActivationDisarmed = ActivationDisarmed

	GoalActionEdit     = ActionEdit
	GoalActionPause    = ActionPause
	GoalActionResume   = ActionResume
	GoalActionComplete = ActionComplete
	GoalActionBlocked  = ActionBlocked

	GoalBlockedCodeRoundLimit    = BlockedCodeRoundLimit
	GoalBlockedCodeModelReported = BlockedCodeModelReported
	GoalRoundLimitMessage        = RoundLimitMessage
)
