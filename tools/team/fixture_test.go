package team

import (
	"context"
	"errors"
	"sync"
	"time"

	v1 "github.com/zeokeel/zeokeel-rig/contracts/v1"
)

// fakeRunner starts fakeSubagent values and records the specs it received.
type fakeRunner struct {
	mu       sync.Mutex
	specs    []v1.SubagentSpec
	startErr error
	subs     []*fakeSubagent
	// startCanceled records whether Start was called with a cancelled context.
	startCanceled bool
}

func newFakeRunner() *fakeRunner { return &fakeRunner{} }

func (r *fakeRunner) Start(ctx context.Context, spec v1.SubagentSpec) (v1.Subagent, error) {
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

func (r *fakeRunner) started() []v1.SubagentSpec {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]v1.SubagentSpec(nil), r.specs...)
}

func (r *fakeRunner) subagents() []*fakeSubagent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]*fakeSubagent(nil), r.subs...)
}

// fakeSubagent is a controllable v1.Subagent.
type fakeSubagent struct {
	name string

	mu        sync.Mutex
	first     v1.SubagentResult
	sendCalls []string
	sendErr   error
	waitErr   error
	stopErr   error
	stopped   int
	lastSend  v1.SubagentResult

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
		first:    v1.SubagentResult{Name: name, Output: firstOutput, Iterations: 1, Usage: v1.Usage{TotalTokens: 3}},
		stopCh:   make(chan struct{}),
		sendGate: make(chan struct{}),
	}
}

func (s *fakeSubagent) Name() string { return s.name }

func (s *fakeSubagent) Send(ctx context.Context, message string) (v1.SubagentResult, error) {
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
		return v1.SubagentResult{}, sendErr
	}
	if block != nil {
		select {
		case <-block:
		case <-s.stopCh:
			return v1.SubagentResult{}, errors.New("subagent was stopped")
		case <-ctx.Done():
			return v1.SubagentResult{}, ctx.Err()
		}
	}
	if err := ctx.Err(); err != nil {
		return v1.SubagentResult{}, err
	}
	result := v1.SubagentResult{Name: s.name, Output: "reply:" + message, Iterations: 1}
	s.mu.Lock()
	s.lastSend = result
	s.mu.Unlock()
	return result, nil
}

func (s *fakeSubagent) Wait(ctx context.Context) (v1.SubagentResult, error) {
	s.mu.Lock()
	waitErr := s.waitErr
	block := s.waitBlock
	result := s.first
	s.mu.Unlock()
	if waitErr != nil {
		return v1.SubagentResult{}, waitErr
	}
	if block != nil {
		select {
		case <-block:
		case <-s.stopCh:
			return v1.SubagentResult{}, errors.New("subagent was stopped")
		case <-ctx.Done():
			return v1.SubagentResult{}, ctx.Err()
		}
	}
	if err := ctx.Err(); err != nil {
		return v1.SubagentResult{}, err
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
