package sqlite_test

// Export 是 ImportDir 的镜像：库 → NDJSON 目录。它与 ImportDir 一样必须是
// **可往返**的——导出、清空、再导入之后，每个会话的条数与字节都要一模一样，
// 否则"出口是改 schema 的唯一安全网"这句话就不成立。

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	rig "github.com/zeokeel/zeokeel-rig"
	sqlitestore "github.com/zeokeel/zeokeel-rig/session/sqlite"
	"github.com/zeokeel/zeokeel-rig/session/store"
)

// seedThreeSessions 造三个会话：中文、emoji、多字节（日文/韩文）混排，外加一条
// 带工具调用与推理文本的消息——这些字段最容易在"序列化再反序列化"里丢。
func seedThreeSessions(t *testing.T, db *sqlitestore.Store) map[string][]rig.Message {
	t.Helper()
	ctx := context.Background()
	want := map[string][]rig.Message{
		"第一会话": {
			userMessage("帮我看看这个日志：致命错误"),
			assistantMessage("好的，我看到「连接超时」，重试 🚀 之后恢复了"),
		},
		"第二会话": {
			userMessage("こんにちは、안녕하세요 — mixed 多字节 and emoji 🧪🎉"),
			{Role: rig.RoleAssistant, Reasoning: "想一想：需要先读文件", ToolCalls: []rig.ToolCall{
				{ID: "call-1", Name: "read_file", Arguments: `{"path":"日志.txt"}`},
			}},
			{Role: rig.RoleTool, ToolCallID: "call-1", Name: "read_file", Content: []rig.ContentPart{
				rig.TextPart("文件内容：🈚️ 空"),
			}},
		},
		"第三会话": {
			userMessage(strings.Repeat("长", 300)),
			assistantMessage("一句话回答。"),
			userMessage("再问一句？"),
		},
	}
	for title, messages := range want {
		meta, err := db.Create(ctx, sqlitestore.Meta{Title: title})
		if err != nil {
			t.Fatalf("Create(%q): %v", title, err)
		}
		if _, err := db.AppendBatch(ctx, meta.ID, messages...); err != nil {
			t.Fatalf("AppendBatch(%q): %v", title, err)
		}
	}
	return want
}

// payloadBytes 是"这个会话在库里占的消息字节"：逐条 JSON 序列化后求和。
// 往返校验用的就是它——条数对、字节对，才算同一条历史。
func payloadBytes(messages []rig.Message) int {
	total := 0
	for _, message := range messages {
		payload, err := json.Marshal(message)
		if err != nil {
			panic(err)
		}
		total += len(payload)
	}
	return total
}

// TestExportImportRoundTrip 是 Export 的主用例：库 → 目录 → 清空 → 再导入，
// 逐会话逐条比对。
func TestExportImportRoundTrip(t *testing.T) {
	ctx := context.Background()
	db, path := openTemp(t, sqlitestore.Options{})
	seedThreeSessions(t, db)

	// 导出前的"应有值"：按会话 ID 记下条数与字节。
	type expectation struct {
		title    string
		messages []rig.Message
		bytes    int
	}
	before := map[string]expectation{}
	metas, err := db.List(ctx, sqlitestore.Filter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(metas) != 3 {
		t.Fatalf("播种了 %d 个会话，want 3", len(metas))
	}
	for _, meta := range metas {
		_, messages, err := db.Load(ctx, meta.ID)
		if err != nil {
			t.Fatalf("Load(%s): %v", meta.ID, err)
		}
		before[meta.ID] = expectation{title: meta.Title, messages: messages, bytes: payloadBytes(messages)}
	}

	dir := filepath.Join(t.TempDir(), "export")
	if err := db.Export(ctx, dir); err != nil {
		t.Fatalf("Export: %v", err)
	}
	// 导出目录里每个会话必须恰好一个 NDJSON 文件。
	files, err := filepath.Glob(filepath.Join(dir, "*.ndjson"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(files) != 3 {
		t.Fatalf("导出目录里 %d 个 NDJSON 文件，want 3（%v）", len(files), files)
	}
	// 再导一次必须幂等：不重复、不丢、不追加（否则"重跑一次备份"就是灾难）。
	if err := db.Export(ctx, dir); err != nil {
		t.Fatalf("第二次 Export: %v", err)
	}
	if files, _ := filepath.Glob(filepath.Join(dir, "*.ndjson")); len(files) != 3 {
		t.Fatalf("第二次导出后 %d 个文件，want 3", len(files))
	}

	// 清空：关库、删文件（连 -wal/-shm），再开一个全新的空库。
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Remove(path + suffix); err != nil && !os.IsNotExist(err) {
			t.Fatalf("清空库文件 %q: %v", path+suffix, err)
		}
	}
	fresh, err := sqlitestore.Open(sqlitestore.Options{Path: path})
	if err != nil {
		t.Fatalf("Open(空库): %v", err)
	}
	t.Cleanup(func() { _ = fresh.Close() })
	if metas, _ := fresh.List(ctx, sqlitestore.Filter{}); len(metas) != 0 {
		t.Fatalf("清空之后还有 %d 个会话", len(metas))
	}

	report, err := sqlitestore.ImportDir(ctx, fresh, sqlitestore.ImportOptions{Dir: dir})
	if err != nil {
		t.Fatalf("ImportDir(Export 的目录): %v", err)
	}
	if report.Sessions != 3 || report.Imported != 3 || len(report.Failed) != 0 {
		t.Fatalf("导入报告 = %+v", report)
	}

	// 逐会话、逐条比对：ID、标题、条数、字节、内容。
	if got, _ := fresh.List(ctx, sqlitestore.Filter{}); len(got) != 3 {
		t.Fatalf("往返之后 %d 个会话，want 3", len(got))
	}
	for id, want := range before {
		meta, messages, err := fresh.Load(ctx, id)
		if err != nil {
			t.Fatalf("往返后 Load(%s): %v", id, err)
		}
		if len(messages) != len(want.messages) {
			t.Fatalf("会话 %s 往返后 %d 条消息，want %d", id, len(messages), len(want.messages))
		}
		if got := payloadBytes(messages); got != want.bytes {
			t.Fatalf("会话 %s 往返后消息字节 %d，want %d", id, got, want.bytes)
		}
		// 标题**不在**往返口径内：目录后端不持久化标题，重开时按首条用户消息重扫
		// （PLATFORM-INVENTORY-B §2）。显式设置的标题、以及超长开场消息的截断
		// 方式（sqlite 无省略号 / 目录后端 clampTitle 加「…」）都会不同——见
		// TestExportRoundTripRederivesTitleLikeTheDirectoryBackend。这里只记录，
		// 因为它不是"历史"的丢失。
		if meta.Title != want.title {
			t.Logf("注意：会话 %s 的标题在往返后由 %q 变为 %q（目录后端按首条用户消息重扫）",
				id, want.title, meta.Title)
		}
		for index := range want.messages {
			if !reflect.DeepEqual(messages[index], want.messages[index]) {
				t.Fatalf("会话 %s 第 %d 条消息往返后不同：\n got = %+v\nwant = %+v",
					id, index, messages[index], want.messages[index])
			}
		}
	}

	// 内置的往返校验（宿主在升级/迁移之后自己就能跑一遍）。
	roundTrip, err := fresh.VerifyExport(ctx, dir)
	if err != nil {
		t.Fatalf("VerifyExport: %v", err)
	}
	if got, want := len(roundTrip.Mismatches), 0; got != want {
		t.Fatalf("VerifyExport 报告 %d 处差异：%+v", got, roundTrip.Mismatches)
	}
	if roundTrip.Sessions != 3 || roundTrip.Messages != report.Messages || roundTrip.Bytes <= 0 {
		t.Fatalf("VerifyExport 报告 = %+v（导入报告 %+v）", roundTrip, report)
	}
	t.Logf("往返校验通过：会话 %d / 消息 %d / 消息字节 %d", roundTrip.Sessions, roundTrip.Messages, roundTrip.Bytes)
}

// TestExportIsReadableByExternalTool 是"出口属于用户"的证明：导出的是纯 NDJSON，
// 一行一个 `rig.Message` 的 JSON，`encoding/json` 逐行就能解析——不需要本 SDK、
// 不需要 SQLite。
func TestExportIsReadableByExternalTool(t *testing.T) {
	ctx := context.Background()
	db, _ := openTemp(t, sqlitestore.Options{})
	seeded := seedThreeSessions(t, db)
	dir := filepath.Join(t.TempDir(), "export")
	if err := db.Export(ctx, dir); err != nil {
		t.Fatalf("Export: %v", err)
	}

	total := 0
	lines := map[string]int{}
	err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() || !strings.HasSuffix(entry.Name(), ".ndjson") {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if len(data) == 0 {
			t.Fatalf("%s 是空文件", path)
		}
		if data[len(data)-1] != '\n' {
			t.Fatalf("%s 没有以换行结尾（不是逐行 NDJSON）", path)
		}
		scanner := bufio.NewScanner(strings.NewReader(string(data)))
		scanner.Buffer(make([]byte, 0, 64*1024), 8<<20)
		count := 0
		for scanner.Scan() {
			line := scanner.Bytes()
			var record map[string]any
			if err := json.Unmarshal(line, &record); err != nil {
				t.Fatalf("%s 第 %d 行不是合法 JSON：%v（%q）", path, count+1, err, line)
			}
			role, ok := record["role"].(string)
			if !ok || role == "" {
				t.Fatalf("%s 第 %d 行缺少 role：%v", path, count+1, record)
			}
			// 内容必须能原样还原成多字节文本，而不是被转义成一堆码点。
			if content, ok := record["content"].([]any); ok {
				for _, part := range content {
					if text, ok := part.(map[string]any)["text"].(string); ok && !utf8RoundTrips(text) {
						t.Fatalf("%s 第 %d 行的文本丢了多字节字符：%q", path, count+1, text)
					}
				}
			}
			count++
		}
		if err := scanner.Err(); err != nil {
			return err
		}
		lines[filepath.Base(path)] = count
		total += count
		return nil
	})
	if err != nil {
		t.Fatalf("walk export dir: %v", err)
	}
	wantMessages := 0
	for _, messages := range seeded {
		wantMessages += len(messages)
	}
	if total != wantMessages {
		t.Fatalf("导出里共 %d 行消息，want %d（每会话 %v）", total, wantMessages, lines)
	}
}

// TestExportRoundTripRederivesTitleLikeTheDirectoryBackend 把一个**已知丢失**
// 钉成断言：目录后端（session/store）不持久化标题，`NewDir` 时按首条用户消息
// 重扫出来。所以 sqlite 里显式设置的标题跨一次 Export→Import 会变成首条用户
// 消息的摘要。
//
// 这条用例的价值在于：如果哪天 session/store 加上了 sidecar 元数据（PLATFORM
// §8-1 的开放决策），它会红——那时 Export 的文档与往返口径要一起更新。
func TestExportRoundTripRederivesTitleLikeTheDirectoryBackend(t *testing.T) {
	ctx := context.Background()
	db, path := openTemp(t, sqlitestore.Options{})
	meta, err := db.Create(ctx, sqlitestore.Meta{Title: "我改过的标题"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := db.AppendBatch(ctx, meta.ID, userMessage("你好")); err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}
	dir := filepath.Join(t.TempDir(), "export")
	if err := db.Export(ctx, dir); err != nil {
		t.Fatalf("Export: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Remove(path + suffix); err != nil && !os.IsNotExist(err) {
			t.Fatalf("清空库: %v", err)
		}
	}
	fresh, err := sqlitestore.Open(sqlitestore.Options{Path: path})
	if err != nil {
		t.Fatalf("Open(空库): %v", err)
	}
	t.Cleanup(func() { _ = fresh.Close() })
	if _, err := sqlitestore.ImportDir(ctx, fresh, sqlitestore.ImportOptions{Dir: dir}); err != nil {
		t.Fatalf("ImportDir: %v", err)
	}
	got, messages, err := fresh.Load(ctx, meta.ID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(messages) != 1 || messages[0].Text() != "你好" {
		t.Fatalf("往返后消息 = %+v", messages)
	}
	if got.Title != "你好" {
		t.Fatalf("往返后标题 = %q，want %q（目录后端重扫首条用户消息）", got.Title, "你好")
	}
}

// TestExportReportsWhatItCannotExport 钉住"导出成功"的边界：事件、花费、检查点
// 这一版**不导出**，报告必须明说，而不是让宿主以为备份是完整的。
func TestExportReportsWhatItCannotExport(t *testing.T) {
	ctx := context.Background()
	db, _ := openTemp(t, sqlitestore.Options{})
	meta, err := db.Create(ctx, sqlitestore.Meta{Title: "有事件与花费的会话"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := db.Append(ctx, meta.ID, userMessage("你好")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := db.SessionLog().AppendEvent(ctx, rig.SessionEvent{
		SessionID: meta.ID, Type: "turn.start", Payload: json.RawMessage(`{"n":1}`),
	}); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
	if err := db.SaveCost(ctx, meta.ID, sqlitestore.CostEntry{Model: "m", PromptTokens: 3, CostUSD: 0.5}); err != nil {
		t.Fatalf("SaveCost: %v", err)
	}
	report, err := sqlitestore.ExportToDir(ctx, db, sqlitestore.ExportOptions{
		Dir: filepath.Join(t.TempDir(), "export"),
	})
	if err != nil {
		t.Fatalf("ExportToDir: %v", err)
	}
	if report.Sessions != 1 || report.Exported != 1 || report.Messages != 1 {
		t.Fatalf("报告 = %+v", report)
	}
	for _, kind := range []string{"events", "costs", "checkpoints"} {
		if !containsString(report.NotExported, kind) {
			t.Fatalf("NotExported = %v，缺 %q（导出不是完整备份，必须明说）", report.NotExported, kind)
		}
	}
}

// TestExportClosedStoreFails：关闭之后的行为与其他方法一致。
func TestExportClosedStoreFails(t *testing.T) {
	db, _ := openTemp(t, sqlitestore.Options{})
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := db.Export(context.Background(), filepath.Join(t.TempDir(), "export")); err == nil {
		t.Fatal("Export on a closed store = nil error")
	}
}

// TestExportSkipsExistingWithoutOverwrite：ExportToDir 的保守模式给宿主预览用。
func TestExportSkipsExistingWithoutOverwrite(t *testing.T) {
	ctx := context.Background()
	db, _ := openTemp(t, sqlitestore.Options{})
	meta, err := db.Create(ctx, sqlitestore.Meta{Title: "一号"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := db.Append(ctx, meta.ID, userMessage("一")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	dir := filepath.Join(t.TempDir(), "export")
	if err := db.Export(ctx, dir); err != nil {
		t.Fatalf("Export: %v", err)
	}
	// 目标已有同名会话：Overwrite=false 时跳过并报告。
	if _, err := db.Append(ctx, meta.ID, assistantMessage("二")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	report, err := sqlitestore.ExportToDir(ctx, db, sqlitestore.ExportOptions{Dir: dir, Overwrite: false})
	if err != nil {
		t.Fatalf("ExportToDir: %v", err)
	}
	if report.Exported != 0 || len(report.Skipped) != 1 || report.Skipped[0] != meta.ID {
		t.Fatalf("保守导出报告 = %+v", report)
	}
	// 目录里的旧版本不能被覆盖（条数还是 1）。
	legacy, err := store.NewDir(dir)
	if err != nil {
		t.Fatalf("NewDir: %v", err)
	}
	defer func() { _ = legacy.Close() }()
	if _, messages, err := legacy.Load(ctx, meta.ID); err != nil || len(messages) != 1 {
		t.Fatalf("保守导出后目录里 %d 条消息（err %v），want 1", len(messages), err)
	}
}

// utf8RoundTrips 确认字符串里的多字节内容没有被破坏（仍是合法 UTF-8 且非空）。
func utf8RoundTrips(text string) bool {
	return utf8.ValidString(text) && len([]rune(text)) > 0
}

// containsString 是给 NotExported 用的小工具（顺序无关）。
func containsString(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}
