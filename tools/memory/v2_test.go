package memory

// v2 工具面的用例。用一个行为可预测的 fake memoryvfs.Store 覆盖：每个动作、
// 参数校验、空结果、token 截断、tags 三种写法、错误映射，以及旧 Wiki 适配器的
// 六方法语义（含 `## Log` 与 not-found 映射）。
//
// 这些用例不依赖 internal/memvfs：v2 工具只依赖 memoryvfs.Store 契约。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/internal/memwiki"
	"github.com/zeokeel/zeokeel-rig/memoryvfs"
)

// ---------------------------------------------------------------------------
// fake store
// ---------------------------------------------------------------------------

type fakeStore struct {
	mu        sync.Mutex
	entries   map[memoryvfs.URI]memoryvfs.Entry
	snapshots []memoryvfs.Snapshot
	rules     []memoryvfs.Rule
	stats     memoryvfs.Stats
	errFor    map[string]error
	calls     map[string]int
	queries   []memoryvfs.Query
	commits   []fakeCommit
	restored  string
	now       time.Time
	closed    bool
}

type fakeCommit struct {
	sessionID string
	opts      memoryvfs.CommitOptions
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		entries: make(map[memoryvfs.URI]memoryvfs.Entry),
		errFor:  make(map[string]error),
		calls:   make(map[string]int),
		now:     time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC),
	}
}

func (f *fakeStore) begin(op string) error {
	f.calls[op]++
	if err := f.errFor[op]; err != nil {
		return err
	}
	if f.closed {
		return memoryvfs.ErrClosed
	}
	return nil
}

func (f *fakeStore) Stat(ctx context.Context, uri memoryvfs.URI) (memoryvfs.Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin("Stat"); err != nil {
		return memoryvfs.Entry{}, err
	}
	entry, ok := f.entries[uri]
	if !ok {
		return memoryvfs.Entry{}, fmt.Errorf("%w: %s", memoryvfs.ErrNotFound, uri)
	}
	return entry.Clone(), nil
}

func (f *fakeStore) List(ctx context.Context, uri memoryvfs.URI) ([]memoryvfs.Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin("List"); err != nil {
		return nil, err
	}
	base := len(uri.Segments())
	seen := make(map[memoryvfs.URI]memoryvfs.Entry)
	for _, entry := range f.entries {
		if !entry.URI.IsUnder(uri) || entry.URI == uri {
			continue
		}
		segments := entry.URI.Segments()
		if len(segments) <= base {
			continue
		}
		child := memoryvfs.URI(memoryvfs.Scheme + strings.Join(segments[:base+1], "/"))
		if len(segments) == base+1 {
			seen[child] = entry.Clone()
			continue
		}
		// 中间层没有实体条目时补一个目录项，模拟真实后端的隐式目录。
		if _, ok := seen[child]; !ok {
			seen[child] = memoryvfs.Entry{URI: child, Kind: memoryvfs.KindDir, Updated: f.now}
		}
	}
	out := make([]memoryvfs.Entry, 0, len(seen))
	for _, entry := range seen {
		out = append(out, entry)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].URI < out[j].URI })
	return out, nil
}

func (f *fakeStore) Tree(ctx context.Context, uri memoryvfs.URI, depth int) ([]memoryvfs.Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin("Tree"); err != nil {
		return nil, err
	}
	base := len(uri.Segments())
	var out []memoryvfs.Entry
	for _, entry := range f.entries {
		if !entry.URI.IsUnder(uri) || entry.URI == uri {
			continue
		}
		if depth > 0 && len(entry.URI.Segments())-base > depth {
			continue
		}
		out = append(out, entry.Clone())
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].URI < out[j].URI })
	return out, nil
}

func (f *fakeStore) Read(ctx context.Context, uri memoryvfs.URI, level memoryvfs.Level) (memoryvfs.Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin("Read"); err != nil {
		return memoryvfs.Entry{}, err
	}
	entry, ok := f.entries[uri]
	if !ok {
		return memoryvfs.Entry{}, fmt.Errorf("%w: %s", memoryvfs.ErrNotFound, uri)
	}
	out := entry.Clone()
	switch level {
	case memoryvfs.Level0:
		out.Overview, out.Content = "", ""
	case memoryvfs.Level1:
		out.Content = ""
	}
	return out, nil
}

func (f *fakeStore) Write(ctx context.Context, entry memoryvfs.Entry) (memoryvfs.Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin("Write"); err != nil {
		return memoryvfs.Entry{}, err
	}
	if err := memoryvfs.ValidateEntry(entry); err != nil {
		return memoryvfs.Entry{}, err
	}
	now := f.now
	out := memoryvfs.NormalizeEntry(entry, func() time.Time { return now })
	if existing, ok := f.entries[entry.URI]; ok {
		out.Created = existing.Created
		out.Version = existing.Version + 1
	} else {
		out.Version = 1
	}
	f.entries[entry.URI] = out
	return out.Clone(), nil
}

func (f *fakeStore) Append(ctx context.Context, uri memoryvfs.URI, text string) (memoryvfs.Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin("Append"); err != nil {
		return memoryvfs.Entry{}, err
	}
	entry, ok := f.entries[uri]
	if !ok {
		return memoryvfs.Entry{}, fmt.Errorf("%w: %s", memoryvfs.ErrNotFound, uri)
	}
	if strings.TrimSpace(entry.Content) == "" {
		entry.Content = text
	} else {
		entry.Content += "\n\n" + text
	}
	entry.Updated = f.now
	entry.Version++
	f.entries[uri] = entry
	return entry.Clone(), nil
}

func (f *fakeStore) Move(ctx context.Context, from, to memoryvfs.URI) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin("Move"); err != nil {
		return err
	}
	entry, ok := f.entries[from]
	if !ok {
		return fmt.Errorf("%w: %s", memoryvfs.ErrNotFound, from)
	}
	delete(f.entries, from)
	entry.URI = to
	f.entries[to] = entry
	return nil
}

func (f *fakeStore) Delete(ctx context.Context, uri memoryvfs.URI) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin("Delete"); err != nil {
		return err
	}
	if _, ok := f.entries[uri]; !ok {
		return fmt.Errorf("%w: %s", memoryvfs.ErrNotFound, uri)
	}
	delete(f.entries, uri)
	return nil
}

func (f *fakeStore) Mkdir(ctx context.Context, uri memoryvfs.URI, description string) (memoryvfs.Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin("Mkdir"); err != nil {
		return memoryvfs.Entry{}, err
	}
	entry := memoryvfs.Entry{URI: uri, Kind: memoryvfs.KindDir, Summary: description, Updated: f.now}
	f.entries[uri] = entry
	return entry, nil
}

func (f *fakeStore) Find(ctx context.Context, query memoryvfs.Query) (memoryvfs.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin("Find"); err != nil {
		return memoryvfs.Result{}, err
	}
	return f.searchLocked(query), nil
}

func (f *fakeStore) Search(ctx context.Context, query memoryvfs.Query) (memoryvfs.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin("Search"); err != nil {
		return memoryvfs.Result{}, err
	}
	result := f.searchLocked(query)
	result.Plan = []memoryvfs.TypedQuery{{Text: query.Text}}
	return result, nil
}

// searchLocked 是确定性的子串/过滤检索：足以覆盖工具层的渲染与参数透传。
func (f *fakeStore) searchLocked(query memoryvfs.Query) memoryvfs.Result {
	query = memoryvfs.NormalizeQuery(query)
	f.queries = append(f.queries, query)
	terms := strings.Fields(strings.ToLower(query.Text))
	type scored struct {
		entry memoryvfs.Entry
		level memoryvfs.Level
	}
	var matches []scored
	for _, entry := range f.entries {
		if entry.Kind == memoryvfs.KindDir {
			continue
		}
		if query.Scope != "" && query.Scope != memoryvfs.RootURI && !entry.URI.IsUnder(query.Scope) {
			continue
		}
		if len(query.Kinds) > 0 && !containsKind(query.Kinds, entry.Kind) {
			continue
		}
		if len(query.Types) > 0 && !containsString(query.Types, entry.Type) {
			continue
		}
		tagOK := true
		for key, want := range query.Tags {
			if entry.Tags[key] != want {
				tagOK = false
				break
			}
		}
		if !tagOK {
			continue
		}
		haystack := strings.ToLower(entry.Title + "\n" + entry.Summary + "\n" + entry.Overview + "\n" + entry.Content)
		matched := true
		for _, term := range terms {
			if !strings.Contains(haystack, term) {
				matched = false
				break
			}
		}
		if !matched {
			continue
		}
		level := memoryvfs.Level2
		lowerSummary := strings.ToLower(entry.Summary)
		for _, term := range terms {
			if strings.Contains(lowerSummary, term) {
				level = memoryvfs.Level0
			}
		}
		matches = append(matches, scored{entry: entry.Clone(), level: level})
	}
	sort.SliceStable(matches, func(i, j int) bool { return matches[i].entry.URI < matches[j].entry.URI })
	total := len(matches)
	if query.Offset > 0 {
		if query.Offset >= len(matches) {
			matches = nil
		} else {
			matches = matches[query.Offset:]
		}
	}
	result := memoryvfs.Result{Total: total}
	for index, match := range matches {
		if len(result.Entries) >= query.Limit {
			break
		}
		match.entry.Score = 1 - float64(index)/10
		match.entry.MatchedLevel = match.level
		if !query.IncludeBody {
			match.entry.Content = ""
		}
		cost := memoryvfs.EstimateTokens(match.entry.Summary)
		if query.TokenBudget > 0 && result.Tokens+cost > query.TokenBudget {
			break
		}
		result.Tokens += cost
		result.Entries = append(result.Entries, match.entry)
	}
	return result
}

func (f *fakeStore) Remember(ctx context.Context, entry memoryvfs.Entry) (memoryvfs.Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin("Remember"); err != nil {
		return memoryvfs.Entry{}, err
	}
	if err := memoryvfs.ValidateEntry(entry); err != nil {
		return memoryvfs.Entry{}, err
	}
	if existing, ok := f.entries[entry.URI]; ok {
		merged := existing.Clone()
		if len(entry.Summary) > len(merged.Summary) {
			merged.Summary = entry.Summary
		}
		if strings.TrimSpace(entry.Content) != "" && !strings.Contains(merged.Content, entry.Content) {
			if merged.Content != "" {
				merged.Content += "\n\n"
			}
			merged.Content += entry.Content
		}
		if merged.Tags == nil {
			merged.Tags = make(map[string]string)
		}
		for key, value := range entry.Tags {
			merged.Tags[key] = value
		}
		merged.Updated = f.now
		merged.Version++
		f.entries[entry.URI] = merged
		return merged.Clone(), nil
	}
	out := memoryvfs.NormalizeEntry(entry, func() time.Time { return f.now })
	out.Version = 1
	f.entries[entry.URI] = out
	return out.Clone(), nil
}

func (f *fakeStore) Forget(ctx context.Context, uri memoryvfs.URI) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin("Forget"); err != nil {
		return err
	}
	if _, ok := f.entries[uri]; !ok {
		return fmt.Errorf("%w: %s", memoryvfs.ErrNotFound, uri)
	}
	delete(f.entries, uri)
	return nil
}

func (f *fakeStore) Capture(ctx context.Context, sessionID string, messages ...memoryvfs.Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.begin("Capture")
}

func (f *fakeStore) Pending(ctx context.Context, sessionID string) (memoryvfs.SessionState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin("Pending"); err != nil {
		return memoryvfs.SessionState{}, err
	}
	return memoryvfs.SessionState{ID: sessionID, Pending: 3, Tokens: 120, Updated: f.now}, nil
}

func (f *fakeStore) Commit(ctx context.Context, sessionID string, opts memoryvfs.CommitOptions) (memoryvfs.CommitResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin("Commit"); err != nil {
		return memoryvfs.CommitResult{}, err
	}
	f.commits = append(f.commits, fakeCommit{sessionID: sessionID, opts: opts})
	return memoryvfs.CommitResult{
		Archive: memoryvfs.URI("mem://sessions/" + sessionID + "/20261120"),
		Written: []memoryvfs.URI{"mem://user/self/memories/events/a", "mem://user/self/memories/events/b"},
		Merged:  []memoryvfs.URI{"mem://user/self/memories/events/c"},
		Skipped: 1,
		Tokens:  42,
	}, nil
}

func (f *fakeStore) CompileRules(ctx context.Context, instruction string, sources []memoryvfs.URI) (memoryvfs.Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin("CompileRules"); err != nil {
		return memoryvfs.Entry{}, err
	}
	entry := memoryvfs.Entry{
		URI:     "mem://user/self/rules/compiled",
		Kind:    memoryvfs.KindRule,
		Summary: firstLine(instruction),
		Content: instruction,
	}
	return entry, nil
}

func (f *fakeStore) SearchRules(ctx context.Context, taskContext string, threshold float64) ([]memoryvfs.Rule, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin("SearchRules"); err != nil {
		return nil, err
	}
	var out []memoryvfs.Rule
	for _, rule := range f.rules {
		if rule.Probability >= threshold {
			out = append(out, rule)
		}
	}
	return out, nil
}

func (f *fakeStore) Snapshot(ctx context.Context, label string) (memoryvfs.Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin("Snapshot"); err != nil {
		return memoryvfs.Snapshot{}, err
	}
	snapshot := memoryvfs.Snapshot{ID: fmt.Sprintf("snap-%d", len(f.snapshots)+1), Label: label, Created: f.now, Entries: len(f.entries)}
	f.snapshots = append(f.snapshots, snapshot)
	return snapshot, nil
}

func (f *fakeStore) Snapshots(ctx context.Context) ([]memoryvfs.Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin("Snapshots"); err != nil {
		return nil, err
	}
	out := append([]memoryvfs.Snapshot(nil), f.snapshots...)
	return out, nil
}

func (f *fakeStore) Diff(ctx context.Context, from, to string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin("Diff"); err != nil {
		return "", err
	}
	return "", nil
}

func (f *fakeStore) Restore(ctx context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin("Restore"); err != nil {
		return err
	}
	for _, snapshot := range f.snapshots {
		if snapshot.ID == id {
			f.restored = id
			return nil
		}
	}
	return fmt.Errorf("%w: snapshot %s", memoryvfs.ErrNotFound, id)
}

func (f *fakeStore) Stats(ctx context.Context) (memoryvfs.Stats, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin("Stats"); err != nil {
		return memoryvfs.Stats{}, err
	}
	return f.stats, nil
}

func (f *fakeStore) GC(ctx context.Context, opts memoryvfs.GCOptions) (memoryvfs.GCResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return memoryvfs.GCResult{}, f.begin("GC")
}

func (f *fakeStore) Rebuild(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.begin("Rebuild")
}

func (f *fakeStore) Export(ctx context.Context, scope memoryvfs.URI) (memoryvfs.Pack, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return memoryvfs.Pack{}, f.begin("Export")
}

func (f *fakeStore) Import(ctx context.Context, pack memoryvfs.Pack, opts memoryvfs.ImportOptions) (memoryvfs.ImportResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return memoryvfs.ImportResult{}, f.begin("Import")
}

func (f *fakeStore) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

func containsKind(kinds []memoryvfs.Kind, want memoryvfs.Kind) bool {
	for _, kind := range kinds {
		if kind == want {
			return true
		}
	}
	return false
}

func containsString(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}

// seed 直接往 fake 里放一条已落库条目（绕过工具）。
func seed(t *testing.T, store *fakeStore, uri, summary, content string) {
	t.Helper()
	entry := memoryvfs.Entry{
		URI:     memoryvfs.URI(uri),
		Kind:    memoryvfs.KindMemory,
		Type:    inferType(memoryvfs.URI(uri)),
		Title:   memoryvfs.URI(uri).Base(),
		Summary: summary,
		Content: content,
		Created: store.now,
		Updated: store.now,
		Version: 1,
	}
	if entry.Type == "" {
		entry.Type = memoryvfs.TypePages
	}
	store.mu.Lock()
	store.entries[entry.URI] = entry
	store.mu.Unlock()
}

// invokeV2 调一次 v2 工具并断言没有 go error。
func invokeV2(t *testing.T, tool rig.Tool, args string) rig.ToolResult {
	t.Helper()
	result, err := tool.Invoke(context.Background(), rig.ToolCall{Name: tool.Name(), Arguments: args})
	if err != nil {
		t.Fatalf("Invoke 返回 go error: %v（args=%s）", err, args)
	}
	return result
}

// newV2Tool 构造一个绑定 fake store 的工具。
func newV2Tool(t *testing.T, opts ...Option) (rig.Tool, *fakeStore) {
	t.Helper()
	store := newFakeStore()
	return ToolsWithStore(store, opts...)[0], store
}

// ---------------------------------------------------------------------------
// metadata / schema
// ---------------------------------------------------------------------------

// TestV2ToolMetadataAndSchemaGuard 断言工具名、动作枚举与 schema 体积上限。
func TestV2ToolMetadataAndSchemaGuard(t *testing.T) {
	tool, _ := newV2Tool(t)
	if tool.Name() != ToolName || ToolName != "memory" {
		t.Fatalf("Name = %q", tool.Name())
	}
	description := tool.Description()
	for _, want := range []string{"mem://", "remember", "summaries"} {
		if !strings.Contains(description, want) {
			t.Fatalf("Description 缺少 %q:\n%s", want, description)
		}
	}
	schema := tool.Schema()
	if schema["type"] != "object" {
		t.Fatalf("schema.type = %v", schema["type"])
	}
	if schema["additionalProperties"] != false {
		t.Fatalf("additionalProperties 必须为 false")
	}
	if got, _ := schema["required"].([]string); len(got) != 1 || got[0] != "action" {
		t.Fatalf("required = %v", schema["required"])
	}
	properties, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("properties 类型 = %T", schema["properties"])
	}
	wantProps := []string{"action", "uri", "to", "name", "title", "summary", "content", "text", "query",
		"kind", "type", "tags", "level", "limit", "offset", "token_budget", "mode", "snapshot", "session_id"}
	if len(properties) != len(wantProps) {
		t.Fatalf("属性数 = %d, want %d", len(properties), len(wantProps))
	}
	for _, name := range wantProps {
		spec, ok := properties[name].(map[string]any)
		if !ok {
			t.Fatalf("缺少属性 %q", name)
		}
		if _, ok := spec["description"].(string); !ok {
			t.Fatalf("%q 缺少 description", name)
		}
	}
	actionSpec := properties["action"].(map[string]any)
	enum, _ := actionSpec["enum"].([]string)
	if len(enum) != 20 {
		t.Fatalf("action 枚举 = %v", actionSpec["enum"])
	}
	for _, want := range v2Actions {
		if !containsString(enum, want) {
			t.Fatalf("action 枚举缺少 %q", want)
		}
	}
	encoded, err := json.Marshal(struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		Parameters  map[string]any `json:"parameters"`
	}{Name: tool.Name(), Description: tool.Description(), Parameters: schema})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	const limit = 2400
	if len(encoded) > limit {
		t.Fatalf("v2 memory schema = %d 字节，超过上限 %d；把细节移到运行时错误", len(encoded), limit)
	}
	t.Logf("v2 memory schema = %d 字节 ≈ %d tokens", len(encoded), len(encoded)/4)
}

// ---------------------------------------------------------------------------
// legacy actions
// ---------------------------------------------------------------------------

// TestV2LegacyActionFlow 覆盖 index/read/write/append/search/delete 的旧语义。
func TestV2LegacyActionFlow(t *testing.T) {
	tool, store := newV2Tool(t)

	if result := invokeV2(t, tool, `{"action":"index"}`); !strings.Contains(result.Content, "no memories yet") {
		t.Fatalf("空库 index = %q", result.Content)
	}

	result := invokeV2(t, tool, `{"action":"write","name":"build-flaky","summary":"偶发失败的构建测试","content":"SECRET-BODY","title":"Build flaky"}`)
	if result.IsError || !strings.Contains(result.Content, "wrote mem://user/self/memories/pages/build-flaky") {
		t.Fatalf("write = %q", result.Content)
	}
	entry, ok := store.entries["mem://user/self/memories/pages/build-flaky"]
	if !ok || entry.Type != memoryvfs.TypePages {
		t.Fatalf("落库条目 = %+v", entry)
	}

	result = invokeV2(t, tool, `{"action":"index"}`)
	if result.IsError || !strings.Contains(result.Content, "build-flaky — 偶发失败的构建测试") {
		t.Fatalf("index = %q", result.Content)
	}
	if strings.Contains(result.Content, "SECRET-BODY") {
		t.Fatalf("index 泄露正文: %q", result.Content)
	}

	result = invokeV2(t, tool, `{"action":"read","name":"build-flaky"}`)
	if result.IsError || !strings.Contains(result.Content, "SECRET-BODY") || !strings.Contains(result.Content, "[L2]") {
		t.Fatalf("read = %q", result.Content)
	}
	result = invokeV2(t, tool, `{"action":"read","name":"build-flaky","level":"L0"}`)
	if result.IsError || strings.Contains(result.Content, "SECRET-BODY") || !strings.Contains(result.Content, "[L0]") {
		t.Fatalf("read L0 = %q", result.Content)
	}

	result = invokeV2(t, tool, `{"action":"append","name":"build-flaky","text":"retry twice"}`)
	if result.IsError || !strings.Contains(result.Content, "appended to") {
		t.Fatalf("append = %q", result.Content)
	}
	result = invokeV2(t, tool, `{"action":"read","name":"build-flaky"}`)
	if !strings.Contains(result.Content, "retry twice") {
		t.Fatalf("append 没写进正文: %q", result.Content)
	}

	result = invokeV2(t, tool, `{"action":"search","query":"构建"}`)
	if result.IsError || !strings.Contains(result.Content, "build-flaky") {
		t.Fatalf("search = %q", result.Content)
	}
	result = invokeV2(t, tool, `{"action":"search","query":"nothing-matches"}`)
	if result.IsError || !strings.Contains(result.Content, "no pages matched") {
		t.Fatalf("空 search = %q", result.Content)
	}

	result = invokeV2(t, tool, `{"action":"delete","name":"build-flaky"}`)
	if result.IsError || !strings.Contains(result.Content, "deleted") {
		t.Fatalf("delete = %q", result.Content)
	}
	result = invokeV2(t, tool, `{"action":"read","name":"build-flaky"}`)
	if !result.IsError || !strings.Contains(result.Content, "not found") {
		t.Fatalf("删除后 read = %q", result.Content)
	}
}

// TestV2WriteWithExplicitURIAndType 断言 uri/type/tags 显式参数。
func TestV2WriteWithExplicitURIAndType(t *testing.T) {
	tool, store := newV2Tool(t)
	result := invokeV2(t, tool, `{"action":"write","uri":"mem://user/self/memories/preferences/editor","summary":"prefers vim","type":"preferences","tags":{"owner":"me"}}`)
	if result.IsError {
		t.Fatalf("write = %q", result.Content)
	}
	entry, ok := store.entries["mem://user/self/memories/preferences/editor"]
	if !ok || entry.Type != memoryvfs.TypePreferences || entry.Tags["owner"] != "me" {
		t.Fatalf("条目 = %+v", entry)
	}
	// 路径推断 type。
	result = invokeV2(t, tool, `{"action":"write","uri":"mem://user/self/memories/entities/rig","summary":"the rig project"}`)
	if result.IsError {
		t.Fatalf("write inferred type = %q", result.Content)
	}
	if entry := store.entries["mem://user/self/memories/entities/rig"]; entry.Type != memoryvfs.TypeEntities {
		t.Fatalf("推断 type = %q", entry.Type)
	}
}

// TestV2ListAndTree 断言 ls/tree 的逐行输出与缩进。
func TestV2ListAndTree(t *testing.T) {
	tool, store := newV2Tool(t)
	seed(t, store, "mem://user/self/memories/preferences/editor", "prefers vim", "body")
	seed(t, store, "mem://user/self/memories/pages/notes", "scratch notes", "body")
	seed(t, store, "mem://user/self/rules/release", "release rule", "body")

	result := invokeV2(t, tool, `{"action":"ls","uri":"mem://user/self/memories"}`)
	if result.IsError {
		t.Fatalf("ls = %q", result.Content)
	}
	lines := strings.Split(result.Content, "\n")
	if len(lines) != 2 {
		t.Fatalf("ls 行数 = %d: %q", len(lines), result.Content)
	}
	for _, line := range lines {
		if !strings.Contains(line, "mem://") || !strings.Contains(line, " — ") {
			t.Fatalf("ls 行不是 `uri — summary`: %q", line)
		}
	}
	result = invokeV2(t, tool, `{"action":"ls","uri":"mem://user/self/memories","limit":1}`)
	if !strings.Contains(result.Content, "more entries") {
		t.Fatalf("ls limit = %q", result.Content)
	}

	result = invokeV2(t, tool, `{"action":"tree","uri":"mem://user/self"}`)
	if result.IsError {
		t.Fatalf("tree = %q", result.Content)
	}
	checked := 0
	for _, line := range strings.Split(result.Content, "\n") {
		switch {
		case strings.HasSuffix(line, "memories/preferences/editor — prefers vim"):
			if !strings.HasPrefix(line, "    ") {
				t.Fatalf("深度 3 的条目缩进 = %q", line)
			}
			checked++
		case strings.HasSuffix(line, "rules/release — release rule"):
			if !strings.HasPrefix(line, "  ") || strings.HasPrefix(line, "    ") {
				t.Fatalf("深度 2 的条目缩进 = %q", line)
			}
			checked++
		}
	}
	if checked != 2 {
		t.Fatalf("tree 缺少条目（checked=%d）: %q", checked, result.Content)
	}
	result = invokeV2(t, tool, `{"action":"ls","uri":"mem://user/self/nothing"}`)
	if !strings.Contains(result.Content, "empty:") {
		t.Fatalf("空 ls = %q", result.Content)
	}
	result = invokeV2(t, tool, `{"action":"tree","uri":"mem://user/self/nothing"}`)
	if !strings.Contains(result.Content, "empty:") {
		t.Fatalf("空 tree = %q", result.Content)
	}
}

// ---------------------------------------------------------------------------
// find
// ---------------------------------------------------------------------------

// TestV2FindRendersScoreLevelAndTotals 断言 find 的行格式与尾部统计。
func TestV2FindRendersScoreLevelAndTotals(t *testing.T) {
	tool, store := newV2Tool(t)
	seed(t, store, "mem://user/self/memories/preferences/editor", "prefers vim for editing", "body")
	seed(t, store, "mem://user/self/memories/entities/rig", "the rig project", "body")

	result := invokeV2(t, tool, `{"action":"find","query":"vim","uri":"mem://user/self/memories"}`)
	if result.IsError {
		t.Fatalf("find = %q", result.Content)
	}
	if !strings.Contains(result.Content, "mem://user/self/memories/preferences/editor [L0] (1.00) — prefers vim for editing") {
		t.Fatalf("find 行 = %q", result.Content)
	}
	if !strings.Contains(result.Content, "total 1") || !strings.Contains(result.Content, "tokens") {
		t.Fatalf("find 尾部 = %q", result.Content)
	}
	query := store.queries[len(store.queries)-1]
	if query.Scope != "mem://user/self/memories" || query.Limit != 10 {
		t.Fatalf("查询 = %+v", query)
	}

	// mode=thinking 走 Search（允许意图分析与多查询融合）。
	result = invokeV2(t, tool, `{"action":"find","query":"vim","mode":"thinking"}`)
	if result.IsError {
		t.Fatalf("find thinking = %q", result.Content)
	}
	if store.calls["Search"] != 1 || store.calls["Find"] != 1 {
		t.Fatalf("calls = %+v", store.calls)
	}

	// 过滤参数透传。
	result = invokeV2(t, tool, `{"action":"find","query":"","kind":"memory","type":"preferences","level":"L1","tags":"owner=me","limit":5,"offset":1}`)
	if result.IsError {
		t.Fatalf("find filters = %q", result.Content)
	}
	query = store.queries[len(store.queries)-1]
	if len(query.Kinds) != 1 || query.Kinds[0] != memoryvfs.KindMemory {
		t.Fatalf("Kinds = %v", query.Kinds)
	}
	if len(query.Types) != 1 || query.Types[0] != "preferences" {
		t.Fatalf("Types = %v", query.Types)
	}
	if len(query.Levels) != 1 || query.Levels[0] != memoryvfs.Level1 {
		t.Fatalf("Levels = %v", query.Levels)
	}
	if query.Tags["owner"] != "me" || query.Limit != 5 || query.Offset != 1 {
		t.Fatalf("Query = %+v", query)
	}

	result = invokeV2(t, tool, `{"action":"find","query":"no-such-thing"}`)
	if result.IsError || !strings.Contains(result.Content, "no matches") {
		t.Fatalf("空 find = %q", result.Content)
	}
}

// TestV2FindTokenBudgetTruncates 断言 token_budget 会截断输出并提示。
func TestV2FindTokenBudgetTruncates(t *testing.T) {
	tool, store := newV2Tool(t)
	for i := 0; i < 8; i++ {
		seed(t, store, fmt.Sprintf("mem://user/self/memories/pages/note-%d", i),
			"shared keyword entry number "+strconv.Itoa(i)+" with a deliberately long summary", "body")
	}
	result := invokeV2(t, tool, `{"action":"find","query":"shared","token_budget":40,"limit":8}`)
	if result.IsError {
		t.Fatalf("find = %q", result.Content)
	}
	if !strings.Contains(result.Content, "truncated at token_budget 40") {
		t.Fatalf("token 截断缺失: %q", result.Content)
	}
	if strings.Count(result.Content, "\n") >= 8 {
		t.Fatalf("截断后仍有太多行: %q", result.Content)
	}
}

// ---------------------------------------------------------------------------
// remember / forget / tag
// ---------------------------------------------------------------------------

// TestV2RememberWritesAndMerges 断言 remember 的新建/合并文案与 URI 派生。
func TestV2RememberWritesAndMerges(t *testing.T) {
	tool, store := newV2Tool(t)

	result := invokeV2(t, tool, `{"action":"remember","summary":"prefers short commits","content":"keep commits small","type":"preferences","tags":"owner=me"}`)
	if result.IsError || !strings.Contains(result.Content, "written mem://user/self/memories/preferences/prefers-short-commits") {
		t.Fatalf("remember = %q", result.Content)
	}
	// 同一摘要 + 同一类型派生同一个 URI → merged。
	result = invokeV2(t, tool, `{"action":"remember","summary":"prefers short commits","content":"keep commits small","type":"preferences"}`)
	if result.IsError || !strings.Contains(result.Content, "merged into") {
		t.Fatalf("第二次 remember = %q", result.Content)
	}

	// 中文摘要没有 ASCII slug，退回稳定哈希：两次调用落到同一 URI。
	result = invokeV2(t, tool, `{"action":"remember","summary":"用户喜欢简洁的提交信息"}`)
	if result.IsError || !strings.Contains(result.Content, "mem://user/self/memories/events/mem-") {
		t.Fatalf("中文 remember = %q", result.Content)
	}
	result = invokeV2(t, tool, `{"action":"remember","summary":"用户喜欢简洁的提交信息"}`)
	if result.IsError || !strings.Contains(result.Content, "merged into") {
		t.Fatalf("中文 remember 第二次 = %q", result.Content)
	}

	// 没有 summary 时从正文首行派生。
	result = invokeV2(t, tool, `{"action":"remember","uri":"mem://user/self/memories/events/release","content":"shipped on Friday\nmore detail"}`)
	if result.IsError || !strings.Contains(result.Content, "written") {
		t.Fatalf("正文派生 summary = %q", result.Content)
	}
	entry := store.entries["mem://user/self/memories/events/release"]
	if entry.Summary != "shipped on Friday" {
		t.Fatalf("派生 summary = %q", entry.Summary)
	}

	// 资源类走自己的根。
	result = invokeV2(t, tool, `{"action":"remember","kind":"resource","summary":"API reference copy","content":"..."}`)
	if result.IsError || !strings.Contains(result.Content, "mem://resources/") {
		t.Fatalf("resource remember = %q", result.Content)
	}
}

// TestV2RememberErrors 断言 remember 的必填参数。
func TestV2RememberErrors(t *testing.T) {
	tool, _ := newV2Tool(t)
	result := invokeV2(t, tool, `{"action":"remember"}`)
	if !result.IsError || !strings.Contains(result.Content, "summary or content is required") {
		t.Fatalf("空 remember = %q", result.Content)
	}
	result = invokeV2(t, tool, `{"action":"remember","summary":"x","kind":"nope"}`)
	if !result.IsError || !strings.Contains(result.Content, "invalid kind") {
		t.Fatalf("非法 kind = %q", result.Content)
	}
	result = invokeV2(t, tool, `{"action":"remember","summary":"x","type":"Bad Type"}`)
	if !result.IsError || !strings.Contains(result.Content, "invalid type") {
		t.Fatalf("非法 type = %q", result.Content)
	}
}

// TestV2ForgetAndTag 断言 forget 与 tags 的 set/append/increment 语义。
func TestV2ForgetAndTag(t *testing.T) {
	tool, store := newV2Tool(t)
	seed(t, store, "mem://user/self/memories/pages/notes", "scratch notes", "body")

	result := invokeV2(t, tool, `{"action":"tag","uri":"mem://user/self/memories/pages/notes","tags":"priority=high,owner=team"}`)
	if result.IsError || !strings.Contains(result.Content, "owner=team, priority=high") {
		t.Fatalf("tag set = %q", result.Content)
	}
	result = invokeV2(t, tool, `{"action":"tag","uri":"mem://user/self/memories/pages/notes","tags":"count=1"}`)
	if result.IsError {
		t.Fatalf("tag count = %q", result.Content)
	}
	result = invokeV2(t, tool, `{"action":"tag","uri":"mem://user/self/memories/pages/notes","tags":"count+=2"}`)
	if result.IsError || !strings.Contains(result.Content, "count=3") {
		t.Fatalf("tag increment = %q", result.Content)
	}
	result = invokeV2(t, tool, `{"action":"tag","uri":"mem://user/self/memories/pages/notes","tags":"count-=1"}`)
	if result.IsError || !strings.Contains(result.Content, "count=2") {
		t.Fatalf("tag decrement = %q", result.Content)
	}
	// 对象形式：值前缀与键尾两种增减写法都接受。
	result = invokeV2(t, tool, `{"action":"tag","uri":"mem://user/self/memories/pages/notes","tags":{"count":"+=1","reads+":"1"}}`)
	if result.IsError || !strings.Contains(result.Content, "count=3") || !strings.Contains(result.Content, "reads=1") {
		t.Fatalf("tag 对象形式 = %q", result.Content)
	}
	// 数字字面量转字符串。
	result = invokeV2(t, tool, `{"action":"tag","uri":"mem://user/self/memories/pages/notes","tags":{"stars":5}}`)
	if result.IsError || !strings.Contains(result.Content, "stars=5") {
		t.Fatalf("tag 数字 = %q", result.Content)
	}

	// 递增非数字 → 可执行错误。
	result = invokeV2(t, tool, `{"action":"tag","uri":"mem://user/self/memories/pages/notes","tags":"owner+=1"}`)
	if !result.IsError || !strings.Contains(result.Content, "not a number") {
		t.Fatalf("tag 非数字递增 = %q", result.Content)
	}
	result = invokeV2(t, tool, `{"action":"tag","uri":"mem://user/self/memories/pages/notes"}`)
	if !result.IsError || !strings.Contains(result.Content, "tags is required") {
		t.Fatalf("tag 缺 tags = %q", result.Content)
	}
	result = invokeV2(t, tool, `{"action":"tag","uri":"mem://user/self/memories/pages/missing","tags":"a=1"}`)
	if !result.IsError || !strings.Contains(result.Content, "not found") {
		t.Fatalf("tag 缺条目 = %q", result.Content)
	}

	result = invokeV2(t, tool, `{"action":"forget","name":"notes"}`)
	if result.IsError || !strings.Contains(result.Content, "forgot") {
		t.Fatalf("forget = %q", result.Content)
	}
	result = invokeV2(t, tool, `{"action":"forget","name":"notes"}`)
	if !result.IsError || !strings.Contains(result.Content, "not found") {
		t.Fatalf("重复 forget = %q", result.Content)
	}
}

// ---------------------------------------------------------------------------
// commit / rules
// ---------------------------------------------------------------------------

// TestV2Commit 断言 commit 的默认/显式 session 与 WorkingMemory。
func TestV2Commit(t *testing.T) {
	tool, store := newV2Tool(t)
	result := invokeV2(t, tool, `{"action":"commit"}`)
	if result.IsError || !strings.Contains(result.Content, `committed session "default"`) ||
		!strings.Contains(result.Content, "wrote 2") || !strings.Contains(result.Content, "merged 1") {
		t.Fatalf("commit = %q", result.Content)
	}
	if !store.commits[0].opts.Extract || store.commits[0].opts.WorkingMemory {
		t.Fatalf("CommitOptions = %+v", store.commits[0].opts)
	}
	result = invokeV2(t, tool, `{"action":"commit","session_id":"sess-42","mode":"working"}`)
	if result.IsError || !strings.Contains(result.Content, `committed session "sess-42"`) {
		t.Fatalf("commit session = %q", result.Content)
	}
	if store.commits[1].sessionID != "sess-42" || !store.commits[1].opts.WorkingMemory {
		t.Fatalf("第二次 CommitOptions = %+v", store.commits[1])
	}
	store.errFor["Commit"] = memoryvfs.ErrQuotaExceeded
	result = invokeV2(t, tool, `{"action":"commit"}`)
	if !result.IsError || !strings.Contains(result.Content, "quota") {
		t.Fatalf("commit 错误映射 = %q", result.Content)
	}
}

// TestV2RulesCompileAndSearch 断言 rules 的两条路径。
func TestV2RulesCompileAndSearch(t *testing.T) {
	tool, store := newV2Tool(t)
	result := invokeV2(t, tool, `{"action":"rules","text":"# Rules\n- must run tests\n- 禁止跳过检查"}`)
	if result.IsError || !strings.Contains(result.Content, "compiled rule mem://user/self/rules/compiled") {
		t.Fatalf("rules compile = %q", result.Content)
	}

	store.rules = []memoryvfs.Rule{
		{URI: "mem://user/self/rules/b", Content: "run tests", Probability: 0.5},
		{URI: "mem://user/self/rules/a", Content: "must never skip tests", Probability: 0.9},
	}
	result = invokeV2(t, tool, `{"action":"rules","query":"tests","mode":"search"}`)
	if result.IsError || !strings.Contains(result.Content, "mem://user/self/rules/a (0.90)") {
		t.Fatalf("rules search = %q", result.Content)
	}
	lines := strings.Split(result.Content, "\n")
	if len(lines) < 2 || !strings.HasPrefix(lines[0], "mem://user/self/rules/a") {
		t.Fatalf("rules 未按概率排序: %q", result.Content)
	}

	result = invokeV2(t, tool, `{"action":"rules","query":"tests"}`)
	if !result.IsError || !strings.Contains(result.Content, `mode: "search"`) {
		t.Fatalf("rules 缺 mode = %q", result.Content)
	}
	result = invokeV2(t, tool, `{"action":"rules"}`)
	if !result.IsError || !strings.Contains(result.Content, "needs text") {
		t.Fatalf("rules 缺参数 = %q", result.Content)
	}
	store.rules = nil
	result = invokeV2(t, tool, `{"action":"rules","query":"tests","mode":"search"}`)
	if result.IsError || !strings.Contains(result.Content, "no rules matched") {
		t.Fatalf("空 rules = %q", result.Content)
	}
}

// ---------------------------------------------------------------------------
// stats / history / restore
// ---------------------------------------------------------------------------

// TestV2Stats 断言统计输出包含分类型计数与固定字段。
func TestV2Stats(t *testing.T) {
	tool, store := newV2Tool(t)
	result := invokeV2(t, tool, `{"action":"stats"}`)
	if result.IsError || !strings.Contains(result.Content, "entries 0") {
		t.Fatalf("空 stats = %q", result.Content)
	}
	store.stats = memoryvfs.Stats{
		Entries: 5, Dirs: 2, Bytes: 1234, Tokens: 567,
		ByKind:   map[memoryvfs.Kind]int{memoryvfs.KindMemory: 4, memoryvfs.KindRule: 1},
		ByType:   map[string]int{"profile": 1, "preferences": 3},
		Sessions: 2, Snapshots: 1, Vectors: 0,
		Oldest: store.now.Add(-48 * time.Hour), Newest: store.now,
	}
	result = invokeV2(t, tool, `{"action":"stats"}`)
	if result.IsError {
		t.Fatalf("stats = %q", result.Content)
	}
	for _, want := range []string{"entries 5", "by kind: memory=4, rule=1", "by type: preferences=3, profile=1", "sessions 2", "oldest"} {
		if !strings.Contains(result.Content, want) {
			t.Fatalf("stats 缺少 %q:\n%s", want, result.Content)
		}
	}
}

// TestV2HistoryAndRestore 断言快照列表、limit 与恢复。
func TestV2HistoryAndRestore(t *testing.T) {
	tool, store := newV2Tool(t)
	result := invokeV2(t, tool, `{"action":"history"}`)
	if result.IsError || !strings.Contains(result.Content, "no snapshots yet") {
		t.Fatalf("空 history = %q", result.Content)
	}
	store.snapshots = []memoryvfs.Snapshot{
		{ID: "s1", Label: "before merge", Created: store.now.Add(-time.Hour), Entries: 2, Bytes: 100},
		{ID: "s2", Label: "", Created: store.now, Entries: 3, Bytes: 200},
		{ID: "s3", Label: "third", Created: store.now.Add(-2 * time.Hour), Entries: 1, Bytes: 50},
	}
	result = invokeV2(t, tool, `{"action":"history","limit":2}`)
	if result.IsError {
		t.Fatalf("history = %q", result.Content)
	}
	lines := strings.Split(result.Content, "\n")
	if !strings.HasPrefix(lines[0], "s2 — snapshot") || !strings.HasPrefix(lines[1], "s1 — before merge") {
		t.Fatalf("history 排序/渲染 = %q", result.Content)
	}
	if !strings.Contains(result.Content, "more snapshots") {
		t.Fatalf("history limit 提示缺失: %q", result.Content)
	}

	result = invokeV2(t, tool, `{"action":"restore","snapshot":"s1"}`)
	if result.IsError || !strings.Contains(result.Content, "restored snapshot s1") || store.restored != "s1" {
		t.Fatalf("restore = %q (restored=%q)", result.Content, store.restored)
	}
	result = invokeV2(t, tool, `{"action":"restore","snapshot":"missing"}`)
	if !result.IsError || !strings.Contains(result.Content, `no snapshot "missing"`) {
		t.Fatalf("restore missing = %q", result.Content)
	}
	result = invokeV2(t, tool, `{"action":"restore"}`)
	if !result.IsError || !strings.Contains(result.Content, "snapshot is required") {
		t.Fatalf("restore 缺参 = %q", result.Content)
	}
}

// ---------------------------------------------------------------------------
// 参数校验与错误映射
// ---------------------------------------------------------------------------

// TestV2ParameterValidation 断言参数错误都是可执行的工具结果。
func TestV2ParameterValidation(t *testing.T) {
	tool, _ := newV2Tool(t)
	cases := []struct {
		args string
		want string
	}{
		{`{}`, "action is required"},
		{`{"action":"nope"}`, `unknown action "nope"`},
		{`{"action":"read"}`, "uri or name is required"},
		{`{"action":"read","name":"Bad Name"}`, "a-z, 0-9"},
		{`{"action":"read","uri":"http://x"}`, "invalid uri"},
		{`{"action":"write","name":"x"}`, "summary is required"},
		{`{"action":"append","name":"x"}`, "text is required"},
		{`{"action":"search"}`, "query is required"},
		{`{"action":"read","name":"x","level":"L9"}`, "unknown level"},
		{`{"action":"find","mode":"search"}`, "only for action: rules"},
		{`{"action":"find","mode":"bogus"}`, "unknown mode"},
		{`{"action":"find","kind":"bogus"}`, "invalid kind"},
		{`{"action":"find","type":"Bad Type"}`, "invalid type"},
		{`{"action":"write","name":"x","summary":"s","tags":"broken"}`, "needs a value"},
		{`{"action":"write","name":"x","summary":"s","tags":"k+=1"}`, "only supported by action: tag"},
		{`{"action":"write","name":"x","summary":"s","kind":"bogus"}`, "invalid kind"},
	}
	for _, tc := range cases {
		result := invokeV2(t, tool, tc.args)
		if !result.IsError {
			t.Fatalf("%s 应当报错，得到 %q", tc.args, result.Content)
		}
		if !strings.Contains(result.Content, tc.want) {
			t.Fatalf("%s 错误 = %q，want 包含 %q", tc.args, result.Content, tc.want)
		}
	}
}

// TestV2DecodeContextAndNilStore 断言解码错误/取消是 go error，nil store 是工具结果。
func TestV2DecodeContextAndNilStore(t *testing.T) {
	tool, _ := newV2Tool(t)
	if _, err := tool.Invoke(context.Background(), rig.ToolCall{Name: "memory", Arguments: "{bad"}); err == nil {
		t.Fatalf("非法 JSON 应当返回 go error")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := tool.Invoke(ctx, rig.ToolCall{Name: "memory", Arguments: `{"action":"index"}`}); !errors.Is(err, context.Canceled) {
		t.Fatalf("取消 err = %v", err)
	}
	nilTool := ToolsWithStore(nil)[0]
	result := invokeV2(t, nilTool, `{"action":"index"}`)
	if !result.IsError || !strings.Contains(result.Content, "no memory store is configured") {
		t.Fatalf("nil store = %q", result.Content)
	}
}

// TestV2StoreErrorMapping 断言存储哨兵错误都被翻译成可执行文案。
func TestV2StoreErrorMapping(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{memoryvfs.ErrInvalidURI, "invalid uri"},
		{memoryvfs.ErrInvalidEntry, "entry rejected"},
		{memoryvfs.ErrSummaryRequired, "summary is required"},
		{memoryvfs.ErrExists, "already exists"},
		{memoryvfs.ErrConflict, "concurrent update conflict"},
		{memoryvfs.ErrQuotaExceeded, "quota exceeded"},
		{memoryvfs.ErrUnsupported, "does not support"},
		{memoryvfs.ErrClosed, "store is closed"},
		{memoryvfs.ErrCorrupt, "corrupt"},
		{errors.New("disk exploded"), "store error"},
	}
	for _, tc := range cases {
		tool, store := newV2Tool(t)
		store.errFor["Tree"] = tc.err
		result := invokeV2(t, tool, `{"action":"index"}`)
		if !result.IsError || !strings.Contains(result.Content, tc.want) {
			t.Fatalf("%v → %q，want 包含 %q", tc.err, result.Content, tc.want)
		}
	}
	// 空命名空间不是错误：index 应给"还没有记忆"的可执行提示，而不是报错。
	tool, store := newV2Tool(t)
	store.errFor["Tree"] = memoryvfs.ErrNotFound
	result := invokeV2(t, tool, `{"action":"index"}`)
	if result.IsError || !strings.Contains(result.Content, "no memories yet") {
		t.Fatalf("not-found index = %q", result.Content)
	}
}

// TestV2OutputLimits 断言 maxRead / maxOutput 的上限可覆盖。
func TestV2OutputLimits(t *testing.T) {
	tool, store := newV2Tool(t, WithMaxReadBytes(16), WithMaxOutputBytes(80))
	seed(t, store, "mem://user/self/memories/pages/big", strings.Repeat("summary ", 20), strings.Repeat("body ", 100))
	result := invokeV2(t, tool, `{"action":"read","name":"big"}`)
	if !strings.Contains(result.Content, "[truncated") {
		t.Fatalf("read 未截断: %q", result.Content)
	}
	result = invokeV2(t, tool, `{"action":"index"}`)
	if !strings.Contains(result.Content, "[truncated") {
		t.Fatalf("index 未截断: %q", result.Content)
	}
}

// ---------------------------------------------------------------------------
// Wiki 适配器
// ---------------------------------------------------------------------------

// TestWikiFromStoreV2IndexReadWrite 覆盖适配器的 Index/Read/Write 语义。
func TestWikiFromStoreV2IndexReadWrite(t *testing.T) {
	store := newFakeStore()
	wiki := WikiFromStoreV2(store)
	ctx := context.Background()

	if err := wiki.Write(ctx, Page{Name: "build-flaky", Summary: "偶发失败的构建测试", Content: "FIRST-BODY"}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	// 覆盖写会在正文里留 "(was N bytes)"。
	if err := wiki.Write(ctx, Page{Name: "build-flaky", Summary: "更新后的摘要", Content: "SECOND-BODY"}); err != nil {
		t.Fatalf("覆盖写: %v", err)
	}
	page, err := wiki.Read(ctx, "build-flaky")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !strings.Contains(page.Content, "SECOND-BODY") || !strings.Contains(page.Content, "(was 10 bytes)") {
		t.Fatalf("覆盖写正文 = %q", page.Content)
	}
	if page.Summary != "更新后的摘要" || page.Title != "build-flaky" {
		t.Fatalf("page = %+v", page)
	}

	metas, err := wiki.Index(ctx)
	if err != nil {
		t.Fatalf("Index: %v", err)
	}
	if len(metas) != 1 || metas[0].Name != "build-flaky" || metas[0].Summary != "更新后的摘要" {
		t.Fatalf("Index = %+v", metas)
	}
	if metas[0].Bytes == 0 {
		t.Fatalf("Index Bytes 应来自正文: %+v", metas[0])
	}

	if _, err := wiki.Read(ctx, "missing"); !errors.Is(err, memwiki.ErrNotFound) {
		t.Fatalf("缺失页 err = %v", err)
	}
	if err := wiki.Write(ctx, Page{Name: "x", Summary: "  "}); !errors.Is(err, memwiki.ErrSummaryRequired) {
		t.Fatalf("缺摘要 err = %v", err)
	}
	if err := wiki.Write(ctx, Page{Name: "Bad Name", Summary: "s"}); !errors.Is(err, memwiki.ErrInvalidName) {
		t.Fatalf("非法页名 err = %v", err)
	}
}

// TestWikiFromStoreV2AppendAndDelete 覆盖 Append 的 Log 语义与 Delete。
func TestWikiFromStoreV2AppendAndDelete(t *testing.T) {
	store := newFakeStore()
	wiki := WikiFromStoreV2(store)
	ctx := context.Background()

	if err := wiki.Append(ctx, "missing", "line"); !errors.Is(err, memwiki.ErrNotFound) {
		t.Fatalf("缺失页 append err = %v", err)
	}
	if err := wiki.Write(ctx, Page{Name: "notes", Summary: "scratch", Content: "body"}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := wiki.Append(ctx, "notes", "first line"); err != nil {
		t.Fatalf("Append: %v", err)
	}
	page, err := wiki.Read(ctx, "notes")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !strings.Contains(page.Content, memwiki.LogHeading) ||
		!strings.Contains(page.Content, "- "+time.Now().UTC().Format("2006-01-02")+" first line") {
		t.Fatalf("append 正文 = %q", page.Content)
	}
	if err := wiki.Append(ctx, "notes", "   "); err == nil {
		t.Fatalf("空 append 应当报错")
	}
	if err := wiki.Delete(ctx, "notes"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := wiki.Delete(ctx, "notes"); !errors.Is(err, memwiki.ErrNotFound) {
		t.Fatalf("重复 delete err = %v", err)
	}
}

// TestWikiFromStoreV2Search 覆盖 Search（含空查询退化为 Index）。
func TestWikiFromStoreV2Search(t *testing.T) {
	store := newFakeStore()
	wiki := WikiFromStoreV2(store)
	ctx := context.Background()
	for _, page := range []Page{
		{Name: "build-flaky", Summary: "flaky build tests", Content: "retry"},
		{Name: "release", Summary: "release checklist", Content: "tag"},
	} {
		if err := wiki.Write(ctx, page); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	metas, err := wiki.Search(ctx, "flaky")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(metas) != 1 || metas[0].Name != "build-flaky" {
		t.Fatalf("Search = %+v", metas)
	}
	all, err := wiki.Search(ctx, "   ")
	if err != nil || len(all) != 2 {
		t.Fatalf("空 Search = %+v (err=%v)", all, err)
	}
	none, err := wiki.Search(ctx, "absent")
	if err != nil || len(none) != 0 {
		t.Fatalf("无命中 Search = %+v (err=%v)", none, err)
	}
}

// TestWikiFromStoreV2NilStore 断言 nil store 不会 panic。
func TestWikiFromStoreV2NilStore(t *testing.T) {
	wiki := WikiFromStoreV2(nil)
	ctx := context.Background()
	if _, err := wiki.Index(ctx); err == nil {
		t.Fatalf("nil Index 应当报错")
	}
	if _, err := wiki.Read(ctx, "x"); err == nil {
		t.Fatalf("nil Read 应当报错")
	}
	if err := wiki.Write(ctx, Page{Name: "x", Summary: "s"}); err == nil {
		t.Fatalf("nil Write 应当报错")
	}
	if err := wiki.Append(ctx, "x", "t"); err == nil {
		t.Fatalf("nil Append 应当报错")
	}
	if err := wiki.Delete(ctx, "x"); err == nil {
		t.Fatalf("nil Delete 应当报错")
	}
	if _, err := wiki.Search(ctx, "q"); err == nil {
		t.Fatalf("nil Search 应当报错")
	}
}

// TestWikiFromStoreV2MapsStoreErrors 断言存储错误映射成 wiki 契约错误。
func TestWikiFromStoreV2MapsStoreErrors(t *testing.T) {
	store := newFakeStore()
	wiki := WikiFromStoreV2(store)
	ctx := context.Background()
	store.errFor["Tree"] = memoryvfs.ErrQuotaExceeded
	if _, err := wiki.Index(ctx); err == nil || !strings.Contains(err.Error(), "quota") || strings.Contains(err.Error(), "memoryvfs:") {
		t.Fatalf("配额错误应映射为可读文案且不泄漏内部包名: %v", err)
	}
	// 空命名空间不是错误：Index 返回空切片（对齐旧 memwiki 的空库行为）。
	store.errFor["Tree"] = memoryvfs.ErrNotFound
	if metas, err := wiki.Index(ctx); err != nil || len(metas) != 0 {
		t.Fatalf("空库 Index = %v, %v；want 空切片 nil", metas, err)
	}
}

// TestV2Helpers 覆盖解析辅助的边界（防止回归到宽松解析）。
func TestV2Helpers(t *testing.T) {
	if level, err := parseLevel(""); err != nil || level != memoryvfs.Level2 {
		t.Fatalf("默认 level = %v, %v", level, err)
	}
	if level, err := parseLevel("any"); err != nil || level != memoryvfs.LevelAny {
		t.Fatalf("any level = %v, %v", level, err)
	}
	if _, err := parseLevel("L7"); err == nil {
		t.Fatalf("非法 level 应当报错")
	}
	if _, err := parseTagString(""); err != nil {
		t.Fatalf("空 tags 字符串: %v", err)
	}
	if ops, err := parseTagString(" a = 1 , b+=2 "); err != nil || len(ops) != 2 || ops[1].delta != 2 {
		t.Fatalf("parseTagString = %+v, %v", ops, err)
	}
	if _, err := parseTagString("=v"); err == nil {
		t.Fatalf("空 key 应当报错")
	}
	if _, err := parseTagString("k="); err != nil {
		t.Fatalf("空 value 允许: %v", err)
	}
	if _, err := parseTagOps(json.RawMessage(`["a"]`)); err == nil {
		t.Fatalf("数组 tags 应当报错")
	}
	if got := deriveSlug("   "); !strings.HasPrefix(got, "mem-") {
		t.Fatalf("空 seed slug = %q", got)
	}
	if got := deriveSlug("Hello World Example"); got != "hello-world-example" {
		t.Fatalf("deriveSlug = %q", got)
	}
	if got := typeOrDash(""); got != "-" {
		t.Fatalf("typeOrDash = %q", got)
	}
	if got := formatTags(nil); got != "(no tags)" {
		t.Fatalf("formatTags = %q", got)
	}
	if got := formatCounts("x", nil); got != "" {
		t.Fatalf("formatCounts = %q", got)
	}
	if got := entrySummary(memoryvfs.Entry{Kind: memoryvfs.KindDir}); got != "(directory)" {
		t.Fatalf("目录摘要 = %q", got)
	}
	if got := entrySummary(memoryvfs.Entry{}); got != "(no summary)" {
		t.Fatalf("空摘要 = %q", got)
	}
}

// TestV2StoresEndToEnd 用真实后端（内存/文件/工作区）跑一遍构造器与 v2 工具面。
// 它验证的是"目录约定 + 选项透传"这层接线，以及 Wiki 适配器在真实存储上的语义。
func TestV2StoresEndToEnd(t *testing.T) {
	ctx := context.Background()

	memoryStore := NewMemoryStore()
	tool := ToolsWithStore(memoryStore)[0]
	result := invokeV2(t, tool, `{"action":"remember","summary":"prefers short commits","content":"keep commits small","type":"preferences"}`)
	if result.IsError || !strings.Contains(result.Content, "written mem://user/self/memories/preferences/") {
		t.Fatalf("内存后端 remember = %q", result.Content)
	}
	result = invokeV2(t, tool, `{"action":"find","query":"commits"}`)
	if result.IsError || !strings.Contains(result.Content, "prefers short commits") {
		t.Fatalf("内存后端 find = %q", result.Content)
	}
	if err := memoryStore.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// 文件后端：写完关掉再打开，内容仍在。
	dir := t.TempDir()
	fileStore, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	fileTool := ToolsWithStore(fileStore)[0]
	if result := invokeV2(t, fileTool, `{"action":"write","name":"persisted","summary":"survives restart","content":"file body"}`); result.IsError {
		t.Fatalf("文件后端 write = %q", result.Content)
	}
	if err := fileStore.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	reopened, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("重开 NewFileStore: %v", err)
	}
	defer func() { _ = reopened.Close() }()
	wiki := WikiFromStoreV2(reopened)
	page, err := wiki.Read(ctx, "persisted")
	if err != nil || !strings.Contains(page.Content, "file body") {
		t.Fatalf("重开后 Read = %+v, err=%v", page, err)
	}
	if err := wiki.Append(ctx, "persisted", "a dated note"); err != nil {
		t.Fatalf("真实后端 Append: %v", err)
	}
	page, err = wiki.Read(ctx, "persisted")
	if err != nil || !strings.Contains(page.Content, memwiki.LogHeading) || !strings.Contains(page.Content, "a dated note") {
		t.Fatalf("真实后端 Append 正文 = %q, err=%v", page.Content, err)
	}
	if err := wiki.Write(ctx, Page{Name: "persisted", Summary: "survives restart", Content: "new body"}); err != nil {
		t.Fatalf("真实后端覆盖写: %v", err)
	}
	page, err = wiki.Read(ctx, "persisted")
	if err != nil || !strings.Contains(page.Content, "new body") || !strings.Contains(page.Content, "(was ") {
		t.Fatalf("真实后端覆盖写正文 = %q, err=%v", page.Content, err)
	}
	if metas, err := wiki.Search(ctx, "new body"); err != nil || len(metas) != 1 || metas[0].Name != "persisted" {
		t.Fatalf("真实后端 Search = %+v, err=%v", metas, err)
	}
	if _, err := NewFileStore(""); err == nil {
		t.Fatalf("空目录应当报错")
	}

	// 工作区后端：目录必须是 <stateDir>/memory/<workspaceID>/v2。
	stateDir := t.TempDir()
	root := t.TempDir()
	workspaceStore, err := NewWorkspaceStore(stateDir, root)
	if err != nil {
		t.Fatalf("NewWorkspaceStore: %v", err)
	}
	workspaceTool := ToolsWithStore(workspaceStore)[0]
	if result := invokeV2(t, workspaceTool, `{"action":"write","name":"scoped","summary":"workspace scoped","content":"body"}`); result.IsError {
		t.Fatalf("工作区后端 write = %q", result.Content)
	}
	id, err := WorkspaceID(root)
	if err != nil {
		t.Fatalf("WorkspaceID: %v", err)
	}
	v2Dir := filepath.Join(stateDir, "memory", id, "v2")
	info, err := os.Stat(v2Dir)
	if err != nil || !info.IsDir() {
		t.Fatalf("v2 目录 %s: %v", v2Dir, err)
	}
	if err := workspaceStore.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestV2FindBudgetTooSmall 断言"有命中但预算装不下一行"不会误报成 no matches。
func TestV2FindBudgetTooSmall(t *testing.T) {
	tool, store := newV2Tool(t)
	seed(t, store, "mem://user/self/memories/pages/budget", "a summary that costs more than one token", "body")
	result := invokeV2(t, tool, `{"action":"find","query":"summary","token_budget":1}`)
	if result.IsError || !strings.Contains(result.Content, "too small for one line") {
		t.Fatalf("预算过小 = %q", result.Content)
	}
}

// TestV2DeleteMissingReportsNotFound 断言 delete 的错误映射。
func TestV2DeleteMissingReportsNotFound(t *testing.T) {
	tool, _ := newV2Tool(t)
	result := invokeV2(t, tool, `{"action":"delete","name":"missing"}`)
	if !result.IsError || !strings.Contains(result.Content, "not found") {
		t.Fatalf("delete missing = %q", result.Content)
	}
}

// TestV2RememberKindRoots 断言 resource/skill/rule 各自落到自己的命名空间。
func TestV2RememberKindRoots(t *testing.T) {
	tool, _ := newV2Tool(t)
	cases := map[string]string{
		"skill":   "mem://skills/",
		"rule":    "mem://user/self/rules/",
		"session": "mem://sessions/",
	}
	for kind, prefix := range cases {
		result := invokeV2(t, tool, fmt.Sprintf(`{"action":"remember","kind":%q,"summary":"%s doc","content":"body"}`, kind, kind))
		if result.IsError || !strings.Contains(result.Content, prefix) {
			t.Fatalf("kind %s = %q", kind, result.Content)
		}
	}
}

// TestMigrateWikiImportsLegacyPages 断言切换默认后端时旧 wiki 页一次性迁入 v2，
// 且重复调用是 no-op（不会把用户手动删除的迁移页复活）。
func TestMigrateWikiImportsLegacyPages(t *testing.T) {
	ctx := context.Background()
	legacyDir := t.TempDir()
	wiki, err := NewFileWiki(legacyDir)
	if err != nil {
		t.Fatalf("NewFileWiki: %v", err)
	}
	if err := wiki.Write(ctx, Page{Name: "build-flaky", Title: "Flaky builds", Summary: "偶发失败与重试", Content: "重试即可"}); err != nil {
		t.Fatalf("write legacy: %v", err)
	}
	store, err := NewFileStore(filepath.Join(legacyDir, "v2"))
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	defer func() { _ = store.Close() }()
	migrated, err := MigrateWiki(store, legacyDir)
	if err != nil || migrated != 1 {
		t.Fatalf("MigrateWiki = %d, %v", migrated, err)
	}
	entry, err := store.Read(ctx, memoryvfs.URI("mem://user/self/memories/pages/build-flaky"), memoryvfs.Level2)
	if err != nil {
		t.Fatalf("Read migrated: %v", err)
	}
	if entry.Summary != "偶发失败与重试" || entry.Content != "重试即可" || entry.Tags["migrated"] != "wiki" {
		t.Fatalf("迁移内容不对: %+v", entry)
	}
	if again, err := MigrateWiki(store, legacyDir); err != nil || again != 0 {
		t.Fatalf("重复迁移 = %d, %v（应当是 no-op）", again, err)
	}
}

// TestV2RememberThenReadByName 断言 remember 写入的记忆能被旧 read 动作按 slug 找到
// （两者默认作用域不同：remember 写 memories/<type>，read 默认 pages）。
func TestV2RememberThenReadByName(t *testing.T) {
	store := NewMemoryStore()
	defer func() { _ = store.Close() }()
	tool := ToolsWithStore(store)[0]
	ctx := context.Background()

	result, err := tool.Invoke(ctx, rig.ToolCall{Name: ToolName, Arguments: `{"action":"remember","type":"preferences","name":"dark-mode","summary":"prefers dark mode","content":"header sentence.\n\nL2-ONLY-MARKER-700"}`})
	if err != nil || result.IsError {
		t.Fatalf("remember: %v %+v", err, result)
	}
	result, err = tool.Invoke(ctx, rig.ToolCall{Name: ToolName, Arguments: `{"action":"read","name":"dark-mode"}`})
	if err != nil || result.IsError || !strings.Contains(result.Content, "L2-ONLY-MARKER-700") {
		t.Fatalf("read by name 没有读到 L2 正文: %v %+v", err, result)
	}
}

// TestV2IndexShowsMoreThanLimit 断言 index 超过 60 条时会写 "… and N more"，
// 而不是悄悄截断。
func TestV2IndexShowsMoreThanLimit(t *testing.T) {
	store := NewMemoryStore()
	defer func() { _ = store.Close() }()
	ctx := context.Background()
	for index := 0; index < memwiki.MaxIndexResults+5; index++ {
		uri, err := memoryvfs.Join("user/self/memories/pages", fmt.Sprintf("page-%02d", index))
		if err != nil {
			t.Fatalf("Join: %v", err)
		}
		if _, err := store.Write(ctx, memoryvfs.Entry{
			URI: uri, Kind: memoryvfs.KindMemory, Type: memoryvfs.TypePages,
			Summary: fmt.Sprintf("page %02d", index), Content: "body",
		}); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	tool := ToolsWithStore(store)[0]
	result, err := tool.Invoke(ctx, rig.ToolCall{Name: ToolName, Arguments: `{"action":"index"}`})
	if err != nil || result.IsError {
		t.Fatalf("index: %v %+v", err, result)
	}
	if !strings.Contains(result.Content, "and 5 more") {
		t.Fatalf("index 没有报告剩余条数: %q", result.Content)
	}
}
