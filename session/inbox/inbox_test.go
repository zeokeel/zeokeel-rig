package inbox_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/session/inbox"
)

// memoryLog is an in-memory inbox.EventLog. It mirrors the session log: it owns
// sequence numbers, copies payloads and can inject failures.
type memoryLog struct {
	mu        sync.Mutex
	events    []rig.SessionEvent
	nextSeq   int64
	appends   int
	appendErr error
	eventsErr error
	reverse   bool
}

func (l *memoryLog) AppendEvent(ctx context.Context, event rig.SessionEvent) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.appendErr != nil {
		return l.appendErr
	}
	if event.Payload != nil {
		event.Payload = append(json.RawMessage(nil), event.Payload...)
	}
	l.nextSeq++
	event.Seq = l.nextSeq
	l.events = append(l.events, event)
	l.appends++
	return nil
}

func (l *memoryLog) Events(ctx context.Context, sessionID string, from, to int64) ([]rig.SessionEvent, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.eventsErr != nil {
		return nil, l.eventsErr
	}
	out := make([]rig.SessionEvent, 0, len(l.events))
	for _, event := range l.events {
		if sessionID != "" && event.SessionID != sessionID {
			continue
		}
		if from > 0 && event.Seq < from {
			continue
		}
		if to > 0 && event.Seq > to {
			continue
		}
		if event.Payload != nil {
			event.Payload = append(json.RawMessage(nil), event.Payload...)
		}
		out = append(out, event)
	}
	if l.reverse {
		for left, right := 0, len(out)-1; left < right; left, right = left+1, right-1 {
			out[left], out[right] = out[right], out[left]
		}
	}
	return out, nil
}

// addRaw appends a hand-built event (corrupt payloads, replayed snapshots).
func (l *memoryLog) addRaw(sessionID, eventType, payload string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.nextSeq++
	l.events = append(l.events, rig.SessionEvent{
		Seq:       l.nextSeq,
		SessionID: sessionID,
		Type:      eventType,
		Time:      time.Unix(1_700_000_000, 0).UTC(),
		Payload:   json.RawMessage(payload),
	})
}

func (l *memoryLog) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.appends
}

func (l *memoryLog) last() rig.SessionEvent {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.events) == 0 {
		return rig.SessionEvent{}
	}
	return l.events[len(l.events)-1]
}

func mustEnqueue(t *testing.T, box *inbox.Inbox, sessionID, text string) inbox.Entry {
	t.Helper()
	entry, err := box.Enqueue(context.Background(), sessionID, text)
	if err != nil {
		t.Fatalf("Enqueue(%q) err = %v", text, err)
	}
	return entry
}

func mustList(t *testing.T, box *inbox.Inbox, sessionID string) []inbox.Entry {
	t.Helper()
	entries, err := box.List(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("List(%q) err = %v", sessionID, err)
	}
	return entries
}

func texts(entries []inbox.Entry) []string {
	out := make([]string, len(entries))
	for index, entry := range entries {
		out[index] = entry.Text
	}
	return out
}

func TestEnqueueListRemove(t *testing.T) {
	log := &memoryLog{}
	box := inbox.New(log)
	ctx := context.Background()

	first := mustEnqueue(t, box, " s1 ", "  hello  ")
	if first.Text != "hello" {
		t.Fatalf("Enqueue text = %q, want trimmed %q", first.Text, "hello")
	}
	if first.ID == "" || first.Created.IsZero() {
		t.Fatalf("Enqueue entry = %+v, want id and created", first)
	}
	second := mustEnqueue(t, box, "s1", "world")

	entries := mustList(t, box, "s1")
	if got, want := texts(entries), []string{"hello", "world"}; !equalStrings(got, want) {
		t.Fatalf("List texts = %v, want %v", got, want)
	}
	if entries[0].ID != first.ID || entries[1].ID != second.ID {
		t.Fatalf("List ids = %q,%q, want %q,%q", entries[0].ID, entries[1].ID, first.ID, second.ID)
	}

	// The returned slice is a copy.
	entries[0].Text = "mutated"
	if got := texts(mustList(t, box, "s1")); got[0] != "hello" {
		t.Fatalf("mutating List result changed the inbox: %v", got)
	}

	// Every mutation is one durable snapshot event of the documented shape.
	if got := log.count(); got != 2 {
		t.Fatalf("append count = %d, want 2", got)
	}
	event := log.last()
	if event.Type != inbox.EventType {
		t.Fatalf("event type = %q, want %q", event.Type, inbox.EventType)
	}
	if event.SessionID != "s1" || event.Time.IsZero() {
		t.Fatalf("event = %+v, want trimmed session id and a time", event)
	}
	var payload struct {
		Prompts []struct {
			ID        string    `json:"id"`
			Text      string    `json:"text"`
			CreatedAt time.Time `json:"createdAt"`
		} `json:"prompts"`
	}
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		t.Fatalf("decode payload: %v (payload %s)", err, event.Payload)
	}
	if len(payload.Prompts) != 2 || payload.Prompts[0].ID != first.ID || payload.Prompts[0].Text != "hello" || payload.Prompts[0].CreatedAt.IsZero() {
		t.Fatalf("payload = %+v, want both entries oldest first", payload.Prompts)
	}

	if err := box.Remove(ctx, "s1", first.ID); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if got, want := texts(mustList(t, box, "s1")), []string{"world"}; !equalStrings(got, want) {
		t.Fatalf("after Remove texts = %v, want %v", got, want)
	}
	// Unknown id and unknown session are idempotent no-ops (no event).
	if err := box.Remove(ctx, "s1", "missing"); err != nil {
		t.Fatalf("Remove(missing) err = %v", err)
	}
	if err := box.Remove(ctx, "other", "missing"); err != nil {
		t.Fatalf("Remove(other) err = %v", err)
	}
	if got := log.count(); got != 3 {
		t.Fatalf("append count after no-op removes = %d, want 3", got)
	}

	if err := box.Remove(ctx, "s1", second.ID); err != nil {
		t.Fatalf("Remove last: %v", err)
	}
	if entries := mustList(t, box, "s1"); len(entries) != 0 {
		t.Fatalf("List after removing last = %+v, want empty", entries)
	}
	if got := log.count(); got != 4 {
		t.Fatalf("append count = %d, want the empty snapshot to be persisted (4)", got)
	}
	var empty struct {
		Prompts []inbox.Entry `json:"prompts"`
	}
	if err := json.Unmarshal(log.last().Payload, &empty); err != nil {
		t.Fatalf("decode empty payload: %v", err)
	}
	if empty.Prompts == nil || len(empty.Prompts) != 0 {
		t.Fatalf("empty snapshot payload = %s, want an empty prompts array", log.last().Payload)
	}
}

func TestEnqueueValidation(t *testing.T) {
	box := inbox.New(&memoryLog{})
	ctx := context.Background()

	if _, err := box.Enqueue(ctx, "s", "   "); !errors.Is(err, inbox.ErrEmptyPrompt) {
		t.Fatalf("Enqueue(blank) err = %v, want ErrEmptyPrompt", err)
	}
	if _, err := box.Enqueue(ctx, "  ", "hello"); !errors.Is(err, inbox.ErrNoSession) {
		t.Fatalf("Enqueue(no session) err = %v, want ErrNoSession", err)
	}

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := box.Enqueue(cancelled, "s", "hello"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Enqueue(cancelled) err = %v, want context.Canceled", err)
	}
	if _, err := box.List(cancelled, "s"); !errors.Is(err, context.Canceled) {
		t.Fatalf("List(cancelled) err = %v, want context.Canceled", err)
	}
	if _, err := box.Count(cancelled, "s"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Count(cancelled) err = %v, want context.Canceled", err)
	}
	if err := box.Remove(cancelled, "s", "id"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Remove(cancelled) err = %v, want context.Canceled", err)
	}
	if _, _, err := box.Take(cancelled, "s"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Take(cancelled) err = %v, want context.Canceled", err)
	}
}

func TestEnqueueBudget(t *testing.T) {
	log := &memoryLog{}
	box := inbox.New(log)
	ctx := context.Background()
	for index := 0; index < inbox.MaxEntries; index++ {
		if _, err := box.Enqueue(ctx, "s", fmt.Sprintf("prompt %d", index)); err != nil {
			t.Fatalf("Enqueue(%d): %v", index, err)
		}
	}
	if _, err := box.Enqueue(ctx, "s", "one too many"); !errors.Is(err, inbox.ErrInboxFull) {
		t.Fatalf("Enqueue over budget err = %v, want ErrInboxFull", err)
	}
	if got := len(mustList(t, box, "s")); got != inbox.MaxEntries {
		t.Fatalf("List len = %d, want %d", got, inbox.MaxEntries)
	}
	if got := log.count(); got != inbox.MaxEntries {
		t.Fatalf("append count = %d, want %d (rejected prompt must not persist)", got, inbox.MaxEntries)
	}
	// Another session has its own budget.
	if _, err := box.Enqueue(ctx, "s2", "hello"); err != nil {
		t.Fatalf("Enqueue(other session): %v", err)
	}
}

func TestCount(t *testing.T) {
	box := inbox.New(&memoryLog{})
	ctx := context.Background()
	count, err := box.Count(ctx, "s")
	if err != nil || count != 0 {
		t.Fatalf("Count empty = %d, %v; want 0, nil", count, err)
	}
	first := mustEnqueue(t, box, "s", "a")
	mustEnqueue(t, box, "s", "b")
	if count, err = box.Count(ctx, "s"); err != nil || count != 2 {
		t.Fatalf("Count = %d, %v; want 2, nil", count, err)
	}
	if err := box.Remove(ctx, "s", first.ID); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if count, err = box.Count(ctx, "s"); err != nil || count != 1 {
		t.Fatalf("Count after Remove = %d, %v; want 1, nil", count, err)
	}
	if count, err = box.Count(ctx, "  "); err != nil || count != 0 {
		t.Fatalf("Count empty session = %d, %v; want 0, nil", count, err)
	}
}

func TestTakeIsDurableFIFO(t *testing.T) {
	log := &memoryLog{}
	box := inbox.New(log)
	ctx := context.Background()
	mustEnqueue(t, box, "s", "a")
	mustEnqueue(t, box, "s", "b")
	mustEnqueue(t, box, "s", "c")

	entry, ok, err := box.Take(ctx, "s")
	if err != nil || !ok {
		t.Fatalf("Take = %+v, %v, %v; want an entry", entry, ok, err)
	}
	if entry.Text != "a" {
		t.Fatalf("Take text = %q, want oldest %q", entry.Text, "a")
	}
	if got := log.count(); got != 4 {
		t.Fatalf("append count = %d, want the dequeue snapshot persisted (4)", got)
	}

	// A fresh inbox over the same log replays the post-dequeue snapshot.
	replayed := inbox.New(log)
	if got, want := texts(mustList(t, replayed, "s")), []string{"b", "c"}; !equalStrings(got, want) {
		t.Fatalf("replayed texts = %v, want %v", got, want)
	}

	if _, ok, err := replayed.Take(ctx, "s"); err != nil || !ok {
		t.Fatalf("second Take ok = %v, err = %v; want entry", ok, err)
	}
	if _, ok, err := replayed.Take(ctx, "s"); err != nil || !ok {
		t.Fatalf("third Take ok = %v, err = %v; want entry", ok, err)
	}
	if _, ok, err := replayed.Take(ctx, "s"); err != nil || ok {
		t.Fatalf("Take empty ok = %v, err = %v; want false, nil", ok, err)
	}
	if _, ok, err := replayed.Take(ctx, "  "); err != nil || ok {
		t.Fatalf("Take empty session ok = %v, err = %v; want false, nil", ok, err)
	}
}

func TestTakePersistFailureKeepsEntry(t *testing.T) {
	log := &memoryLog{}
	box := inbox.New(log)
	ctx := context.Background()
	mustEnqueue(t, box, "s", "a")
	mustEnqueue(t, box, "s", "b")

	log.appendErr = errors.New("disk full")
	entry, ok, err := box.Take(ctx, "s")
	if err == nil || !errors.Is(err, log.appendErr) {
		t.Fatalf("Take err = %v, want the persistence error", err)
	}
	if ok || entry.ID != "" {
		t.Fatalf("Take returned %+v/%v on persistence failure, want no entry", entry, ok)
	}
	if got, want := texts(mustList(t, box, "s")), []string{"a", "b"}; !equalStrings(got, want) {
		t.Fatalf("texts after failed Take = %v, want %v", got, want)
	}

	log.appendErr = nil
	entry, ok, err = box.Take(ctx, "s")
	if err != nil || !ok || entry.Text != "a" {
		t.Fatalf("Take after recovery = %+v, %v, %v; want a", entry, ok, err)
	}
}

func TestEnqueuePersistFailureRollsBack(t *testing.T) {
	log := &memoryLog{}
	box := inbox.New(log)
	ctx := context.Background()
	mustEnqueue(t, box, "s", "a")
	log.appendErr = errors.New("disk full")
	if _, err := box.Enqueue(ctx, "s", "b"); err == nil {
		t.Fatal("Enqueue with failing log err = nil, want error")
	}
	if got, want := texts(mustList(t, box, "s")), []string{"a"}; !equalStrings(got, want) {
		t.Fatalf("texts after failed Enqueue = %v, want %v", got, want)
	}
	log.appendErr = nil
	mustEnqueue(t, box, "s", "b")
	if got, want := texts(mustList(t, box, "s")), []string{"a", "b"}; !equalStrings(got, want) {
		t.Fatalf("texts after recovery = %v, want %v", got, want)
	}
}

func TestRequeueFrontAndDedupe(t *testing.T) {
	log := &memoryLog{}
	box := inbox.New(log)
	ctx := context.Background()
	mustEnqueue(t, box, "s", "a")
	mustEnqueue(t, box, "s", "b")

	entry, ok, err := box.Take(ctx, "s")
	if err != nil || !ok {
		t.Fatalf("Take: %v, %v", ok, err)
	}
	if err := box.Requeue(ctx, "s", entry); err != nil {
		t.Fatalf("Requeue: %v", err)
	}
	if got, want := texts(mustList(t, box, "s")), []string{"a", "b"}; !equalStrings(got, want) {
		t.Fatalf("after Requeue texts = %v, want %v", got, want)
	}
	appends := log.count()
	if err := box.Requeue(ctx, "s", entry); err != nil {
		t.Fatalf("duplicate Requeue: %v", err)
	}
	if got := log.count(); got != appends {
		t.Fatalf("duplicate Requeue appended %d events, want 0", got-appends)
	}
	if got := len(mustList(t, box, "s")); got != 2 {
		t.Fatalf("duplicate Requeue changed length to %d", got)
	}

	if err := box.Requeue(ctx, "s", inbox.Entry{Text: "no id"}); err == nil {
		t.Fatal("Requeue without id err = nil, want error")
	}
	if err := box.Requeue(ctx, "s", inbox.Entry{ID: "x"}); !errors.Is(err, inbox.ErrEmptyPrompt) {
		t.Fatalf("Requeue without text err = %v, want ErrEmptyPrompt", err)
	}
	if err := box.Requeue(ctx, " ", entry); !errors.Is(err, inbox.ErrNoSession) {
		t.Fatalf("Requeue without session err = %v, want ErrNoSession", err)
	}
}

func TestRequeueIsNotBoundedByMaxEntries(t *testing.T) {
	log := &memoryLog{}
	box := inbox.New(log)
	ctx := context.Background()
	for index := 0; index < inbox.MaxEntries; index++ {
		mustEnqueue(t, box, "s", fmt.Sprintf("prompt %d", index))
	}
	taken, ok, err := box.Take(ctx, "s")
	if err != nil || !ok {
		t.Fatalf("Take: %v, %v", ok, err)
	}
	// A concurrent Enqueue fills the slot Take just freed; the requeued entry
	// must still come back rather than be dropped as "full".
	mustEnqueue(t, box, "s", "concurrent")
	if err := box.Requeue(ctx, "s", taken); err != nil {
		t.Fatalf("Requeue into a full inbox err = %v, want success", err)
	}
	entries := mustList(t, box, "s")
	if len(entries) != inbox.MaxEntries+1 || entries[0].ID != taken.ID {
		t.Fatalf("after Requeue len = %d, first = %+v; want %d entries with the requeued one first", len(entries), entries[0], inbox.MaxEntries+1)
	}
}

func TestReplayLatestSnapshotWins(t *testing.T) {
	log := &memoryLog{}
	box := inbox.New(log)
	ctx := context.Background()
	first := mustEnqueue(t, box, "s", "a")
	mustEnqueue(t, box, "s", "b")
	if err := box.Remove(ctx, "s", first.ID); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	replayed := inbox.New(log)
	if got, want := texts(mustList(t, replayed, "s")), []string{"b"}; !equalStrings(got, want) {
		t.Fatalf("replayed texts = %v, want %v (latest snapshot only)", got, want)
	}

	// An empty snapshot is authoritative and clears the replayed inbox.
	second := mustList(t, replayed, "s")[0]
	if err := replayed.Remove(ctx, "s", second.ID); err != nil {
		t.Fatalf("Remove last: %v", err)
	}
	empty := inbox.New(log)
	if entries := mustList(t, empty, "s"); len(entries) != 0 {
		t.Fatalf("replayed texts = %v, want empty (empty snapshot wins)", texts(entries))
	}
}

func TestReplaySelectsHighestSeqNotSliceOrder(t *testing.T) {
	log := &memoryLog{}
	log.addRaw("s", inbox.EventType, `{"prompts":[{"id":"a","text":"older"}]}`)
	log.addRaw("s", inbox.EventType, `{"prompts":[{"id":"b","text":"newer"}]}`)
	log.reverse = true

	box := inbox.New(log)
	if got, want := texts(mustList(t, box, "s")), []string{"newer"}; !equalStrings(got, want) {
		t.Fatalf("texts = %v, want %v (highest seq wins regardless of slice order)", got, want)
	}
}

func TestReplayToleratesMalformedEntries(t *testing.T) {
	log := &memoryLog{}
	log.addRaw("s", inbox.EventType, `{not json`)
	box := inbox.New(log)
	entries, err := box.List(context.Background(), "s")
	if err != nil {
		t.Fatalf("List over corrupt snapshot err = %v, want nil", err)
	}
	if len(entries) != 0 {
		t.Fatalf("List over corrupt snapshot = %+v, want empty", entries)
	}

	log2 := &memoryLog{}
	log2.addRaw("s", "message", `{"prompts":[{"id":"other","text":"not an inbox event"}]}`)
	log2.addRaw("s", inbox.EventType, `{"prompts":[`+
		`{"id":"","text":"blank id"},`+
		`{"id":"blank-text","text":"   "},`+
		`{"id":"dup","text":"first"},`+
		`{"id":"dup","text":"duplicate"},`+
		`{"id":"ok","text":"  trimmed  "},`+
		`{"id":"unrelated"}]}`)
	log2.addRaw("other", inbox.EventType, `{"prompts":[{"id":"foreign","text":"other session"}]}`)

	replayed := inbox.New(log2)
	entries, err = replayed.List(context.Background(), "s")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if got, want := texts(entries), []string{"first", "trimmed"}; !equalStrings(got, want) {
		t.Fatalf("decoded texts = %v, want %v", got, want)
	}
	if entries[1].ID != "ok" {
		t.Fatalf("decoded ids = %+v, want the surviving entries", entries)
	}
}

func TestReplayCapsAtMaxEntries(t *testing.T) {
	prompts := make([]map[string]any, 0, inbox.MaxEntries+10)
	for index := 0; index < inbox.MaxEntries+10; index++ {
		prompts = append(prompts, map[string]any{"id": fmt.Sprintf("p-%d", index), "text": fmt.Sprintf("prompt %d", index)})
	}
	payload, err := json.Marshal(map[string]any{"prompts": prompts})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	log := &memoryLog{}
	log.addRaw("s", inbox.EventType, string(payload))
	box := inbox.New(log)
	if got := len(mustList(t, box, "s")); got != inbox.MaxEntries {
		t.Fatalf("replayed len = %d, want cap %d", got, inbox.MaxEntries)
	}
}

func TestDropKeepsLogUntouched(t *testing.T) {
	log := &memoryLog{}
	box := inbox.New(log)
	mustEnqueue(t, box, "s", "a")
	appends := log.count()

	box.Drop("s")
	if entries := mustList(t, box, "s"); len(entries) != 0 {
		t.Fatalf("List after Drop = %+v, want empty", entries)
	}
	if got := log.count(); got != appends {
		t.Fatalf("Drop appended %d events, want 0", got-appends)
	}
	box.Drop("  ")
	box.Drop("")

	// The id stays usable; only the replayed history is tombstoned.
	mustEnqueue(t, box, "s", "b")
	if got, want := texts(mustList(t, box, "s")), []string{"b"}; !equalStrings(got, want) {
		t.Fatalf("texts after Drop+Enqueue = %v, want %v", got, want)
	}
}

func TestPendingMarker(t *testing.T) {
	box := inbox.New(&memoryLog{})
	if box.Pending("s") {
		t.Fatal("Pending on a fresh inbox = true, want false")
	}
	box.MarkPending("s")
	if !box.Pending("s") {
		t.Fatal("Pending after MarkPending = false, want true")
	}
	box.ClearPending("s")
	if box.Pending("s") {
		t.Fatal("Pending after ClearPending = true, want false")
	}
	box.MarkPending("  ")
	if box.Pending("") {
		t.Fatal("MarkPending with a blank session set a marker")
	}

	ctx := context.Background()
	first := mustEnqueue(t, box, "s", "a")
	second := mustEnqueue(t, box, "s", "b")
	box.MarkPending("s")
	if err := box.Remove(ctx, "s", first.ID); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if !box.Pending("s") {
		t.Fatal("removing a non-last entry cleared the pending marker")
	}
	if err := box.Remove(ctx, "s", second.ID); err != nil {
		t.Fatalf("Remove last: %v", err)
	}
	if box.Pending("s") {
		t.Fatal("removing the last entry left the pending marker set")
	}
}

func TestNilLogIsMemoryOnly(t *testing.T) {
	box := inbox.New(nil)
	ctx := context.Background()
	first := mustEnqueue(t, box, "s", "a")
	if got := len(mustList(t, box, "s")); got != 1 {
		t.Fatalf("List len = %d, want 1", got)
	}
	entry, ok, err := box.Take(ctx, "s")
	if err != nil || !ok || entry.ID != first.ID {
		t.Fatalf("Take = %+v, %v, %v; want the enqueued entry", entry, ok, err)
	}
	if err := box.Remove(ctx, "s", first.ID); err != nil {
		t.Fatalf("Remove after Take: %v", err)
	}
}

func TestLoadFailureRetries(t *testing.T) {
	log := &memoryLog{}
	box := inbox.New(log)
	ctx := context.Background()
	mustEnqueue(t, box, "s", "a")

	fresh := inbox.New(log)
	log.eventsErr = errors.New("log unavailable")
	if _, err := fresh.List(ctx, "s"); err == nil {
		t.Fatal("List with failing log err = nil, want error")
	}
	log.eventsErr = nil
	if got, want := texts(mustList(t, fresh, "s")), []string{"a"}; !equalStrings(got, want) {
		t.Fatalf("texts after log recovery = %v, want %v", got, want)
	}
}

func TestConcurrentEnqueue(t *testing.T) {
	log := &memoryLog{}
	box := inbox.New(log)
	const workers = 64
	var group sync.WaitGroup
	ids := make([]string, workers)
	errorsFound := make([]error, workers)
	for worker := 0; worker < workers; worker++ {
		group.Add(1)
		go func(worker int) {
			defer group.Done()
			entry, err := box.Enqueue(context.Background(), "s", fmt.Sprintf("prompt %d", worker))
			ids[worker], errorsFound[worker] = entry.ID, err
		}(worker)
	}
	group.Wait()
	seen := make(map[string]bool, workers)
	for worker := 0; worker < workers; worker++ {
		if errorsFound[worker] != nil {
			t.Fatalf("worker %d Enqueue err = %v", worker, errorsFound[worker])
		}
		if ids[worker] == "" || seen[ids[worker]] {
			t.Fatalf("worker %d id = %q (duplicate or empty)", worker, ids[worker])
		}
		seen[ids[worker]] = true
	}
	if got := len(mustList(t, box, "s")); got != workers {
		t.Fatalf("List len = %d, want %d", got, workers)
	}
	if got := log.count(); got != workers {
		t.Fatalf("append count = %d, want %d", got, workers)
	}
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for index := range got {
		if got[index] != want[index] {
			return false
		}
	}
	return true
}
