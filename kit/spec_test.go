package kit_test

// Spec/Build is the declarative composition path: names are resolved through the
// compile-time registry, modules contribute tools/prompt sections/stages, and an
// unknown name is an error (a config cannot execute arbitrary code).

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/kit"
)

type fakeProvider struct{ requests []rig.Request }

func (p *fakeProvider) Name() string { return "fake" }

func (p *fakeProvider) Stream(ctx context.Context, req rig.Request) (rig.Stream, error) {
	p.requests = append(p.requests, req)
	return &fakeStream{}, nil
}

type fakeStream struct{ done bool }

func (s *fakeStream) Recv() (rig.Chunk, error) {
	if s.done {
		return rig.Chunk{}, io.EOF
	}
	s.done = true
	return rig.Chunk{Delta: "ok", FinishReason: "stop"}, nil
}

type specTool struct{ name string }

func (t *specTool) Name() string           { return t.name }
func (t *specTool) Description() string    { return "test tool" }
func (t *specTool) Schema() map[string]any { return map[string]any{"type": "object"} }
func (t *specTool) Invoke(context.Context, rig.ToolCall) (rig.ToolResult, error) {
	return rig.ToolResult{Content: "ok"}, nil
}

type demoModule struct{ promptSeen *bool }

func (m *demoModule) Name() string      { return "demo" }
func (m *demoModule) Order() int        { return 10 }
func (m *demoModule) Tools() []rig.Tool { return []rig.Tool{&specTool{name: "demo_tool"}} }
func (m *demoModule) PromptSections() []kit.PromptSection {
	return []kit.PromptSection{{Name: "demo", Body: "module rules"}}
}
func (m *demoModule) Stages() kit.StageSet {
	return kit.StageSet{Prompt: []rig.PromptStage{demoPromptStage{seen: m.promptSeen}}}
}
func (m *demoModule) Hooks() []rig.TurnHook { return nil }

type demoPromptStage struct{ seen *bool }

func (s demoPromptStage) Prompt(_ context.Context, draft *rig.PromptDraft) error {
	if s.seen != nil {
		*s.seen = strings.Contains(draft.SystemPrompt, "module rules")
	}
	return nil
}

func TestBuildResolvesModules(t *testing.T) {
	provider := &fakeProvider{}
	if err := kit.RegisterProvider("fake", func(json.RawMessage) (rig.Provider, error) {
		return provider, nil
	}); err != nil {
		t.Fatal(err)
	}
	promptSeen := false
	if err := kit.RegisterModule("demo", func(json.RawMessage) (kit.Module, error) {
		return &demoModule{promptSeen: &promptSeen}, nil
	}); err != nil {
		t.Fatal(err)
	}
	agent, err := kit.Build(context.Background(), kit.Spec{
		Model:    "m",
		Provider: kit.ProviderSpec{Name: "fake"},
		Prompt:   kit.PromptSpec{System: "base"},
		Modules:  []kit.ModuleRef{{Name: "demo"}},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if _, ok := agent.Tool("demo_tool"); !ok {
		t.Fatal("module tool was not registered")
	}
	if _, err := agent.Run(context.Background(), kit.RunRequest{}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !promptSeen {
		t.Fatal("module prompt section was not part of the system prompt")
	}
	if len(provider.requests) == 0 {
		t.Fatal("provider was not called")
	}
	prompt := provider.requests[0].Messages[0].Text()
	if !strings.Contains(prompt, "base") || !strings.Contains(prompt, "module rules") {
		t.Fatalf("request prompt = %q", prompt)
	}
}

func TestBuildRejectsUnknownNames(t *testing.T) {
	if _, err := kit.Build(context.Background(), kit.Spec{Model: "m", Modules: []kit.ModuleRef{{Name: "nope"}}}); err == nil {
		t.Fatal("unknown module accepted")
	}
	if _, err := kit.Build(context.Background(), kit.Spec{Model: "m", Tools: kit.ToolsSpec{Extra: []string{"nope"}}}); err == nil {
		t.Fatal("unknown tool accepted")
	}
	if _, err := kit.Build(context.Background(), kit.Spec{Model: "m", Stages: []kit.StageRef{{Name: "nope", Phase: kit.PhasePrompt}}}); err == nil {
		t.Fatal("unknown stage accepted")
	}
}

func TestSpecValidate(t *testing.T) {
	if err := (kit.Spec{}).Validate(); err == nil {
		t.Fatal("empty model accepted")
	}
	if err := (kit.Spec{Model: "m", Version: 9}).Validate(); err == nil {
		t.Fatal("unknown version accepted")
	}
	if err := (kit.Spec{Model: "m", Provider: kit.ProviderSpec{Name: "x", Kind: "openai"}}).Validate(); err == nil {
		t.Fatal("name+kind accepted")
	}
}

func TestLoadFileStrictAndEnv(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.json")
	t.Setenv("SPEC_KEY", "secret")
	content := `{"version":1,"model":"m","provider":{"kind":"openai","apiKey":"${SPEC_KEY}"}}`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	spec, err := kit.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if spec.Provider.APIKey != "secret" {
		t.Fatalf("apiKey = %q, want env expansion", spec.Provider.APIKey)
	}
	if err := os.WriteFile(path, []byte(`{"model":"m","unknown":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := kit.LoadFile(path); err == nil {
		t.Fatal("unknown field accepted")
	}
}
