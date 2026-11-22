package importer_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/session/importer"
)

func writeLog(t *testing.T, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "session.v4.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestParseSessionFile(t *testing.T) {
	path := writeLog(t,
		`{"type":"session","version":4,"id":"s1","createdAt":1700000000000,"cwd":"/tmp/ws"}`,
		`{"type":"user/message","seq":1,"data":{"role":"user","content":[{"type":"text","text":"Hello"}],"source":{"kind":"user"}}}`,
		`{"type":"assistant/message","seq":2,"data":{"message":{"role":"assistant","content":[{"type":"text","text":"Hi"},{"type":"reasoning","text":"think"}]}}}`,
		`{"type":"user/message","seq":3,"data":{"role":"user","content":[{"type":"text","text":"ctx"}],"source":{"kind":"time-context"}}}`,
	)
	session, err := importer.ParseSessionFile(path)
	if err != nil {
		t.Fatalf("ParseSessionFile: %v", err)
	}
	if session.ID != "s1" || session.Cwd != "/tmp/ws" || len(session.Messages) != 2 {
		t.Fatalf("session = %+v", session)
	}
	if session.Messages[0].Text != "Hello" || session.Messages[1].Text != "Hi" || session.Messages[1].Reasoning != "think" {
		t.Fatalf("messages = %+v", session.Messages)
	}
}

func TestUnsupportedVersionAndBrokenJSON(t *testing.T) {
	path := writeLog(t, `{"type":"session","version":3,"id":"s1"}`)
	if _, err := importer.ParseSessionFile(path); err == nil {
		t.Fatal("unsupported version accepted")
	}
	path = writeLog(t, `{broken`)
	if _, err := importer.ParseSessionFile(path); err == nil {
		t.Fatal("broken JSON accepted")
	}
}

func TestMessagesTitleAndReasoning(t *testing.T) {
	msgs := []importer.Message{
		{Role: "assistant", Text: "only assistant"},
		{Role: "user", Text: "  spaced  "},
		{Role: "assistant", Text: "answer", Reasoning: "thought"},
	}
	if title := importer.Title(msgs); title != "spaced" {
		t.Fatalf("Title = %q", title)
	}
	if title := importer.Title([]importer.Message{{Role: "assistant", Text: "x"}}); title == "" {
		t.Fatal("Title fallback is empty")
	}
	converted := importer.Messages(msgs)
	if len(converted) != 3 {
		t.Fatalf("Messages = %+v", converted)
	}
	if converted[2].Role != "assistant" {
		t.Fatalf("converted role = %q", converted[2].Role)
	}
}

func TestParseLineShapesAndAttachments(t *testing.T) {
	path := writeLog(t,
		`{"type":"session","version":4,"id":"s2","cwd":"/tmp/ws"}`,
		`{"type":"user/message","seq":1,"data":{"source":{"kind":"user"},"message":{"content":[{"type":"text","text":"nested user"}]}}}`,
		`{"type":"assistant/message","seq":2,"data":{"content":[{"type":"text","text":"flat assistant"},{"type":"reasoning","text":"r"},{"type":"image","text":""}]}}`,
		`{"type":"user/message","seq":3,"data":{"role":"user","content":[{"type":"text","text":"ctx"}],"source":{"kind":"automation"}}}`,
	)
	session, err := importer.ParseSessionFile(path)
	if err != nil {
		t.Fatalf("ParseSessionFile: %v", err)
	}
	if len(session.Messages) != 2 || session.Messages[0].Text != "nested user" || session.Messages[1].Reasoning != "r" {
		t.Fatalf("messages = %+v", session.Messages)
	}
	if session.DroppedAttachments != 1 {
		t.Fatalf("dropped = %d", session.DroppedAttachments)
	}
}

func TestMissingHeaderIsAnError(t *testing.T) {
	path := writeLog(t, `{"type":"user/message","seq":1,"data":{"role":"user","content":[]}}`)
	if _, err := importer.ParseSessionFile(path); err == nil {
		t.Fatal("missing header accepted")
	}
}

func TestDiscoverSessions(t *testing.T) {
	root := t.TempDir()
	sessions := filepath.Join(root, "sessions", "ws-a")
	if err := os.MkdirAll(sessions, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(sessions, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	header := `{"type":"session","version":4,"id":"s1","cwd":"` + root + `"}`
	write("session.v4.jsonl", header+"\n"+`{"type":"user/message","seq":1,"data":{"role":"user","content":[{"type":"text","text":"Hello"}],"source":{"kind":"user"}}}`+"\n")
	write("other.zstd", "x")
	write("broken.txt", "x")
	if err := os.Symlink(filepath.Join(sessions, "session.v4.jsonl"), filepath.Join(sessions, "link.v4.jsonl")); err != nil {
		t.Fatal(err)
	}
	discovery, err := importer.Discover(context.Background(), root)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if discovery.Sessions != 1 || discovery.Messages != 1 || discovery.Workspaces != 1 {
		t.Fatalf("discovery = %+v", discovery)
	}
	if discovery.Skipped != 1 || len(discovery.Sources) != 1 {
		t.Fatalf("skipped/sources = %d/%d", discovery.Skipped, len(discovery.Sources))
	}
	if len(discovery.Notes) < 2 {
		t.Fatalf("notes = %v", discovery.Notes)
	}
	if d, err := importer.Discover(context.Background(), t.TempDir()); err != nil || d.Sessions != 0 {
		t.Fatalf("empty home = %+v/%v", d, err)
	}
}

type fakeTarget struct {
	ids     []string
	created map[string]string
	history map[string][]rig.Message
	events  []rig.SessionEvent
}

func newFakeTarget() *fakeTarget {
	return &fakeTarget{created: map[string]string{}, history: map[string][]rig.Message{}}
}

func (f *fakeTarget) SessionIDs(context.Context) ([]string, error) { return f.ids, nil }
func (f *fakeTarget) CreateSession(_ context.Context, id, title string) error {
	f.created[id] = title
	return nil
}
func (f *fakeTarget) RenameSession(context.Context, string, string) error { return nil }
func (f *fakeTarget) ReplaceHistory(_ context.Context, id string, messages []rig.Message) error {
	f.history[id] = messages
	return nil
}
func (f *fakeTarget) AppendEvent(_ context.Context, event rig.SessionEvent) error {
	f.events = append(f.events, event)
	return nil
}

type fakeWorkspaces struct{ created, opened []string }

func (w *fakeWorkspaces) Current(context.Context) (string, bool, error) {
	return "ws-current", true, nil
}
func (w *fakeWorkspaces) Create(_ context.Context, name, root string) error {
	w.created = append(w.created, root)
	return nil
}
func (w *fakeWorkspaces) Open(_ context.Context, id string) error {
	w.opened = append(w.opened, id)
	return nil
}

func TestImportWritesSessionsAndRestoresWorkspace(t *testing.T) {
	dir := t.TempDir()
	session := &importer.Session{ID: "s1", Cwd: dir, Messages: []importer.Message{{Role: "user", Text: "Hello"}}}
	target := newFakeTarget()
	workspaces := &fakeWorkspaces{}
	result, err := importer.Import(context.Background(), []importer.Source{{Session: session, RelativePath: "sessions/s1/session.v4.jsonl"}}, 2, target, workspaces, nil)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if result.ImportedSessions != 1 || result.ImportedMessages != 1 || result.ImportedWorkspaces != 1 || result.Skipped != 2 {
		t.Fatalf("result = %+v", result)
	}
	if target.created["dsh-s1"] != "Hello" || len(target.history["dsh-s1"]) != 1 || len(target.events) != 1 {
		t.Fatalf("target = %+v", target)
	}
	if len(workspaces.created) != 1 || len(workspaces.opened) != 1 || workspaces.opened[0] != "ws-current" {
		t.Fatalf("workspaces = %+v", workspaces)
	}
	// Existing sessions are skipped before creation.
	existing := newFakeTarget()
	existing.ids = []string{"dsh-s1"}
	skipped, err := importer.Import(context.Background(), []importer.Source{{Session: session}}, 0, existing, nil, nil)
	if err != nil || skipped.Skipped != 1 || len(existing.created) != 0 {
		t.Fatalf("existing = %+v/%v", skipped, err)
	}
}
