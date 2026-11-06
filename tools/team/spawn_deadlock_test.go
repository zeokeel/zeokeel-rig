package team

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig"
)

// gatedRunner blocks Start until release is closed (or the caller cancels):
// it models a SubagentRunner at its concurrency limit.
type gatedRunner struct {
	started chan struct{}
	release chan struct{}
}

func newGatedRunner() *gatedRunner {
	return &gatedRunner{started: make(chan struct{}, 16), release: make(chan struct{})}
}

func (r *gatedRunner) Start(ctx context.Context, spec rig.SubagentSpec) (rig.Subagent, error) {
	select {
	case r.started <- struct{}{}:
	default:
	}
	select {
	case <-r.release:
		return newFakeSubagent(spec.Name, "first:"+spec.Prompt), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// TestSpawnDoesNotHoldManagerLockWhileRunnerBlocks reproduces the v0.9.0
// deadlock: Spawn held the manager lock while the runner waited for capacity,
// so any concurrent team_list/team_wait/team_close (and every other spawn)
// blocked forever.
func TestSpawnDoesNotHoldManagerLockWhileRunnerBlocks(t *testing.T) {
	runner := newGatedRunner()
	manager := NewManager(runner)

	spawnDone := make(chan error, 1)
	go func() {
		_, err := manager.Spawn(context.Background(), rig.SubagentSpec{Name: "one", Prompt: "p"})
		spawnDone <- err
	}()
	select {
	case <-runner.started:
	case <-time.After(2 * time.Second):
		t.Fatal("the spawn never reached the runner")
	}

	// While the spawn is blocked in the runner, manager operations must respond.
	readDone := make(chan struct{})
	go func() {
		manager.Len()
		manager.Members()
		close(readDone)
	}()
	select {
	case <-readDone:
	case <-time.After(2 * time.Second):
		t.Fatal("manager read blocked while a spawn waited on the runner: the manager lock is held across Start")
	}

	// A caller with its own deadline can abandon the capacity wait.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := manager.Spawn(ctx, rig.SubagentSpec{Name: "two", Prompt: "p"}); err == nil {
		t.Fatal("second spawn returned nil error while the runner was blocked and the context expired")
	} else if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second spawn error = %v, want context.DeadlineExceeded", err)
	}

	close(runner.release)
	select {
	case err := <-spawnDone:
		if err != nil {
			t.Fatalf("first spawn failed: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first spawn did not complete after the runner released capacity")
	}
	if manager.Len() != 1 {
		t.Fatalf("manager.Len() = %d, want 1", manager.Len())
	}
}

// TestSpawnLimitCountsInFlightSpawns: a caller cannot exceed MaxMembers by
// starting more spawns than the limit while earlier starts are still in flight.
func TestSpawnLimitCountsInFlightSpawns(t *testing.T) {
	runner := newGatedRunner()
	manager := NewManager(runner, WithMaxMembers(1))

	firstDone := make(chan error, 1)
	go func() {
		_, err := manager.Spawn(context.Background(), rig.SubagentSpec{Name: "one", Prompt: "p"})
		firstDone <- err
	}()
	select {
	case <-runner.started:
	case <-time.After(2 * time.Second):
		t.Fatal("first spawn never reached the runner")
	}
	if _, err := manager.Spawn(context.Background(), rig.SubagentSpec{Name: "two", Prompt: "p"}); !errors.Is(err, ErrTeamFull) {
		t.Fatalf("second spawn error = %v, want ErrTeamFull", err)
	}
	close(runner.release)
	select {
	case err := <-firstDone:
		if err != nil {
			t.Fatalf("first spawn failed: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first spawn did not complete")
	}
}
