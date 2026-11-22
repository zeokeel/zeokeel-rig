package kit

// Spec/Build is the declarative composition layer: a host (or a config file)
// describes the agent, names the modules/tools/providers/stages it wants, and
// Build resolves those names through the compile-time registry before calling
// New. Names that are not registered are errors, so a config can never trigger
// arbitrary code.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/sandbox"
)

// Spec is the serializable agent description. Interface-shaped pieces are
// referenced by name and resolved from the registry.
type Spec struct {
	Version    int                        `json:"version,omitempty"`
	Provider   ProviderSpec               `json:"provider,omitempty"`
	Model      string                     `json:"model"`
	Prompt     PromptSpec                 `json:"prompt,omitempty"`
	Tools      ToolsSpec                  `json:"tools,omitempty"`
	Policy     PolicySpec                 `json:"policy,omitempty"`
	Approvals  ApprovalsSpec              `json:"approvals,omitempty"`
	Modules    []ModuleRef                `json:"modules,omitempty"`
	Stages     []StageRef                 `json:"stages,omitempty"`
	Extensions map[string]json.RawMessage `json:"extensions,omitempty"`
}

// ProviderSpec selects the provider: either a registered provider name or the
// built-in OpenAI-compatible kind plus connection fields.
type ProviderSpec struct {
	Name      string `json:"name,omitempty"`
	Kind      string `json:"kind,omitempty"`
	BaseURL   string `json:"baseURL,omitempty"`
	APIKey    string `json:"apiKey,omitempty"`
	APIKeyEnv string `json:"apiKeyEnv,omitempty"`
}

// PromptSpec is the static prompt contribution of the config.
type PromptSpec struct {
	System   string          `json:"system,omitempty"`
	Sections []PromptSection `json:"sections,omitempty"`
}

// ToolsSpec narrows/extends the built-in tool set.
type ToolsSpec struct {
	Preset        string   `json:"preset,omitempty"`
	Discovery     *bool    `json:"discovery,omitempty"`
	InitialActive []string `json:"initialActive,omitempty"`
	Extra         []string `json:"extra,omitempty"`
}

// PolicySpec is the workspace/sandbox policy.
type PolicySpec struct {
	Root        string   `json:"root,omitempty"`
	Roots       []string `json:"roots,omitempty"`
	Mode        string   `json:"mode,omitempty"`
	Network     *bool    `json:"network,omitempty"`
	HiddenPaths []string `json:"hiddenPaths,omitempty"`
}

// ApprovalsSpec is the approval preset and behavior.
type ApprovalsSpec struct {
	Preset       string `json:"preset,omitempty"`
	AskTimeoutMS int    `json:"askTimeoutMs,omitempty"`
	Scope        string `json:"scope,omitempty"`
}

// ModuleRef selects a registered module.
type ModuleRef struct {
	Name   string          `json:"name"`
	Params json.RawMessage `json:"params,omitempty"`
}

// StagePhase names the stage slot a StageRef resolves into.
type StagePhase string

const (
	PhasePrompt     StagePhase = "prompt"
	PhaseRequest    StagePhase = "request"
	PhaseResponse   StagePhase = "response"
	PhaseTurn       StagePhase = "turn"
	PhaseCompaction StagePhase = "compaction"
)

// StageRef selects a registered stage.
type StageRef struct {
	Name   string          `json:"name"`
	Phase  StagePhase      `json:"phase"`
	Params json.RawMessage `json:"params,omitempty"`
}

// StageSet is the set of stages a module contributes.
type StageSet struct {
	Prompt     []rig.PromptStage
	Request    []rig.RequestStage
	Response   []rig.ResponseStage
	Turn       []rig.TurnStage
	Compaction []rig.CompactionStage
}

// Module is a host capability bundle: tools, prompt sections, stages and hooks
// that are contributed to one compiled agent.
type Module interface {
	Name() string
	Order() int
	Tools() []rig.Tool
	PromptSections() []PromptSection
	Stages() StageSet
	Hooks() []rig.TurnHook
}

// BuildContext is handed to module initializers after the Config has been
// compiled but before the agent is constructed. A module may adjust the Config
// (for example add a prompt section) in Init.
type BuildContext struct {
	Spec   Spec
	Config *Config
}

// ModuleInitializer is the optional Init hook of a Module.
type ModuleInitializer interface {
	Init(ctx context.Context, build *BuildContext) error
}

// ModuleCloser is the optional Close hook of a Module; Build registers it with
// the agent so Shutdown closes modules in reverse order.
type ModuleCloser interface {
	Close(ctx context.Context) error
}

// ModuleFactory builds one module from its JSON params.
type ModuleFactory func(json.RawMessage) (Module, error)

// ToolFactory builds one tool from its JSON params.
type ToolFactory func(json.RawMessage) (rig.Tool, error)

// ProviderFactory builds one provider from its JSON params.
type ProviderFactory func(json.RawMessage) (rig.Provider, error)

// StageFactory builds one stage (an implementation of the phase interface).
type StageFactory func(json.RawMessage) (any, error)

var registry = struct {
	sync.RWMutex
	modules   map[string]ModuleFactory
	tools     map[string]ToolFactory
	providers map[string]ProviderFactory
	stages    map[string]stageEntry
}{}

type stageEntry struct {
	phase   StagePhase
	factory StageFactory
}

// RegisterModule adds a module factory. Duplicate names are errors.
func RegisterModule(name string, factory ModuleFactory) error {
	registry.Lock()
	defer registry.Unlock()
	if registry.modules == nil {
		registry.modules = make(map[string]ModuleFactory)
	}
	if _, exists := registry.modules[name]; exists {
		return fmt.Errorf("kit: module %q is already registered", name)
	}
	registry.modules[name] = factory
	return nil
}

// RegisterTool adds a tool factory. Duplicate names are errors.
func RegisterTool(name string, factory ToolFactory) error {
	registry.Lock()
	defer registry.Unlock()
	if registry.tools == nil {
		registry.tools = make(map[string]ToolFactory)
	}
	if _, exists := registry.tools[name]; exists {
		return fmt.Errorf("kit: tool %q is already registered", name)
	}
	registry.tools[name] = factory
	return nil
}

// RegisterProvider adds a provider factory. Duplicate names are errors.
func RegisterProvider(name string, factory ProviderFactory) error {
	registry.Lock()
	defer registry.Unlock()
	if registry.providers == nil {
		registry.providers = make(map[string]ProviderFactory)
	}
	if _, exists := registry.providers[name]; exists {
		return fmt.Errorf("kit: provider %q is already registered", name)
	}
	registry.providers[name] = factory
	return nil
}

// RegisterStage adds a stage factory for one phase. Duplicate names are errors.
func RegisterStage(name string, phase StagePhase, factory StageFactory) error {
	registry.Lock()
	defer registry.Unlock()
	if registry.stages == nil {
		registry.stages = make(map[string]stageEntry)
	}
	if _, exists := registry.stages[name]; exists {
		return fmt.Errorf("kit: stage %q is already registered", name)
	}
	registry.stages[name] = stageEntry{phase: phase, factory: factory}
	return nil
}

// FromAgentSpec maps a directly-built rig.AgentSpec onto kit.Config. It exists
// for hosts migrating from agent.New: every field kit can express is copied;
// kit-specific defaults (presets, discovery, sandbox assembly) are NOT applied
// because the caller already resolved them.
func FromAgentSpec(spec rig.AgentSpec) Config {
	return Config{
		Provider:           spec.Provider,
		Model:              spec.Model,
		SystemPrompt:       spec.SystemPrompt,
		SystemPromptFunc:   spec.SystemPromptFunc,
		MaxIterations:      spec.MaxIterations,
		Temperature:        spec.Temperature,
		TopP:               spec.TopP,
		MaxTokens:          spec.MaxTokens,
		Tools:              spec.Tools,
		InitialActiveTools: spec.InitialActiveTools,
		Gate:               spec.Gate,
		History:            spec.History,
		ContextBudget:      spec.ContextBudget,
		Compactor:          spec.Compactor,
		ContextRecovery:    spec.ContextRecovery,
		Spill:              spec.Spill,
		CheckpointStore:    spec.CheckpointStore,
		Hooks:              spec.Hooks,
		PromptStages:       spec.PromptStages,
		RequestStages:      spec.RequestStages,
		ResponseStages:     spec.ResponseStages,
		TurnStages:         spec.TurnStages,
		CompactionStages:   spec.CompactionStages,
		Activations:        spec.ActivationStore,
	}
}

// Build resolves the spec through the registry and constructs the agent.
func Build(ctx context.Context, spec Spec) (*Agent, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	cfg, modules, err := spec.compile()
	if err != nil {
		return nil, err
	}
	build := &BuildContext{Spec: spec, Config: &cfg}
	for _, module := range modules {
		if initializer, ok := module.(ModuleInitializer); ok {
			if err := initializer.Init(ctx, build); err != nil {
				return nil, fmt.Errorf("kit: module %q init: %w", module.Name(), err)
			}
		}
	}
	agent, err := New(cfg)
	if err != nil {
		return nil, err
	}
	agent.attachModules(modules)
	return agent, nil
}

// BuildConfig builds an agent from a fully resolved Config plus dynamic modules.
// This is the composition path for hosts whose capabilities are per-run Go
// values (a product's live history/gate/prompt): Spec/Build is for declarative
// hosts, BuildConfig is for imperative ones. Modules contribute tools, prompt
// sections, stages, hooks and capabilities exactly as in Build.
func BuildConfig(ctx context.Context, cfg Config, modules ...Module) (*Agent, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	sort.SliceStable(modules, func(i, j int) bool { return modules[i].Order() < modules[j].Order() })
	if err := mergeModules(&cfg, modules); err != nil {
		return nil, err
	}
	build := &BuildContext{Config: &cfg}
	for _, module := range modules {
		if initializer, ok := module.(ModuleInitializer); ok {
			if err := initializer.Init(ctx, build); err != nil {
				return nil, fmt.Errorf("kit: module %q init: %w", module.Name(), err)
			}
		}
	}
	agent, err := New(cfg)
	if err != nil {
		return nil, err
	}
	agent.attachModules(modules)
	return agent, nil
}

// mergeModules applies module contributions to cfg.
func mergeModules(cfg *Config, modules []Module) error {
	stages := StageSet{}
	for _, module := range modules {
		cfg.ExtraTools = append(cfg.ExtraTools, module.Tools()...)
		cfg.PromptSections = append(cfg.PromptSections, module.PromptSections()...)
		cfg.Hooks = append(cfg.Hooks, module.Hooks()...)
		stages = mergeStages(stages, module.Stages())
		if provider, ok := module.(ModuleCapabilities); ok {
			if err := ApplyCapabilities(cfg, provider.Capabilities()); err != nil {
				return fmt.Errorf("kit: module %q: %w", module.Name(), err)
			}
		}
	}
	cfg.PromptStages = append(cfg.PromptStages, stages.Prompt...)
	cfg.RequestStages = append(cfg.RequestStages, stages.Request...)
	cfg.ResponseStages = append(cfg.ResponseStages, stages.Response...)
	cfg.TurnStages = append(cfg.TurnStages, stages.Turn...)
	cfg.CompactionStages = append(cfg.CompactionStages, stages.Compaction...)
	return nil
}

// Validate rejects specs that cannot describe an agent.
func (s Spec) Validate() error {
	if s.Version != 0 && s.Version != 1 {
		return fmt.Errorf("kit: unsupported spec version %d", s.Version)
	}
	if strings.TrimSpace(s.Model) == "" {
		return errors.New("kit: spec model is required")
	}
	if s.Provider.Name != "" && strings.TrimSpace(s.Provider.Kind) != "" {
		return errors.New("kit: spec provider.name and provider.kind are mutually exclusive")
	}
	if s.Provider.Name == "" && s.Provider.Kind == "" && s.Provider.BaseURL == "" {
		// The built-in default (OpenAI-compatible + env key) is still a valid
		// description; New reports a missing key at run time.
	}
	if s.Approvals.AskTimeoutMS < 0 {
		return errors.New("kit: approvals.askTimeoutMs must not be negative")
	}
	return nil
}

// compile compiles the spec (and its modules) into a kit.Config.
func (s Spec) compile() (Config, []Module, error) {
	cfg := Config{
		Model:              strings.TrimSpace(s.Model),
		SystemPrompt:       s.Prompt.System,
		PromptSections:     append([]PromptSection(nil), s.Prompt.Sections...),
		ToolPreset:         s.Tools.Preset,
		ToolDiscovery:      s.Tools.Discovery,
		InitialActiveTools: append([]string(nil), s.Tools.InitialActive...),
		Root:               s.Policy.Root,
		Roots:              append([]string(nil), s.Policy.Roots...),
		AllowNetwork:       s.Policy.Network,
		HiddenPaths:        append([]string(nil), s.Policy.HiddenPaths...),
		Approval:           ApprovalMode(s.Approvals.Preset),
		ApprovalScope:      s.Approvals.Scope,
	}
	if s.Policy.Mode != "" {
		cfg.Mode = sandbox.Mode(s.Policy.Mode)
	}
	if s.Approvals.AskTimeoutMS > 0 {
		cfg.AskTimeout = time.Duration(s.Approvals.AskTimeoutMS) * time.Millisecond
	}
	if s.Provider.Name != "" {
		provider, err := resolveProvider(s.Provider.Name, nil)
		if err != nil {
			return Config{}, nil, err
		}
		cfg.Provider = provider
	} else {
		cfg.ProviderKind = ProviderKind(strings.TrimSpace(s.Provider.Kind))
		cfg.BaseURL = os.ExpandEnv(s.Provider.BaseURL)
		cfg.APIKey = os.ExpandEnv(s.Provider.APIKey)
		cfg.APIKeyEnv = s.Provider.APIKeyEnv
	}
	// Modules: resolve, order, merge.
	modules := make([]Module, 0, len(s.Modules))
	for _, ref := range s.Modules {
		factory, ok := lookupModule(ref.Name)
		if !ok {
			return Config{}, nil, fmt.Errorf("kit: module %q is not registered", ref.Name)
		}
		module, err := factory(ref.Params)
		if err != nil {
			return Config{}, nil, fmt.Errorf("kit: module %q: %w", ref.Name, err)
		}
		modules = append(modules, module)
	}
	sort.SliceStable(modules, func(i, j int) bool { return modules[i].Order() < modules[j].Order() })
	stages := StageSet{}
	for _, module := range modules {
		cfg.ExtraTools = append(cfg.ExtraTools, module.Tools()...)
		cfg.PromptSections = append(cfg.PromptSections, module.PromptSections()...)
		cfg.Hooks = append(cfg.Hooks, module.Hooks()...)
		stages = mergeStages(stages, module.Stages())
		if provider, ok := module.(ModuleCapabilities); ok {
			if err := ApplyCapabilities(&cfg, provider.Capabilities()); err != nil {
				return Config{}, nil, fmt.Errorf("kit: module %q: %w", module.Name(), err)
			}
		}
	}
	// Explicit tool refs (business tools registered by name).
	for _, name := range s.Tools.Extra {
		tool, err := resolveTool(name, nil)
		if err != nil {
			return Config{}, nil, err
		}
		cfg.ExtraTools = append(cfg.ExtraTools, tool)
	}
	// Explicit stage refs.
	for _, ref := range s.Stages {
		stage, err := resolveStage(ref)
		if err != nil {
			return Config{}, nil, err
		}
		stages = addStage(stages, ref.Phase, stage)
	}
	cfg.PromptStages = stages.Prompt
	cfg.RequestStages = stages.Request
	cfg.ResponseStages = stages.Response
	cfg.TurnStages = stages.Turn
	cfg.CompactionStages = stages.Compaction
	return cfg, modules, nil
}

func lookupModule(name string) (ModuleFactory, bool) {
	registry.RLock()
	defer registry.RUnlock()
	factory, ok := registry.modules[name]
	return factory, ok
}

func resolveTool(name string, params json.RawMessage) (rig.Tool, error) {
	registry.RLock()
	factory, ok := registry.tools[name]
	registry.RUnlock()
	if !ok {
		return nil, fmt.Errorf("kit: tool %q is not registered", name)
	}
	return factory(params)
}

func resolveProvider(name string, params json.RawMessage) (rig.Provider, error) {
	registry.RLock()
	factory, ok := registry.providers[name]
	registry.RUnlock()
	if !ok {
		return nil, fmt.Errorf("kit: provider %q is not registered", name)
	}
	return factory(params)
}

func resolveStage(ref StageRef) (any, error) {
	registry.RLock()
	entry, ok := registry.stages[ref.Name]
	registry.RUnlock()
	if !ok {
		return nil, fmt.Errorf("kit: stage %q is not registered", ref.Name)
	}
	if entry.phase != ref.Phase {
		return nil, fmt.Errorf("kit: stage %q is registered for phase %q, not %q", ref.Name, entry.phase, ref.Phase)
	}
	return entry.factory(ref.Params)
}

func mergeStages(into, from StageSet) StageSet {
	into.Prompt = append(into.Prompt, from.Prompt...)
	into.Request = append(into.Request, from.Request...)
	into.Response = append(into.Response, from.Response...)
	into.Turn = append(into.Turn, from.Turn...)
	into.Compaction = append(into.Compaction, from.Compaction...)
	return into
}

func addStage(set StageSet, phase StagePhase, stage any) StageSet {
	switch phase {
	case PhasePrompt:
		if typed, ok := stage.(rig.PromptStage); ok {
			set.Prompt = append(set.Prompt, typed)
		}
	case PhaseRequest:
		if typed, ok := stage.(rig.RequestStage); ok {
			set.Request = append(set.Request, typed)
		}
	case PhaseResponse:
		if typed, ok := stage.(rig.ResponseStage); ok {
			set.Response = append(set.Response, typed)
		}
	case PhaseTurn:
		if typed, ok := stage.(rig.TurnStage); ok {
			set.Turn = append(set.Turn, typed)
		}
	case PhaseCompaction:
		if typed, ok := stage.(rig.CompactionStage); ok {
			set.Compaction = append(set.Compaction, typed)
		}
	}
	return set
}

// LoadFile decodes a strict JSON spec (unknown fields are errors) and expands
// ${ENV} in the provider connection fields.
func LoadFile(path string) (Spec, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Spec{}, fmt.Errorf("kit: read spec: %w", err)
	}
	var spec Spec
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&spec); err != nil {
		return Spec{}, fmt.Errorf("kit: decode spec %q: %w", path, err)
	}
	spec.Provider.BaseURL = os.ExpandEnv(spec.Provider.BaseURL)
	spec.Provider.APIKey = os.ExpandEnv(spec.Provider.APIKey)
	if err := spec.Validate(); err != nil {
		return Spec{}, fmt.Errorf("kit: spec %q: %w", path, err)
	}
	return spec, nil
}
