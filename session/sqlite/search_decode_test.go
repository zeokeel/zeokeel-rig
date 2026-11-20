package sqlite_test

// 检索质量的回归：`payload` 是 `json.Marshal(rig.Message)` 的原始 JSON，
// 所以"对 `CAST(payload AS TEXT)` 做 LIKE"只能搜到**没被 JSON 转义**的片段。
// 正文里的 `"`、`<`、`>`、`&` 会被写成 `\"` / `\u003c` / `\u003e` / `\u0026`，
// 换行写成 `\n`；tool_call 参数里的引号同样被转义，于是"用户写过那句话、搜出来
// 零条"，而且没有任何提示。
//
// 修法是一条解码列 `rig_messages.search_text`：写入时把消息解码成可检索文本
// （正文 + 推理 + 工具名 + 调用参数），查询直接 LIKE 它；升级前写入的老行
// （该列为空）走原 payload 兜底，不丢会话；`BackfillSearchText` 负责一次性补齐。

import (
	"context"
	"database/sql"
	"testing"

	rig "github.com/zeokeel/zeokeel-rig"
	sqlitestore "github.com/zeokeel/zeokeel-rig/session/sqlite"
)

// TestSearchFindsEscapedCharactersInBody 是本次修复的主回归：正文里的引号、
// 尖括号、& 与换行都必须能搜到。
func TestSearchFindsEscapedCharactersInBody(t *testing.T) {
	db, _ := openTemp(t, sqlitestore.Options{})
	ctx := context.Background()
	created, err := db.Create(ctx, sqlitestore.Meta{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	body := "他说\"这里要小心\" <div> 与 a&b\n换行之后的内容"
	if _, err := db.Append(ctx, created.ID, userMessage(body)); err != nil {
		t.Fatalf("Append: %v", err)
	}
	for _, needle := range []string{`"这里要小心"`, "<div>", "a&b", "换行之后"} {
		hits, err := db.SearchMessages(ctx, sqlitestore.SearchQuery{Text: needle})
		if err != nil {
			t.Fatalf("SearchMessages(%q): %v", needle, err)
		}
		if len(hits) != 1 {
			t.Fatalf("搜 %q 得到 %d 条命中，want 1（JSON 转义把正文藏起来了）", needle, len(hits))
		}
		if hits[0].Snippet == "" {
			t.Fatalf("搜 %q 的命中没有摘要：%+v", needle, hits[0])
		}
	}
}

// TestSearchFindsToolCallArguments 覆盖 tool_call：参数以原始 JSON 字符串落进
// search_text，所以带引号的那一段（payload 里是 `\"`）也必须搜得到。
func TestSearchFindsToolCallArguments(t *testing.T) {
	db, _ := openTemp(t, sqlitestore.Options{})
	ctx := context.Background()
	created, err := db.Create(ctx, sqlitestore.Meta{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	call := rig.Message{
		Role:      rig.RoleAssistant,
		Reasoning: "先确认分支再动手",
		ToolCalls: []rig.ToolCall{{
			ID:        "call-1",
			Name:      "bash",
			Arguments: `{"command":"git rebase -i HEAD~3"}`,
		}},
	}
	if _, err := db.Append(ctx, created.ID, call); err != nil {
		t.Fatalf("Append: %v", err)
	}
	// 第三个才是关键：带引号的参数在 payload 里是转义过的，旧查询一条都命中不了。
	for _, needle := range []string{"rebase -i HEAD~3", "先确认分支再动手", `"command":"git`} {
		hits, err := db.SearchMessages(ctx, sqlitestore.SearchQuery{Text: needle})
		if err != nil {
			t.Fatalf("SearchMessages(%q): %v", needle, err)
		}
		if len(hits) != 1 {
			t.Fatalf("搜 %q 得到 %d 条命中，want 1（工具参数/推理没进检索文本）", needle, len(hits))
		}
	}
}

// TestSearchFindsRowsWrittenBeforeDecodedColumn 钉住升级路径：写入时还没有
// search_text 的老行（这里手工清空模拟）必须仍然搜得到。
func TestSearchFindsRowsWrittenBeforeDecodedColumn(t *testing.T) {
	db, path := openTemp(t, sqlitestore.Options{})
	ctx := context.Background()
	created, err := db.Create(ctx, sqlitestore.Meta{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := db.Append(ctx, created.ID, userMessage("升级前写入的老会话正文")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	rawExec(t, path, `UPDATE rig_messages SET search_text = ''`)

	again, err := sqlitestore.Open(sqlitestore.Options{Path: path})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = again.Close() })
	hits, err := again.SearchMessages(ctx, sqlitestore.SearchQuery{Text: "老会话正文"})
	if err != nil {
		t.Fatalf("SearchMessages: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("老行命中 = %d 条，want 1（升级后整段历史都搜不到了）", len(hits))
	}
}

// TestBackfillSearchTextFillsLegacyRowsAndIsIdempotent 覆盖一次性补齐入口：
// 回填之后 search_text 非空、正文能搜到，再跑一次返回 0。
func TestBackfillSearchTextFillsLegacyRowsAndIsIdempotent(t *testing.T) {
	db, path := openTemp(t, sqlitestore.Options{})
	ctx := context.Background()
	created, err := db.Create(ctx, sqlitestore.Meta{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	for _, body := range []string{"第一条 引号\"内容", "第二条 尖括号<内容>"} {
		if _, err := db.Append(ctx, created.ID, userMessage(body)); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	rawExec(t, path, `UPDATE rig_messages SET search_text = ''`)

	again, err := sqlitestore.Open(sqlitestore.Options{Path: path})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = again.Close() })
	updated, err := again.BackfillSearchText(ctx)
	if err != nil {
		t.Fatalf("BackfillSearchText: %v", err)
	}
	if updated != 2 {
		t.Fatalf("回填 %d 行，want 2", updated)
	}
	stored := rawStrings(t, path, `SELECT search_text FROM rig_messages ORDER BY ordinal`)
	if len(stored) != 2 {
		t.Fatalf("读回 %d 行，want 2", len(stored))
	}
	for _, row := range stored {
		if row == "" {
			t.Fatalf("回填之后仍有空的 search_text：rows = %v", stored)
		}
	}
	hits, err := again.SearchMessages(ctx, sqlitestore.SearchQuery{Text: `引号"内容`})
	if err != nil || len(hits) != 1 {
		t.Fatalf("回填后搜不到老行：hits = %d (err %v)", len(hits), err)
	}
	// 幂等：没有新的空行，第二次什么都不写。
	second, err := again.BackfillSearchText(ctx)
	if err != nil {
		t.Fatalf("BackfillSearchText (2nd): %v", err)
	}
	if second != 0 {
		t.Fatalf("第二次回填更新了 %d 行，want 0", second)
	}
}

// TestSearchOffsetPagesWithoutGapsOrRepeats 覆盖翻页：Offset 跳过前 N 条命中，
// 第二页的第一条不能是第一页的末条，也不许漏。
func TestSearchOffsetPagesWithoutGapsOrRepeats(t *testing.T) {
	db, _ := openTemp(t, sqlitestore.Options{})
	ctx := context.Background()
	created, err := db.Create(ctx, sqlitestore.Meta{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	for i := 0; i < 5; i++ {
		if _, err := db.Append(ctx, created.ID, userMessage("分页命中的句子")); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	page := func(offset int) []sqlitestore.MessageHit {
		t.Helper()
		hits, err := db.SearchMessages(ctx, sqlitestore.SearchQuery{Text: "分页命中", Limit: 2, Offset: offset})
		if err != nil {
			t.Fatalf("SearchMessages(offset=%d): %v", offset, err)
		}
		return hits
	}
	first, second, third := page(0), page(2), page(4)
	if len(first) != 2 || len(second) != 2 || len(third) != 1 {
		t.Fatalf("页大小 = %d/%d/%d，want 2/2/1（Limit 是上限）", len(first), len(second), len(third))
	}
	if second[0].Ordinal == first[len(first)-1].Ordinal {
		t.Fatalf("第二页第一条 = 第一页末条（ordinal %d）：翻页重复了", second[0].Ordinal)
	}
	seen := map[int]bool{}
	total := 0
	for _, hits := range [][]sqlitestore.MessageHit{first, second, third} {
		for _, hit := range hits {
			if seen[hit.Ordinal] {
				t.Fatalf("ordinal %d 出现了两次：翻页有重", hit.Ordinal)
			}
			seen[hit.Ordinal] = true
			total++
		}
	}
	if total != 5 {
		t.Fatalf("三页合计 %d 条，want 5（翻页有漏）", total)
	}
}

// rawExec 在**同一个库文件**上执行一条写语句：用来把新列改回"升级前"的样子。
func rawExec(t *testing.T, path, statement string) {
	t.Helper()
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	defer func() { _ = raw.Close() }()
	if _, err := raw.Exec(statement); err != nil {
		t.Fatalf("raw exec (%s): %v", statement, err)
	}
}

// rawStrings 在只读连接上读一列文本，用于钉住"回填写进去了"。
func rawStrings(t *testing.T, path, query string) []string {
	t.Helper()
	raw, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	defer func() { _ = raw.Close() }()
	rows, err := raw.Query(query)
	if err != nil {
		t.Fatalf("raw query (%s): %v", query, err)
	}
	defer func() { _ = rows.Close() }()
	var values []string
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			t.Fatalf("scan: %v", err)
		}
		values = append(values, value)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate rows: %v", err)
	}
	return values
}
