// Package insights folds a session event log into one windowed cross-session
// usage snapshot: token totals, steps, turns, tool calls, active sessions, one
// bucket per local calendar day / model / workspace / session and a leaderboard
// of the busiest sessions.
//
// The package is host-agnostic. It reads through a narrow EventLog (a session
// list plus one bounded Last per session), takes the clock and time zone as
// options, and prices usage through an optional Cost callback so the host keeps
// its model catalog and currency outside the SDK. It never scans a whole event
// table: a session updated before the window is not read, and a session whose
// read hits the per-session bound is reported as truncated instead of being
// silently under-counted.
//
// Accounting rules (kept in one place so totals cannot drift):
//
//   - tokens: one contribution per model_call (per provider call). A session
//     with no in-window model_call falls back to its usage events (one per
//     completed turn); a session with any model_call uses only those tokens, so
//     a turn is never counted twice.
//   - turns: one per usage event (a running turn has none yet).
//   - steps: one per model_call; a legacy usage event contributes its
//     iterations count (at least one).
//   - tool calls: one per tool_call event.
//   - cost: every token-bearing contribution is priced through the Cost
//     callback. A contribution whose model the callback does not price is
//     counted as unpriced, never as free; CostKnown reports whether every
//     token-bearing contribution was priced.
//   - days: local calendar days of the configured time zone; the window
//     includes today and the end is exclusive.
//
// Malformed or unknown events are skipped: an undecodable model_call/usage
// payload contributes nothing (not even a step or turn), malformed model-name
// payloads simply do not update attribution, and unknown event types are
// ignored. The fold is pure for a fixed log, clock and options.
package insights

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	rig "github.com/zeokeel/zeokeel-rig"
)

// Durable event types the fold accounts. The strings match the event log.
const (
	// EventRunStart starts one agent run and normally names its model.
	EventRunStart = "run_start"
	// EventContextSnapshot is the per-run request snapshot and normally names
	// its model.
	EventContextSnapshot = "context_snapshot"
	// EventModelCall is one provider call (the per-call token truth).
	EventModelCall = "model_call"
	// EventUsage is one completed turn (the token fallback for sessions
	// recorded before per-call events existed).
	EventUsage = "usage"
	// EventToolCall is one recorded tool call.
	EventToolCall = "tool_call"
	// EventCompact is one context compaction.
	EventCompact = "compact"
	// EventSpill is one tool result replaced by a spill summary.
	EventSpill = "spill"
)

const (
	// DefaultDays is the window used when Options.Days is zero.
	DefaultDays = 7
	// MaxDays bounds the window.
	MaxDays = 90
	// DefaultSessionLimit is the leaderboard size used when
	// Options.SessionLimit is zero.
	DefaultSessionLimit = 8
	// MaxSessionLimit bounds the leaderboard size.
	MaxSessionLimit = 50
	// DefaultEventsPerSession bounds the per-session Last read when
	// Options.EventsPerSession is zero.
	DefaultEventsPerSession = 4000
	// MaxEventsPerSession bounds the configured per-session read.
	MaxEventsPerSession = 20000
)

// ErrNoLog means Fold was given no event source (nil).
var ErrNoLog = errors.New("insights: no event log")

// SessionInfo is the durable session identity the fold needs.
type SessionInfo struct {
	// ID is the durable session id.
	ID string
	// Title is the display title (may be empty).
	Title string
	// WorkspaceID associates the session with a workspace (empty = none).
	WorkspaceID string
	// UpdatedAt is the session's durable update time; a session updated before
	// the window start is skipped without reading its events (zero = unknown,
	// read it).
	UpdatedAt time.Time
}

// EventLog is the durable view the fold reads. It is deliberately narrow: a
// session list plus the newest events of one session.
type EventLog interface {
	// Sessions returns the durable sessions (order is irrelevant).
	Sessions(ctx context.Context) ([]SessionInfo, error)
	// Last returns up to limit of the session's newest events in ascending
	// sequence order (limit <= 0 means the host's own bound).
	Last(ctx context.Context, sessionID string, limit int) ([]rig.SessionEvent, error)
}

// TokenCount is one token-bearing contribution or bucket. Cached is a subset of
// Prompt (the provider convention); Total is the provider-reported total or, for
// an event that omitted it, Prompt+Completion.
type TokenCount struct {
	Prompt     int
	Cached     int
	Completion int
	Total      int
}

// CostFunc prices one token-bearing contribution from the host's model catalog.
// tokens are already clamped to non-negative values; the callback owns the
// cached-subset-of-prompt clamp and the per-million arithmetic. priced=false
// means the model has no pricing and must never be rendered as free.
type CostFunc func(model string, tokens TokenCount) (cost float64, priced bool)

// Options configures one Fold. The zero value asks for the default week, the
// default leaderboard and the default per-session bound in local time.
type Options struct {
	// Days is the window length in local calendar days (0 = DefaultDays,
	// values above MaxDays are clamped).
	Days int
	// SessionLimit bounds the returned session leaderboard (0 =
	// DefaultSessionLimit, values above MaxSessionLimit are clamped).
	SessionLimit int
	// EventsPerSession bounds the per-session read (0 = DefaultEventsPerSession,
	// values above MaxEventsPerSession are clamped).
	EventsPerSession int
	// Location buckets days (nil = time.Local).
	Location *time.Location
	// Now is the clock (nil = time.Now); it also defines "today".
	Now func() time.Time
	// Cost prices token-bearing contributions (nil = no pricing, so every
	// contribution is unpriced and CostKnown stays false).
	Cost CostFunc
}

// Totals is the window-wide accounting of every read session.
type Totals struct {
	// Turns is the number of completed turns (usage events).
	Turns int
	// Steps is the number of provider calls (model_call events; legacy usage
	// events contribute their iterations).
	Steps int
	// ToolCalls is the number of recorded tool calls.
	ToolCalls int
	// Sessions is the number of sessions with recorded activity in the window.
	Sessions int
	// Compactions and Spills count the context audit events in the window.
	Compactions int
	Spills      int
	// Tokens is the provider-reported token sums.
	Tokens TokenCount
	// CostUSD is the estimated cost of the priced token-bearing calls. It is a
	// lower bound when UnpricedCalls is non-zero.
	CostUSD float64
	// CostKnown reports that at least one token-bearing call in the window was
	// priced and none was unpriced (false when nothing was priced).
	CostKnown bool
	// PricedCalls and UnpricedCalls classify token-bearing contributions.
	PricedCalls   int
	UnpricedCalls int
}

// Day is one local calendar day of the window.
type Day struct {
	// Day is the day's local midnight (the bucket identity).
	Day time.Time
	// Turns, Steps and ToolCalls count the day's activity.
	Turns     int
	Steps     int
	ToolCalls int
	// Tokens is the day's token sums.
	Tokens TokenCount
	// Sessions is the number of distinct sessions active that day.
	Sessions int
	// CostUSD is the day's estimated cost for priced calls; CostKnown reports
	// whether at least one call that day was priced and every token-bearing call
	// that day was priced.
	CostUSD   float64
	CostKnown bool
}

// Model is one model's accounting in the window.
type Model struct {
	// Model is the model identifier (may be empty when no event named one).
	Model string
	// Calls is the number of provider calls.
	Calls int
	// Tokens is the model's token sums.
	Tokens TokenCount
	// CostUSD is the model's estimated cost; CostKnown is false for a model
	// the callback does not price.
	CostUSD   float64
	CostKnown bool
}

// Workspace is one workspace's accounting in the window. Sessions without a
// workspace are grouped under the empty WorkspaceID.
type Workspace struct {
	// WorkspaceID is the durable workspace id (empty for unassociated
	// sessions).
	WorkspaceID string
	// Sessions is the number of distinct sessions active in the window.
	Sessions int
	// Tokens is the workspace's token sums.
	Tokens TokenCount
	// CostUSD is the workspace's estimated cost; CostKnown reports whether
	// every token-bearing call was priced.
	CostUSD   float64
	CostKnown bool
}

// Session is one row of the busiest-session leaderboard.
type Session struct {
	// SessionID is the durable session id.
	SessionID string
	// Title is the session's durable display title (may be empty).
	Title string
	// WorkspaceID is the session's workspace (empty when unassociated).
	WorkspaceID string
	// Model is the session's most-called model in the window (empty when
	// unknown).
	Model string
	// UpdatedAt is the session's durable update time.
	UpdatedAt time.Time
	// LastActivity is the newest accounted event's time.
	LastActivity time.Time
	// Turns, Steps and ToolCalls count the session's activity in the window.
	Turns     int
	Steps     int
	ToolCalls int
	// Tokens is the session's token sums.
	Tokens TokenCount
	// CostUSD is the session's estimated cost; CostKnown reports whether every
	// token-bearing call was priced.
	CostUSD   float64
	CostKnown bool
}

// Result is one windowed cross-session aggregation.
type Result struct {
	// Days is the effective window length in local calendar days.
	Days int
	// From is the local midnight the window starts at (inclusive); To is the
	// local midnight after the last day (exclusive).
	From time.Time
	To   time.Time
	// GeneratedAt is when the fold ran (now).
	GeneratedAt time.Time
	// Totals is the window-wide accounting.
	Totals Totals
	// Daily has one entry per day of the window, oldest first (zero-activity
	// days included).
	Daily []Day
	// Models is sorted by total tokens, descending (ties by model id).
	Models []Model
	// Workspaces is sorted by total tokens, descending (ties by workspace id).
	Workspaces []Workspace
	// Sessions is the leaderboard, sorted by total tokens, then steps, then
	// last activity, descending, and bounded by Options.SessionLimit.
	Sessions []Session
	// Truncated reports that at least one session's event read hit the
	// per-session bound while the oldest read event was still inside the
	// window, so older in-window events may be missing.
	Truncated bool
	// TruncatedSessions is the number of sessions that hit the read bound.
	TruncatedSessions int
}

// Fold reads every bounded session slice of the window and folds it into one
// Result. Store failures are returned wrapped; a canceled context aborts the
// fold between sessions.
func Fold(ctx context.Context, log EventLog, opts Options) (Result, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if log == nil {
		return Result{}, ErrNoLog
	}
	resolved := resolveOptions(opts)
	now := resolved.now()
	local := now.In(resolved.location)
	from := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, resolved.location).
		AddDate(0, 0, -(resolved.days - 1))
	to := from.AddDate(0, 0, resolved.days)
	acc := newAccumulator(resolved.days, from, to, resolved.location, resolved.cost)

	sessions, err := log.Sessions(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("insights: list sessions: %w", err)
	}
	for _, session := range sessions {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		// A session whose durable update time predates the window has no event
		// inside it (every durable write bumps updated_at), so skipping it keeps
		// the read bounded without losing in-window data.
		if !session.UpdatedAt.IsZero() && session.UpdatedAt.Before(from) {
			continue
		}
		events, err := log.Last(ctx, session.ID, resolved.eventsPerSession)
		if err != nil {
			return Result{}, fmt.Errorf("insights: read events of session %q: %w", session.ID, err)
		}
		acc.addSession(session, events, resolved.eventsPerSession)
	}
	return acc.finish(now, resolved.sessionLimit), nil
}

// resolvedOptions is Options with defaults applied and clamps enforced.
type resolvedOptions struct {
	days             int
	sessionLimit     int
	eventsPerSession int
	location         *time.Location
	now              func() time.Time
	cost             CostFunc
}

func resolveOptions(opts Options) resolvedOptions {
	days := opts.Days
	if days <= 0 {
		days = DefaultDays
	}
	if days > MaxDays {
		days = MaxDays
	}
	limit := opts.SessionLimit
	if limit <= 0 {
		limit = DefaultSessionLimit
	}
	if limit > MaxSessionLimit {
		limit = MaxSessionLimit
	}
	events := opts.EventsPerSession
	if events <= 0 {
		events = DefaultEventsPerSession
	}
	if events > MaxEventsPerSession {
		events = MaxEventsPerSession
	}
	location := opts.Location
	if location == nil {
		location = time.Local
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return resolvedOptions{
		days:             days,
		sessionLimit:     limit,
		eventsPerSession: events,
		location:         location,
		now:              now,
		cost:             opts.Cost,
	}
}

// modelCallPayload mirrors the durable model_call payload (one per provider
// call; the per-call token truth).
type modelCallPayload struct {
	Iteration        int    `json:"iteration"`
	Model            string `json:"model"`
	PromptTokens     int    `json:"promptTokens"`
	CachedTokens     int    `json:"cachedTokens"`
	CompletionTokens int    `json:"completionTokens"`
	TotalTokens      int    `json:"totalTokens"`
	ContextTokens    int    `json:"contextTokens"`
}

// usagePayload mirrors the durable usage payload (one per completed turn; the
// fallback token source for sessions recorded before per-call events existed).
type usagePayload struct {
	PromptTokens     int   `json:"promptTokens"`
	CompletionTokens int   `json:"completionTokens"`
	TotalTokens      int   `json:"totalTokens"`
	CachedTokens     int   `json:"cachedTokens"`
	Iterations       int   `json:"iterations"`
	DurationMS       int64 `json:"durationMs"`
	ContextTokens    int   `json:"contextTokens"`
}

// modelNamePayload covers the events that only carry the run's model name
// (run_start, context_snapshot).
type modelNamePayload struct {
	Model string `json:"model"`
}

// tokenSample is one token contribution.
type tokenSample struct {
	model      string
	prompt     int
	completion int
	cached     int
	total      int
}

// accumulator folds the window's events into buckets.
type accumulator struct {
	days     int
	from     time.Time
	to       time.Time
	location *time.Location
	cost     CostFunc

	totals totalsAcc
	daily  []dayAcc

	models     map[string]*modelAcc
	workspaces map[string]*workspaceAcc
	sessions   map[string]*sessionAcc
	// activeSessions is the set of sessions with recorded activity (the
	// window's "sessions" total).
	activeSessions map[string]struct{}

	// truncated counts the sessions whose event read hit the per-session bound
	// while the oldest read event was still inside the window (older in-window
	// events may be missing).
	truncated int
}

// totalsAcc accumulates the window-wide numbers.
type totalsAcc struct {
	turns, steps, toolCalls           int
	prompt, completion, cached, total int
	compactions, spills               int
	cost                              float64
	priced, unpriced                  int
}

// dayAcc is one local calendar day's bucket.
type dayAcc struct {
	day        time.Time
	turns      int
	steps      int
	toolCalls  int
	prompt     int
	completion int
	cached     int
	total      int
	cost       float64
	priced     int
	unpriced   int
	sessions   map[string]struct{}
}

// modelAcc is one model's bucket.
type modelAcc struct {
	model      string
	calls      int
	prompt     int
	completion int
	cached     int
	total      int
	cost       float64
	priced     int
	unpriced   int
}

// workspaceAcc is one workspace's bucket.
type workspaceAcc struct {
	id         string
	sessions   map[string]struct{}
	prompt     int
	completion int
	cached     int
	total      int
	cost       float64
	priced     int
	unpriced   int
}

// sessionAcc is one session's bucket.
type sessionAcc struct {
	id           string
	title        string
	workspaceID  string
	updatedAt    time.Time
	lastActivity time.Time
	turns        int
	steps        int
	toolCalls    int
	prompt       int
	completion   int
	cached       int
	total        int
	cost         float64
	priced       int
	unpriced     int
	modelCalls   map[string]int
	lastModel    string
}

// newAccumulator builds the empty hourglass for one window.
func newAccumulator(days int, from, to time.Time, loc *time.Location,
	cost CostFunc) *accumulator {
	acc := &accumulator{
		days:           days,
		from:           from,
		to:             to,
		location:       loc,
		cost:           cost,
		daily:          make([]dayAcc, days),
		models:         make(map[string]*modelAcc),
		workspaces:     make(map[string]*workspaceAcc),
		sessions:       make(map[string]*sessionAcc),
		activeSessions: make(map[string]struct{}),
	}
	for index := range acc.daily {
		acc.daily[index] = dayAcc{
			day:      from.AddDate(0, 0, index),
			sessions: make(map[string]struct{}),
		}
	}
	return acc
}

// addSession folds one session's durable events (newest bound events, ascending
// order) into the window.
func (acc *accumulator) addSession(meta SessionInfo, events []rig.SessionEvent, bound int) {
	if len(events) == 0 {
		return
	}
	// The store returned the newest bound events: when the oldest returned one
	// is already inside the window, older in-window events may be missing.
	if bound > 0 && len(events) >= bound {
		if oldest := events[0].Time; !oldest.IsZero() && !oldest.Before(acc.from) {
			acc.truncated++
		}
	}
	// The most recent model mentioned anywhere in the read window is the
	// attribution fallback for a legacy usage event whose run started before
	// the read window.
	fallbackModel := ""
	for index := len(events) - 1; index >= 0; index-- {
		if model := eventModel(events[index]); model != "" {
			fallbackModel = model
			break
		}
	}
	hasModelCalls := false
	for _, event := range events {
		if event.Type == EventModelCall && acc.inWindow(event.Time) {
			hasModelCalls = true
			break
		}
	}
	currentModel := fallbackModel
	for _, event := range events {
		at := event.Time
		if !acc.inWindow(at) {
			continue
		}
		switch event.Type {
		case EventRunStart, EventContextSnapshot:
			if model := eventModel(event); model != "" {
				currentModel = model
			}
		case EventModelCall:
			var payload modelCallPayload
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				continue
			}
			model := strings.TrimSpace(payload.Model)
			if model == "" {
				model = currentModel
			}
			if model != "" {
				currentModel = model
			}
			session := acc.sessionFor(meta)
			session.steps++
			acc.totals.steps++
			if day := acc.dayAt(at); day != nil {
				day.steps++
			}
			acc.modelFor(model).calls++
			if session.modelCalls == nil {
				session.modelCalls = make(map[string]int, 2)
			}
			session.modelCalls[model]++
			if model != "" {
				session.lastModel = model
			}
			acc.markActive(session, at)
			acc.credit(session, at, tokenSample{
				model:      model,
				prompt:     payload.PromptTokens,
				completion: payload.CompletionTokens,
				cached:     payload.CachedTokens,
				total:      payload.TotalTokens,
			})
		case EventUsage:
			var payload usagePayload
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				continue
			}
			session := acc.sessionFor(meta)
			session.turns++
			acc.totals.turns++
			if day := acc.dayAt(at); day != nil {
				day.turns++
			}
			acc.markActive(session, at)
			if hasModelCalls {
				// The per-call events already account for this turn's tokens.
				continue
			}
			model := currentModel
			if model == "" {
				model = fallbackModel
			}
			if session.modelCalls == nil {
				session.modelCalls = make(map[string]int, 2)
			}
			session.modelCalls[model]++
			if model != "" {
				session.lastModel = model
			}
			// A legacy usage event carries the turn's provider-call count, so a
			// session without per-call events still reports its steps.
			iterations := payload.Iterations
			if iterations <= 0 {
				iterations = 1
			}
			session.steps += iterations
			acc.totals.steps += iterations
			if day := acc.dayAt(at); day != nil {
				day.steps += iterations
			}
			acc.modelFor(model).calls += iterations
			acc.credit(session, at, tokenSample{
				model:      model,
				prompt:     payload.PromptTokens,
				completion: payload.CompletionTokens,
				cached:     payload.CachedTokens,
				total:      payload.TotalTokens,
			})
		case EventToolCall:
			session := acc.sessionFor(meta)
			session.toolCalls++
			acc.totals.toolCalls++
			if day := acc.dayAt(at); day != nil {
				day.toolCalls++
			}
			acc.markActive(session, at)
		case EventCompact:
			session := acc.sessionFor(meta)
			acc.totals.compactions++
			acc.markActive(session, at)
		case EventSpill:
			session := acc.sessionFor(meta)
			acc.totals.spills++
			acc.markActive(session, at)
		}
	}
}

// inWindow reports whether at belongs to the window (both boundaries are local
// midnights; the end is exclusive).
func (acc *accumulator) inWindow(at time.Time) bool {
	return !at.IsZero() && !at.Before(acc.from) && at.Before(acc.to)
}

// dayAt returns the local-day bucket of at (nil when outside the window).
func (acc *accumulator) dayAt(at time.Time) *dayAcc {
	local := at.In(acc.location)
	start := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, acc.location)
	// Round: a DST transition makes a "day" 23 or 25 hours long.
	index := int(math.Round(start.Sub(acc.from).Hours() / 24))
	if index < 0 || index >= len(acc.daily) {
		return nil
	}
	return &acc.daily[index]
}

// sessionFor returns (creating on first use) the session bucket.
func (acc *accumulator) sessionFor(meta SessionInfo) *sessionAcc {
	if existing, ok := acc.sessions[meta.ID]; ok {
		return existing
	}
	session := &sessionAcc{id: meta.ID, title: meta.Title, workspaceID: meta.WorkspaceID, updatedAt: meta.UpdatedAt}
	acc.sessions[meta.ID] = session
	return session
}

// modelFor returns (creating on first use) the model bucket.
func (acc *accumulator) modelFor(model string) *modelAcc {
	if existing, ok := acc.models[model]; ok {
		return existing
	}
	bucket := &modelAcc{model: model}
	acc.models[model] = bucket
	return bucket
}

// workspaceFor returns (creating on first use) the workspace bucket.
func (acc *accumulator) workspaceFor(id string) *workspaceAcc {
	if existing, ok := acc.workspaces[id]; ok {
		return existing
	}
	bucket := &workspaceAcc{id: id, sessions: make(map[string]struct{})}
	acc.workspaces[id] = bucket
	return bucket
}

// markActive records that a session had activity at at.
func (acc *accumulator) markActive(session *sessionAcc, at time.Time) {
	if session.lastActivity.IsZero() || at.After(session.lastActivity) {
		session.lastActivity = at
	}
	acc.activeSessions[session.id] = struct{}{}
	if day := acc.dayAt(at); day != nil {
		day.sessions[session.id] = struct{}{}
	}
	acc.workspaceFor(session.workspaceID).sessions[session.id] = struct{}{}
}

// credit adds one token sample to every bucket it belongs to, including the
// priced/unpriced accounting.
func (acc *accumulator) credit(session *sessionAcc, at time.Time, sample tokenSample) {
	total := sample.total
	if total <= 0 {
		total = sample.prompt + sample.completion
	}
	if sample.prompt < 0 {
		sample.prompt = 0
	}
	if sample.completion < 0 {
		sample.completion = 0
	}
	if sample.cached < 0 {
		sample.cached = 0
	}
	if total < 0 {
		total = 0
	}
	// Keep the callback's view consistent with the contribution that is
	// actually credited (the total falls back to prompt+completion).
	sample.total = total
	day := acc.dayAt(at)
	model := acc.modelFor(sample.model)
	workspace := acc.workspaceFor(session.workspaceID)

	acc.totals.prompt += sample.prompt
	acc.totals.completion += sample.completion
	acc.totals.cached += sample.cached
	acc.totals.total += total
	session.prompt += sample.prompt
	session.completion += sample.completion
	session.cached += sample.cached
	session.total += total
	if day != nil {
		day.prompt += sample.prompt
		day.completion += sample.completion
		day.cached += sample.cached
		day.total += total
	}
	model.prompt += sample.prompt
	model.completion += sample.completion
	model.cached += sample.cached
	model.total += total
	workspace.prompt += sample.prompt
	workspace.completion += sample.completion
	workspace.cached += sample.cached
	workspace.total += total

	cost, priced := acc.price(sample)
	if sample.prompt+sample.completion > 0 {
		if priced {
			acc.totals.priced++
			session.priced++
			model.priced++
			workspace.priced++
			if day != nil {
				day.priced++
			}
		} else {
			acc.totals.unpriced++
			session.unpriced++
			model.unpriced++
			workspace.unpriced++
			if day != nil {
				day.unpriced++
			}
		}
	}
	if !priced {
		return
	}
	acc.totals.cost += cost
	session.cost += cost
	model.cost += cost
	workspace.cost += cost
	if day != nil {
		day.cost += cost
	}
}

// price estimates one sample's cost through the host callback. priced=false
// means the model has no pricing (never a free call).
func (acc *accumulator) price(sample tokenSample) (float64, bool) {
	if acc.cost == nil {
		return 0, false
	}
	return acc.cost(sample.model, TokenCount{
		Prompt:     sample.prompt,
		Cached:     sample.cached,
		Completion: sample.completion,
		Total:      sample.total,
	})
}

// eventModel extracts the model name of an event that carries one.
func eventModel(event rig.SessionEvent) string {
	switch event.Type {
	case EventRunStart, EventContextSnapshot:
		var payload modelNamePayload
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return ""
		}
		return strings.TrimSpace(payload.Model)
	case EventModelCall:
		var payload modelCallPayload
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return ""
		}
		return strings.TrimSpace(payload.Model)
	default:
		return ""
	}
}

// finish converts the buckets into the result.
func (acc *accumulator) finish(now time.Time, limit int) Result {
	result := Result{
		Days:              acc.days,
		From:              acc.from,
		To:                acc.to,
		GeneratedAt:       now,
		Truncated:         acc.truncated > 0,
		TruncatedSessions: acc.truncated,
		Totals: Totals{
			Turns:       acc.totals.turns,
			Steps:       acc.totals.steps,
			ToolCalls:   acc.totals.toolCalls,
			Sessions:    len(acc.activeSessions),
			Compactions: acc.totals.compactions,
			Spills:      acc.totals.spills,
			Tokens: TokenCount{
				Prompt:     acc.totals.prompt,
				Cached:     acc.totals.cached,
				Completion: acc.totals.completion,
				Total:      acc.totals.total,
			},
			CostUSD:       acc.totals.cost,
			CostKnown:     acc.totals.priced > 0 && acc.totals.unpriced == 0,
			PricedCalls:   acc.totals.priced,
			UnpricedCalls: acc.totals.unpriced,
		},
		Daily:      make([]Day, len(acc.daily)),
		Models:     make([]Model, 0, len(acc.models)),
		Workspaces: make([]Workspace, 0, len(acc.workspaces)),
		Sessions:   make([]Session, 0, len(acc.sessions)),
	}
	for index := range acc.daily {
		result.Daily[index] = acc.daily[index].contract()
	}
	for _, bucket := range acc.models {
		result.Models = append(result.Models, Model{
			Model: bucket.model,
			Calls: bucket.calls,
			Tokens: TokenCount{
				Prompt:     bucket.prompt,
				Cached:     bucket.cached,
				Completion: bucket.completion,
				Total:      bucket.total,
			},
			CostUSD:   bucket.cost,
			CostKnown: bucket.priced > 0 && bucket.unpriced == 0,
		})
	}
	sort.Slice(result.Models, func(i, j int) bool {
		if result.Models[i].Tokens.Total != result.Models[j].Tokens.Total {
			return result.Models[i].Tokens.Total > result.Models[j].Tokens.Total
		}
		return result.Models[i].Model < result.Models[j].Model
	})
	for _, bucket := range acc.workspaces {
		result.Workspaces = append(result.Workspaces, Workspace{
			WorkspaceID: bucket.id,
			Sessions:    len(bucket.sessions),
			Tokens: TokenCount{
				Prompt:     bucket.prompt,
				Cached:     bucket.cached,
				Completion: bucket.completion,
				Total:      bucket.total,
			},
			CostUSD:   bucket.cost,
			CostKnown: bucket.priced > 0 && bucket.unpriced == 0,
		})
	}
	sort.Slice(result.Workspaces, func(i, j int) bool {
		if result.Workspaces[i].Tokens.Total != result.Workspaces[j].Tokens.Total {
			return result.Workspaces[i].Tokens.Total > result.Workspaces[j].Tokens.Total
		}
		return result.Workspaces[i].WorkspaceID < result.Workspaces[j].WorkspaceID
	})
	for _, bucket := range acc.sessions {
		result.Sessions = append(result.Sessions, Session{
			SessionID:    bucket.id,
			Title:        bucket.title,
			WorkspaceID:  bucket.workspaceID,
			Model:        bucket.mostCalledModel(),
			UpdatedAt:    bucket.updatedAt,
			LastActivity: bucket.lastActivity,
			Turns:        bucket.turns,
			Steps:        bucket.steps,
			ToolCalls:    bucket.toolCalls,
			Tokens: TokenCount{
				Prompt:     bucket.prompt,
				Cached:     bucket.cached,
				Completion: bucket.completion,
				Total:      bucket.total,
			},
			CostUSD:   bucket.cost,
			CostKnown: bucket.priced > 0 && bucket.unpriced == 0,
		})
	}
	sort.Slice(result.Sessions, func(i, j int) bool {
		left, right := result.Sessions[i], result.Sessions[j]
		if left.Tokens.Total != right.Tokens.Total {
			return left.Tokens.Total > right.Tokens.Total
		}
		if left.Steps != right.Steps {
			return left.Steps > right.Steps
		}
		if !left.LastActivity.Equal(right.LastActivity) {
			return left.LastActivity.After(right.LastActivity)
		}
		return left.SessionID < right.SessionID
	})
	if limit > 0 && len(result.Sessions) > limit {
		result.Sessions = result.Sessions[:limit]
	}
	return result
}

// contract converts a day bucket.
func (day dayAcc) contract() Day {
	return Day{
		Day:       day.day,
		Turns:     day.turns,
		Steps:     day.steps,
		ToolCalls: day.toolCalls,
		Tokens: TokenCount{
			Prompt:     day.prompt,
			Cached:     day.cached,
			Completion: day.completion,
			Total:      day.total,
		},
		Sessions:  len(day.sessions),
		CostUSD:   day.cost,
		CostKnown: day.priced > 0 && day.unpriced == 0,
	}
}

// mostCalledModel reports the session's most-called model (the lexicographically
// first model breaks a tie; empty when unknown).
func (session *sessionAcc) mostCalledModel() string {
	best, bestCount := "", 0
	for model, count := range session.modelCalls {
		if count > bestCount || (count == bestCount && model < best) {
			best, bestCount = model, count
		}
	}
	if best != "" {
		return best
	}
	return session.lastModel
}
