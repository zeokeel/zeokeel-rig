package settings_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	v1 "github.com/zeokeel/zeokeel-rig/contracts/v1"
	"github.com/zeokeel/zeokeel-rig/tools/settings"
)

// memorySettings is a minimal v1.Settings test double.
type memorySettings struct {
	mu     sync.Mutex
	value  map[string]string
	setErr error
}

func newMemorySettings() *memorySettings {
	return &memorySettings{value: make(map[string]string)}
}

func (s *memorySettings) Get(key, def string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if value, ok := s.value[key]; ok {
		return value
	}
	return def
}

func (s *memorySettings) Set(key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.setErr != nil {
		return s.setErr
	}
	s.value[key] = value
	return nil
}

func toolsMap(tools []v1.Tool) map[string]v1.Tool {
	byName := make(map[string]v1.Tool, len(tools))
	for _, tool := range tools {
		byName[tool.Name()] = tool
	}
	return byName
}

func invoke(t *testing.T, tools map[string]v1.Tool, name, arguments string) v1.ToolResult {
	t.Helper()
	tool, ok := tools[name]
	if !ok {
		t.Fatalf("tool %q not found", name)
	}
	result, err := tool.Invoke(context.Background(), v1.ToolCall{ID: "call-1", Name: name, Arguments: arguments})
	if err != nil {
		t.Fatalf("%s.Invoke returned Go error: %v", name, err)
	}
	return result
}

func invokeError(t *testing.T, tools map[string]v1.Tool, name, arguments string) string {
	t.Helper()
	result := invoke(t, tools, name, arguments)
	if !result.IsError {
		t.Fatalf("%s: result = %q, want IsError", name, result.Content)
	}
	return result.Content
}

func TestToolsShape(t *testing.T) {
	tools := settings.Tools(newMemorySettings())
	if len(tools) != 2 {
		t.Fatalf("settings.Tools returned %d tools, want 2", len(tools))
	}
	byName := toolsMap(tools)
	for _, name := range []string{"settings_get", "settings_set"} {
		tool, ok := byName[name]
		if !ok {
			t.Fatalf("missing tool %q", name)
		}
		if tool.Description() == "" {
			t.Fatalf("tool %q has no description", name)
		}
		schema := tool.Schema()
		if schema["type"] != "object" {
			t.Fatalf("tool %q schema type = %v", name, schema["type"])
		}
		if _, exists := schema["required"]; !exists {
			t.Fatalf("tool %q schema has no required list", name)
		}
	}
}

func TestGetSet(t *testing.T) {
	store := newMemorySettings()
	byName := toolsMap(settings.Tools(store))

	result := invoke(t, byName, "settings_set", `{"key":"ui.theme","value":"dark"}`)
	if result.IsError || !strings.Contains(result.Content, "ui.theme") {
		t.Fatalf("settings_set = %q (IsError=%v)", result.Content, result.IsError)
	}
	result = invoke(t, byName, "settings_get", `{"key":"ui.theme"}`)
	if result.IsError || result.Content != "dark" {
		t.Fatalf("settings_get = %q (IsError=%v), want dark", result.Content, result.IsError)
	}
	// Missing key: the explicit default, then the empty-string default.
	result = invoke(t, byName, "settings_get", `{"key":"missing","default":"fallback"}`)
	if result.IsError || result.Content != "fallback" {
		t.Fatalf("settings_get default = %q", result.Content)
	}
	result = invoke(t, byName, "settings_get", `{"key":"missing"}`)
	if result.IsError || result.Content != "" {
		t.Fatalf("settings_get without default = %q", result.Content)
	}
	// Empty values are allowed.
	invoke(t, byName, "settings_set", `{"key":"empty","value":""}`)
	if result := invoke(t, byName, "settings_get", `{"key":"empty","default":"x"}`); result.IsError || result.Content != "" {
		t.Fatalf("empty value round-trip = %q", result.Content)
	}
}

func TestArbitraryKeys(t *testing.T) {
	byName := toolsMap(settings.Tools(newMemorySettings()))
	keys := []string{"a", "model.name", "ui.theme/dark mode", "key with spaces", "ÜTF-8☃"}
	for index, key := range keys {
		value := fmt.Sprintf("value-%d", index)
		arguments := fmt.Sprintf(`{"key":%q,"value":%q}`, key, value)
		if result := invoke(t, byName, "settings_set", arguments); result.IsError {
			t.Fatalf("set key %q: %q", key, result.Content)
		}
		getArguments := fmt.Sprintf(`{"key":%q}`, key)
		if result := invoke(t, byName, "settings_get", getArguments); result.IsError || result.Content != value {
			t.Fatalf("get key %q = %q", key, result.Content)
		}
	}
}

func TestNilSettings(t *testing.T) {
	for name, arguments := range map[string]string{
		"settings_get": `{"key":"a"}`,
		"settings_set": `{"key":"a","value":"b"}`,
	} {
		text := invokeError(t, toolsMap(settings.Tools(nil)), name, arguments)
		if !strings.Contains(text, "Settings") {
			t.Fatalf("%s nil-store error = %q", name, text)
		}
	}
}

type nilStore struct{}

func (s *nilStore) Get(string, string) string { return "never" }
func (s *nilStore) Set(string, string) error  { return nil }

func TestTypedNilSettings(t *testing.T) {
	var store *nilStore
	text := invokeError(t, toolsMap(settings.Tools(store)), "settings_get", `{"key":"a"}`)
	if !strings.Contains(text, "Settings") {
		t.Fatalf("typed nil store error = %q", text)
	}
}

func TestSetError(t *testing.T) {
	store := newMemorySettings()
	store.setErr = errors.New("disk is full")
	text := invokeError(t, toolsMap(settings.Tools(store)), "settings_set", `{"key":"a","value":"b"}`)
	if !strings.Contains(text, "disk is full") {
		t.Fatalf("set error = %q", text)
	}
}

func TestEmptyKey(t *testing.T) {
	byName := toolsMap(settings.Tools(newMemorySettings()))
	for name, arguments := range map[string]string{
		"settings_get": `{"key":""}`,
		"settings_set": `{"key":"","value":"x"}`,
	} {
		text := invokeError(t, byName, name, arguments)
		if !strings.Contains(text, "key is required") {
			t.Fatalf("%s empty key error = %q", name, text)
		}
	}
}

func TestInvalidArgumentsJSON(t *testing.T) {
	byName := toolsMap(settings.Tools(newMemorySettings()))
	for _, name := range []string{"settings_get", "settings_set"} {
		if _, err := byName[name].Invoke(context.Background(), v1.ToolCall{Arguments: `{"bad":`}); err == nil {
			t.Fatalf("%s: invalid JSON did not return a Go error", name)
		}
	}
}

func TestContextCancellation(t *testing.T) {
	byName := toolsMap(settings.Tools(newMemorySettings()))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for name, arguments := range map[string]string{
		"settings_get": `{"key":"a"}`,
		"settings_set": `{"key":"a","value":"b"}`,
	} {
		if _, err := byName[name].Invoke(ctx, v1.ToolCall{Arguments: arguments}); !errors.Is(err, context.Canceled) {
			t.Fatalf("%s: cancelled invoke error = %v", name, err)
		}
	}
}

func TestConcurrentGetSet(t *testing.T) {
	byName := toolsMap(settings.Tools(newMemorySettings()))
	var wg sync.WaitGroup
	var mu sync.Mutex
	var failures []string
	for index := 0; index < 20; index++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			key := fmt.Sprintf("key-%d", index)
			for round := 0; round < 10; round++ {
				value := fmt.Sprintf("value-%d-%d", index, round)
				setArguments := fmt.Sprintf(`{"key":%q,"value":%q}`, key, value)
				if result, err := byName["settings_set"].Invoke(context.Background(),
					v1.ToolCall{Arguments: setArguments}); err != nil || result.IsError {
					mu.Lock()
					failures = append(failures, result.Content)
					mu.Unlock()
				}
				getArguments := fmt.Sprintf(`{"key":%q}`, key)
				if result, err := byName["settings_get"].Invoke(context.Background(),
					v1.ToolCall{Arguments: getArguments}); err != nil || result.IsError {
					mu.Lock()
					failures = append(failures, result.Content)
					mu.Unlock()
				}
			}
		}(index)
	}
	wg.Wait()
	if len(failures) > 0 {
		t.Fatalf("concurrent settings calls failed: %v", failures)
	}
}
