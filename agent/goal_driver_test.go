package agent_test

// P2-8 goal 回合驱动的单元测试：预算上限、默认值/钳制、新目标重置、pause/block/
// complete 停止、会话隔离、并发安全与 nil 接收者。

import (
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/zeokeel/zeokeel-rig/agent"
)

// activeGoal 构造一个活跃目标快照。
func activeGoal(id string) agent.GoalRoundState {
	return agent.GoalRoundState{ID: id, Status: "active"}
}

func TestGoalRoundDriverMaxRounds(t *testing.T) {
	var prompts []string
	driver := agent.NewGoalRoundDriver(agent.GoalRoundDriverConfig{
		MaxRounds: 3,
		Continuable: func(status, reason string) bool {
			return status == "active"
		},
		ContinuationPrompt: func(goal agent.GoalRoundState) string {
			got := "continue:" + goal.ID + ":" + goal.Status
			prompts = append(prompts, got)
			return got
		},
	})

	for round := 1; round <= 3; round++ {
		cont, prompt := driver.TurnFinished("s1", activeGoal("goal-1"), false)
		if !cont {
			t.Fatalf("round %d: continue = false, want true", round)
		}
		if prompt == "" {
			t.Fatalf("round %d: prompt is empty", round)
		}
		if got, want := driver.Rounds("s1"), round; got != want {
			t.Fatalf("round %d: Rounds = %d, want %d", round, got, want)
		}
	}
	if cont, prompt := driver.TurnFinished("s1", activeGoal("goal-1"), false); cont || prompt != "" {
		t.Fatalf("exhausted budget: continue=%v prompt=%q, want false/empty", cont, prompt)
	}
	if got, want := driver.Rounds("s1"), 3; got != want {
		t.Fatalf("Rounds = %d, want %d", got, want)
	}
	if len(prompts) != 3 || prompts[0] != "continue:goal-1:active" {
		t.Fatalf("prompts = %#v, want three continue:* entries", prompts)
	}
}

func TestGoalRoundDriverDefaultBudgetAndClamp(t *testing.T) {
	countActive := func(driver *agent.GoalRoundDriver, sessionID string) int {
		t.Helper()
		count := 0
		for {
			cont, _ := driver.TurnFinished(sessionID, activeGoal("goal-1"), false)
			if !cont {
				return count
			}
			count++
			if count > agent.MaxGoalRounds+1 {
				t.Fatalf("driver returned more continuations than the hard cap")
			}
		}
	}

	zero := agent.NewGoalRoundDriver(agent.GoalRoundDriverConfig{})
	if got, want := countActive(zero, "s-zero"), agent.DefaultGoalRounds; got != want {
		t.Fatalf("zero-config budget = %d, want %d", got, want)
	}

	zero.SetMaxRounds(agent.MaxGoalRounds + 100)
	if got, want := countActive(zero, "s-clamp"), agent.MaxGoalRounds; got != want {
		t.Fatalf("clamped budget = %d, want %d", got, want)
	}

	zero.SetMaxRounds(1)
	if got, want := countActive(zero, "s-one"), 1; got != want {
		t.Fatalf("budget 1 = %d, want %d", got, want)
	}

	zero.SetMaxRounds(-7)
	if got, want := countActive(zero, "s-negative"), agent.DefaultGoalRounds; got != want {
		t.Fatalf("negative budget = %d, want %d", got, want)
	}
}

func TestGoalRoundDriverNewGoalResetsBudget(t *testing.T) {
	driver := agent.NewGoalRoundDriver(agent.GoalRoundDriverConfig{MaxRounds: 2})
	if cont, _ := driver.TurnFinished("s1", activeGoal("goal-1"), false); !cont {
		t.Fatal("first round: continue = false")
	}
	if cont, _ := driver.TurnFinished("s1", activeGoal("goal-1"), false); !cont {
		t.Fatal("second round: continue = false")
	}
	if cont, _ := driver.TurnFinished("s1", activeGoal("goal-1"), false); cont {
		t.Fatal("third round: continue = true, want budget exhausted")
	}

	// 新目标实例：changed=true 先重置预算，因此可以继续。
	if cont, _ := driver.TurnFinished("s1", activeGoal("goal-2"), true); !cont {
		t.Fatal("new goal: continue = false, want true after budget reset")
	}
	if got, want := driver.Rounds("s1"), 1; got != want {
		t.Fatalf("Rounds after new goal = %d, want %d", got, want)
	}
	if cont, _ := driver.TurnFinished("s1", activeGoal("goal-2"), false); !cont {
		t.Fatal("new goal second round: continue = false")
	}
	if cont, _ := driver.TurnFinished("s1", activeGoal("goal-2"), false); cont {
		t.Fatal("new goal third round: continue = true, want budget exhausted")
	}
}

func TestGoalRoundDriverStopsOnPausedBlockedAndComplete(t *testing.T) {
	for _, status := range []string{"paused", "blocked", "complete", "completed", "", "unknown"} {
		t.Run("status="+status, func(t *testing.T) {
			driver := agent.NewGoalRoundDriver(agent.GoalRoundDriverConfig{MaxRounds: 5})
			goal := agent.GoalRoundState{ID: "goal-1", Status: status}
			if status == "blocked" {
				goal.Reason = "waiting for credentials"
			}
			cont, prompt := driver.TurnFinished("s1", goal, false)
			if cont || prompt != "" {
				t.Fatalf("status %q: continue=%v prompt=%q, want false/empty", status, cont, prompt)
			}
			if got := driver.Rounds("s1"); got != 0 {
				t.Fatalf("status %q: Rounds = %d, want 0", status, got)
			}
		})
	}

	// 状态大小写/空白归一化后，active 仍然继续。
	driver := agent.NewGoalRoundDriver(agent.GoalRoundDriverConfig{MaxRounds: 1})
	if cont, _ := driver.TurnFinished("s1", agent.GoalRoundState{ID: "g", Status: " Active "}, false); !cont {
		t.Fatal("normalized ACTIVE: continue = false, want true")
	}
}

func TestGoalRoundDriverChangedResetsEvenWhenStopped(t *testing.T) {
	driver := agent.NewGoalRoundDriver(agent.GoalRoundDriverConfig{MaxRounds: 2})
	if cont, _ := driver.TurnFinished("s1", activeGoal("g1"), false); !cont {
		t.Fatal("first round: continue = false")
	}
	// changed=true 先 Reset 再判断：paused 新目标仍停止，但预算已清零。
	if cont, _ := driver.TurnFinished("s1", agent.GoalRoundState{ID: "g2", Status: "paused"}, true); cont {
		t.Fatal("paused new goal: continue = true, want false")
	}
	if got := driver.Rounds("s1"); got != 0 {
		t.Fatalf("Rounds after changed reset = %d, want 0", got)
	}
}

func TestGoalRoundDriverCustomContinuableAndPrompt(t *testing.T) {
	var seenStatus, seenReason string
	driver := agent.NewGoalRoundDriver(agent.GoalRoundDriverConfig{
		MaxRounds: 1,
		Continuable: func(status, reason string) bool {
			seenStatus, seenReason = status, reason
			return status == "waiting" && strings.Contains(reason, "build")
		},
		ContinuationPrompt: func(goal agent.GoalRoundState) string {
			return "goal=" + goal.ID + ";status=" + goal.Status + ";reason=" + goal.Reason
		},
	})
	cont, prompt := driver.TurnFinished("s1", agent.GoalRoundState{
		ID: "  g-1  ", Status: " WAITING ", Reason: "build still running",
	}, false)
	if !cont {
		t.Fatal("custom continuable: continue = false, want true")
	}
	if want := "goal=g-1;status=waiting;reason=build still running"; prompt != want {
		t.Fatalf("prompt = %q, want %q", prompt, want)
	}
	if seenStatus != "waiting" || seenReason != "build still running" {
		t.Fatalf("continuable args = (%q, %q), want normalized status and raw reason", seenStatus, seenReason)
	}
	if cont, _ := driver.TurnFinished("s1", agent.GoalRoundState{ID: "g-1", Status: "waiting"}, false); cont {
		t.Fatal("exhausted custom budget: continue = true, want false")
	}
}

func TestGoalRoundDriverSessionsIsolated(t *testing.T) {
	driver := agent.NewGoalRoundDriver(agent.GoalRoundDriverConfig{MaxRounds: 2})
	if cont, _ := driver.TurnFinished("s1", activeGoal("g"), false); !cont {
		t.Fatal("s1 round 1: continue = false")
	}
	if cont, _ := driver.TurnFinished("s1", activeGoal("g"), false); !cont {
		t.Fatal("s1 round 2: continue = false")
	}
	if cont, _ := driver.TurnFinished("s1", activeGoal("g"), false); cont {
		t.Fatal("s1 round 3: continue = true, want exhausted")
	}
	if got, want := driver.Rounds("s2"), 0; got != want {
		t.Fatalf("s2 Rounds = %d, want %d (sessions are isolated)", got, want)
	}
	if cont, _ := driver.TurnFinished("s2", activeGoal("g"), false); !cont {
		t.Fatal("s2 first round: continue = false, want true")
	}
	if got, want := driver.Rounds("s1"), 2; got != want {
		t.Fatalf("s1 Rounds = %d, want %d", got, want)
	}
	driver.Reset("s1")
	if got := driver.Rounds("s1"); got != 0 {
		t.Fatalf("Rounds after Reset = %d, want 0", got)
	}
}

func TestGoalRoundDriverConcurrent(t *testing.T) {
	const (
		limit    = 5
		workers  = 64
		sessions = 4
	)
	driver := agent.NewGoalRoundDriver(agent.GoalRoundDriverConfig{MaxRounds: limit})
	var successes atomic.Int64
	var promptOK atomic.Int64
	var wg sync.WaitGroup
	for session := 0; session < sessions; session++ {
		sessionID := "s" + string(rune('0'+session))
		for worker := 0; worker < workers; worker++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				cont, prompt := driver.TurnFinished(sessionID, agent.GoalRoundState{ID: "g", Status: "active"}, false)
				if cont {
					successes.Add(1)
					if prompt != "" {
						promptOK.Add(1)
					}
				}
				_ = driver.Rounds(sessionID)
			}()
		}
	}
	wg.Wait()

	if got, want := successes.Load(), int64(limit*sessions); got != want {
		t.Fatalf("successful continuations = %d, want %d", got, want)
	}
	if got, want := promptOK.Load(), int64(limit*sessions); got != want {
		t.Fatalf("non-empty prompts = %d, want %d", got, want)
	}
	for session := 0; session < sessions; session++ {
		sessionID := "s" + string(rune('0'+session))
		if got, want := driver.Rounds(sessionID), limit; got != want {
			t.Fatalf("%s Rounds = %d, want %d", sessionID, got, want)
		}
	}
}

func TestGoalRoundDriverNilReceiver(t *testing.T) {
	var driver *agent.GoalRoundDriver
	driver.Reset("s1")
	driver.SetMaxRounds(3)
	if got := driver.Rounds("s1"); got != 0 {
		t.Fatalf("nil Rounds = %d, want 0", got)
	}
	if cont, prompt := driver.TurnFinished("s1", activeGoal("g"), false); cont || prompt != "" {
		t.Fatalf("nil TurnFinished = (%v, %q), want false/empty", cont, prompt)
	}
}

func TestGoalRoundDriverZeroValue(t *testing.T) {
	var driver agent.GoalRoundDriver
	if cont, _ := driver.TurnFinished("s1", activeGoal("g"), false); !cont {
		t.Fatal("zero-value driver: continue = false, want true")
	}
	if got, want := driver.Rounds("s1"), 1; got != want {
		t.Fatalf("zero-value Rounds = %d, want %d", got, want)
	}
}

func TestDefaultGoalPrompt(t *testing.T) {
	prompt := agent.DefaultGoalPrompt(agent.GoalRoundState{ID: "g-1", Status: "active", Reason: "step 2 done"})
	for _, want := range []string{"g-1", "step 2 done", "active goal"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("DefaultGoalPrompt = %q, want it to contain %q", prompt, want)
		}
	}
	if got := agent.DefaultGoalPrompt(agent.GoalRoundState{}); got == "" {
		t.Fatal("DefaultGoalPrompt returned empty for empty goal")
	}
}
