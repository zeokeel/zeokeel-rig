package sqlite

// 分段的 core schema 迁移执行器。
//
// 旧实现是"一条扁平列表 + 一个 `if user_version < coreVersion` 全量重跑"，
// 每条语句都是 CREATE IF NOT EXISTS。它对**已经存在的表**加列完全无效：
// 语句成功、版本号前进、运行期 `no such column`。本文件把执行拆成
// "从当前 user_version 逐段推进到 coreVersion"，并且给 ALTER 加了列探测。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// rowQuerier 是 `PRAGMA user_version` 这类单行查询需要的最小接口。
//
// 抽出来是为了让预检（CanOpen，只有一个 *sql.DB）与迁移（事务里的 *sql.Tx）
// 共用同一段读版本号的代码。
type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// probeVersion 读数据库的 user_version。
//
// 它**不做任何写操作**：CanOpen 会在只读连接上调用它。
func probeVersion(ctx context.Context, q rowQuerier) (int, error) {
	var version int
	if err := q.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return 0, fmt.Errorf("sqlite: read schema version: %w", err)
	}
	return version, nil
}

// applyCoreMigrations 把库从 `from` 逐段推进到 coreVersion，返回执行到的版本号。
//
// 语义：
//   - `from >= coreVersion`：什么都不做（已经是最新；更新的库由调用方拦下）。
//   - `from < coreVersion`：先对**已应用过的段**（1..from）重跑其中的
//     `CREATE ... IF NOT EXISTS`——这是旧实现唯一做对的事（自愈"表在、索引被
//     手工删了"的库，v1 时代的库本来就没有索引），不能因为分段而丢掉。
//   - 未应用的段（from+1..coreVersion）逐段执行，每段执行完立刻写
//     `PRAGMA user_version = 段号`。整个迁移在一个事务里，所以中途失败会整体
//     回滚：不会留下"列加了一半、版本号却前进"的库。
//
// 调用方必须持有写事务（SQLite 的 DDL 需要写锁）。
func applyCoreMigrations(ctx context.Context, tx *sql.Tx, from int) (int, error) {
	if from > coreVersion {
		// 防御性：正常路径上 migrate 已经先拦下"库比程序新"。这里不返回可恢复
		// 错误，因为调用方（Open）会把它当成数据库损坏来处理更危险。
		return from, fmt.Errorf("sqlite: database schema v%d is newer than this build's v%d", from, coreVersion)
	}
	// 自愈：把**已应用过**的段里的幂等语句重跑一遍。它修的是"表/索引被手工删了"
	// 的库，因此对"已经是最新版本"的库也要跑——否则删过表的库会安静地半坏（打开
	// 成功、第一次写入才发现表不存在）。
	for version := 1; version <= min(from, coreVersion); version++ {
		statements, ok := migrations[version]
		if !ok || len(statements) == 0 {
			return from, fmt.Errorf("sqlite: no migration statements for core version %d", version)
		}
		for _, statement := range repairStatements(statements) {
			if err := execMigrationStatement(ctx, tx, statement); err != nil {
				return from, fmt.Errorf("sqlite: repair core schema at v%d: %w", version, err)
			}
		}
	}
	if from == coreVersion {
		return from, nil
	}
	for version := 1; version <= coreVersion; version++ {
		statements, ok := migrations[version]
		if !ok || len(statements) == 0 {
			// 段缺失 = 打包/开发事故。宁可打不开也不能当"没有变更"跳过：跳过等于
			// 把 user_version 写到一个 schema 并不完整的库上。
			return from, fmt.Errorf("sqlite: no migration statements for core version %d", version)
		}
		if version <= from {
			continue
		}
		for _, statement := range statements {
			if err := execMigrationStatement(ctx, tx, statement); err != nil {
				return from, fmt.Errorf("sqlite: migrate core schema to v%d: %w", version, err)
			}
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version = %d`, version)); err != nil {
			return from, fmt.Errorf("sqlite: record schema version %d: %w", version, err)
		}
	}
	return coreVersion, nil
}

// repairStatements 只保留可以无条件重跑的语句：`CREATE ... IF NOT EXISTS`、
// `DROP ... IF EXISTS`，以及 `ALTER TABLE ... ADD COLUMN`（执行器先探测列）。
//
// 为什么 ALTER 也算自愈：`rig_messages` 被手工删掉时，段 1 会用**v1 形状**重建表
// （没有后来加的列），之后段 6 的 `CREATE INDEX ... (search_text)` 就会撞上
// "no such column"。把 ADD COLUMN 一起重跑，重建的表才会补齐后面几段加过的列。
func repairStatements(statements []string) []string {
	idempotent := make([]string, 0, len(statements))
	for _, statement := range statements {
		upper := strings.ToUpper(statement)
		switch {
		case strings.Contains(upper, "IF NOT EXISTS"):
			idempotent = append(idempotent, statement)
		case strings.HasPrefix(upper, "DROP ") && strings.Contains(upper, "IF EXISTS"):
			// 删除也要重放：否则"删掉的索引"会在下一次 Open 的自愈里被前面段的
			// CREATE INDEX 重新建回来（v9 删 rig_messages_search_text 就是这样）。
			idempotent = append(idempotent, statement)
		default:
			if _, _, ok := parseAddColumn(statement); ok {
				idempotent = append(idempotent, statement)
			}
		}
	}
	return idempotent
}

// execMigrationStatement 执行一条迁移语句，并保证它**可以重复执行**。
//
// 绝大多数语句本身就是 `CREATE ... IF NOT EXISTS`。唯一的例外是
// `ALTER TABLE ... ADD COLUMN`：SQLite 不支持它的 IF NOT EXISTS，重跑会报
// `duplicate column name`。所以这里先把语句认出来，再问一遍表里有这一列没有。
func execMigrationStatement(ctx context.Context, tx *sql.Tx, statement string) error {
	if table, column, ok := parseAddColumn(statement); ok {
		exists, err := columnExists(ctx, tx, table, column)
		if err != nil {
			return err
		}
		if exists {
			return nil
		}
	}
	if _, err := tx.ExecContext(ctx, statement); err != nil {
		return fmt.Errorf("%w (statement: %s)", err, firstLineOf(statement))
	}
	return nil
}

// columnExists 报告 table.column 是否已经存在。
func columnExists(ctx context.Context, q rowQuerier, table, column string) (bool, error) {
	var name string
	err := q.QueryRowContext(ctx,
		`SELECT name FROM pragma_table_info(?) WHERE name = ? COLLATE NOCASE LIMIT 1`,
		table, column).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("sqlite: probe column %s.%s: %w", table, column, err)
	}
	return true, nil
}

// parseAddColumn 从 `ALTER TABLE <t> ADD [COLUMN] <c> ...` 里取出表名与列名。
//
// 只认这一种形状（本包的迁移只允许用它加列）；认不出来就返回 ok=false，语句
// 原样执行——这样将来出现别的 ALTER 形状时不会**静默跳过**，而是照旧失败。
func parseAddColumn(statement string) (table, column string, ok bool) {
	fields := splitSQLFields(strings.TrimSpace(statement))
	if len(fields) < 5 {
		return "", "", false
	}
	if !strings.EqualFold(fields[0], "ALTER") || !strings.EqualFold(fields[1], "TABLE") {
		return "", "", false
	}
	// ADD 之后可选 COLUMN：`ALTER TABLE t ADD COLUMN c ...` / `ADD c ...`。
	index := 3
	if strings.EqualFold(fields[index], "ADD") {
		index++
	} else {
		return "", "", false
	}
	if index < len(fields) && strings.EqualFold(fields[index], "COLUMN") {
		index++
	}
	if index >= len(fields) {
		return "", "", false
	}
	table = trimIdentifier(fields[2])
	column = trimIdentifier(fields[index])
	if table == "" || column == "" {
		return "", "", false
	}
	return table, column, true
}

// splitSQLFields 按空白切分，但**不**切引号内的空白：`"col name"` 是一个标识符。
//
// 只覆盖 SQLite 的三类标识符引号（"…"、`…`、[…]）；本包的迁移语句形状固定，
// 不需要完整的 SQL 词法器。
func splitSQLFields(statement string) []string {
	var (
		fields  []string
		current strings.Builder
		quote   rune
	)
	for _, r := range statement {
		switch {
		case quote != 0:
			current.WriteRune(r)
			if (quote == '[' && r == ']') || (quote != '[' && r == quote) {
				quote = 0
			}
		case r == '"' || r == '`' || r == '[':
			quote = r
			current.WriteRune(r)
		case r == ' ' || r == '\t' || r == '\n' || r == '\r':
			if current.Len() > 0 {
				fields = append(fields, current.String())
				current.Reset()
			}
		default:
			current.WriteRune(r)
		}
	}
	if current.Len() > 0 {
		fields = append(fields, current.String())
	}
	return fields
}

// trimIdentifier 去掉标识符外面的引号（"x" / `x` / [x]）。
func trimIdentifier(raw string) string {
	raw = strings.TrimSuffix(raw, ";")
	for _, pair := range [][2]string{{`"`, `"`}, {"`", "`"}, {"[", "]"}} {
		if len(raw) >= 2 && strings.HasPrefix(raw, pair[0]) && strings.HasSuffix(raw, pair[1]) {
			return raw[1 : len(raw)-1]
		}
	}
	return raw
}

// firstLineOf 把多行 DDL 压成一行，便于错误信息里读。
func firstLineOf(statement string) string {
	if index := strings.IndexByte(statement, '\n'); index >= 0 {
		return strings.TrimSpace(statement[:index])
	}
	return strings.TrimSpace(statement)
}
