package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	v1 "github.com/zeokeel/zeokeel-rig/contracts/v1"
	"github.com/zeokeel/zeokeel-rig/session"
)

// Defaults for RunnerOptions.
const (
	// DefaultMaxDepth is the default number of subagent levels a runner may
	// start. NewRunner's zero-value MaxDepth means this default; a negative
	// value disables subagents entirely (Start returns an error).
	DefaultMaxDepth = 1
	// DefaultMaxConcurrent bounds how many live subagents one runner keeps at
	// the same time when RunnerOptions.MaxConcurrent is zero.
	DefaultMaxConcurrent = 4
)

// DefaultExcludedTools are filtered out of every child agent by default, so a
// child cannot start subagents, workflows or teams and recurse. It lists the
// tool names the SDK ships (subagent, workflow and the tools/team set); hosts
// with their own orchestration tools append those names via RunnerOptions.
var DefaultExcludedTools = []string{
	"subagent", "workflow",
	"team", "team_spawn", "team_list", "team_send", "team_wait", "team_last",
	"team_close", "team_interrupt",
}

// RunnerOptions configures NewRunner.
type RunnerOptions struct {
	// MaxDepth is how many levels of subagents this runner allows. Zero means
	// DefaultMaxDepth; a negative value disallows subagents (Start fails).
	MaxDepth int
	// MaxConcurrent bounds live subagents; zero means DefaultMaxConcurrent.
	MaxConcurrent int
	// ExcludedTools are removed from the child's tool set. Nil means
	// DefaultExcludedTools; an explicit empty slice keeps every tool (the host
	// then authors its own recursion guard).
	ExcludedTools []string
	// SystemPromptAppend is appended to the child's system prompt (after the
	// spec override), usually for orchestration guidance ("you are a focused
	// subagent; return a concise result").
	SystemPromptAppend string
}

// DefaultRunnerOptions returns the documented defaults.
func DefaultRunnerOptions() RunnerOptions {
	return RunnerOptions{
		MaxDepth:      DefaultMaxDepth,
		MaxConcurrent: DefaultMaxConcurrent,
		ExcludedTools: append([]string(nil), DefaultExcludedTools...),
	}
}

// runner is the default v1.SubagentRunner implementation: it clones the parent
// AgentSpec for each child, gives the child an isolated in-memory history,
// filters orchestration tools and bounds live children with a semaphore.
type runner struct {
	parent       v1.AgentSpec
	depth        int
	maxDepth     int
	excluded     map[string]struct{}
	systemAppend string
	sem          chan struct{}
	started      atomic.Uint64
}

var _ v1.SubagentRunner = (*runner)(nil)

// NewRunner builds the default SubagentRunner for parent.
//
// The parent spec is validated exactly like agent.New validates its own spec
// (provider, model, tools). Each Start creates a child agent that:
//
//   - shares the parent's Provider, Gate, sampling options and Logger;
//   - gets a filtered tool set (ExcludedTools, default subagent/workflow/team);
//   - gets a fresh session.Memory history (child conversations never touch the
//     parent's session);
//   - overrides Model/SystemPrompt/MaxIterations from the SubagentSpec when
//     those fields are set.
//
// Runs on one Subagent are serialized. Callers must Stop a subagent when done;
// Stop releases its concurrency slot.
func NewRunner(parent v1.AgentSpec, opts RunnerOptions) (v1.SubagentRunner, error) {
	return newRunnerAtDepth(parent, opts, 0)
}

// newRunnerAtDepth is NewRunner with an explicit starting depth. Children
// created by Start do not get orchestration tools, so this runner's depth is
// only ever non-zero for a nested runner a host constructs deliberately.
func newRunnerAtDepth(parent v1.AgentSpec, opts RunnerOptions, depth int) (*runner, error) {
	if depth < 0 {
		return nil, errors.New("agent: runner depth must not be negative")
	}
	if _, err := New(parent); err != nil {
		return nil, fmt.Errorf("agent: NewRunner parent spec: %w", err)
	}
	maxDepth := opts.MaxDepth
	if maxDepth == 0 {
		maxDepth = DefaultMaxDepth
	}
	if maxDepth < 0 {
		maxDepth = 0 // negative disables subagents
	}
	maxConcurrent := opts.MaxConcurrent
	if maxConcurrent < 0 {
		return nil, errors.New("agent: RunnerOptions.MaxConcurrent must not be negative")
	}
	if maxConcurrent == 0 {
		maxConcurrent = DefaultMaxConcurrent
	}
	excluded := opts.ExcludedTools
	if excluded == nil {
		excluded = DefaultExcludedTools
	}
	excludedSet := make(map[string]struct{}, len(excluded))
	for _, name := range excluded {
		if name = strings.TrimSpace(name); name != "" {
			excludedSet[name] = struct{}{}
		}
	}
	return &runner{
		parent:       parent,
		depth:        depth,
		maxDepth:     maxDepth,
		excluded:     excludedSet,
		systemAppend: strings.TrimSpace(opts.SystemPromptAppend),
		sem:          make(chan struct{}, maxConcurrent),
	}, nil
}

// Start implements v1.SubagentRunner.
func (r *runner) Start(ctx context.Context, spec v1.SubagentSpec) (v1.Subagent, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if r.depth >= r.maxDepth {
		return nil, fmt.Errorf(
			"agent: subagent depth limit reached (depth %d, MaxDepth %d); children never receive orchestration tools",
			r.depth, r.maxDepth)
	}
	name := strings.TrimSpace(spec.Name)
	if name == "" {
		name = fmt.Sprintf("subagent-%d", r.started.Add(1))
	}
	if strings.TrimSpace(spec.Prompt) == "" {
		return nil, errors.New("agent: SubagentSpec.Prompt is required")
	}
	if spec.MaxIterations < 0 {
		return nil, errors.New("agent: SubagentSpec.MaxIterations must not be negative")
	}

	select {
	case r.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	childSpec := r.childSpec(spec)
	childAgent, err := New(childSpec)
	if err != nil {
		r.release()
		return nil, fmt.Errorf("agent: start subagent %q: %w", name, err)
	}
	childCtx, cancel := context.WithCancel(context.Background())
	s := &subagent{
		runner:    r,
		name:      name,
		agent:     childAgent,
		sessionID: fmt.Sprintf("subagent:%s:%d", name, r.started.Add(1)),
		baseCtx:   childCtx,
		cancel:    cancel,
		firstDone: make(chan struct{}),
	}
	go s.runFirst(spec.Prompt)
	return s, nil
}

// release frees one concurrency slot.
func (r *runner) release() {
	select {
	case <-r.sem:
	default:
	}
}

// childSpec builds the child's AgentSpec from the parent and one SubagentSpec.
func (r *runner) childSpec(spec v1.SubagentSpec) v1.AgentSpec {
	child := r.parent // shallow copy: scalar/interface fields are read-only here
	child.Tools = r.filterTools(r.parent.Tools)
	child.History = session.NewMemory()
	if model := strings.TrimSpace(spec.Model); model != "" {
		child.Model = model
	}
	if spec.MaxIterations > 0 {
		child.MaxIterations = spec.MaxIterations
	}
	systemPrompt := r.parent.SystemPrompt
	if override := strings.TrimSpace(spec.SystemPrompt); override != "" {
		systemPrompt = override
	}
	if r.systemAppend != "" {
		systemPrompt = strings.TrimSpace(systemPrompt + "\n\n" + r.systemAppend)
	}
	child.SystemPrompt = systemPrompt
	return child
}

// filterTools returns the parent's tools without the orchestration tools.
func (r *runner) filterTools(tools []v1.Tool) []v1.Tool {
	filtered := make([]v1.Tool, 0, len(tools))
	for _, tool := range tools {
		if tool == nil {
			continue
		}
		if _, excluded := r.excluded[strings.TrimSpace(tool.Name())]; excluded {
			continue
		}
		filtered = append(filtered, tool)
	}
	return filtered
}

// subagent is one live child agent.
type subagent struct {
	runner    *runner
	name      string
	agent     v1.Agent
	sessionID string
	baseCtx   context.Context
	cancel    context.CancelFunc

	// runMu serializes runs (the first run and every Send).
	runMu sync.Mutex
	// firstDone closes when the first run has stored its result.
	firstDone   chan struct{}
	firstResult v1.SubagentResult
	firstErr    error

	stopped  atomic.Bool
	stopOnce sync.Once
	stopErr  error
}

var _ v1.Subagent = (*subagent)(nil)

// Name implements v1.Subagent.
func (s *subagent) Name() string { return s.name }

// runFirst executes the initial prompt on its own goroutine so Start returns
// immediately; Wait blocks for it.
func (s *subagent) runFirst(prompt string) {
	defer close(s.firstDone)
	s.runMu.Lock()
	defer s.runMu.Unlock()
	s.firstResult, s.firstErr = s.execute(prompt)
}

// execute runs one turn on the child agent and reduces it to a SubagentResult.
func (s *subagent) execute(prompt string) (v1.SubagentResult, error) {
	result, err := s.agent.Run(s.baseCtx, v1.RunRequest{
		SessionID: s.sessionID,
		Input:     []v1.Message{v1.NewMessage(v1.RoleUser, prompt)},
	})
	if err != nil {
		return v1.SubagentResult{Name: s.name}, err
	}
	return v1.SubagentResult{
		Name:       s.name,
		Output:     finalAssistantText(result.Messages),
		Usage:      result.Usage,
		Iterations: result.Iterations,
	}, nil
}

// Wait implements v1.Subagent: it waits for the first run (or the caller's
// context) and returns that run's result. Repeated calls return the same
// result.
func (s *subagent) Wait(ctx context.Context) (v1.SubagentResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-s.firstDone:
		return s.firstResult, s.firstErr
	case <-ctx.Done():
		return v1.SubagentResult{Name: s.name}, ctx.Err()
	}
}

// Send implements v1.Subagent: it waits for any in-flight run, then runs one
// follow-up turn and blocks until it ends.
func (s *subagent) Send(ctx context.Context, message string) (v1.SubagentResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if strings.TrimSpace(message) == "" {
		return v1.SubagentResult{Name: s.name}, errors.New("agent: subagent message is empty")
	}
	s.runMu.Lock()
	defer s.runMu.Unlock()
	if s.stopped.Load() {
		return v1.SubagentResult{Name: s.name}, errors.New("agent: subagent is stopped")
	}
	if err := ctx.Err(); err != nil {
		return v1.SubagentResult{Name: s.name}, err
	}
	return s.execute(message)
}

// Stop implements v1.Subagent: it cancels the in-flight run and releases the
// concurrency slot. It is idempotent.
func (s *subagent) Stop() error {
	s.stopOnce.Do(func() {
		s.stopped.Store(true)
		s.cancel()
		s.runner.release()
	})
	return s.stopErr
}

// finalAssistantText returns the last non-empty assistant text in messages.
func finalAssistantText(messages []v1.Message) string {
	for index := len(messages) - 1; index >= 0; index-- {
		if messages[index].Role != v1.RoleAssistant {
			continue
		}
		if text := strings.TrimSpace(messages[index].Text()); text != "" {
			return text
		}
	}
	return ""
}
