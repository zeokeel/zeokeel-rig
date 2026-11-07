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

// RunnerOptions 的默认值。
const (
	// DefaultMaxDepth 是 runner 允许启动的子 agent 层数默认值。NewRunner 的
	// MaxDepth 零值表示该默认值；负值会完全禁用子 agent（Start 返回错误）。
	DefaultMaxDepth = 1
	// DefaultMaxConcurrent 是 RunnerOptions.MaxConcurrent 为零时，一个 runner
	// 同时保持的存活子 agent 数量上限。
	DefaultMaxConcurrent = 4
)

// DefaultExcludedTools 默认会从每个子 agent 中过滤掉，使子 agent 无法启动子 agent、
// workflow 或 team 而形成递归。它列出 SDK 自带的工具名（subagent、workflow 以及
// tools/team 工具集）；带有自己编排工具的宿主应通过 RunnerOptions 追加这些名字。
var DefaultExcludedTools = []string{
	"subagent", "workflow",
	"team", "team_spawn", "team_list", "team_send", "team_wait", "team_last",
	"team_close", "team_interrupt",
}

// RunnerOptions 配置 NewRunner。
type RunnerOptions struct {
	// MaxDepth 是该 runner 允许的子 agent 层数。零表示 DefaultMaxDepth；负值
	// 禁用子 agent（Start 失败）。
	MaxDepth int
	// MaxConcurrent 限制存活的子 agent 数量；零表示 DefaultMaxConcurrent。
	MaxConcurrent int
	// ExcludedTools 会从子 agent 的工具集中移除。nil 表示 DefaultExcludedTools；
	// 显式的空切片会保留全部工具（此时递归守卫由宿主自行负责）。
	ExcludedTools []string
	// SystemPromptAppend 会追加到子 agent 的系统提示词末尾（在 spec 覆盖之后），
	// 通常用于编排指导（“你是一个专注的子 agent；请返回简洁结果”）。
	SystemPromptAppend string
	// ParentSessionID 是被 fork 的子 agent 复制初始历史时使用的父会话。
	// （SubagentSpec.Fork。）为空时 Start 会以可读的配置错误拒绝 fork。
	ParentSessionID string
	// ParentHistory 用于加载被 fork 的父历史。nil 表示父 AgentSpec.History。
	ParentHistory rig.History
}

// DefaultRunnerOptions 返回文档化的默认值。
func DefaultRunnerOptions() RunnerOptions {
	return RunnerOptions{
		MaxDepth:      DefaultMaxDepth,
		MaxConcurrent: DefaultMaxConcurrent,
		ExcludedTools: slices.Clone(DefaultExcludedTools),
	}
}

// runner 是默认的 rig.SubagentRunner 实现：它为每个子 agent 克隆父 AgentSpec，
// 给子 agent 独立的内存历史，过滤编排工具，并用信号量限制存活的子 agent 数量。
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

// NewRunner 为 parent 构造默认的 SubagentRunner。
//
// parent spec 的校验方式与 agent.New 校验自身 spec 完全一致（provider、模型、
// 工具）。每次 Start 创建一个子 agent，它会：
//
//   - 共享父 agent 的 Provider、Gate、采样选项与 Logger；
//   - 得到过滤后的工具集（ExcludedTools，默认是 subagent/workflow/team）；
//   - 得到全新的 session.Memory 历史（子对话永远不会触碰父会话）；
//   - 在 SubagentSpec 对应字段非空时覆盖 Model/SystemPrompt/MaxIterations。
//
// 同一个 Subagent 上的运行会被串行化。调用方必须在用完后 Stop 子 agent；Stop 会
// 释放它的并发槽位。
func NewRunner(parent rig.AgentSpec, opts RunnerOptions) (rig.SubagentRunner, error) {
	return newRunnerAtDepth(parent, opts, 0)
}

// newRunnerAtDepth 是带显式起始深度的 NewRunner。Start 创建的子 agent 不会得到
// 编排工具，因此只有宿主刻意构造的嵌套 runner 的 depth 才会非零。
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
		maxDepth = 0 // 负值禁用子 agent
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

// Start 实现 rig.SubagentRunner。
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

// seedForkHistory 把配置的父会话已完成消息复制进被 fork 子 agent 的全新历史。
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

// release 释放一个并发槽位。
func (r *runner) release() {
	select {
	case <-r.sem:
	default:
	}
}

// childSpec 根据父 spec 与一个 SubagentSpec 构造子 agent 的 AgentSpec。工具集与
// 历史（fork 或全新）由 runner 提供。
func (r *runner) childSpec(
	spec rig.SubagentSpec,
	tools []rig.Tool,
	history rig.History,
	outputSchema map[string]any,
) rig.AgentSpec {
	child := r.parent // 浅拷贝：标量/接口字段在这里都是只读的
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

// filterTools 返回父工具集中去掉编排工具后的结果。
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

// subagent 是一个存活的子 agent。
type subagent struct {
	runner    *runner
	name      string
	agent     rig.Agent
	sessionID string
	baseCtx   context.Context
	cancel    context.CancelFunc

	// runMu 串行化运行（首次运行与每次 Send）。
	runMu sync.Mutex
	// firstDone 在首次运行保存结果后关闭。
	firstDone   chan struct{}
	firstResult rig.SubagentResult
	firstErr    error

	// outputSchema 非 nil 时，每次运行都要求结构化结果：要么调用
	// structured_output 工具，要么最终文本是符合 schema 的合法 JSON。structured
	// 保存当前运行的捕获结果。
	outputSchema map[string]any
	structuredMu sync.Mutex
	structured   map[string]any

	stopped  atomic.Bool
	stopOnce sync.Once
	stopErr  error
}

var _ rig.Subagent = (*subagent)(nil)

// Name 实现 rig.Subagent。
func (s *subagent) Name() string { return s.name }

// runFirst 在自己的 goroutine 上执行初始 prompt，使 Start 立即返回；Wait 会阻塞
// 等待它。
func (s *subagent) runFirst(prompt string) {
	defer close(s.firstDone)
	s.runMu.Lock()
	defer s.runMu.Unlock()
	s.firstResult, s.firstErr = s.execute(prompt)
}

// execute 在子 agent 上运行一个轮次，并归约为 SubagentResult。
func (s *subagent) execute(prompt string) (rig.SubagentResult, error) {
	if s.outputSchema != nil {
		s.captureStructured(nil) // 每次运行都需要自己的结构化结果
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

// captureStructured 保存当前运行的结构化输出（nil 表示重置）。
func (s *subagent) captureStructured(value map[string]any) {
	s.structuredMu.Lock()
	s.structured = value
	s.structuredMu.Unlock()
}

// takeStructured 返回并清空当前运行捕获的结构化输出。
func (s *subagent) takeStructured() map[string]any {
	s.structuredMu.Lock()
	defer s.structuredMu.Unlock()
	value := s.structured
	s.structured = nil
	return value
}

// structuredFromText 把最终助手文本解析为 JSON 并按 schema 校验；返回 nil 表示它
// 不是可用的结构化结果。
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

// Wait 实现 rig.Subagent：等待首次运行（或调用方的 context）并返回该次运行的结果。
// 重复调用会返回同一结果。
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

// Send 实现 rig.Subagent：先等待正在进行的运行结束，再运行一个后续轮次并阻塞到
// 它结束。
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

// Stop 实现 rig.Subagent：取消正在进行的运行并释放并发槽位。可重复调用（幂等）。
func (s *subagent) Stop() error {
	s.stopOnce.Do(func() {
		s.stopped.Store(true)
		s.cancel()
		s.runner.release()
	})
	return s.stopErr
}

// finalAssistantText 返回 messages 中最后一条非空助手文本。
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
