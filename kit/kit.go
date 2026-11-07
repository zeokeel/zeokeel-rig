package kit

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/agent"
	"github.com/zeokeel/zeokeel-rig/gate"
	"github.com/zeokeel/zeokeel-rig/provider/openai"
	"github.com/zeokeel/zeokeel-rig/sandbox"
	"github.com/zeokeel/zeokeel-rig/session"
	"github.com/zeokeel/zeokeel-rig/tools/full"
	"github.com/zeokeel/zeokeel-rig/tools/ptc"
)

// DefaultRoot is the workspace root used when Config.Root is empty.
const DefaultRoot = "."

// DefaultMaxIterations is the model-round cap used when Config.MaxIterations is
// zero or negative. It is the same value as agent.DefaultMaxIterations.
const DefaultMaxIterations = agent.DefaultMaxIterations

// defaultAllowedTools is the read-only tool set the default approval gate
// allows. Every other tool call is denied and reported to the model; hosts that
// need writes or shell execution configure Config.Gate explicitly.
var defaultAllowedTools = []string{"read_file", "list_dir", "glob", "grep"}

// ApprovalMode is the built-in approval policy used when Config.Gate is nil.
type ApprovalMode string

const (
	// ApprovalAsk is the fail-safe default: only the read-only tool set is
	// allowed and every other call is denied. A host that wants an interactive
	// prompt supplies its own Config.Gate (which may block on user input).
	ApprovalAsk ApprovalMode = "ask"
	// ApprovalAuto allows every tool call. Pair it with the permission Mode the
	// deployment trusts; it is the "auto-approve" preset.
	ApprovalAuto ApprovalMode = "auto"
	// ApprovalNever denies every tool call.
	ApprovalNever ApprovalMode = "never"
)

// Config configures New. Model is the only required field; every other field has
// a documented default, so the zero Config plus a model is a working and
// deliberately conservative agent.
//
// Defaults:
//   - Provider: an openai.Provider built from BaseURL/APIKey/APIKeyEnv with
//     DefaultModel set to Model. Construction does not need an API key; the first
//     Run reports the provider error when no key is available.
//   - Policy: sandbox.New(WithRoots(Root), WithMode(Mode),
//     WithNetwork(AllowNetwork), WithCommandAllowlist(CommandAllowlist...)).
//     The default has no shell allowlist, so shell execution is refused by the
//     policy as well as by the gate.
//   - Tools: tools/full.All with that policy and Root (the complete built-in tool
//     set, including sandboxed file tools and shell).
//   - Gate: depends on Approval: "ask" (default) allows read_file, list_dir,
//     glob and grep and denies the rest; "auto" allows every call; "never"
//     denies every call. A non-nil Config.Gate always wins.
//   - History: session.NewMemory() (in-memory, per-SessionID).
//   - Root: "." (the process working directory at construction time).
//   - MaxIterations: 20.
type Config struct {
	// Model is the model name sent to the provider. Required.
	Model string
	// SystemPrompt is prepended to every model request when non-empty.
	SystemPrompt string
	// MaxIterations caps model<->tool rounds; zero or negative means
	// DefaultMaxIterations (20).
	MaxIterations int

	// BaseURL overrides the chat-completions API root, for example
	// "https://api.deepseek.com/v1". Empty uses the OpenAI default.
	BaseURL string
	// APIKey is the bearer token. When empty, APIKeyEnv (default
	// OPENAI_API_KEY) is read from the environment.
	APIKey string
	// APIKeyEnv names the environment variable holding the API key. Empty
	// means OPENAI_API_KEY.
	APIKeyEnv string
	// Provider overrides the default OpenAI-compatible provider. When non-nil,
	// BaseURL, APIKey and APIKeyEnv are ignored.
	Provider Provider

	// Tools overrides the default tool set. A nil slice means the full default
	// set from tools/full; a non-nil empty slice means no tools at all.
	Tools []Tool
	// Gate overrides the default read-only approval gate.
	Gate Gate
	// History overrides the default in-memory history. A nil History means a
	// fresh session.NewMemory().
	History History

	// Root is the workspace root used by the sandbox policy and the root-aware
	// tools. Empty means DefaultRoot (".").
	Root string
	// Policy overrides the sandbox policy entirely. When non-nil, Root still
	// positions the root-aware reporting tools but Mode, AllowNetwork and
	// CommandAllowlist are ignored (the supplied policy owns those decisions).
	Policy *sandbox.Policy
	// Mode selects the permission mode when Policy is nil:
	// sandbox.ModeWorkspaceWrite (default), sandbox.ModeReadOnly or
	// sandbox.ModeDangerFullAccess. Read-only denies file mutations and shell
	// execution; danger-full-access removes the workspace/allow-list fences and
	// should only be paired with explicit or auto approval.
	Mode sandbox.Mode
	// PTC enables the model-facing run_code tool: programs written by the model
	// run in a system-installed Python 3 or Node.js runtime and call the other
	// tools through the configured approval gate. Off by default; inner calls use
	// the same Gate as normal tool calls.
	PTC bool
	// Approval selects how the built-in gate treats tool calls when Gate is nil:
	// ApprovalAsk (default) allows only the read-only tool set and denies the
	// rest (hosts that prompt install their own Gate), ApprovalAuto allows every
	// call, and ApprovalNever denies every call.
	Approval ApprovalMode
	// AllowNetwork enables outgoing network access when Policy is nil. Even when
	// enabled, the sandbox still refuses loopback/private/link-local/metadata
	// destinations. Default false.
	AllowNetwork bool
	// CommandAllowlist allows the listed shell command basenames when Policy is
	// nil. The default is empty: shell execution is refused.
	CommandAllowlist []string
}

// New assembles a ready-to-run Agent from cfg.
//
// It returns an error wrapping rig.ErrModelRequired when cfg.Model is empty, a
// provider/configuration error when the default provider cannot be built, and a
// validation error when a custom Tool set is invalid (nil tool, empty or
// duplicate name). A workspace root that does not exist is also an error when
// kit builds the sandbox policy itself.
func New(cfg Config) (*Agent, error) {
	model := strings.TrimSpace(cfg.Model)
	if model == "" {
		return nil, fmt.Errorf("kit: %w", rig.ErrModelRequired)
	}
	root := strings.TrimSpace(cfg.Root)
	if root == "" {
		root = DefaultRoot
	}
	maxIterations := cfg.MaxIterations
	if maxIterations <= 0 {
		maxIterations = DefaultMaxIterations
	}

	provider := cfg.Provider
	if provider == nil {
		defaultProvider, err := openai.New(openai.Config{
			BaseURL:      cfg.BaseURL,
			APIKey:       cfg.APIKey,
			APIKeyEnv:    cfg.APIKeyEnv,
			DefaultModel: model,
		})
		if err != nil {
			return nil, fmt.Errorf("kit: provider: %w", err)
		}
		provider = defaultProvider
	}

	mode := cfg.Mode
	if mode == "" {
		mode = sandbox.ModeWorkspaceWrite
	}
	switch mode {
	case sandbox.ModeWorkspaceWrite, sandbox.ModeReadOnly, sandbox.ModeDangerFullAccess:
	default:
		return nil, fmt.Errorf("kit: unknown permission mode %q", mode)
	}
	approval := cfg.Approval
	if approval == "" {
		approval = ApprovalAsk
	}
	switch approval {
	case ApprovalAsk, ApprovalAuto, ApprovalNever:
	default:
		return nil, fmt.Errorf("kit: unknown approval mode %q", approval)
	}

	policy := cfg.Policy
	if policy == nil {
		info, err := os.Stat(root)
		if err != nil {
			return nil, fmt.Errorf("kit: workspace root %q: %w", root, err)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("kit: workspace root %q is not a directory", root)
		}
		policy = sandbox.New(
			sandbox.WithRoots(root),
			sandbox.WithMode(mode),
			sandbox.WithNetwork(cfg.AllowNetwork),
			sandbox.WithCommandAllowlist(cfg.CommandAllowlist...),
		)
	}

	toolSet := cfg.Tools
	if toolSet == nil {
		toolSet = full.All(full.AllOptions{Policy: policy, Root: root})
	}

	gateImpl := cfg.Gate
	if gateImpl == nil {
		switch approval {
		case ApprovalAuto:
			gateImpl = gate.AllowAll()
		case ApprovalNever:
			gateImpl = gate.DenyAll()
		default:
			gateImpl = gate.AllowTools(defaultAllowedTools...)
		}
	}

	if cfg.PTC {
		toolSet = append(toolSet, ptc.New(ptc.Config{
			Tools: slices.Clone(toolSet),
			Gate:  gateImpl,
			Root:  root,
		}))
	}

	history := cfg.History
	if history == nil {
		history = session.NewMemory()
	}

	underlying, err := agent.New(rig.AgentSpec{
		Provider:      provider,
		Model:         model,
		SystemPrompt:  cfg.SystemPrompt,
		MaxIterations: maxIterations,
		Tools:         toolSet,
		Gate:          gateImpl,
		History:       history,
	})
	if err != nil {
		return nil, fmt.Errorf("kit: %w", err)
	}
	return &Agent{agent: underlying}, nil
}

// Agent is a ready-to-run agent assembled by New. It implements rig.Agent
// (RunRequest/RunResult are aliases of the contract types), so it can be passed
// anywhere the interface is expected.
type Agent struct {
	agent rig.Agent
}

var _ rig.Agent = (*Agent)(nil)

// Run implements rig.Agent. It executes one turn of the model<->tool loop.
func (a *Agent) Run(ctx context.Context, req RunRequest) (RunResult, error) {
	return a.agent.Run(ctx, req)
}

// RunText runs one prompt in sessionID and returns the concatenated assistant
// text. emit may be nil; when non-nil it observes every event in order (the same
// events a RunRequest.Emit callback would receive). On error the returned text is
// the partial text emitted before the failure and RunResult is the partial
// result.
func (a *Agent) RunText(ctx context.Context, sessionID, prompt string, emit func(Event)) (string, RunResult, error) {
	var text strings.Builder
	req := RunRequest{
		SessionID: sessionID,
		Input:     []Message{NewMessage(RoleUser, prompt)},
		Emit: func(event Event) {
			if event.Type == EventTextDelta {
				text.WriteString(event.Text)
			}
			if emit != nil {
				emit(event)
			}
		},
	}
	result, err := a.Run(ctx, req)
	return text.String(), result, err
}
