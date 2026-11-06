package hosttools_test

// Goal lifecycle tests: the goal tool's action=update path and the in-memory
// host's v1.GoalUpdater state machine.

import (
	"context"
	"strings"
	"sync"
	"testing"

	v1 "github.com/zeokeel/zeokeel-rig/contracts/v1"
	"github.com/zeokeel/zeokeel-rig/tools/hosttools"
)

// goalStoreOnly implements v1.GoalStore but deliberately not v1.GoalUpdater.
type goalStoreOnly struct {
	mu     sync.Mutex
	id     string
	text   string
	status string
}

func (s *goalStoreOnly) Set(text string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.id = "goal-only"
	s.text = text
	s.status = "active"
	return s.id, nil
}

func (s *goalStoreOnly) Get() (string, string, string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.id == "" {
		return "", "", "", false
	}
	return s.id, s.text, s.status, true
}

func (s *goalStoreOnly) Clear() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	had := s.id != ""
	s.id, s.text, s.status = "", "", ""
	return had
}

func TestHostImplementsGoalUpdater(t *testing.T) {
	var updater v1.GoalUpdater = hosttools.NewMemoryHost()
	if updater == nil {
		t.Fatal("NewMemoryHost does not implement v1.GoalUpdater")
	}
}

func TestGoalLifecycleTool(t *testing.T) {
	host := hosttools.NewMemoryHost()
	byName := toolMap(hosttools.Tools(hosttools.Options{Goal: host}))

	// update before any goal is set is a readable error.
	if text := invokeError(t, byName, "goal", `{"action":"update","update":"pause"}`); !strings.Contains(text, "no goal") {
		t.Fatalf("update without goal error = %q", text)
	}
	if result := invoke(t, byName, "goal", `{"action":"set","text":"ship v0.7"}`); result.IsError {
		t.Fatalf("goal set = %q", result.Content)
	}
	if result := invoke(t, byName, "goal", `{"action":"update","update":"pause"}`); result.IsError || result.Content != "paused" {
		t.Fatalf("pause = %q (IsError=%v)", result.Content, result.IsError)
	}
	if result := invoke(t, byName, "goal", `{"action":"get"}`); !strings.Contains(result.Content, "status: paused") {
		t.Fatalf("get after pause = %q", result.Content)
	}
	// Illegal transition: pausing a paused goal.
	if text := invokeError(t, byName, "goal", `{"action":"update","update":"pause"}`); !strings.Contains(text, "cannot pause") {
		t.Fatalf("double pause error = %q", text)
	}
	if result := invoke(t, byName, "goal", `{"action":"update","update":"resume"}`); result.IsError || result.Content != "active" {
		t.Fatalf("resume = %q (IsError=%v)", result.Content, result.IsError)
	}
	if result := invoke(t, byName, "goal", `{"action":"update","update":"blocked","text":"waiting for tests"}`); result.IsError || result.Content != "blocked" {
		t.Fatalf("blocked = %q (IsError=%v)", result.Content, result.IsError)
	}
	if reason := host.GoalReason(); reason != "waiting for tests" {
		t.Fatalf("goal reason = %q", reason)
	}
	// Illegal: a blocked goal cannot be completed directly.
	if text := invokeError(t, byName, "goal", `{"action":"update","update":"complete"}`); !strings.Contains(text, "cannot complete") {
		t.Fatalf("complete blocked goal error = %q", text)
	}
	// edit is allowed from blocked and replaces the objective.
	if result := invoke(t, byName, "goal", `{"action":"update","update":"edit","text":"new objective"}`); result.IsError || result.Content != "blocked" {
		t.Fatalf("edit from blocked = %q (IsError=%v)", result.Content, result.IsError)
	}
	if result := invoke(t, byName, "goal", `{"action":"get"}`); !strings.Contains(result.Content, "new objective") {
		t.Fatalf("get after edit = %q", result.Content)
	}
}

func TestGoalLifecycleComplete(t *testing.T) {
	host := hosttools.NewMemoryHost()
	byName := toolMap(hosttools.Tools(hosttools.Options{Goal: host}))

	invoke(t, byName, "goal", `{"action":"set","text":"finish task"}`)
	invoke(t, byName, "goal", `{"action":"update","update":"pause"}`)
	if result := invoke(t, byName, "goal", `{"action":"update","update":"complete"}`); result.IsError || result.Content != "completed" {
		t.Fatalf("complete from paused = %q (IsError=%v)", result.Content, result.IsError)
	}
	if result := invoke(t, byName, "goal", `{"action":"get"}`); !strings.Contains(result.Content, "status: completed") {
		t.Fatalf("get after complete = %q", result.Content)
	}
	if text := invokeError(t, byName, "goal", `{"action":"update","update":"complete"}`); !strings.Contains(text, "cannot complete") {
		t.Fatalf("double complete error = %q", text)
	}
	if text := invokeError(t, byName, "goal", `{"action":"update","update":"pause"}`); !strings.Contains(text, "cannot pause") {
		t.Fatalf("pause completed error = %q", text)
	}
	if text := invokeError(t, byName, "goal", `{"action":"update","update":"edit","text":"too late"}`); !strings.Contains(text, "cannot edit") {
		t.Fatalf("edit completed error = %q", text)
	}
	// A completed goal can be replaced with Set (existing semantics).
	if result := invoke(t, byName, "goal", `{"action":"set","text":"next goal"}`); result.IsError {
		t.Fatalf("set after complete = %q", result.Content)
	}
	if result := invoke(t, byName, "goal", `{"action":"get"}`); !strings.Contains(result.Content, "status: active") {
		t.Fatalf("get after replacing completed goal = %q", result.Content)
	}
}

func TestGoalUpdateValidation(t *testing.T) {
	host := hosttools.NewMemoryHost()
	byName := toolMap(hosttools.Tools(hosttools.Options{Goal: host}))
	invoke(t, byName, "goal", `{"action":"set","text":"validate"}`)

	cases := map[string]string{
		`{"action":"update","update":"explode"}`:             "unknown update",
		`{"action":"update","update":"blocked"}`:             "reason",
		`{"action":"update","update":"blocked","text":"  "}`: "reason",
		`{"action":"update","update":"edit"}`:                "new objective",
		`{"action":"update","update":"edit","text":"   "}`:   "new objective",
	}
	for arguments, want := range cases {
		if text := invokeError(t, byName, "goal", arguments); !strings.Contains(text, want) {
			t.Fatalf("%s error = %q, want mention of %q", arguments, text, want)
		}
	}
}

func TestGoalUpdateUnsupportedStore(t *testing.T) {
	store := &goalStoreOnly{}
	byName := toolMap(hosttools.Tools(hosttools.Options{Goal: store}))

	// Plain GoalStore: set/get/clear still work.
	if result := invoke(t, byName, "goal", `{"action":"set","text":"plain store"}`); result.IsError {
		t.Fatalf("set on plain store = %q", result.Content)
	}
	if result := invoke(t, byName, "goal", `{"action":"get"}`); !strings.Contains(result.Content, "plain store") {
		t.Fatalf("get on plain store = %q", result.Content)
	}
	text := invokeError(t, byName, "goal", `{"action":"update","update":"complete"}`)
	if !strings.Contains(text, "does not support lifecycle updates") {
		t.Fatalf("plain store update error = %q", text)
	}
}

func TestGoalUpdateHostDirect(t *testing.T) {
	host := hosttools.NewMemoryHost()
	ctx := context.Background()
	if _, err := host.Update(ctx, v1.GoalComplete, ""); err == nil {
		t.Fatal("Update without a goal should fail")
	}
	if _, err := host.Set("direct"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if status, err := host.Update(ctx, v1.GoalPause, ""); err != nil || status != "paused" {
		t.Fatalf("pause: status=%q err=%v", status, err)
	}
	if _, err := host.Update(ctx, v1.GoalPause, ""); err == nil {
		t.Fatal("double pause should fail")
	}
	if status, err := host.Update(ctx, v1.GoalResume, ""); err != nil || status != "active" {
		t.Fatalf("resume: status=%q err=%v", status, err)
	}
	if _, err := host.Update(ctx, v1.GoalBlocked, ""); err == nil {
		t.Fatal("blocked without reason should fail")
	}
	if status, err := host.Update(ctx, v1.GoalBlocked, "needs input"); err != nil || status != "blocked" {
		t.Fatalf("blocked: status=%q err=%v", status, err)
	}
	if _, err := host.Update(ctx, v1.GoalResume, ""); err == nil {
		t.Fatal("resume from blocked should fail")
	}
	if _, err := host.Update(ctx, "dance", ""); err == nil {
		t.Fatal("unknown update should fail")
	}
	if _, err := host.Set(""); err == nil {
		t.Fatal("empty goal text should fail")
	}
}

func TestGoalUpdateConcurrent(t *testing.T) {
	host := hosttools.NewMemoryHost()
	byName := toolMap(hosttools.Tools(hosttools.Options{Goal: host}))
	invoke(t, byName, "goal", `{"action":"set","text":"concurrent"}`)

	var wg sync.WaitGroup
	for index := 0; index < 20; index++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			arguments := []string{
				`{"action":"update","update":"pause"}`,
				`{"action":"update","update":"resume"}`,
				`{"action":"update","update":"blocked","text":"race"}`,
				`{"action":"update","update":"complete"}`,
				`{"action":"get"}`,
				`{"action":"update","update":"nope"}`,
			}[index%6]
			result, err := byName["goal"].Invoke(context.Background(), v1.ToolCall{Arguments: arguments})
			if err != nil {
				t.Errorf("concurrent goal invoke returned Go error: %v", err)
			}
			_ = result // invalid transitions are expected during the race
		}(index)
	}
	wg.Wait()
	// The state must still be one of the documented statuses.
	_, _, status, ok := host.Get()
	if !ok {
		t.Fatal("goal disappeared during concurrent updates")
	}
	switch status {
	case "active", "paused", "blocked", "completed":
	default:
		t.Fatalf("unexpected goal status %q", status)
	}
}
