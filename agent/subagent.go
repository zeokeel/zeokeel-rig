package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/zeokeel/zeokeel-rig"
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
	// ParentSessionID is the session a forked child copies its initial history
	// from (SubagentSpec.Fork). Empty means forks are rejected at Start with a
	// readable configuration error.
	ParentSessionID string
	// ParentHistory loads the forked parent history. Nil means the parent
	// AgentSpec.History.
	ParentHistory rig.History
}

// DefaultRunnerOptions returns the documented defaults.
func DefaultRunnerOptions() RunnerOptions {
	return RunnerOptions{
		MaxDepth:      DefaultMaxDepth,
		MaxConcurrent: DefaultMaxConcurrent,
		ExcludedTools: slices.Clone(DefaultExcludedTools),
	}
}

// runner is the default rig.SubagentRunner implementation: it clones the parent
// AgentSpec for each child, gives the child an isolated in-memory history,
// filters orchestration tools and bounds live children with a semaphore.
type runner struct {
	parent          rig.AgentSpec
	depth           int
	maxDepth        int
	excluded        map[string]struct{}
	systemAppend    string
	parentSessionID string
	parentHistory   rig.History
	sem             chan struct{}
	started         atomic.Uint64
}

var _ rig.SubagentRunner = (*runner)(nil)

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
func NewRunner(parent rig.AgentSpec, opts RunnerOptions) (rig.SubagentRunner, error) {
	return newRunnerAtDepth(parent, opts, 0)
}

// newRunnerAtDepth is NewRunner with an explicit starting depth. Children
// created by Start do not get orchestration tools, so this runner's depth is
// only ever non-zero for a nested runner a host constructs deliberately.
func newRunnerAtDepth(parent rig.AgentSpec, opts RunnerOptions, depth int) (*runner, error) {
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
	parentHistory := opts.ParentHistory
	if parentHistory == nil {
		parentHistory = parent.History
	}
	return &runner{
		parent:          parent,
		depth:           depth,
		maxDepth:        maxDepth,
		excluded:        excludedSet,
		systemAppend:    strings.TrimSpace(opts.SystemPromptAppend),
		parentSessionID: strings.TrimSpace(opts.ParentSessionID),
		parentHistory:   parentHistory,
		sem:             make(chan struct{}, maxConcurrent),
	}, nil
}

// Start implements rig.SubagentRunner.
func (r *runner) Start(ctx context.Context, spec rig.SubagentSpec) (rig.Subagent, error) {
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

	outputSchema, err := normalizeOutputSchema(spec.OutputSchema)
	if err != nil {
		return nil, fmt.Errorf("agent: start subagent %q: %w", name, err)
	}

	select {
	case r.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	sessionID := fmt.Sprintf("subagent:%s:%d", name, r.started.Add(1))
	history := session.NewMemory()
	if spec.Fork {
		if err := r.seedForkHistory(ctx, history, sessionID); err != nil {
			r.release()
			return nil, fmt.Errorf("agent: start fork %q: %w", name, err)
		}
	}
	childCtx, cancel := context.WithCancel(context.Background())
	s := &subagent{
		runner:       r,
		name:         name,
		sessionID:    sessionID,
		baseCtx:      childCtx,
		cancel:       cancel,
		firstDone:    make(chan struct{}),
		outputSchema: outputSchema,
	}
	tools := r.filterTools(r.parent.Tools)
	if outputSchema != nil {
		for _, tool := range tools {
			if tool != nil && strings.TrimSpace(tool.Name()) == StructuredOutputToolName {
				cancel()
				r.release()
				return nil, fmt.Errorf("agent: start subagent %q: parent tools already contain the reserved tool name %q",
					name, StructuredOutputToolName)
			}
		}
		tools = append(tools, &structuredOutputTool{schema: outputSchema, capture: s.captureStructured})
	}
	childSpec := r.childSpec(spec, tools, history, outputSchema)
	childAgent, err := New(childSpec)
	if err != nil {
		cancel()
		r.release()
		return nil, fmt.Errorf("agent: start subagent %q: %w", name, err)
	}
	s.agent = childAgent
	go s.runFirst(spec.Prompt)
	return s, nil
}

// seedForkHistory copies the configured parent session's completed messages into
// a forked child's fresh history.
func (r *runner) seedForkHistory(ctx context.Context, target *session.Memory, sessionID string) error {
	if r.parentSessionID == "" {
		return errors.New("fork requires RunnerOptions.ParentSessionID")
	}
	if r.parentHistory == nil {
		return errors.New("fork requires a parent history (RunnerOptions.ParentHistory or the parent AgentSpec.History)")
	}
	messages, err := r.parentHistory.Load(ctx, r.parentSessionID)
	if err != nil {
		return fmt.Errorf("load parent session %q: %w", r.parentSessionID, err)
	}
	if len(messages) == 0 {
		return nil
	}
	if err := target.Append(ctx, sessionID, cloneMessages(messages)...); err != nil {
		return fmt.Errorf("copy parent session %q: %w", r.parentSessionID, err)
	}
	return nil
}

// release frees one concurrency slot.
func (r *runner) release() {
	select {
	case <-r.sem:
	default:
	}
}

// childSpec builds the child's AgentSpec from the parent and one SubagentSpec.
// The runner supplies the child's tool set and history (forked or fresh).
func (r *runner) childSpec(
	spec rig.SubagentSpec,
	tools []rig.Tool,
	history rig.History,
	outputSchema map[string]any,
) rig.AgentSpec {
	child := r.parent // shallow copy: scalar/interface fields are read-only here
	child.Tools = tools
	child.History = history
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
	if outputSchema != nil {
		systemPrompt = strings.TrimSpace(systemPrompt + "\n\n" + structuredOutputInstruction)
	}
	child.SystemPrompt = systemPrompt
	return child
}

// filterTools returns the parent's tools without the orchestration tools.
func (r *runner) filterTools(tools []rig.Tool) []rig.Tool {
	filtered := make([]rig.Tool, 0, len(tools))
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
	agent     rig.Agent
	sessionID string
	baseCtx   context.Context
	cancel    context.CancelFunc

	// runMu serializes runs (the first run and every Send).
	runMu sync.Mutex
	// firstDone closes when the first run has stored its result.
	firstDone   chan struct{}
	firstResult rig.SubagentResult
	firstErr    error

	// outputSchema, when non-nil, makes every run require a structured result:
	// either a structured_output tool call or a final text that is valid JSON
	// matching the schema. structured holds the capture of the current run.
	outputSchema map[string]any
	structuredMu sync.Mutex
	structured   map[string]any

	stopped  atomic.Bool
	stopOnce sync.Once
	stopErr  error
}

var _ rig.Subagent = (*subagent)(nil)

// Name implements rig.Subagent.
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
func (s *subagent) execute(prompt string) (rig.SubagentResult, error) {
	if s.outputSchema != nil {
		s.captureStructured(nil) // every run needs its own structured result
	}
	result, err := s.agent.Run(s.baseCtx, rig.RunRequest{
		SessionID: s.sessionID,
		Input:     []rig.Message{rig.NewMessage(rig.RoleUser, prompt)},
	})
	if err != nil {
		return rig.SubagentResult{Name: s.name}, err
	}
	output := finalAssistantText(result.Messages)
	subagentResult := rig.SubagentResult{
		Name:       s.name,
		Output:     output,
		Usage:      result.Usage,
		Iterations: result.Iterations,
	}
	if s.outputSchema == nil {
		return subagentResult, nil
	}
	structured := s.takeStructured()
	if structured == nil {
		structured = structuredFromText(s.outputSchema, output)
	}
	if structured == nil {
		return subagentResult, fmt.Errorf(
			"agent: subagent %q did not produce structured output matching OutputSchema "+
				"(call the %s tool or answer with a JSON object)", s.name, StructuredOutputToolName)
	}
	subagentResult.Structured = structured
	return subagentResult, nil
}

// captureStructured stores the current run's structured output (nil resets it).
func (s *subagent) captureStructured(value map[string]any) {
	s.structuredMu.Lock()
	s.structured = value
	s.structuredMu.Unlock()
}

// takeStructured returns and clears the current run's captured structured output.
func (s *subagent) takeStructured() map[string]any {
	s.structuredMu.Lock()
	defer s.structuredMu.Unlock()
	value := s.structured
	s.structured = nil
	return value
}

// structuredFromText parses a final assistant text as JSON and validates it
// against schema; nil means it is not a usable structured result.
func structuredFromText(schema map[string]any, text string) map[string]any {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	var value any
	if err := json.Unmarshal([]byte(text), &value); err != nil {
		return nil
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	if err := validateStructured(schema, object); err != nil {
		return nil
	}
	return object
}

// Wait implements rig.Subagent: it waits for the first run (or the caller's
// context) and returns that run's result. Repeated calls return the same
// result.
func (s *subagent) Wait(ctx context.Context) (rig.SubagentResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-s.firstDone:
		return s.firstResult, s.firstErr
	case <-ctx.Done():
		return rig.SubagentResult{Name: s.name}, ctx.Err()
	}
}

// Send implements rig.Subagent: it waits for any in-flight run, then runs one
// follow-up turn and blocks until it ends.
func (s *subagent) Send(ctx context.Context, message string) (rig.SubagentResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if strings.TrimSpace(message) == "" {
		return rig.SubagentResult{Name: s.name}, errors.New("agent: subagent message is empty")
	}
	s.runMu.Lock()
	defer s.runMu.Unlock()
	if s.stopped.Load() {
		return rig.SubagentResult{Name: s.name}, errors.New("agent: subagent is stopped")
	}
	if err := ctx.Err(); err != nil {
		return rig.SubagentResult{Name: s.name}, err
	}
	return s.execute(message)
}

// Stop implements rig.Subagent: it cancels the in-flight run and releases the
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
func finalAssistantText(messages []rig.Message) string {
	for index := len(messages) - 1; index >= 0; index-- {
		if messages[index].Role != rig.RoleAssistant {
			continue
		}
		if text := strings.TrimSpace(messages[index].Text()); text != "" {
			return text
		}
	}
	return ""
}
