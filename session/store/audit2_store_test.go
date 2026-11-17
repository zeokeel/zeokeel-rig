package store_test

// 第 2D 轮对抗审计（只读）：session/store 会不会丢数据 / 会不会骗人。
//
// 覆盖：
//   - 4 个 goroutine 并发写不同会话（配合 -race）；
//   - 半行（kill -9 现场）与中间损坏行的恢复；
//   - Load 的 Messages 与文件里有效行数一致；
//   - List 的 Title 与实际首条 user 消息一致（多行 / 非法 UTF-8 / 300 KiB 超长）；
//   - Delete 之后不复活；
//   - 同一目录两个实例：列表可能不完整，但数据不能丢。

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/session/store"
)

// openStore 打开一个 store 并在测试结束时关闭。
func openStore(t *testing.T, dir string) *store.Dir {
	t.Helper()
	opened, err := store.NewDir(dir)
	if err != nil {
		t.Fatalf("NewDir: %v", err)
	}
	t.Cleanup(func() { _ = opened.Close() })
	return opened
}

// user 构造一条 user 消息。
func user(text string) rig.Message { return rig.NewMessage(rig.RoleUser, text) }

// assistant 构造一条 assistant 消息。

// 1) 4 个 goroutine 并发写不同会话（配 -race 跑）。
func TestAuditConcurrentDifferentSessions(t *testing.T) {
	dir := t.TempDir()
	first := openStore(t, dir)
	second := openStore(t, dir) // 第二个实例共享同一目录

	const writers = 4
	const perWriter = 25

	var wg sync.WaitGroup
	errs := make(chan error, writers)
	ids := make([]string, writers)
	owners := make([]*store.Dir, writers)
	for writer := 0; writer < writers; writer++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			target := first
			if index%2 == 1 {
				target = second // 交叉两个实例，压并发路径
			}
			meta, err := target.Create(context.Background(), store.Meta{Workspace: "/ws"})
			if err != nil {
				errs <- fmt.Errorf("writer %d Create: %w", index, err)
				return
			}
			ids[index] = meta.ID
			owners[index] = target
			for message := 0; message < perWriter; message++ {
				if _, err := target.Append(context.Background(), meta.ID,
					user(fmt.Sprintf("w%d-%d", index, message))); err != nil {
					errs <- fmt.Errorf("writer %d Append %d: %w", index, message, err)
					return
				}
			}
		}(writer)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("%v", err)
	}

	// 每个会话都必须完整。
	for writer, id := range ids {
		// 用创建它的那个实例读（跨实例 Load 的限制单独记录在 TestAuditCrossInstanceLoad）。
		meta, messages, err := owners[writer].Load(context.Background(), id)
		if err != nil {
			t.Fatalf("Load(%s): %v", id, err)
		}
		if len(messages) != perWriter {
			t.Fatalf("会话 %d 的消息数 = %d, want %d", writer, len(messages), perWriter)
		}
		if meta.Messages != perWriter {
			t.Fatalf("Meta.Messages = %d, want %d", meta.Messages, perWriter)
		}
		if got := messages[0].Text(); got != fmt.Sprintf("w%d-0", writer) {
			t.Fatalf("首条消息 = %q", got)
		}
	}
}

// 2) 半行恢复：模拟 kill -9 留下的未完成写入。
func TestAuditPartialLineAfterCrash(t *testing.T) {
	dir := t.TempDir()
	opened := openStore(t, dir)
	meta, err := opened.Create(context.Background(), store.Meta{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	for i := 0; i < 3; i++ {
		if _, err := opened.Append(context.Background(), meta.ID, user(fmt.Sprintf("msg-%d", i))); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	path := sessionFile(dir, meta.ID)
	validBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if err := opened.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// 追加半行（没有换行的残片），模拟进程在写一半时被杀。
	half := `{"role":"user","content":[{"type":"text","text":"tor`
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	if _, err := file.WriteString(half); err != nil {
		t.Fatalf("WriteString: %v", err)
	}
	_ = file.Close()

	// 重新打开：半行必须被丢弃，前 3 条必须完好。
	reopened := openStore(t, dir)
	loadedMeta, messages, err := reopened.Load(context.Background(), meta.ID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(messages) != 3 {
		t.Fatalf("半行之后 Load 的消息数 = %d, want 3", len(messages))
	}
	if loadedMeta.Messages != 3 {
		t.Fatalf("Meta.Messages = %d, want 3", loadedMeta.Messages)
	}
	// 继续追加必须可用，且不能把半行接上（否则会造出一条坏记录）。
	if _, err := reopened.Append(context.Background(), meta.ID, user("after-crash")); err != nil {
		t.Fatalf("崩溃后追加: %v", err)
	}
	_, messages, err = reopened.Load(context.Background(), meta.ID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(messages) != 4 {
		t.Fatalf("崩溃后追加的 Load 条数 = %d, want 4", len(messages))
	}
	if got := messages[3].Text(); got != "after-crash" {
		t.Fatalf("最后一条 = %q", got)
	}
	// 原有字节一个都没被破坏。
	current, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.HasPrefix(string(current), string(validBytes)) {
		t.Fatalf("崩溃恢复破坏了已写入的字节")
	}
}

// 3) Load 的条数与文件里有效行数一致（含中间一条损坏行）。
func TestAuditLoadCountMatchesFileLines(t *testing.T) {
	dir := t.TempDir()
	opened := openStore(t, dir)
	meta, err := opened.Create(context.Background(), store.Meta{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	for i := 0; i < 10; i++ {
		if _, err := opened.Append(context.Background(), meta.ID, user(fmt.Sprintf("m%d", i))); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	path := sessionFile(dir, meta.ID)

	// 在第 5 条之后插一条完整的坏行（合法换行、非法 JSON）。
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	lines := strings.SplitAfter(string(raw), "\n")
	// 找到第 5 个非空行之后的位置。
	inserted := false
	var rebuilt strings.Builder
	seen := 0
	for _, line := range lines {
		rebuilt.WriteString(line)
		if line == "" {
			continue
		}
		seen++
		if seen == 5 && !inserted {
			rebuilt.WriteString("{\"role\":\"user\",\"content\":[{\"type\":\"text\",\"text\":\"broken\"}]\n") // 少了右括号
			inserted = true
		}
	}
	if err := os.WriteFile(path, []byte(rebuilt.String()), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	reopened := openStore(t, dir)
	loadedMeta, messages, err := reopened.Load(context.Background(), meta.ID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(messages) != 10 {
		t.Fatalf("损坏行之外的消息数 = %d, want 10", len(messages))
	}
	if loadedMeta.Messages != 10 {
		t.Fatalf("Meta.Messages = %d, want 10", loadedMeta.Messages)
	}
	// 消息顺序必须保持。
	for index, message := range messages {
		if want := fmt.Sprintf("m%d", index); message.Text() != want {
			t.Fatalf("第 %d 条 = %q, want %q（顺序被打乱）", index, message.Text(), want)
		}
	}
}

// 4) List 的 Title = 首条 user 消息（多行 / 非法 UTF-8 / 300 KiB 超长）。
func TestAuditListTitleMatchesFirstUserMessage(t *testing.T) {
	cases := []struct {
		name       string
		text       string
		wantTitle  string
		wantNotice bool // 超长首行会放弃提标题
	}{
		{"单行", "hello world", "hello world", false},
		{"多行折叠", "first line\nsecond\tline   third", "first line second line third", false},
		{"前导空白", "   \n\t  trimmed text  \n", "trimmed text", false},
		{"中文", "帮我修一下这个 bug", "帮我修一下这个 bug", false},
		{"非法 UTF-8", "bad \xff\xfe bytes", "bad \uFFFD\uFFFD bytes", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			opened := openStore(t, dir)
			meta, err := opened.Create(context.Background(), store.Meta{})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			if _, err := opened.Append(context.Background(), meta.ID, user(tc.text)); err != nil {
				t.Fatalf("Append: %v", err)
			}
			_ = opened.Close()

			reopened := openStore(t, dir)
			listed, err := reopened.List(context.Background(), store.Filter{})
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			var found *store.Meta
			for index := range listed {
				if listed[index].ID == meta.ID {
					found = &listed[index]
				}
			}
			if found == nil {
				t.Fatalf("List 里没有这个会话")
			}
			if found.Title != tc.wantTitle {
				t.Fatalf("Title = %q, want %q", found.Title, tc.wantTitle)
			}
		})
	}
}

// 5) 300 KiB 的超长首行：不能因为一行撑爆标题扫描（也不能卡住）。
func TestAuditListTitleWith300KiBFirstLine(t *testing.T) {
	dir := t.TempDir()
	notices := make([]string, 0, 4)
	var noticeMu sync.Mutex
	opened, err := store.NewDir(dir, store.WithNotice(func(format string, args ...any) {
		noticeMu.Lock()
		defer noticeMu.Unlock()
		notices = append(notices, fmt.Sprintf(format, args...))
	}))
	if err != nil {
		t.Fatalf("NewDir: %v", err)
	}
	t.Cleanup(func() { _ = opened.Close() })

	meta, err := opened.Create(context.Background(), store.Meta{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	huge := strings.Repeat("x", 300<<10)
	if _, err := opened.Append(context.Background(), meta.ID, user(huge)); err != nil {
		t.Fatalf("Append 300 KiB: %v", err)
	}
	// 第二条 user 消息才是"可提取"的标题来源。
	if _, err := opened.Append(context.Background(), meta.ID, user("readable title")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	_ = opened.Close()

	reopened := openStore(t, dir)
	listed, err := reopened.List(context.Background(), store.Filter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, item := range listed {
		if item.ID != meta.ID {
			continue
		}
		// 首行超长时实现放弃提取（标题留空）——这本身可接受，但不能崩/不能卡。
		if len(item.Title) > 3*store.MaxTitleRunes {
			t.Fatalf("标题没有被截断到 MaxTitleRunes 附近: 长度 %d", len(item.Title))
		}
		t.Logf("300 KiB 首行时的 Title = %q（长度 %d）", item.Title, len(item.Title))
	}
	// 数据本身必须完整。
	_, messages, err := reopened.Load(context.Background(), meta.ID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(messages) != 2 {
		t.Fatalf("消息数 = %d, want 2", len(messages))
	}
	if got := len(messages[0].Text()); got != len(huge) {
		t.Fatalf("300 KiB 消息内容被截断: %d", got)
	}
}

// 6) Delete 之后不复活（重开也不复活）。
func TestAuditDeleteDoesNotResurrect(t *testing.T) {
	dir := t.TempDir()
	opened := openStore(t, dir)
	meta, err := opened.Create(context.Background(), store.Meta{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := opened.Append(context.Background(), meta.ID, user("bye")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := opened.Delete(context.Background(), meta.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, _, err := opened.Load(context.Background(), meta.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("删除后 Load 错误 = %v, want ErrNotFound", err)
	}
	// 同一个实例上重复删除是 ErrNotFound。
	if err := opened.Delete(context.Background(), meta.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("重复 Delete 错误 = %v, want ErrNotFound", err)
	}
	_ = opened.Close()

	// 重开（扫目录）之后也不能复活。
	reopened := openStore(t, dir)
	list, err := reopened.List(context.Background(), store.Filter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, item := range list {
		if item.ID == meta.ID {
			t.Fatal("删除的会话在重开后复活了")
		}
	}
}

// 7) 同一目录两个实例：列表可能不完整，但数据不能丢。
func TestAuditTwoInstancesOnSameDir(t *testing.T) {
	dir := t.TempDir()
	first := openStore(t, dir)
	second := openStore(t, dir) // 在 first 写入之前就打开了

	meta, err := first.Create(context.Background(), store.Meta{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := first.Append(context.Background(), meta.ID, user("first instance")); err != nil {
		t.Fatalf("Append: %v", err)
	}

	// 第二个实例的内存索引可能还不知道这个会话——这是文档承认的"列表可能不完整"。
	listSecond, err := second.List(context.Background(), store.Filter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	seen := false
	for _, item := range listSecond {
		if item.ID == meta.ID {
			seen = true
		}
	}
	t.Logf("第二个实例此刻能否列出新会话: %v（不要求 true）", seen)

	// 关键：数据不能丢。直接读文件必须能看到；重新打开一个实例也必须能看到。
	raw, err := os.ReadFile(sessionFile(dir, meta.ID))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.Contains(string(raw), "first instance") {
		t.Fatalf("数据没有落盘: %q", raw)
	}
	third := openStore(t, dir)
	if _, messages, err := third.Load(context.Background(), meta.ID); err != nil {
		t.Fatalf("新实例 Load: %v", err)
	} else if len(messages) != 1 {
		t.Fatalf("新实例读到 %d 条消息, want 1", len(messages))
	}
}

// 8)【已修复】跨实例 Load：另一个实例新建的会话，本实例 `Load` 必须成功。
//
// 本条审计用例最初记录的是缺陷（ErrNotFound）；S-P2 修复后语义变为"未命中索引时
// 按磁盘补索引"。这里保留为**回归守卫**：一旦有人去掉 recoverIndex，本条会红。
func TestAuditCrossInstanceLoad(t *testing.T) {
	dir := t.TempDir()
	first := openStore(t, dir)
	second := openStore(t, dir)

	meta, err := first.Create(context.Background(), store.Meta{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := first.Append(context.Background(), meta.ID, user("written by first")); err != nil {
		t.Fatalf("Append: %v", err)
	}

	loadedMeta, messages, loadErr := second.Load(context.Background(), meta.ID)
	if loadErr != nil {
		t.Fatalf("跨实例 Load 失败（S-P2 修复被回退了？）: %v", loadErr)
	}
	if len(messages) != 1 || loadedMeta.Messages != 1 {
		t.Fatalf("跨实例 Load 条数 = %d / Meta.Messages = %d, want 1/1", len(messages), loadedMeta.Messages)
	}
	if got := messages[0].Text(); got != "written by first" {
		t.Fatalf("内容 = %q", got)
	}
}
