package sqlite_test

// CanOpen 是"打开就报错"的死胡同的替代：它只**预检**、不迁移、不写一个字节，
// 然后把结论说全——能不能读、库是哪个版本、要不要升级、是不是比程序新，以及
// 降级场景下**哪三种动作是可行的**。

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sqlitestore "github.com/zeokeel/zeokeel-rig/session/sqlite"
)

// mustCanOpen 预检一个库并要求它没有预检层面的失败。
func mustCanOpen(t *testing.T, path string) sqlitestore.Verdict {
	t.Helper()
	verdict, err := sqlitestore.CanOpen(path)
	if err != nil {
		t.Fatalf("CanOpen(%s): %v", path, err)
	}
	return verdict
}

// hasAction 判断三选一里有没有这一项。
func hasAction(actions []sqlitestore.Action, want sqlitestore.Action) bool {
	for _, action := range actions {
		if action == want {
			return true
		}
	}
	return false
}

// TestCanOpenReportsTooNew 是降级场景的核心：新版本程序写的库，回滚后的旧程序
// 打不开它——但**必须能预检、能导出、能只读打开**，而不是一块砖头。
func TestCanOpenReportsTooNew(t *testing.T) {
	ctx := context.Background()
	db, path := openTemp(t, sqlitestore.Options{})
	if _, err := db.Append(ctx, "keep-me", userMessage("新版本写下的会话")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	core := mustCanOpen(t, path).CoreVersion
	if core <= 0 {
		t.Fatalf("Verdict.CoreVersion = %d", core)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// 模拟"这个库是更新版本的构建写的"。
	toolNew := core + 1
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	if _, err := raw.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, toolNew)); err != nil {
		_ = raw.Close()
		t.Fatalf("bump user_version: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw: %v", err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat before: %v", err)
	}

	verdict := mustCanOpen(t, path)
	if !verdict.TooNew {
		t.Fatalf("TooNew = false; verdict = %+v", verdict)
	}
	if verdict.NeedsUpgrade {
		t.Fatalf("TooNew 的库不该同时报 NeedsUpgrade；verdict = %+v", verdict)
	}
	if verdict.DatabaseVersion != toolNew || verdict.CoreVersion != core {
		t.Fatalf("版本报告 = 库 v%d / 程序 v%d, want v%d / v%d",
			verdict.DatabaseVersion, verdict.CoreVersion, toolNew, core)
	}
	// 降级必须给出三选一：升级程序 / 先导出备份 / 只读打开。
	for _, want := range []sqlitestore.Action{
		sqlitestore.ActionUpgradeBuild,
		sqlitestore.ActionBackupFirst,
		sqlitestore.ActionReadOnly,
	} {
		if !hasAction(verdict.Actions, want) {
			t.Fatalf("TooNew 缺少动作 %q；Actions = %v", want, verdict.Actions)
		}
	}
	if strings.TrimSpace(verdict.Reason) == "" {
		t.Fatal("TooNew 没有给出 Reason（宿主 UI 没东西可显示）")
	}
	// 读仍是可用的：只读打开 + 导出是"先备份"的前提。
	if !verdict.Readable {
		t.Fatalf("TooNew 的库仍应可读（Readable=false）；verdict = %+v", verdict)
	}

	// 预检**不能**破坏库：文件字节数不变、版本号不变、数据还在。
	after, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat after: %v", err)
	}
	if got := rawUserVersion(t, path); got != toolNew {
		t.Fatalf("预检之后 user_version = %d, want %d", got, toolNew)
	}
	if after.Size() != before.Size() {
		t.Fatalf("预检改动了文件大小：%d → %d", before.Size(), after.Size())
	}
	if got := rawSessionCount(t, path); got != 1 {
		t.Fatalf("预检之后会话数 = %d, want 1", got)
	}
	// Open 仍然拒绝（旧二进制不该写新库），但错误要让调用方知道去用 CanOpen。
	if _, err := sqlitestore.Open(sqlitestore.Options{Path: path}); err == nil {
		t.Fatal("Open(a too-new database) = nil error, want a refusal")
	} else if !strings.Contains(err.Error(), "CanOpen") {
		t.Fatalf("Open 的拒绝理由没有指向 CanOpen: %v", err)
	}
}

// TestCanOpenOnTheCurrentBuild covers the ordinary case: same version, nothing to do.
func TestCanOpenOnTheCurrentBuild(t *testing.T) {
	db, path := openTemp(t, sqlitestore.Options{})
	_ = db
	verdict := mustCanOpen(t, path)
	if !verdict.Readable || verdict.TooNew || verdict.NeedsUpgrade || verdict.NeedsCreate {
		t.Fatalf("当前版本库的结论 = %+v", verdict)
	}
	if verdict.DatabaseVersion != verdict.CoreVersion {
		t.Fatalf("DatabaseVersion = %d, CoreVersion = %d", verdict.DatabaseVersion, verdict.CoreVersion)
	}
	if !hasAction(verdict.Actions, sqlitestore.ActionOpen) {
		t.Fatalf("Actions = %v, want %q", verdict.Actions, sqlitestore.ActionOpen)
	}
}

// TestCanOpenReportsAPendingUpgrade：老库要升级——预检要说清"先备份再升级"，
// 并且**不**自己动手（user_version 与列都保持原样）。
func TestCanOpenReportsAPendingUpgrade(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v1.sqlite")
	buildV1Database(t, path)

	verdict := mustCanOpen(t, path)
	if !verdict.NeedsUpgrade || verdict.TooNew || !verdict.Readable {
		t.Fatalf("v1 库的结论 = %+v", verdict)
	}
	if verdict.DatabaseVersion != 1 {
		t.Fatalf("DatabaseVersion = %d, want 1", verdict.DatabaseVersion)
	}
	if verdict.DatabaseVersion >= verdict.CoreVersion {
		t.Fatalf("NeedsUpgrade 但版本 = 库 v%d / 程序 v%d", verdict.DatabaseVersion, verdict.CoreVersion)
	}
	if !hasAction(verdict.Actions, sqlitestore.ActionBackupFirst) {
		t.Fatalf("升级前缺少「先备份」动作；Actions = %v", verdict.Actions)
	}
	// 预检不许动库：版本号还是 1，format_version 还没加。
	if got := rawUserVersion(t, path); got != 1 {
		t.Fatalf("预检之后 user_version = %d, want 1", got)
	}
	if has(rawSessionColumns(t, path), "format_version") {
		t.Fatal("预检自己执行了迁移（format_version 已被加上）")
	}
}

// TestCanOpenOnAMissingFile：库还不存在是**正常**的第一次启动，不是错误。
func TestCanOpenOnAMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not-yet.sqlite")
	verdict := mustCanOpen(t, path)
	if !verdict.NeedsCreate || !verdict.Readable || verdict.TooNew || verdict.NeedsUpgrade {
		t.Fatalf("不存在的库的结论 = %+v", verdict)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("预检把库文件创建出来了：%v", err)
	}
}

// TestCanOpenRequiresAPath：路径缺失是调用方的程序错误，不是库的问题。
func TestCanOpenRequiresAPath(t *testing.T) {
	if _, err := sqlitestore.CanOpen("   "); err == nil {
		t.Fatal("CanOpen(blank) = nil error, want an error")
	}
}

// TestCanOpenOnACorruptFile：文件读不出来要报成"不可读"，而不是让宿主拿着
// 一个看起来正常的 Verdict 去 Open。
func TestCanOpenOnACorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "garbage.sqlite")
	if err := os.WriteFile(path, []byte("这不是一个 SQLite 数据库，只是一段文本。"), 0o600); err != nil {
		t.Fatalf("write garbage: %v", err)
	}
	verdict, err := sqlitestore.CanOpen(path)
	if err != nil {
		t.Fatalf("CanOpen(corrupt) 不该返回预检失败：%v", err)
	}
	if verdict.Readable {
		t.Fatalf("损坏的文件被判为可读：%+v", verdict)
	}
	if verdict.TooNew || verdict.NeedsUpgrade || verdict.NeedsCreate {
		t.Fatalf("损坏的文件不该有迁移结论：%+v", verdict)
	}
	if strings.TrimSpace(verdict.Reason) == "" {
		t.Fatal("不可读时没有 Reason")
	}
}

// TestTooNewDatabaseIsStillExportable 是"三选一真的可执行"的证明：库比程序新时
// Open 拒绝，但 OpenReadOnly + Export 能把数据完整拿出来——否则 CanOpen 给的
// "先导出备份"就是个点不动的按钮。
func TestTooNewDatabaseIsStillExportable(t *testing.T) {
	ctx := context.Background()
	db, path := openTemp(t, sqlitestore.Options{})
	meta, err := db.Create(ctx, sqlitestore.Meta{Title: "回滚前的会话"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := db.AppendBatch(ctx, meta.ID, userMessage("这条必须能导出"), assistantMessage("好的")); err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}
	core := mustCanOpen(t, path).CoreVersion
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	if _, err := raw.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, core+1)); err != nil {
		_ = raw.Close()
		t.Fatalf("bump user_version: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw: %v", err)
	}

	// 写打开仍然拒绝（安全），只读打开必须成功。
	if _, err := sqlitestore.Open(sqlitestore.Options{Path: path}); err == nil {
		t.Fatal("Open(too-new) = nil error")
	}
	ro, err := sqlitestore.OpenReadOnly(path)
	if err != nil {
		t.Fatalf("OpenReadOnly(too-new): %v", err)
	}
	t.Cleanup(func() { _ = ro.Close() })

	dir := filepath.Join(t.TempDir(), "downgrade-backup")
	if err := ro.Export(ctx, dir); err != nil {
		t.Fatalf("只读 Export: %v", err)
	}
	// 导出内容要能被一个新库吃回去（这才是"备份可用"的定义）。
	fresh, err := sqlitestore.Open(sqlitestore.Options{Path: filepath.Join(t.TempDir(), "restored.sqlite")})
	if err != nil {
		t.Fatalf("Open(restore target): %v", err)
	}
	t.Cleanup(func() { _ = fresh.Close() })
	if _, err := sqlitestore.ImportDir(ctx, fresh, sqlitestore.ImportOptions{Dir: dir}); err != nil {
		t.Fatalf("ImportDir: %v", err)
	}
	_, messages, err := fresh.Load(ctx, meta.ID)
	if err != nil {
		t.Fatalf("Load(restored): %v", err)
	}
	if len(messages) != 2 || messages[0].Text() != "这条必须能导出" {
		t.Fatalf("恢复出来的消息 = %+v", messages)
	}
	// 只读打开之后写必须失败（否则"只读"是假承诺）。
	if _, err := ro.Append(ctx, meta.ID, userMessage("写入应该被拒绝")); err == nil {
		t.Fatal("只读 Store 的 Append = nil error")
	}
	// 关一个只读库不该返回错误（Store.Close 的 checkpoint 在只读连接上必然失败，
	// ReadOnlyStore.Close 就是为这件事存在的）。
	if err := ro.Close(); err != nil {
		t.Fatalf("只读 Close: %v", err)
	}
	// 而且预检不能因此把库的版本改回去。
	if got := rawUserVersion(t, path); got != core+1 {
		t.Fatalf("只读导出之后 user_version = %d, want %d", got, core+1)
	}
}

// rawUserVersion 直接读 user_version，绕开 Store（预检用例要在"还没升级"时用）。
func rawUserVersion(t *testing.T, path string) int {
	t.Helper()
	raw, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	defer func() { _ = raw.Close() }()
	var version int
	if err := raw.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	return version
}

// rawSessionCount 数一遍会话行，用来证明预检没删数据。
func rawSessionCount(t *testing.T, path string) int {
	t.Helper()
	raw, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	defer func() { _ = raw.Close() }()
	var count int
	if err := raw.QueryRow(`SELECT count(*) FROM rig_sessions`).Scan(&count); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	return count
}
