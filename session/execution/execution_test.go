package execution_test

import (
	"context"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/session/execution"
)

func event(kind, id, state string, at time.Time) rig.SessionEvent {
	return rig.SessionEvent{
		Type:    kind,
		Time:    at,
		Payload: []byte(`{"id":"` + id + `","command":"sleep 1","state":"` + state + `"}`),
	}
}

type live map[string]bool

func (l live) Running(kind, id string) bool { return l[kind+"/"+id] }

func TestFoldPairsAndOverlays(t *testing.T) {
	start := time.Now().Add(-time.Minute)
	events := []rig.SessionEvent{
		event(executionEvent("job_started"), "a", "", start),
		event(executionEvent("terminal_opened"), "b", "", start.Add(time.Second)),
		event(executionEvent("job_finished"), "a", "exited", start.Add(2*time.Second)),
	}
	records := execution.Fold(events, "s1", live{"terminal/b": true})
	if len(records) != 2 {
		t.Fatalf("records = %+v", records)
	}
	// Newest start first: terminal b then job a.
	if records[0].ID != "b" || records[0].Kind != execution.KindTerminal || records[0].State != execution.StateRunning {
		t.Fatalf("terminal record = %+v", records[0])
	}
	if records[1].ID != "a" || records[1].State != execution.StateFinished || records[1].Outcome != "exited" {
		t.Fatalf("job record = %+v", records[1])
	}
}

func TestFoldOrphansAndInterrupted(t *testing.T) {
	events := []rig.SessionEvent{
		event(executionEvent("job_finished"), "orphan", "exited", time.Now()),
		event(executionEvent("job_started"), "alive", "", time.Now()),
	}
	records := execution.Fold(events, "s1", nil)
	if len(records) != 1 || records[0].ID != "alive" || records[0].State != execution.StateInterrupted {
		t.Fatalf("records = %+v", records)
	}
}

func TestHistoryLimitAndMissingStore(t *testing.T) {
	if records := execution.NewService(nil, nil).History("s1", 0); records != nil {
		t.Fatalf("nil store returned %+v", records)
	}
	log := &fakeLog{}
	service := execution.NewService(log, nil)
	if records := service.History("s1", 1); len(records) != 1 {
		t.Fatalf("limit 1 = %d records", len(records))
	}
}

func executionEvent(name string) string { return "execution/" + name }

type fakeLog struct{}

func (fakeLog) Last(_ context.Context, _ string, _ int) ([]rig.SessionEvent, error) {
	now := time.Now()
	return []rig.SessionEvent{
		event(executionEvent("job_started"), "a", "", now.Add(-2*time.Second)),
		event(executionEvent("job_started"), "b", "", now.Add(-time.Second)),
	}, nil
}
