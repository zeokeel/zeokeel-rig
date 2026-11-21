package importer_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

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
