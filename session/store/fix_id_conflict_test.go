package store_test

// 会话文件名归属：两个不同的会话 ID 落到**同一个文件**上时必须报错，不得读写
// 另一个会话的内容。
//
// 真实触发场景是大小写不敏感文件系统（macOS APFS / Windows NTFS）：索引里是
// `Prod`，调用 `Append("prod")` 会打开同一个 `Prod.ndjson`。Linux 上大小写变体
// 是两个不同的文件，因此这里用符号链接复现同一形状（`Alias.ndjson` ->
// `real.ndjson`）：两个名字、一个 inode，正是缺陷的判定条件。

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/session/store"
)

func conflictMessage(t *testing.T, text string) rig.Message {
	t.Helper()
	return rig.NewMessage(rig.RoleUser, text)
}

func textsOf(messages []rig.Message) []string {
	out := make([]string, 0, len(messages))
	for _, message := range messages {
		out = append(out, message.Text())
	}
	return out
}

// TestAliasedSessionFileIsRejected 是本次修复的主用例：别名 ID 的四个入口
// （Append / Load / Replace / Delete）都必须返回可读错误，且真实会话逐字节不变。
func TestAliasedSessionFileIsRejected(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "sessions")
	dir, err := store.NewDir(root)
	if err != nil {
		t.Fatalf("NewDir: %v", err)
	}
	defer func() { _ = dir.Close() }()

	real, err := dir.Create(ctx, store.Meta{Title: "真实会话"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := dir.AppendBatch(ctx, real.ID, conflictMessage(t, "一"), conflictMessage(t, "二")); err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}
	realPath := filepath.Join(root, real.ID+".ndjson")
	before, err := os.ReadFile(realPath)
	if err != nil {
		t.Fatalf("read real session file: %v", err)
	}

	// 别名：同一个 inode 的第二个名字（Linux 上等价于大小写不敏感文件系统上的
	// Prod/prod 两个 ID）。
	const alias = "Alias"
	if err := os.Symlink(real.ID+".ndjson", filepath.Join(root, alias+".ndjson")); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	if _, err := dir.Append(ctx, alias, conflictMessage(t, "串线")); err == nil {
		t.Fatal("Append(别名) 成功了：它写进了另一个会话的文件")
	} else if !errors.Is(err, store.ErrSessionIDConflict) {
		t.Fatalf("Append(别名) = %v，want errors.Is(err, ErrSessionIDConflict)", err)
	}
	if _, _, err := dir.Load(ctx, alias); !errors.Is(err, store.ErrSessionIDConflict) {
		t.Fatalf("Load(别名) = %v，want errors.Is(err, ErrSessionIDConflict)", err)
	}
	if _, err := dir.Replace(ctx, alias, []rig.Message{conflictMessage(t, "覆盖")}); !errors.Is(err, store.ErrSessionIDConflict) {
		t.Fatalf("Replace(别名) = %v，want errors.Is(err, ErrSessionIDConflict)", err)
	}
	if err := dir.Delete(ctx, alias); !errors.Is(err, store.ErrSessionIDConflict) {
		t.Fatalf("Delete(别名) = %v，want errors.Is(err, ErrSessionIDConflict)", err)
	}

	after, err := os.ReadFile(realPath)
	if err != nil {
		t.Fatalf("read real session file after alias operations: %v", err)
	}
	if string(after) != string(before) {
		t.Fatalf("真实会话文件被改写了：\n before = %q\n after  = %q", string(before), string(after))
	}
	_, messages, err := dir.Load(ctx, real.ID)
	if err != nil {
		t.Fatalf("Load(真实会话): %v", err)
	}
	if got := textsOf(messages); len(got) != 2 || got[0] != "一" || got[1] != "二" {
		t.Fatalf("真实会话内容 = %v，want [一 二]", got)
	}

	// 重启后别名会被扫进索引（文件名就是索引）。此时归属校验仍然必须生效，
	// 否则"扫目录 + 大小写变体"就是一条绕过路径。
	if err := dir.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	reopened, err := store.NewDir(root)
	if err != nil {
		t.Fatalf("NewDir(重开): %v", err)
	}
	defer func() { _ = reopened.Close() }()
	if _, err := reopened.Append(ctx, alias, conflictMessage(t, "重启后串线")); !errors.Is(err, store.ErrSessionIDConflict) {
		t.Fatalf("重开后 Append(别名) = %v，want errors.Is(err, ErrSessionIDConflict)", err)
	}
	_, messages, err = reopened.Load(ctx, real.ID)
	if err != nil {
		t.Fatalf("重开后 Load(真实会话): %v", err)
	}
	if got := textsOf(messages); len(got) != 2 {
		t.Fatalf("重开后真实会话 = %v，want 2 条", got)
	}
	if data, err := os.ReadFile(realPath); err != nil || string(data) != string(before) {
		t.Fatalf("重开后真实会话文件被改写（err=%v）", err)
	}
}

// TestNormalSessionIDsAreUnaffected 钉住反向：不同文件名 stem 的会话互不干扰，
// 别名校验引入后正常路径的行为逐条不变。
func TestNormalSessionIDsAreUnaffected(t *testing.T) {
	ctx := context.Background()
	dir, err := store.NewDir(filepath.Join(t.TempDir(), "sessions"))
	if err != nil {
		t.Fatalf("NewDir: %v", err)
	}
	defer func() { _ = dir.Close() }()

	first, err := dir.Create(ctx, store.Meta{ID: "prod", Title: "生产"})
	if err != nil {
		t.Fatalf("Create prod: %v", err)
	}
	second, err := dir.Create(ctx, store.Meta{ID: "staging", Title: "预发"})
	if err != nil {
		t.Fatalf("Create staging: %v", err)
	}
	if _, err := dir.Append(ctx, first.ID, conflictMessage(t, "p1")); err != nil {
		t.Fatalf("Append prod: %v", err)
	}
	if _, err := dir.Append(ctx, second.ID, conflictMessage(t, "s1")); err != nil {
		t.Fatalf("Append staging: %v", err)
	}
	if _, err := dir.Append(ctx, first.ID, conflictMessage(t, "p2")); err != nil {
		t.Fatalf("Append prod #2: %v", err)
	}
	_, prodMessages, err := dir.Load(ctx, first.ID)
	if err != nil {
		t.Fatalf("Load prod: %v", err)
	}
	if got := textsOf(prodMessages); strings.Join(got, ",") != "p1,p2" {
		t.Fatalf("prod = %v", got)
	}
	_, stagingMessages, err := dir.Load(ctx, second.ID)
	if err != nil {
		t.Fatalf("Load staging: %v", err)
	}
	if got := textsOf(stagingMessages); strings.Join(got, ",") != "s1" {
		t.Fatalf("staging = %v", got)
	}
	if _, err := dir.Replace(ctx, second.ID, []rig.Message{conflictMessage(t, "s2")}); err != nil {
		t.Fatalf("Replace staging: %v", err)
	}
	if err := dir.Delete(ctx, first.ID); err != nil {
		t.Fatalf("Delete prod: %v", err)
	}
	if _, _, err := dir.Load(ctx, first.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("删除后 Load(prod) = %v，want ErrNotFound", err)
	}
	if _, remaining, err := dir.Load(ctx, second.ID); err != nil {
		t.Fatalf("Delete 之后 Load(staging): %v", err)
	} else if got := textsOf(remaining); strings.Join(got, ",") != "s2" {
		t.Fatalf("staging after delete = %v", got)
	}
}

// TestCaseVaryingIDsStaySeparateOnCaseSensitiveFS 钉住反向：大小写不同的两个 ID 在
// **大小写敏感**的文件系统上本来就是两个会话，归属校验不得把它们判成冲突
// （折叠比较必须用 os.SameFile 求证，不能只看字符串）。
func TestCaseVaryingIDsStaySeparateOnCaseSensitiveFS(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "sessions")
	dir, err := store.NewDir(root)
	if err != nil {
		t.Fatalf("NewDir: %v", err)
	}
	defer func() { _ = dir.Close() }()

	upper, err := dir.Create(ctx, store.Meta{ID: "Prod", Title: "大写"})
	if err != nil {
		t.Fatalf("Create Prod: %v", err)
	}
	// 先判断本机文件系统是否区分大小写：不区分时两个名字是同一个文件，本用例的
	// 前提不成立（那种平台由 TestAliasedSessionFileIsRejected 覆盖）。
	if _, statErr := os.Stat(filepath.Join(root, "prod.ndjson")); statErr == nil {
		t.Skip("文件系统大小写不敏感，本用例的前提不成立")
	}
	lower, err := dir.Create(ctx, store.Meta{ID: "prod", Title: "小写"})
	if err != nil {
		t.Fatalf("Create prod: %v", err)
	}
	if _, err := dir.Append(ctx, upper.ID, conflictMessage(t, "U1")); err != nil {
		t.Fatalf("Append Prod: %v", err)
	}
	if _, err := dir.Append(ctx, lower.ID, conflictMessage(t, "L1")); err != nil {
		t.Fatalf("Append prod: %v", err)
	}
	if _, messages, err := dir.Load(ctx, upper.ID); err != nil {
		t.Fatalf("Load Prod: %v", err)
	} else if got := textsOf(messages); strings.Join(got, ",") != "U1" {
		t.Fatalf("Prod = %v，want [U1]", got)
	}
	if _, messages, err := dir.Load(ctx, lower.ID); err != nil {
		t.Fatalf("Load prod: %v", err)
	} else if got := textsOf(messages); strings.Join(got, ",") != "L1" {
		t.Fatalf("prod = %v，want [L1]", got)
	}
}
