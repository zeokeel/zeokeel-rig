package team

import (
	"context"
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/zeokeel/zeokeel-rig"
)

// fakeRunner starts fakeSubagent values and records the specs it received.
type fakeRunner struct {
	mu       sync.Mutex
	specs    []rig.SubagentSpec
	startErr error
	subs     []*fakeSubagent
	// startCanceled records whether Start was called with a cancelled context.
	startCanceled bool
}

func newFakeRunner() *fakeRunner { return &fakeRunner{} }

func (r *fakeRunner) Start(ctx context.Context, spec rig.SubagentSpec) (rig.Subagent, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.specs = append(r.specs, spec)
	if ctx != nil && ctx.Err() != nil {
		r.startCanceled = true
	}
	if r.startErr != nil {
		return nil, r.startErr
	}
	sub := newFakeSubagent(spec.Name, "first:"+spec.Prompt)
	r.subs = append(r.subs, sub)
	return sub, nil
}

func (r *fakeRunner) started() []rig.SubagentSpec {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.specs)
}

func (r *fakeRunner) subagents() []*fakeSubagent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.subs)
}

// fakeSubagent is a controllable rig.Subagent.
type fakeSubagent struct {
	name string

	mu        sync.Mutex
	first     rig.SubagentResult
	sendCalls []string
	sendErr   error
	waitErr   error
	stopErr   error
	stopped   int
	lastSend  rig.SubagentResult

	// sendBlock, when non-nil, makes Send block until it is closed, Stop is
	// called, or the context ends.
	sendBlock chan struct{}
	// waitBlock, when non-nil, makes Wait block until it is closed or the
	// context ends.
	waitBlock chan struct{}

	// active/maxActive count concurrent Send calls, to assert serialization.
	active    int
	maxActive int
	sendDelay time.Duration

	stopOnce sync.Once
	stopCh   chan struct{}
	sendGate chan struct{} // closed on the first Send entry (test synchronization)
}

func newFakeSubagent(name, firstOutput string) *fakeSubagent {
	return &fakeSubagent{
		name:     name,
		first:    rig.SubagentResult{Name: name, Output: firstOutput, Iterations: 1, Usage: rig.Usage{TotalTokens: 3}},
		stopCh:   make(chan struct{}),
		sendGate: make(chan struct{}),
	}
}

func (s *fakeSubagent) Name() string { return s.name }

func (s *fakeSubagent) Send(ctx context.Context, message string) (rig.SubagentResult, error) {
	s.mu.Lock()
	s.active++
	if s.active > s.maxActive {
		s.maxActive = s.active
	}
	s.sendCalls = append(s.sendCalls, message)
	if s.active == 1 {
		select {
		case <-s.sendGate:
		default:
			close(s.sendGate)
		}
	}
	block := s.sendBlock
	sendErr := s.sendErr
	delay := s.sendDelay
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.active--
		s.mu.Unlock()
	}()

	if delay > 0 {
		time.Sleep(delay)
	}
	if sendErr != nil {
		return rig.SubagentResult{}, sendErr
	}
	if block != nil {
		select {
		case <-block:
		case <-s.stopCh:
			return rig.SubagentResult{}, errors.New("subagent was stopped")
		case <-ctx.Done():
			return rig.SubagentResult{}, ctx.Err()
		}
	}
	if err := ctx.Err(); err != nil {
		return rig.SubagentResult{}, err
	}
	result := rig.SubagentResult{Name: s.name, Output: "reply:" + message, Iterations: 1}
	s.mu.Lock()
	s.lastSend = result
	s.mu.Unlock()
	return result, nil
}

func (s *fakeSubagent) Wait(ctx context.Context) (rig.SubagentResult, error) {
	s.mu.Lock()
	waitErr := s.waitErr
	block := s.waitBlock
	result := s.first
	s.mu.Unlock()
	if waitErr != nil {
		return rig.SubagentResult{}, waitErr
	}
	if block != nil {
		select {
		case <-block:
		case <-s.stopCh:
			return rig.SubagentResult{}, errors.New("subagent was stopped")
		case <-ctx.Done():
			return rig.SubagentResult{}, ctx.Err()
		}
	}
	if err := ctx.Err(); err != nil {
		return rig.SubagentResult{}, err
	}
	return result, nil
}

func (s *fakeSubagent) Stop() error {
	s.mu.Lock()
	s.stopped++
	stopErr := s.stopErr
	s.mu.Unlock()
	s.stopOnce.Do(func() { close(s.stopCh) })
	return stopErr
}

func (s *fakeSubagent) stopCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stopped
}

func (s *fakeSubagent) sendCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sendCalls)
}

func (s *fakeSubagent) maxConcurrentSends() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.maxActive
}
