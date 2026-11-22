package memoryvfs_test

import (
	"strings"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig/memoryvfs"
)

func TestKindsLevelsAndTypes(t *testing.T) {
	for _, kind := range []memoryvfs.Kind{memoryvfs.KindMemory, memoryvfs.KindResource, memoryvfs.KindSkill, memoryvfs.KindRule, memoryvfs.KindSession, memoryvfs.KindDir} {
		if !kind.Valid() {
			t.Fatalf("kind %q invalid", kind)
		}
	}
	if memoryvfs.Kind("bogus").Valid() {
		t.Fatal("bogus kind accepted")
	}
	if len(memoryvfs.MemoryTypes()) != 9 {
		t.Fatalf("types = %v", memoryvfs.MemoryTypes())
	}
	for level, want := range map[memoryvfs.Level]string{memoryvfs.Level0: "L0", memoryvfs.Level1: "L1", memoryvfs.Level2: "L2", memoryvfs.LevelAny: "any", memoryvfs.Level(7): "L7"} {
		if got := level.String(); got != want {
			t.Fatalf("Level(%d).String = %q, want %q", level, got, want)
		}
	}
	if !memoryvfs.ModeQuick.Valid() || memoryvfs.Mode("bogus").Valid() {
		t.Fatal("mode validity wrong")
	}
}

func TestURIParsingAndNavigation(t *testing.T) {
	uri, err := memoryvfs.ParseURI("mem://user/self/memories/profile/name")
	if err != nil {
		t.Fatalf("ParseURI: %v", err)
	}
	if uri.Base() != "name" || uri.Parent().Base() != "profile" || len(uri.Segments()) != 5 {
		t.Fatalf("uri = %q segments=%v base=%q parent=%q", uri, uri.Segments(), uri.Base(), uri.Parent())
	}
	if !uri.IsUnder(memoryvfs.RootURI) {
		t.Fatal("uri is not under the root")
	}
	parent := memoryvfs.URI("mem://user/self/memories")
	if !uri.IsUnder(parent) || memoryvfs.URI("mem://user/self/memoriesx").IsUnder(parent) {
		t.Fatalf("IsUnder wrong for %q", uri)
	}
	child, err := parent.Child("profile")
	if err != nil || child != "mem://user/self/memories/profile" {
		t.Fatalf("Child = %q/%v", child, err)
	}
	if _, err := parent.Child("bad/name"); err == nil {
		t.Fatal("multi-segment child accepted")
	}
	if joined, err := memoryvfs.Join("user", "self", "memories"); err != nil || joined != "mem://user/self/memories" {
		t.Fatalf("Join = %q/%v", joined, err)
	}
	for _, bad := range []string{"", "http://x", "mem://a//b", "mem://a/../b", "mem://A", "mem://a?b"} {
		if _, err := memoryvfs.ParseURI(bad); err == nil {
			t.Fatalf("ParseURI(%q) accepted", bad)
		}
	}
	if root, err := memoryvfs.ParseURI("mem://"); err != nil || root != memoryvfs.RootURI {
		t.Fatalf("root = %q/%v", root, err)
	}
	if memoryvfs.URI("").IsZero() != true || uri.IsZero() {
		t.Fatal("IsZero wrong")
	}
}

func TestEntryValidationAndNormalization(t *testing.T) {
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	entry := memoryvfs.Entry{
		URI:     "mem://user/self/memories/preferences/theme",
		Kind:    memoryvfs.KindMemory,
		Type:    "preferences",
		Summary: "  prefers   dark  ",
		Tags:    map[string]string{"k": "v"},
		Links:   []memoryvfs.URI{"mem://user/self/memories/profile/name"},
	}
	if err := memoryvfs.ValidateEntry(entry); err != nil {
		t.Fatalf("ValidateEntry: %v", err)
	}
	normalized := memoryvfs.NormalizeEntry(entry, func() time.Time { return now })
	if normalized.Summary != "prefers dark" || normalized.Title != "theme" || !normalized.Created.Equal(now) || normalized.Tokens == 0 {
		t.Fatalf("normalized = %+v", normalized)
	}
	clone := entry.Clone()
	clone.Tags["k"] = "changed"
	if entry.Tags["k"] != "v" {
		t.Fatal("Clone shares the tags map")
	}
	if memoryvfs.CollapseWhitespace(" a\n b\t c ") != "a b c" {
		t.Fatal("CollapseWhitespace wrong")
	}
	invalid := []memoryvfs.Entry{
		{URI: "mem://x", Kind: "bogus", Summary: "s"},
		{URI: "mem://x", Kind: memoryvfs.KindMemory, Summary: "s"},
		{URI: "mem://x", Kind: memoryvfs.KindMemory, Type: "Bad Type", Summary: "s"},
		{URI: "mem://x", Kind: memoryvfs.KindResource},
		{URI: "mem://x", Kind: memoryvfs.KindMemory, Type: "profile", Summary: "s", Links: []memoryvfs.URI{"bad"}},
	}
	for index, bad := range invalid {
		if err := memoryvfs.ValidateEntry(bad); err == nil {
			t.Fatalf("invalid entry %d accepted", index)
		}
	}
	if err := memoryvfs.ValidateSessionID("sess-1"); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"", "../x", "a/b", strings.Repeat("x", 200)} {
		if err := memoryvfs.ValidateSessionID(bad); err == nil {
			t.Fatalf("session id %q accepted", bad)
		}
	}
}

func TestQueryLimitsAndTokens(t *testing.T) {
	query := memoryvfs.NormalizeQuery(memoryvfs.Query{Text: "  hello  ", Limit: -1, Offset: -2})
	if query.Text != "hello" || query.Mode != memoryvfs.ModeQuick || query.Limit != 10 || query.Offset != 0 {
		t.Fatalf("normalized query = %+v", query)
	}
	if got := memoryvfs.NormalizeQuery(memoryvfs.Query{Limit: 1000}).Limit; got != 100 {
		t.Fatalf("limit cap = %d", got)
	}
	if text, truncated := memoryvfs.TruncateBytes("héllo", 2); !truncated || text == "" || len(text) > 2 {
		t.Fatalf("TruncateBytes = %q/%v", text, truncated)
	}
	if tokens := memoryvfs.EstimateTokens("hello world 世界"); tokens <= 0 {
		t.Fatalf("EstimateTokens = %d", tokens)
	}
	if text, truncated := memoryvfs.TruncateTokens("hello world again and again", 2); !truncated || text == "" {
		t.Fatalf("TruncateTokens = %q/%v", text, truncated)
	}
	entry := memoryvfs.Entry{Summary: "s", Overview: "o", Content: "c"}
	if entry.TokenEstimate(memoryvfs.Level0) == 0 || entry.TokenEstimate(memoryvfs.Level2) == 0 {
		t.Fatal("TokenEstimate zero")
	}
}

func TestOptionsConstructors(t *testing.T) {
	opts := memoryvfs.ApplyOptions(
		memoryvfs.WithMaxEntries(7),
		memoryvfs.WithMaxBytes(9),
		memoryvfs.WithTTL(time.Hour),
		memoryvfs.WithCommitTokens(11),
		memoryvfs.WithRecallTokens(12),
		memoryvfs.WithPageSize(13),
		memoryvfs.WithHybridWeights(0.5, 0.5),
		memoryvfs.WithClock(func() time.Time { return time.Unix(0, 0) }),
	)
	if opts.MaxEntries != 7 || opts.MaxBytes != 9 || opts.TTL != time.Hour || opts.CommitTokens != 11 || opts.RecallTokens != 12 || opts.PageSize != 13 {
		t.Fatalf("opts = %+v", opts)
	}
	if opts.BM25Weight != 0.5 || opts.VectorWeight != 0.5 || opts.Now() != time.Unix(0, 0) {
		t.Fatalf("weights/clock = %+v", opts)
	}
	defaults := memoryvfs.DefaultOptions()
	if defaults.MaxEntries == 0 || defaults.CommitTokens == 0 {
		t.Fatalf("defaults = %+v", defaults)
	}
}
