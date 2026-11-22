package export

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	rig "github.com/zeokeel/zeokeel-rig"
)

// memLog is an in-memory EventsLog. It also implements RecentLog so tests can
// prove Archive prefers Events.
type memLog struct {
	events []rig.SessionEvent
	err    error

	eventsCalls int
	lastCalls   int
	lastLimit   int
	from        int64
	to          int64
}

func (l *memLog) Events(ctx context.Context, sessionID string, from, to int64) ([]rig.SessionEvent, error) {
	l.eventsCalls++
	l.from, l.to = from, to
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if l.err != nil {
		return nil, l.err
	}
	return filterEvents(l.events, sessionID, from, to), nil
}

func (l *memLog) Last(ctx context.Context, sessionID string, limit int) ([]rig.SessionEvent, error) {
	l.lastCalls++
	l.lastLimit = limit
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if l.err != nil {
		return nil, l.err
	}
	events := filterEvents(l.events, sessionID, 0, 0)
	if limit > 0 && len(events) > limit {
		events = events[len(events)-limit:]
	}
	return events, nil
}

// tailLog is an in-memory RecentLog (no Events).
type tailLog struct {
	events []rig.SessionEvent
	err    error

	calls int
	limit int
}

func (l *tailLog) Last(ctx context.Context, sessionID string, limit int) ([]rig.SessionEvent, error) {
	l.calls++
	l.limit = limit
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if l.err != nil {
		return nil, l.err
	}
	events := filterEvents(l.events, sessionID, 0, 0)
	if limit > 0 && len(events) > limit {
		events = events[len(events)-limit:]
	}
	return events, nil
}

func filterEvents(events []rig.SessionEvent, sessionID string, from, to int64) []rig.SessionEvent {
	out := make([]rig.SessionEvent, 0, len(events))
	for _, event := range events {
		if event.SessionID != "" && event.SessionID != sessionID {
			continue
		}
		if event.Seq < from {
			continue
		}
		if to > 0 && event.Seq > to {
			continue
		}
		out = append(out, event)
	}
	return out
}

func TestArchiveRoundTrip(t *testing.T) {
	created := time.Date(2026, 11, 22, 9, 0, 0, 0, time.UTC)
	messages := created.Add(time.Minute)
	tools := created.Add(2 * time.Minute)
	events := []rig.SessionEvent{
		{Seq: 1, SessionID: "s1", Type: "session.created", Time: created, Payload: json.RawMessage(`{"title":"hi"}`)},
		{Seq: 2, SessionID: "s1", Type: "message", Time: messages, Payload: json.RawMessage(`{"text":"你好","n":1}`)},
		{Seq: 3, SessionID: "s1", Type: "tool_call", Time: tools, Payload: json.RawMessage(`{"id":"c1"}`)},
	}
	log := &memLog{events: events}
	exportedAt := time.Date(2026, 11, 22, 10, 30, 0, 0, time.UTC)

	data, err := Archive(context.Background(), log, "s1", Meta{
		Title:      "My session",
		ExportedAt: exportedAt,
	})
	if err != nil {
		t.Fatalf("Archive: %v", err)
	}
	files := readArchive(t, data, "meta.json", "events.jsonl")
	if log.eventsCalls != 1 || log.lastCalls != 0 {
		t.Fatalf("calls: Events=%d Last=%d, want 1/0", log.eventsCalls, log.lastCalls)
	}
	if log.from != 1 || log.to != 0 {
		t.Fatalf("Events window: from=%d to=%d, want 1/0", log.from, log.to)
	}

	var meta metaDocument
	if err := json.Unmarshal(files["meta.json"], &meta); err != nil {
		t.Fatalf("decode meta.json: %v", err)
	}
	wantMeta := metaDocument{
		ID:         "s1",
		Title:      "My session",
		ExportedAt: "2026-11-22T10:30:00Z",
		Version:    ArchiveVersion,
		Schema:     ArchiveSchema,
	}
	if meta != wantMeta {
		t.Fatalf("meta.json = %+v, want %+v", meta, wantMeta)
	}
	assertKeys(t, files["meta.json"], "id", "title", "exportedAt", "version", "schema")

	lines := jsonlLines(t, files["events.jsonl"])
	if len(lines) != len(events) {
		t.Fatalf("events.jsonl lines = %d, want %d", len(lines), len(events))
	}
	for index, line := range lines {
		var decoded eventLine
		if err := json.Unmarshal(line, &decoded); err != nil {
			t.Fatalf("line %d is not one JSON object: %v (%s)", index+1, err, line)
		}
		want := events[index]
		if decoded.Seq != want.Seq || decoded.Type != want.Type || !decoded.Time.Equal(want.Time) {
			t.Fatalf("line %d = %+v, want seq/type/time of %+v", index+1, decoded, want)
		}
		assertRawJSONEqual(t, decoded.Payload, want.Payload)
		assertKeys(t, line, "seq", "type", "time", "payload")
	}
	if bytes.Contains(files["events.jsonl"], []byte("sessionId")) {
		t.Fatal("events.jsonl repeats the session id; meta.json owns identity")
	}
}

func TestArchiveMetaOverrideAndUTCNormalization(t *testing.T) {
	zone := time.FixedZone("UTC+2", 2*60*60)
	exportedAt := time.Date(2026, 11, 22, 12, 0, 0, 0, zone)
	data, err := Archive(context.Background(), &memLog{}, "s1", Meta{
		ID:         "custom-id",
		Title:      "",
		ExportedAt: exportedAt,
	})
	if err != nil {
		t.Fatalf("Archive: %v", err)
	}
	var meta metaDocument
	if err := json.Unmarshal(readArchive(t, data)["meta.json"], &meta); err != nil {
		t.Fatalf("decode meta.json: %v", err)
	}
	if meta.ID != "custom-id" {
		t.Fatalf("meta id = %q, want custom-id", meta.ID)
	}
	if meta.ExportedAt != "2026-11-22T10:00:00Z" {
		t.Fatalf("meta exportedAt = %q, want UTC copy", meta.ExportedAt)
	}
}

func TestArchiveDefaultsClock(t *testing.T) {
	before := time.Now().UTC().Add(-time.Second)
	data, err := Archive(context.Background(), &memLog{}, "s1", Meta{})
	if err != nil {
		t.Fatalf("Archive: %v", err)
	}
	var meta metaDocument
	if err := json.Unmarshal(readArchive(t, data)["meta.json"], &meta); err != nil {
		t.Fatalf("decode meta.json: %v", err)
	}
	parsed, err := time.Parse(time.RFC3339, meta.ExportedAt)
	if err != nil {
		t.Fatalf("exportedAt %q is not RFC3339: %v", meta.ExportedAt, err)
	}
	if parsed.Before(before) || parsed.After(time.Now().UTC().Add(time.Second)) {
		t.Fatalf("exportedAt %v not near now", parsed)
	}
	if meta.ID != "s1" {
		t.Fatalf("meta id = %q, want session id fallback", meta.ID)
	}
}

func TestArchiveEmptyLog(t *testing.T) {
	data, err := Archive(context.Background(), &memLog{}, "empty", Meta{ExportedAt: time.Unix(0, 0)})
	if err != nil {
		t.Fatalf("Archive: %v", err)
	}
	files := readArchive(t, data, "meta.json", "events.jsonl")
	if len(files["events.jsonl"]) != 0 {
		t.Fatalf("events.jsonl = %q, want empty", files["events.jsonl"])
	}
}

func TestArchiveNilPayloadAndZeroTime(t *testing.T) {
	log := &memLog{events: []rig.SessionEvent{{Seq: 1, SessionID: "s1", Type: "x"}}}
	data, err := Archive(context.Background(), log, "s1", Meta{})
	if err != nil {
		t.Fatalf("Archive: %v", err)
	}
	lines := jsonlLines(t, readArchive(t, data)["events.jsonl"])
	if len(lines) != 1 {
		t.Fatalf("lines = %d, want 1", len(lines))
	}
	var decoded eventLine
	if err := json.Unmarshal(lines[0], &decoded); err != nil {
		t.Fatalf("decode line: %v", err)
	}
	if string(decoded.Payload) != "null" {
		t.Fatalf("payload = %s, want null", decoded.Payload)
	}
	if !decoded.Time.IsZero() {
		t.Fatalf("time = %v, want zero time preserved", decoded.Time)
	}
}

func TestArchiveTailOnlyLog(t *testing.T) {
	base := time.Date(2026, 11, 22, 8, 0, 0, 0, time.UTC)
	log := &tailLog{events: []rig.SessionEvent{
		{Seq: 3, SessionID: "s1", Type: "c", Time: base.Add(2 * time.Minute), Payload: json.RawMessage(`{}`)},
		{Seq: 1, SessionID: "s1", Type: "a", Time: base, Payload: json.RawMessage(`{}`)},
		{Seq: 2, SessionID: "s1", Type: "b", Time: base.Add(time.Minute), Payload: json.RawMessage(`{}`)},
	}}
	// Deliberately unsorted input: the archive must be chronological.
	data, err := Archive(context.Background(), log, "s1", Meta{})
	if err != nil {
		t.Fatalf("Archive: %v", err)
	}
	if log.calls != 1 {
		t.Fatalf("Last calls = %d, want 1", log.calls)
	}
	if log.limit != MaxEvents+1 {
		t.Fatalf("Last limit = %d, want MaxEvents+1", log.limit)
	}
	lines := jsonlLines(t, readArchive(t, data)["events.jsonl"])
	if len(lines) != 3 {
		t.Fatalf("lines = %d, want 3", len(lines))
	}
	var seqs []int64
	for _, line := range lines {
		var decoded eventLine
		if err := json.Unmarshal(line, &decoded); err != nil {
			t.Fatalf("decode line: %v", err)
		}
		seqs = append(seqs, decoded.Seq)
	}
	if !slices.Equal(seqs, []int64{1, 2, 3}) {
		t.Fatalf("seqs = %v, want ascending 1,2,3", seqs)
	}
}

func TestArchivePrefersEventsOverLast(t *testing.T) {
	log := &memLog{events: []rig.SessionEvent{{Seq: 1, SessionID: "s1", Type: "a"}}}
	if _, err := Archive(context.Background(), log, "s1", Meta{}); err != nil {
		t.Fatalf("Archive: %v", err)
	}
	if log.eventsCalls != 1 || log.lastCalls != 0 {
		t.Fatalf("calls: Events=%d Last=%d, want 1/0", log.eventsCalls, log.lastCalls)
	}
}

func TestArchiveEventLimit(t *testing.T) {
	log := &memLog{events: []rig.SessionEvent{
		{Seq: 1, SessionID: "s1", Type: "a"},
		{Seq: 2, SessionID: "s1", Type: "b"},
		{Seq: 3, SessionID: "s1", Type: "c"},
	}}
	data, err := archiveWithLimits(context.Background(), log, "s1", Meta{},
		limits{maxEvents: 2, maxBytes: MaxBytes})
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("error = %v, want ErrTooLarge", err)
	}
	if data != nil {
		t.Fatal("Archive returned bytes for an over-limit log")
	}
	if !strings.Contains(err.Error(), "3 events (limit 2)") {
		t.Fatalf("error %q should name the count and limit", err)
	}
}

func TestArchiveTailEventLimit(t *testing.T) {
	events := make([]rig.SessionEvent, 5)
	for index := range events {
		events[index] = rig.SessionEvent{Seq: int64(index + 1), SessionID: "s1", Type: "x"}
	}
	log := &tailLog{events: events}
	_, err := archiveWithLimits(context.Background(), log, "s1", Meta{},
		limits{maxEvents: 2, maxBytes: MaxBytes})
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("error = %v, want ErrTooLarge", err)
	}
	if log.limit != 3 {
		t.Fatalf("Last limit = %d, want maxEvents+1", log.limit)
	}
}

func TestArchiveByteLimit(t *testing.T) {
	log := &memLog{events: []rig.SessionEvent{
		{Seq: 1, SessionID: "s1", Type: "a", Payload: json.RawMessage(`"` + strings.Repeat("x", 256) + `"`)},
	}}
	_, err := archiveWithLimits(context.Background(), log, "s1", Meta{},
		limits{maxEvents: MaxEvents, maxBytes: 64})
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("error = %v, want ErrTooLarge", err)
	}
	if !strings.Contains(err.Error(), "bytes") {
		t.Fatalf("error %q should mention bytes", err)
	}
}

func TestArchiveRejectsMissingInputs(t *testing.T) {
	if _, err := Archive(context.Background(), nil, "s1", Meta{}); !errors.Is(err, ErrNoLog) {
		t.Fatalf("nil log error = %v, want ErrNoLog", err)
	}
	for _, sessionID := range []string{"", "   "} {
		if _, err := Archive(context.Background(), &memLog{}, sessionID, Meta{}); !errors.Is(err, ErrSessionRequired) {
			t.Fatalf("session id %q error = %v, want ErrSessionRequired", sessionID, err)
		}
	}
	if _, err := Archive(context.Background(), struct{}{}, "s1", Meta{}); !errors.Is(err, ErrUnsupportedLog) {
		t.Fatalf("unsupported log error = %v, want ErrUnsupportedLog", err)
	}
}

func TestArchivePropagatesContextAndLogErrors(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	log := &memLog{}
	if _, err := Archive(canceled, log, "s1", Meta{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled ctx error = %v, want context.Canceled", err)
	}
	if log.eventsCalls != 0 {
		t.Fatalf("Events called %d times after cancellation, want 0", log.eventsCalls)
	}
	failing := &memLog{err: context.DeadlineExceeded}
	if _, err := Archive(context.Background(), failing, "s1", Meta{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("log error = %v, want DeadlineExceeded", err)
	}
	tailFailing := &tailLog{err: errors.New("tail broke")}
	if _, err := Archive(context.Background(), tailFailing, "s1", Meta{}); err == nil || err.Error() != "tail broke" {
		t.Fatalf("tail error = %v, want passthrough", err)
	}
}

func TestArchiveNilContext(t *testing.T) {
	//lint:ignore SA1012 nil is documented as the background context
	if _, err := Archive(nil, &memLog{}, "s1", Meta{}); err != nil { //nolint:staticcheck
		t.Fatalf("Archive(nil ctx): %v", err)
	}
}

// readArchive opens the ZIP and asserts its exact entry names.
func readArchive(t *testing.T, data []byte, wantNames ...string) map[string][]byte {
	t.Helper()
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("open archive: %v", err)
	}
	files := make(map[string][]byte, len(reader.File))
	names := make([]string, 0, len(reader.File))
	for _, file := range reader.File {
		handle, err := file.Open()
		if err != nil {
			t.Fatalf("open archive entry %q: %v", file.Name, err)
		}
		content, err := io.ReadAll(handle)
		handle.Close()
		if err != nil {
			t.Fatalf("read archive entry %q: %v", file.Name, err)
		}
		files[file.Name] = content
		names = append(names, file.Name)
	}
	if wantNames == nil {
		wantNames = []string{"meta.json", "events.jsonl"}
	}
	if !slices.Equal(names, wantNames) {
		t.Fatalf("archive entries = %v, want %v", names, wantNames)
	}
	return files
}

// jsonlLines splits the event log; a non-empty log must end with exactly one
// newline and contain no blank lines.
func jsonlLines(t *testing.T, data []byte) [][]byte {
	t.Helper()
	if len(data) == 0 {
		return nil
	}
	if !bytes.HasSuffix(data, []byte("\n")) {
		t.Fatalf("events.jsonl does not end with a newline: %q", data)
	}
	trimmed := data[:len(data)-1]
	lines := bytes.Split(trimmed, []byte("\n"))
	for index, line := range lines {
		if len(line) == 0 {
			t.Fatalf("events.jsonl line %d is blank", index+1)
		}
	}
	return lines
}

func assertKeys(t *testing.T, data []byte, want ...string) {
	t.Helper()
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("decode JSON object: %v (%s)", err, data)
	}
	keys := make([]string, 0, len(decoded))
	for key := range decoded {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	slices.Sort(want)
	if !slices.Equal(keys, want) {
		t.Fatalf("keys = %v, want %v", keys, want)
	}
}

func assertRawJSONEqual(t *testing.T, got, want json.RawMessage) {
	t.Helper()
	var gotValue, wantValue any
	if err := json.Unmarshal(got, &gotValue); err != nil {
		t.Fatalf("decode payload %s: %v", got, err)
	}
	if err := json.Unmarshal(want, &wantValue); err != nil {
		t.Fatalf("decode expected payload %s: %v", want, err)
	}
	if !reflect.DeepEqual(gotValue, wantValue) {
		t.Fatalf("payload = %s, want %s", got, want)
	}
}
