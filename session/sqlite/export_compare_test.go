package sqlite_test

// 往返校验的"元数据维度"。CompareSessions 只比条数与字节时，一次导出/导入把
// 标题换掉也不会被报告——而标题正是目录后端重扫首条用户消息时会丢的那一项。
// 这里先钉住"标题不同必须被看见"。

import (
	"context"
	"testing"

	sqlitestore "github.com/zeokeel/zeokeel-rig/session/sqlite"
)

// seedTitledSession 在两个库里造出**消息完全相同、标题不同**的同一个会话 ID。
func seedTitledSession(t *testing.T, db *sqlitestore.Store, id, title string) {
	t.Helper()
	ctx := context.Background()
	if _, err := db.Create(ctx, sqlitestore.Meta{ID: id, Title: title}); err != nil {
		t.Fatalf("Create(%q): %v", title, err)
	}
	if _, err := db.AppendBatch(ctx, id, userMessage("同一个问题"), assistantMessage("同一个回答")); err != nil {
		t.Fatalf("AppendBatch(%q): %v", title, err)
	}
}

// TestCompareSessionsReportsTitleDifference 钉住"标题也是往返口径"。
//
// 条数与字节一致、只有标题不同时，旧实现报告"一致"——导出目录里标题被改成别的东西
// 这件事就被静默吞掉了。修复之后必须出现一条 Field=title 的差异。
func TestCompareSessionsReportsTitleDifference(t *testing.T) {
	ctx := context.Background()
	want, _ := openTemp(t, sqlitestore.Options{})
	got, _ := openTemp(t, sqlitestore.Options{})
	seedTitledSession(t, want, "session-1", "宿主改过的标题")
	seedTitledSession(t, got, "session-1", "同一个问题")

	report, err := sqlitestore.CompareSessions(ctx, want, got)
	if err != nil {
		t.Fatalf("CompareSessions: %v", err)
	}
	if report.Sessions != 1 || report.Messages != 2 {
		t.Fatalf("规模 = %d 会话 / %d 消息，want 1 / 2", report.Sessions, report.Messages)
	}
	var found bool
	for _, mismatch := range report.Mismatches {
		if mismatch.Field == "title" {
			found = true
			if mismatch.Want != "宿主改过的标题" || mismatch.Got != "同一个问题" {
				t.Fatalf("title 差异 = %+v", mismatch)
			}
		}
	}
	if !found {
		t.Fatalf("标题不同却没有被报告：mismatches = %+v", report.Mismatches)
	}

	// 标题一致时不能有差异（否则这个维度就成了噪声，宿主会学会忽略报告）。
	same, _ := openTemp(t, sqlitestore.Options{})
	seedTitledSession(t, same, "session-1", "宿主改过的标题")
	report, err = sqlitestore.CompareSessions(ctx, want, same)
	if err != nil {
		t.Fatalf("CompareSessions(标题一致): %v", err)
	}
	if len(report.Mismatches) != 0 {
		t.Fatalf("标题一致的往返报告了差异：%+v", report.Mismatches)
	}
}
