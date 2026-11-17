package store_test

// WithScanContents：扫目录时顺带恢复标题（默认开启）。
//
// 动机：个人客户端的"最近会话"侧栏必须有标题；没有标题的会话列表等于没用。
// 取舍：只读每个会话的开头一小段（≤ titleScanBytes、最多前 4 条记录）取标题，
// Messages 仍留 0 等 Load 补全——启动成本 O(会话数)，不是 O(总字节)。

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/session/store"
)

// titleOf 返回某个会话在 List 里的标题（不存在则失败）。
func titleOf(t *testing.T, s *store.Dir, id string) string {
	t.Helper()
	metas, err := s.List(context.Background(), store.Filter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, meta := range metas {
		if meta.ID == id {
			return meta.Title
		}
	}
	t.Fatalf("List 里没有会话 %q: %+v", id, metas)
	return ""
}

// 默认开启：重启后 List 立刻带标题（不需要先 Load）。
func TestScanContentsDefaultOnGivesTitles(t *testing.T) {
	dir := t.TempDir()
	writer, err := store.NewDir(dir)
	if err != nil {
		t.Fatalf("NewDir: %v", err)
	}
	first := mustCreate(t, writer, store.Meta{})
	mustAppend(t, writer, first.ID,
		rig.NewMessage(rig.RoleUser, "排查  构建失败\n为什么 cargo build 挂了"),
		rig.NewMessage(rig.RoleAssistant, "先看日志"),
	)
	second := mustCreate(t, writer, store.Meta{})
	mustAppend(t, writer, second.ID, rig.NewMessage(rig.RoleUser, "第二个会话"))
	if err := writer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, err := store.NewDir(dir) // 默认 WithScanContents
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = reopened.Close() }()
	// 文档里写了 `WithScanContents()`（无参数 = 开启），这里保证那种写法也能用。
	explicit, err := store.NewDir(dir, store.WithScanContents())
	if err != nil {
		t.Fatalf("NewDir(WithScanContents()): %v", err)
	}
	defer func() { _ = explicit.Close() }()
	if got := titleOf(t, explicit, first.ID); got == "" {
		t.Fatal("WithScanContents() 无参形式没有生效")
	}

	// 标题来自首条 user 消息，空白折叠、单行。
	if got := titleOf(t, reopened, first.ID); got != "排查 构建失败 为什么 cargo build 挂了" {
		t.Fatalf("第一个会话标题 = %q", got)
	}
	if got := titleOf(t, reopened, second.ID); got != "第二个会话" {
		t.Fatalf("第二个会话标题 = %q", got)
	}
	// Messages 仍然留给 Load（扫目录不数条数）。
	metas, err := reopened.List(context.Background(), store.Filter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, meta := range metas {
		if meta.Messages != 0 {
			t.Fatalf("会话 %q 的 Messages = %d，want 0（扫目录不数条数）", meta.ID, meta.Messages)
		}
	}
	// Load 之后才补全条数（标题保持在）。
	meta, messages, err := reopened.Load(context.Background(), first.ID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(messages) != 2 || meta.Messages != 2 || meta.Title == "" {
		t.Fatalf("Load 之后 = %d 条, meta %+v", len(messages), meta)
	}
}

// 显式关闭：只 stat，不给标题、不读内容。
func TestScanContentsCanBeDisabled(t *testing.T) {
	dir := t.TempDir()
	writer, err := store.NewDir(dir)
	if err != nil {
		t.Fatalf("NewDir: %v", err)
	}
	created := mustCreate(t, writer, store.Meta{})
	mustAppend(t, writer, created.ID, rig.NewMessage(rig.RoleUser, "有标题的会话"))
	if err := writer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	quiet, err := store.NewDir(dir, store.WithScanContents(false))
	if err != nil {
		t.Fatalf("NewDir: %v", err)
	}
	defer func() { _ = quiet.Close() }()
	if got := titleOf(t, quiet, created.ID); got != "" {
		t.Fatalf("关闭内容扫描后标题 = %q, want 空", got)
	}
	// 但会话本身在，Load 之后标题会被补回来。
	meta, messages, err := quiet.Load(context.Background(), created.ID)
	if err != nil || len(messages) != 1 {
		t.Fatalf("Load = %d 条, err=%v", len(messages), err)
	}
	if meta.Title == "" {
		t.Fatal("Load 之后标题应被补全")
	}
}

// 首行损坏：跳过它，用下一条能解析的 user 消息作标题（不阻断启动）。
func TestScanContentsSkipsCorruptFirstLine(t *testing.T) {
	dir := t.TempDir()
	line := mustJSON(t, rig.NewMessage(rig.RoleUser, "第二行才是真的用户消息"))
	body := "{\"role\":\"user\",\"content\":[ 坏掉的 JSON\n" + line + "\n"
	if err := os.WriteFile(sessionFile(dir, "broken"), []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	opened, err := store.NewDir(dir)
	if err != nil {
		t.Fatalf("NewDir: %v", err)
	}
	defer func() { _ = opened.Close() }()
	if got := titleOf(t, opened, "broken"); got != "第二行才是真的用户消息" {
		t.Fatalf("标题 = %q, want 跳过错行后的用户消息", got)
	}
}

// 文件开头全是坏行：标题留空 + Notice，但会话仍在、启动不失败。
func TestScanContentsAllCorruptHeadLeavesEmptyTitleWithNotice(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(sessionFile(dir, "garbage"), []byte("not json\nnor this\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	var notices []string
	opened, err := store.NewDir(dir, store.WithNotice(func(format string, args ...any) {
		notices = append(notices, fmt.Sprintf(format, args...))
	}))
	if err != nil {
		t.Fatalf("NewDir: %v", err)
	}
	defer func() { _ = opened.Close() }()
	if got := titleOf(t, opened, "garbage"); got != "" {
		t.Fatalf("标题 = %q, want 空", got)
	}
	if len(notices) != 1 {
		t.Fatalf("Notice 次数 = %d, want 恰好 1 次: %v", len(notices), notices)
	}
	if !strings.Contains(notices[0], "无法解析") {
		t.Fatalf("Notice = %q", notices[0])
	}
}

// 空文件：等价于空会话，没有标题也不报错、不发 Notice。
func TestScanContentsEmptyFileIsQuiet(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(sessionFile(dir, "empty"), nil, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	var notices []string
	opened, err := store.NewDir(dir, store.WithNotice(func(format string, args ...any) {
		notices = append(notices, fmt.Sprintf(format, args...))
	}))
	if err != nil {
		t.Fatalf("NewDir: %v", err)
	}
	defer func() { _ = opened.Close() }()
	if got := titleOf(t, opened, "empty"); got != "" {
		t.Fatalf("空文件标题 = %q, want 空", got)
	}
	if len(notices) != 0 {
		t.Fatalf("空文件不该产生 Notice: %v", notices)
	}
}

// 超大首行（超过 256 KiB 且**没有换行**——即半行/未完成的写入）：标题留空 + 一次
// Notice，启动照常。完整但超长的记录由 TestFixTitleSkipsOversizeRecord 覆盖（跳过
// 该条继续找标题）。
func TestScanContentsHugeFirstLineIsSkipped(t *testing.T) {
	dir := t.TempDir()
	huge := strings.Repeat("x", 300<<10) // 300 KiB，中间没有换行
	if err := os.WriteFile(sessionFile(dir, "huge"), []byte(huge), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	var notices []string
	opened, err := store.NewDir(dir, store.WithNotice(func(format string, args ...any) {
		notices = append(notices, fmt.Sprintf(format, args...))
	}))
	if err != nil {
		t.Fatalf("NewDir: %v", err)
	}
	defer func() { _ = opened.Close() }()
	if got := titleOf(t, opened, "huge"); got != "" {
		t.Fatalf("超大首行标题 = %q, want 空", got)
	}
	if len(notices) != 1 || !strings.Contains(notices[0], "超过") ||
		!strings.Contains(notices[0], "跳过") {
		t.Fatalf("Notice = %v, want 一条'超过上限、已跳过'的提示", notices)
	}
}

// 标题超长：按 MaxTitleRunes 截断（含省略号），不会把整段消息塞进 Meta。
func TestScanContentsClampsLongTitle(t *testing.T) {
	dir := t.TempDir()
	long := strings.Repeat("很长", 200) // 400 rune
	body := mustJSON(t, rig.NewMessage(rig.RoleUser, long)) + "\n"
	if err := os.WriteFile(sessionFile(dir, "long"), []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	opened, err := store.NewDir(dir)
	if err != nil {
		t.Fatalf("NewDir: %v", err)
	}
	defer func() { _ = opened.Close() }()
	title := titleOf(t, opened, "long")
	if runes := []rune(title); len(runes) > store.MaxTitleRunes {
		t.Fatalf("标题 %d 个 rune，超过上限 %d", len(runes), store.MaxTitleRunes)
	}
	if !strings.HasSuffix(title, "…") {
		t.Fatalf("截断后的标题应以省略号结尾: %q", title)
	}
}

// 100 个会话的启动耗时：给出真实 ms 数（同时防止回归到"读完整文件"）。
//
// 每个会话 20 条消息 ≈ 8 KiB，总数据 ~800 KiB；扫目录只读开头一小段。
func TestScanContentsStartupCost100Sessions(t *testing.T) {
	if testing.Short() {
		t.Skip("启动耗时用例在 -short 下跳过")
	}
	dir := t.TempDir()
	writer, err := store.NewDir(dir)
	if err != nil {
		t.Fatalf("NewDir: %v", err)
	}
	for index := 0; index < 100; index++ {
		created := mustCreate(t, writer, store.Meta{})
		messages := make([]rig.Message, 0, 20)
		for turn := 0; turn < 20; turn++ {
			role := rig.RoleAssistant
			if turn%2 == 0 {
				role = rig.RoleUser
			}
			messages = append(messages, rig.NewMessage(role,
				strings.Repeat("这是一条用于撑大文件的消息内容 ", 20)))
		}
		if _, err := writer.AppendBatch(context.Background(), created.ID, messages...); err != nil {
			t.Fatalf("AppendBatch: %v", err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// 预热一次（OS page cache），再量一次。
	first, err := store.NewDir(dir)
	if err != nil {
		t.Fatalf("NewDir: %v", err)
	}
	_ = first.Close()

	// 关闭内容扫描的基线（同样 100 个会话，只 stat）。
	var statOnlyTotal time.Duration
	for round := 0; round < 5; round++ {
		start := time.Now()
		opened, openErr := store.NewDir(dir, store.WithScanContents(false))
		statOnlyTotal += time.Since(start)
		if openErr != nil {
			t.Fatalf("NewDir(scan off): %v", openErr)
		}
		_ = opened.Close()
	}

	const rounds = 5
	var total time.Duration
	var best time.Duration
	for round := 0; round < rounds; round++ {
		start := time.Now()
		opened, openErr := store.NewDir(dir)
		elapsed := time.Since(start)
		if openErr != nil {
			t.Fatalf("NewDir: %v", openErr)
		}
		metas, listErr := opened.List(context.Background(), store.Filter{})
		if listErr != nil {
			t.Fatalf("List: %v", listErr)
		}
		if len(metas) != 100 {
			t.Fatalf("扫到 %d 个会话, want 100", len(metas))
		}
		titled := 0
		for _, meta := range metas {
			if meta.Title != "" {
				titled++
			}
		}
		if titled != 100 {
			t.Fatalf("带标题的会话 = %d, want 100", titled)
		}
		_ = opened.Close()
		total += elapsed
		if best == 0 || elapsed < best {
			best = elapsed
		}
	}
	t.Logf("100 个会话（20 条/个，~%d KiB 总量）：NewDir 平均 %.2f ms / 最快 %.2f ms（含标题扫描）；"+
		"关闭内容扫描（只 stat）平均 %.2f ms",
		100*20*90/1024,
		float64(total.Microseconds())/float64(rounds)/1000,
		float64(best.Microseconds())/1000,
		float64(statOnlyTotal.Microseconds())/5/1000)
}

// BenchmarkNewDir100Sessions 给出可重复的启动成本基线。
func BenchmarkNewDir100Sessions(b *testing.B) {
	dir := b.TempDir()
	writer, err := store.NewDir(dir)
	if err != nil {
		b.Fatalf("NewDir: %v", err)
	}
	for index := 0; index < 100; index++ {
		created, createErr := writer.Create(context.Background(), store.Meta{})
		if createErr != nil {
			b.Fatalf("Create: %v", createErr)
		}
		messages := make([]rig.Message, 0, 20)
		for turn := 0; turn < 20; turn++ {
			role := rig.RoleAssistant
			if turn%2 == 0 {
				role = rig.RoleUser
			}
			messages = append(messages, rig.NewMessage(role,
				strings.Repeat("这是一条用于撑大文件的消息内容 ", 20)))
		}
		if _, appendErr := writer.AppendBatch(context.Background(), created.ID, messages...); appendErr != nil {
			b.Fatalf("AppendBatch: %v", appendErr)
		}
	}
	_ = writer.Close()
	b.ResetTimer()
	for round := 0; round < b.N; round++ {
		opened, openErr := store.NewDir(dir)
		if openErr != nil {
			b.Fatalf("NewDir: %v", openErr)
		}
		_ = opened.Close()
	}
}

// mustJSON 把一个消息编码成一行 NDJSON。
func mustJSON(t *testing.T, message rig.Message) string {
	t.Helper()
	line, err := json.Marshal(message)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(line)
}
