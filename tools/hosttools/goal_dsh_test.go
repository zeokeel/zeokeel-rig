package hosttools_test

// DSH-named goal tool tests: get_goal/create_goal/update_goal revision/CAS
// semantics over the optional hosttools.GoalRevisionStore capability.

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/tools/hosttools"
)

// decodedGoal is the canonical model result of the DSH goal tools.
type decodedGoal struct {
	Goal *struct {
		ID            string `json:"id"`
		Revision      int    `json:"revision"`
		Objective     string `json:"objective"`
		Phase         string `json:"phase"`
		RoundsStarted int    `json:"roundsStarted"`
		MaxGoalRounds int    `json:"maxGoalRounds"`
		BlockedReason *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"blockedReason"`
	} `json:"goal"`
	Activation string `json:"activation"`
}

// decodeGoalResult decodes one successful goal tool result.
func decodeGoalResult(t *testing.T, result rig.ToolResult) decodedGoal {
	t.Helper()
	if result.IsError {
		t.Fatalf("goal tool returned an error: %q", result.Content)
	}
	var decoded decodedGoal
	if err := json.Unmarshal([]byte(result.Content), &decoded); err != nil {
		t.Fatalf("goal result %q is not JSON: %v", result.Content, err)
	}
	return decoded
}

// requireGoal decodes a result and fails unless it is the expected goal shape.
// requireErrorContains asserts that one tool error text mentions want.
func requireErrorContains(t *testing.T, text, want string) {
	t.Helper()
	if !strings.Contains(text, want) {
		t.Fatalf("tool error %q does not contain %q", text, want)
	}
}

func requireGoal(t *testing.T, result rig.ToolResult, wantPhase string, wantRevision int) decodedGoal {
	t.Helper()
	decoded := decodeGoalResult(t, result)
	if decoded.Goal == nil {
		t.Fatalf("goal result has no goal: %q", result.Content)
	}
	if decoded.Goal.Phase != wantPhase || decoded.Goal.Revision != wantRevision {
		t.Fatalf("goal = phase %q revision %d, want phase %q revision %d", decoded.Goal.Phase, decoded.Goal.Revision, wantPhase, wantRevision)
	}
	return decoded
}

// TestDSHGoalToolShape checks the three DSH-named tools, their schemas and the
// canonical empty result.
func TestDSHGoalToolShape(t *testing.T) {
	host := hosttools.NewMemoryHost()
	byName := toolMap(hosttools.Tools(hosttools.Options{Goal: host}))
	for _, name := range []string{hosttools.ToolGetGoal, hosttools.ToolCreateGoal, hosttools.ToolUpdateGoal} {
		tool, ok := byName[name]
		if !ok {
			t.Fatalf("missing tool %q", name)
		}
		if strings.TrimSpace(tool.Description()) == "" {
			t.Fatalf("%s has an empty description", name)
		}
		if schema := tool.Schema(); schema["type"] != "object" {
			t.Fatalf("%s schema = %v", name, schema)
		}
		if _, err := json.Marshal(tool.Schema()); err != nil {
			t.Fatalf("%s schema is not JSON-marshalable: %v", name, err)
		}
	}

	get := byName[hosttools.ToolGetGoal]
	if got := invoke(t, byName, hosttools.ToolGetGoal, `{}`); got.Content != `{"goal":null}` {
		t.Fatalf("empty get_goal = %q, want {\"goal\":null}", got.Content)
	}
	if required := get.Schema()["required"]; required != nil {
		t.Fatalf("get_goal required = %v, want none", required)
	}

	create := byName[hosttools.ToolCreateGoal]
	createRequired, ok := create.Schema()["required"].([]string)
	if !ok || len(createRequired) != 1 || createRequired[0] != "objective" {
		t.Fatalf("create_goal required = %v", create.Schema()["required"])
	}

	update := byName[hosttools.ToolUpdateGoal]
	updateRequired, ok := update.Schema()["required"].([]string)
	if !ok || len(updateRequired) != 3 || updateRequired[0] != "goal_id" || updateRequired[1] != "revision" || updateRequired[2] != "action" {
		t.Fatalf("update_goal required = %v", update.Schema()["required"])
	}
	properties, ok := update.Schema()["properties"].(map[string]any)
	if !ok {
		t.Fatalf("update_goal properties = %v", update.Schema()["properties"])
	}
	action, ok := properties["action"].(map[string]any)
	if !ok {
		t.Fatalf("update_goal action schema = %v", properties["action"])
	}
	enum, ok := action["enum"].([]string)
	if !ok || len(enum) != 5 {
		t.Fatalf("update_goal action enum = %v", action["enum"])
	}
}

// TestDSHGoalLifecycle walks create/get/update through the DSH phases.
func TestDSHGoalLifecycle(t *testing.T) {
	host := hosttools.NewMemoryHost()
	byName := toolMap(hosttools.Tools(hosttools.Options{Goal: host}))

	created := requireGoal(t, invoke(t, byName, hosttools.ToolCreateGoal, `{"objective":"ship v1"}`), "active", 1)
	if created.Goal.ID != "goal-1" || created.Goal.Objective != "ship v1" {
		t.Fatalf("created goal = %+v", created.Goal)
	}
	if created.Goal.RoundsStarted != 0 || created.Goal.MaxGoalRounds != hosttools.DefaultMaxGoalRounds {
		t.Fatalf("created counters = rounds %d cap %d", created.Goal.RoundsStarted, created.Goal.MaxGoalRounds)
	}
	if created.Activation != string(hosttools.GoalActivationArmed) {
		t.Fatalf("created activation = %q", created.Activation)
	}

	// A second create is rejected while the goal is not complete.
	requireErrorContains(t, invokeError(t, byName, hosttools.ToolCreateGoal, `{"objective":"second"}`), "already exists")

	// get_goal returns the same exact ref.
	got := requireGoal(t, invoke(t, byName, hosttools.ToolGetGoal, `{}`), "active", 1)
	if got.Goal.ID != created.Goal.ID || got.Goal.Objective != "ship v1" {
		t.Fatalf("get_goal = %+v", got.Goal)
	}

	// A stale revision is rejected instead of overwriting newer state.
	requireErrorContains(t, invokeError(t, byName, hosttools.ToolUpdateGoal,
		`{"goal_id":"goal-1","revision":99,"action":"pause"}`), "stale goal ref")

	paused := requireGoal(t, invoke(t, byName, hosttools.ToolUpdateGoal,
		`{"goal_id":"goal-1","revision":1,"action":"pause"}`), "paused", 2)
	if paused.Activation != string(hosttools.GoalActivationDisarmed) {
		t.Fatalf("paused activation = %q", paused.Activation)
	}

	resumed := requireGoal(t, invoke(t, byName, hosttools.ToolUpdateGoal,
		`{"goal_id":"goal-1","revision":2,"action":"resume"}`), "active", 3)
	if resumed.Activation != string(hosttools.GoalActivationArmed) {
		t.Fatalf("resumed activation = %q", resumed.Activation)
	}

	edited := requireGoal(t, invoke(t, byName, hosttools.ToolUpdateGoal,
		`{"goal_id":"goal-1","revision":3,"action":"edit","objective":"ship v2","max_goal_rounds":10}`), "active", 4)
	if edited.Goal.Objective != "ship v2" || edited.Goal.MaxGoalRounds != 10 {
		t.Fatalf("edited goal = %+v", edited.Goal)
	}

	// blocked requires a non-empty reason and records the DSH blocked reason.
	requireErrorContains(t, invokeError(t, byName, hosttools.ToolUpdateGoal,
		`{"goal_id":"goal-1","revision":4,"action":"blocked"}`), "blocked_reason is required")
	blocked := requireGoal(t, invoke(t, byName, hosttools.ToolUpdateGoal,
		`{"goal_id":"goal-1","revision":4,"action":"blocked","blocked_reason":"waiting on tests"}`), "blocked", 5)
	if blocked.Goal.BlockedReason == nil || blocked.Goal.BlockedReason.Code != "model-reported" ||
		blocked.Goal.BlockedReason.Message != "waiting on tests" {
		t.Fatalf("blocked reason = %+v", blocked.Goal.BlockedReason)
	}
	if blocked.Activation != string(hosttools.GoalActivationDisarmed) {
		t.Fatalf("blocked activation = %q", blocked.Activation)
	}

	// DSH allows complete from a blocked goal.
	completed := requireGoal(t, invoke(t, byName, hosttools.ToolUpdateGoal,
		`{"goal_id":"goal-1","revision":5,"action":"complete"}`), "complete", 6)
	if completed.Goal.BlockedReason != nil {
		t.Fatalf("completed goal kept blocked reason: %+v", completed.Goal.BlockedReason)
	}
	requireErrorContains(t, invokeError(t, byName, hosttools.ToolUpdateGoal,
		`{"goal_id":"goal-1","revision":6,"action":"resume"}`), "cannot resume")

	// A completed goal may be replaced by a fresh goal.
	replaced := requireGoal(t, invoke(t, byName, hosttools.ToolCreateGoal, `{"objective":"next","max_goal_rounds":3}`), "active", 1)
	if replaced.Goal.ID != "goal-2" || replaced.Goal.MaxGoalRounds != 3 {
		t.Fatalf("replacement goal = %+v", replaced.Goal)
	}
	// An edit may replace only the round cap.
	rebuilt := requireGoal(t, invoke(t, byName, hosttools.ToolUpdateGoal,
		`{"goal_id":"goal-2","revision":1,"action":"edit","max_goal_rounds":7}`), "active", 2)
	if rebuilt.Goal.Objective != "next" || rebuilt.Goal.MaxGoalRounds != 7 {
		t.Fatalf("cap-only edit = %+v", rebuilt.Goal)
	}
}

// TestDSHGoalUpdateValidation covers the structural DSH update rules.
func TestDSHGoalUpdateValidation(t *testing.T) {
	host := hosttools.NewMemoryHost()
	byName := toolMap(hosttools.Tools(hosttools.Options{Goal: host}))
	invoke(t, byName, hosttools.ToolCreateGoal, `{"objective":"validate"}`)

	cases := []struct {
		name string
		args string
		want string
	}{
		{"blank goal id", `{"goal_id":" ","revision":1,"action":"pause"}`, "goal_id must be non-empty"},
		{"missing goal id", `{"revision":1,"action":"pause"}`, "goal_id must be non-empty"},
		{"zero revision", `{"goal_id":"goal-1","revision":0,"action":"pause"}`, "revision must be a positive integer"},
		{"negative revision", `{"goal_id":"goal-1","revision":-2,"action":"pause"}`, "revision must be a positive integer"},
		{"unknown action", `{"goal_id":"goal-1","revision":1,"action":"dance"}`, "unknown action"},
		{"edit without replacement", `{"goal_id":"goal-1","revision":1,"action":"edit"}`, "requires objective and/or max_goal_rounds"},
		{"pause with objective", `{"goal_id":"goal-1","revision":1,"action":"pause","objective":"x"}`, "valid only with action edit"},
		{"complete with cap", `{"goal_id":"goal-1","revision":1,"action":"complete","max_goal_rounds":5}`, "valid only with action edit"},
		{"complete with reason", `{"goal_id":"goal-1","revision":1,"action":"complete","blocked_reason":"why"}`, "blocked_reason is valid only"},
		{"edit with reason", `{"goal_id":"goal-1","revision":1,"action":"edit","objective":"x","blocked_reason":"why"}`, "blocked_reason is valid only"},
		{"blocked without reason", `{"goal_id":"goal-1","revision":1,"action":"blocked"}`, "blocked_reason is required"},
		{"blocked blank reason", `{"goal_id":"goal-1","revision":1,"action":"blocked","blocked_reason":"   "}`, "blocked_reason is required"},
		{"negative cap", `{"goal_id":"goal-1","revision":1,"action":"edit","max_goal_rounds":-1}`, "max_goal_rounds must be a positive integer"},
		{"blank objective", `{"goal_id":"goal-1","revision":1,"action":"edit","objective":"   ","max_goal_rounds":2}`, "objective must be non-empty"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			requireErrorContains(t, invokeError(t, byName, hosttools.ToolUpdateGoal, testCase.args), testCase.want)
		})
	}

	requireErrorContains(t, invokeError(t, byName, hosttools.ToolCreateGoal, `{"objective":"   "}`), "objective is required")
	requireErrorContains(t, invokeError(t, byName, hosttools.ToolCreateGoal, `{"objective":"x","max_goal_rounds":-1}`), "positive integer")
}

// TestDSHGoalRevisionStoreUnavailable checks that plain rig.GoalStore hosts get
// a readable error from the DSH tools while the legacy goal tool keeps working.
func TestDSHGoalRevisionStoreUnavailable(t *testing.T) {
	store := &goalStoreOnly{}
	byName := toolMap(hosttools.Tools(hosttools.Options{Goal: store}))

	for _, testCase := range []struct{ name, args string }{
		{hosttools.ToolGetGoal, `{}`},
		{hosttools.ToolCreateGoal, `{"objective":"plain"}`},
		{hosttools.ToolUpdateGoal, `{"goal_id":"goal-only","revision":1,"action":"pause"}`},
	} {
		requireErrorContains(t, invokeError(t, byName, testCase.name, testCase.args), "GoalRevisionStore")
	}
	// The legacy goal tool still reads and writes the plain store.
	if result := invoke(t, byName, hosttools.ToolGoal, `{"action":"set","text":"plain store"}`); result.IsError {
		t.Fatalf("legacy goal set = %q", result.Content)
	}
}

// TestDSHGoalLegacyInterplay verifies the legacy goal tool and the DSH tools
// share one revisioned state.
func TestDSHGoalLegacyInterplay(t *testing.T) {
	host := hosttools.NewMemoryHost()
	byName := toolMap(hosttools.Tools(hosttools.Options{Goal: host}))

	invoke(t, byName, hosttools.ToolGoal, `{"action":"set","text":"legacy goal"}`)
	got := requireGoal(t, invoke(t, byName, hosttools.ToolGetGoal, `{}`), "active", 1)
	if got.Goal.ID != "goal-1" || got.Goal.Objective != "legacy goal" {
		t.Fatalf("get_goal after legacy set = %+v", got.Goal)
	}

	// A legacy lifecycle update also advances the revision.
	if result := invoke(t, byName, hosttools.ToolGoal, `{"action":"update","update":"pause"}`); result.IsError {
		t.Fatalf("legacy pause = %q", result.Content)
	}
	got = requireGoal(t, invoke(t, byName, hosttools.ToolGetGoal, `{}`), "paused", 2)

	// The DSH tool can resume on the revision the legacy tool advanced to.
	resumed := requireGoal(t, invoke(t, byName, hosttools.ToolUpdateGoal,
		`{"goal_id":"goal-1","revision":2,"action":"resume"}`), "active", 3)
	if resumed.Activation != string(hosttools.GoalActivationArmed) {
		t.Fatalf("resumed activation = %q", resumed.Activation)
	}
	// The legacy goal tool reads the state the DSH tool left behind.
	legacyGet := invoke(t, byName, hosttools.ToolGoal, `{"action":"get"}`)
	if !strings.Contains(legacyGet.Content, "status: active") || !strings.Contains(legacyGet.Content, "legacy goal") {
		t.Fatalf("legacy get after DSH resume = %q", legacyGet.Content)
	}
}

// TestDSHGoalCASConcurrent proves the revision compare-and-set: many callers
// using the same ref produce exactly one accepted mutation.
func TestDSHGoalCASConcurrent(t *testing.T) {
	host := hosttools.NewMemoryHost()
	byName := toolMap(hosttools.Tools(hosttools.Options{Goal: host}))
	invoke(t, byName, hosttools.ToolCreateGoal, `{"objective":"race"}`)

	const workers = 16
	var (
		waitGroup sync.WaitGroup
		accepted  atomic.Int32
		stale     atomic.Int32
	)
	for index := 0; index < workers; index++ {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			result, err := byName[hosttools.ToolUpdateGoal].Invoke(context.Background(), rig.ToolCall{
				Arguments: `{"goal_id":"goal-1","revision":1,"action":"pause"}`,
			})
			if err != nil {
				t.Errorf("concurrent update returned Go error: %v", err)
				return
			}
			if !result.IsError {
				accepted.Add(1)
				return
			}
			if strings.Contains(result.Content, "stale goal ref") {
				stale.Add(1)
				return
			}
			t.Errorf("unexpected concurrent update result: %q", result.Content)
		}()
	}
	waitGroup.Wait()
	if accepted.Load() != 1 {
		t.Fatalf("accepted updates = %d, want 1", accepted.Load())
	}
	if stale.Load() != workers-1 {
		t.Fatalf("stale rejections = %d, want %d", stale.Load(), workers-1)
	}
	got := requireGoal(t, invoke(t, byName, hosttools.ToolGetGoal, `{}`), "paused", 2)
	if got.Goal.ID != "goal-1" {
		t.Fatalf("goal after race = %+v", got.Goal)
	}
}
