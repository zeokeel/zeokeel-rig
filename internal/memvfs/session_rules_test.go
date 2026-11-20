package memvfs

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig/memoryvfs"
)

func TestCaptureIdempotentAndPending(t *testing.T) {
	store := newStore(t, memoryvfs.WithClock(testClock))
	ctx := context.Background()
	message := memoryvfs.Message{Role: "user", Content: "hello there, remember this preference"}

	if err := store.Capture(ctx, "sess-1", message); err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if err := store.Capture(ctx, "sess-1", message); err != nil {
		t.Fatalf("Capture duplicate: %v", err)
	}
	repeat := message
	repeat.At = testNow
	if err := store.Capture(ctx, "sess-1", repeat); err != nil {
		t.Fatalf("Capture with At: %v", err)
	}
	if err := store.Capture(ctx, "sess-1", repeat); err != nil {
		t.Fatalf("Capture with At duplicate: %v", err)
	}

	state, err := store.Pending(ctx, "sess-1")
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if state.ID != "sess-1" {
		t.Fatalf("id = %q", state.ID)
	}
	if state.Pending != 2 {
		t.Fatalf("pending = %d, want 2", state.Pending)
	}
	if state.Tokens == 0 {
		t.Fatal("tokens should be estimated")
	}
	if state.Updated.IsZero() {
		t.Fatal("updated should be set")
	}

	empty, err := store.Pending(ctx, "sess-unknown")
	if err != nil {
		t.Fatalf("Pending unknown: %v", err)
	}
	if empty.Pending != 0 {
		t.Fatalf("unknown pending = %d", empty.Pending)
	}

	err = store.Capture(ctx, "../evil", message)
	wantErrIs(t, err, memoryvfs.ErrInvalidEntry, "invalid session id")
}

func TestCommitArchivesAndExtracts(t *testing.T) {
	dir := t.TempDir()
	store := newStoreAt(t, dir, memoryvfs.WithClock(testClock))
	ctx := context.Background()
	messages := []memoryvfs.Message{
		{Role: "user", Content: "I prefer dark mode for my editor always"},
		{Role: "assistant", Content: "noted, dark mode it is"},
	}
	if err := store.Capture(ctx, "sess-commit", messages...); err != nil {
		t.Fatalf("Capture: %v", err)
	}

	result, err := store.Commit(ctx, "sess-commit", memoryvfs.CommitOptions{Extract: true})
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if result.Archive.IsZero() {
		t.Fatal("archive uri not set")
	}
	if !result.Archive.IsUnder("mem://sessions/sess-commit") {
		t.Fatalf("archive uri = %s", result.Archive)
	}
	archive, err := store.Read(ctx, result.Archive, memoryvfs.Level2)
	if err != nil {
		t.Fatalf("Read archive: %v", err)
	}
	if archive.Kind != memoryvfs.KindSession {
		t.Fatalf("archive kind = %s", archive.Kind)
	}
	if !strings.Contains(archive.Summary, "dark mode") {
		t.Fatalf("archive summary = %q", archive.Summary)
	}
	if !strings.Contains(archive.Content, "assistant") {
		t.Fatalf("archive content = %q", archive.Content)
	}
	if archive.Overview == "" {
		t.Fatal("archive overview empty")
	}
	if result.Tokens == 0 {
		t.Fatal("result tokens should be reported")
	}
	if len(result.Written) == 0 {
		t.Fatalf("expected extracted candidates, got %+v", result)
	}
	for _, uri := range result.Written {
		if _, err := store.Stat(ctx, uri); err != nil {
			t.Fatalf("written entry %s missing: %v", uri, err)
		}
	}

	state, err := store.Pending(ctx, "sess-commit")
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if state.Pending != 0 {
		t.Fatalf("pending after commit = %d", state.Pending)
	}
	if state.LastCommit.IsZero() {
		t.Fatal("last commit not recorded")
	}

	// 二次提交（无 pending）应无害。
	second, err := store.Commit(ctx, "sess-commit", memoryvfs.CommitOptions{Extract: true})
	if err != nil {
		t.Fatalf("second Commit: %v", err)
	}
	if !second.Archive.IsZero() || second.Skipped != 0 {
		t.Fatalf("second commit = %+v", second)
	}
}

func TestCommitWorkingMemory(t *testing.T) {
	store := newStore(t, memoryvfs.WithClock(testClock))
	ctx := context.Background()
	if err := store.Capture(ctx, "sess-wm",
		memoryvfs.Message{Role: "user", Content: "we decided to ship the memory v2 backend"},
		memoryvfs.Message{Role: "assistant", Content: "agreed"}); err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if _, err := store.Commit(ctx, "sess-wm", memoryvfs.CommitOptions{WorkingMemory: true}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	overview, err := store.Read(ctx, "mem://sessions/sess-wm/overview", memoryvfs.Level2)
	if err != nil {
		t.Fatalf("overview entry: %v", err)
	}
	if overview.Kind != memoryvfs.KindSession || overview.Summary == "" {
		t.Fatalf("overview = %+v", overview)
	}
	checkpoint, ok := store.Checkpoint("sess-wm")
	if !ok || checkpoint.IsZero() {
		t.Fatalf("checkpoint = %v ok=%v", checkpoint, ok)
	}
}

func TestCommitFailureKeepsPending(t *testing.T) {
	store := newStore(t, memoryvfs.WithClock(testClock))
	ctx := context.Background()
	if err := store.Capture(ctx, "sess-fail",
		memoryvfs.Message{Role: "user", Content: "I prefer tabs for indentation always"}); err != nil {
		t.Fatalf("Capture: %v", err)
	}
	extractErr := errors.New("extractor exploded")
	if _, err := store.Commit(ctx, "sess-fail", memoryvfs.CommitOptions{Extract: true}); err != nil {
		t.Fatalf("Commit without extractor: %v", err)
	}
	// 重新放入 pending，再用失败的抽取器提交。
	if err := store.Capture(ctx, "sess-fail2",
		memoryvfs.Message{Role: "user", Content: "I prefer tabs for indentation always"}); err != nil {
		t.Fatalf("Capture: %v", err)
	}
	failing := &failingExtractor{err: extractErr}
	broken := newStore(t, memoryvfs.WithClock(testClock), memoryvfs.WithExtractor(failing))
	if err := broken.Capture(ctx, "sess-fail2",
		memoryvfs.Message{Role: "user", Content: "I prefer tabs for indentation always"}); err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if _, err := broken.Commit(ctx, "sess-fail2", memoryvfs.CommitOptions{Extract: true}); !errors.Is(err, extractErr) {
		t.Fatalf("Commit error = %v", err)
	}
	state, err := broken.Pending(ctx, "sess-fail2")
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if state.Pending != 1 {
		t.Fatalf("pending after failed commit = %d, want 1", state.Pending)
	}
}

func TestCommitMinConfidenceAndDisabledExtract(t *testing.T) {
	extractor := &fixedExtractor{candidates: []memoryvfs.Candidate{
		{Kind: memoryvfs.KindMemory, Type: "preferences", Summary: "low confidence note", Confidence: 0.2},
		{Kind: memoryvfs.KindMemory, Type: "preferences", Summary: "high confidence note", Confidence: 0.95},
	}}
	store := newStore(t, memoryvfs.WithClock(testClock), memoryvfs.WithExtractor(extractor))
	ctx := context.Background()
	if err := store.Capture(ctx, "sess-conf", memoryvfs.Message{Role: "user", Content: "something"}); err != nil {
		t.Fatalf("Capture: %v", err)
	}
	result, err := store.Commit(ctx, "sess-conf", memoryvfs.CommitOptions{Extract: true, MinConfidence: 0.9})
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if len(result.Written) != 1 {
		t.Fatalf("written = %+v", result.Written)
	}
	if result.Skipped != 1 {
		t.Fatalf("skipped = %d, want 1", result.Skipped)
	}

	noExtract := newStore(t, memoryvfs.WithClock(testClock), memoryvfs.WithExtractor(extractor))
	if err := noExtract.Capture(ctx, "sess-noextract", memoryvfs.Message{Role: "user", Content: "something"}); err != nil {
		t.Fatalf("Capture: %v", err)
	}
	plain, err := noExtract.Commit(ctx, "sess-noextract", memoryvfs.CommitOptions{})
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if len(plain.Written) != 0 || plain.Skipped != 0 {
		t.Fatalf("extract disabled but candidates written: %+v", plain)
	}
}

func TestCommitTwiceIsIdempotentForCandidates(t *testing.T) {
	store := newStore(t, memoryvfs.WithClock(testClock))
	ctx := context.Background()
	message := memoryvfs.Message{Role: "user", Content: "I prefer dark mode for my editor always"}
	if err := store.Capture(ctx, "sess-idem", message); err != nil {
		t.Fatalf("Capture: %v", err)
	}
	first, err := store.Commit(ctx, "sess-idem", memoryvfs.CommitOptions{Extract: true})
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err := store.Capture(ctx, "sess-idem", message); err != nil {
		t.Fatalf("Capture: %v", err)
	}
	second, err := store.Commit(ctx, "sess-idem", memoryvfs.CommitOptions{Extract: true})
	if err != nil {
		t.Fatalf("second Commit: %v", err)
	}
	if len(second.Written) != 0 {
		t.Fatalf("second commit rewrote entries: %+v", second.Written)
	}
	if len(second.Merged) == 0 {
		t.Fatalf("second commit should merge duplicates: %+v", second)
	}
	if len(first.Written) == 0 {
		t.Fatalf("first commit wrote nothing: %+v", first)
	}
}

func TestCompileRulesStoresUnderRulesRoot(t *testing.T) {
	store := newStore(t, memoryvfs.WithClock(testClock))
	ctx := context.Background()
	instruction := "- Always run gofmt before every commit\n- Never commit secret keys to the repo\n- Prefer table driven tests for go code"
	root, err := store.CompileRules(ctx, instruction, nil)
	if err != nil {
		t.Fatalf("CompileRules: %v", err)
	}
	if root.Kind != memoryvfs.KindDir || !root.URI.IsUnder("mem://user/self") {
		t.Fatalf("rules root = %+v", root)
	}
	entries, err := store.List(ctx, root.URI)
	if err != nil {
		t.Fatalf("List rules: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("rules = %d, want 3 (%+v)", len(entries), entries)
	}
	for _, entry := range entries {
		if entry.Kind != memoryvfs.KindRule || entry.Type != memoryvfs.TypeRules {
			t.Fatalf("rule entry = %+v", entry)
		}
		if entry.Summary == "" || len(entry.Summary) > memoryvfs.MaxSummaryBytes {
			t.Fatalf("rule summary = %q", entry.Summary)
		}
	}

	if _, err := store.CompileRules(ctx, "", nil); err == nil {
		t.Fatal("empty instruction and sources should fail")
	}

	source := mustWrite(t, store, memoryvfs.Entry{
		URI:     "mem://resources/docs/conventions",
		Kind:    memoryvfs.KindResource,
		Summary: "conventions",
		Content: "Use short functions. Document every exported symbol.",
	})
	root2, err := store.CompileRules(ctx, "", []memoryvfs.URI{source.URI})
	if err != nil {
		t.Fatalf("CompileRules sources: %v", err)
	}
	entries, err = store.List(ctx, root2.URI)
	if err != nil {
		t.Fatalf("List rules: %v", err)
	}
	if len(entries) != 4 {
		t.Fatalf("rules after source compile = %d", len(entries))
	}
}

func TestSearchRulesScoresAndThreshold(t *testing.T) {
	store := newStore(t, memoryvfs.WithClock(testClock))
	ctx := context.Background()
	instruction := "Always run gofmt before every go commit\nNever commit secret keys to the repo"
	if _, err := store.CompileRules(ctx, instruction, nil); err != nil {
		t.Fatalf("CompileRules: %v", err)
	}

	rules, err := store.SearchRules(ctx, "gofmt before commit", 0)
	if err != nil {
		t.Fatalf("SearchRules: %v", err)
	}
	if len(rules) != 2 {
		t.Fatalf("rules = %d", len(rules))
	}
	for index, rule := range rules {
		if rule.URI.IsZero() || rule.Content == "" {
			t.Fatalf("rule = %+v", rule)
		}
		if rule.Probability < 0 || rule.Probability > 1 {
			t.Fatalf("probability = %f", rule.Probability)
		}
		if index > 0 && rules[index-1].Probability < rule.Probability {
			t.Fatalf("rules not sorted by probability: %+v", rules)
		}
	}
	if rules[0].Scenario == "" {
		t.Fatal("scenario should be reported")
	}

	filtered, err := store.SearchRules(ctx, "gofmt before commit", 0.9)
	if err != nil {
		t.Fatalf("SearchRules: %v", err)
	}
	for _, rule := range filtered {
		if rule.Probability < 0.9 {
			t.Fatalf("below-threshold rule returned: %+v", rule)
		}
	}

	none, err := store.SearchRules(ctx, "cooking pasta recipes", 0.99)
	if err != nil {
		t.Fatalf("SearchRules: %v", err)
	}
	if len(none) != 0 {
		t.Fatalf("unrelated task matched rules: %+v", none)
	}
}

func TestRuleHashIsStable(t *testing.T) {
	store := newStore(t, memoryvfs.WithClock(testClock))
	ctx := context.Background()
	instruction := "Always run gofmt before every commit"
	if _, err := store.CompileRules(ctx, instruction, nil); err != nil {
		t.Fatalf("CompileRules: %v", err)
	}
	first, err := store.List(ctx, "mem://user/self/rules")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if _, err := store.CompileRules(ctx, instruction, nil); err != nil {
		t.Fatalf("CompileRules again: %v", err)
	}
	second, err := store.List(ctx, "mem://user/self/rules")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(first) != len(second) || first[0].URI != second[0].URI {
		t.Fatalf("rule uri not stable: %+v vs %+v", first, second)
	}
}

func TestSessionArchiveTimestampOrdering(t *testing.T) {
	clock := func() time.Time { return testNow }
	store := newStore(t, memoryvfs.WithClock(clock))
	ctx := context.Background()
	if err := store.Capture(ctx, "sess-ts", memoryvfs.Message{Role: "user", Content: "first message here"}); err != nil {
		t.Fatalf("Capture: %v", err)
	}
	first, err := store.Commit(ctx, "sess-ts", memoryvfs.CommitOptions{})
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err := store.Capture(ctx, "sess-ts", memoryvfs.Message{Role: "user", Content: "second message here"}); err != nil {
		t.Fatalf("Capture: %v", err)
	}
	second, err := store.Commit(ctx, "sess-ts", memoryvfs.CommitOptions{})
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if first.Archive == second.Archive {
		t.Fatalf("archives collide at %s", first.Archive)
	}
}
