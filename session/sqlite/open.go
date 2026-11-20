package sqlite

// Package sqlite 把一个 agent 会话写下的**所有东西**——消息、事件日志、中断检
// 查点以及将来任何特性要持久化的内容——存进**一个** SQLite 数据库，而不是一堆
// 散落的文件目录。
//
// # 文件归宿主所有
//
// 数据库路径由调用方给出；本包从不自己发明路径，也从不写到别处。SDK 自作主张挑
// 存储位置，等于把宿主的数据塞进一个宿主不备份、不迁移、不清理的地方。
//
// # 为什么要用数据库
//
// "一个会话一个文件"的目录只能回答"把这一个会话读出来"。产品还会问：最近改过
// 哪些会话、每个会话有多少条消息、上周我写的那段话在哪、这一轮的事件按类型列出
// 来、把最后一次写入撤掉。这些都是**查询**，数据库用索引回答，目录只能全量扫。
//
// # 它不是什么
//
// 这个存储**不含策略**：它不决定何时压缩上下文、删哪些会话、一条消息意味着什么。
// 它只是把 Rig 其余部分已经定义好的值，放到一个持久、可并发、崩溃安全的地方。
//
// # 扩展表结构
//
// 宿主特性需要自己的表时注册一个 Extension：扩展在核心表结构之后执行，记在独立
// 的账本里，只执行一次；核心表结构不会因为宿主加了东西而改变。
//
// # 保护路径与关闭语义（调用方须知）
//
// Close 之后**所有**导出方法都返回 ErrClosed（不会 panic）；Close 本身可重复调用。
// 写路径在数据库被别的连接锁住时会**有限重试**，重试仍失败则返回可用 errors.Is
// 判定的 ErrBusy。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite" // 纯 Go 驱动：整个模块保持"无 cgo"这条门禁
)

// ErrNoPath 表示 Open 时宿主没有告诉数据库放在哪里。
var ErrNoPath = errors.New("sqlite: Options.Path is required; the host chooses the database location")

// ErrBusy 表示"另一个连接/进程正握着写锁"，重试之后仍然拿不到。
//
// 它与 SQLite 的 SQLITE_BUSY（"database is locked"）一一对应，抽象成哨兵是为了让
// 宿主能分支处理（例如"稍后重试一次"），而不必去匹配驱动的错误文案。
var ErrBusy = errors.New("sqlite: database is locked by another connection")

// Options 配置 Open。零值不可用：Path 是必填项。
type Options struct {
	// Path 是宿主选定的数据库文件。它所在的**目录必须已存在**；本包创建文件，
	// 但不创建上层的目录（创建目录属于宿主的部署动作）。
	Path string

	// Extensions 是核心表之后要应用的宿主 schema，按传入顺序执行；名字在一次
	// 调用内必须唯一，应用成功后记进账本，不会重复执行。
	Extensions []Extension

	// BusyTimeout 限制一次写入等锁多久。零值使用 DefaultBusyTimeout。
	BusyTimeout time.Duration

	// Workspace 是本 store 认领的工作区 ID：非空时 `Memory()` 返回的 wiki 只读写
	// 该工作区的页（Index/Read/Write/Append/Delete/Search 都只作用于它）。
	//
	// 为什么要这一维：一个库常常被多个仓库共用，而文件后端（internal/memwiki）
	// 早就按 `<状态目录>/memory/<workspaceID>` 分区；数据库后端此前是全局命名
	// 空间，于是 A 仓的记忆会出现在 B 仓（跨仓串味）。
	//
	// 空串保持既有行为：不按工作区过滤，全部可见（升级前写下的老行 workspace_id
	// 为空串，也仍然读得出来）。
	Workspace string
}

// DefaultBusyTimeout 是未设置 Options.BusyTimeout 时写者等锁的时长。
//
// 桌面产品里"一个窗口 + 一个后台任务"同时开着同一个文件是常态而不是异常，所以
// 默认给一个足够宽的等待窗口；等不到再由写路径的有限重试兜一次。
const DefaultBusyTimeout = 5 * time.Second

// Extension 是某个宿主特性的一组 schema：命名的语句、按顺序、在核心 schema 之后
// 只执行一次。
type Extension struct {
	// Name 在账本里标识这个扩展，约定写成 "<宿主>/<特性>"。不能为空，且在同一次
	// Open 内必须唯一。
	Name string

	// Statements 在一个事务里按顺序执行一次。请写成幂等的（CREATE TABLE IF NOT
	// EXISTS …）：宿主手工删掉账本之后必须能重新应用。
	Statements []string
}

// dbHandle 是数据库句柄的并发安全外壳。
//
// 为什么不是直接放一个 *sql.DB 字段：Close 会把句柄置空，而其余方法在关闭之后
// 仍然可能被调用（UI 的迟到刷新、后台任务收尾）。裸字段读写下 `-race` 会报
// DATA RACE，接着就是空指针 SIGSEGV。这里用原子指针 + 四个转发方法，让
// `s.db.ExecContext(...)` 这类既有写法保持不变，同时做到：
//   - 关闭之后返回 ErrClosed，而不是 panic；
//   - 读句柄本身无锁、无撕裂。
type dbHandle struct {
	ptr atomic.Pointer[sql.DB]
	// closed 与 ptr 分开记：Close 之后 ptr **保持非 nil**，只是标记关闭。
	//
	// 为什么不能把 ptr 置 nil：`QueryRowContext` 在关闭时若能返回 nil，调用方紧接着
	// `.Scan(...)` 就是空指针 panic——而 Close 与"已经通过入口检查、正要使用句柄"的
	// 调用之间本来就有窗口（关窗口时后台任务还在收尾）。保留句柄后，这种窗口退化成
	// database/sql 自己的 `sql: database is closed` 错误，不会崩。
	closed atomic.Bool
}

// store 保存句柄并清掉关闭标记。
func (h *dbHandle) store(db *sql.DB) {
	h.closed.Store(false)
	h.ptr.Store(db)
}

// markClosed 标记已关闭并返回当前句柄（供 Close 真正关闭连接池）。
func (h *dbHandle) markClosed() *sql.DB {
	h.closed.Store(true)
	return h.ptr.Load()
}

// load 返回当前句柄；已关闭时返回 ErrClosed。
func (h *dbHandle) load() (*sql.DB, error) {
	db := h.ptr.Load()
	if db == nil || h.closed.Load() {
		return nil, ErrClosed
	}
	return db, nil
}

// ExecContext 转发到真正的句柄；关闭之后返回 ErrClosed。
func (h *dbHandle) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	db, err := h.load()
	if err != nil {
		return nil, err
	}
	return db.ExecContext(ctx, query, args...)
}

// QueryContext 转发到真正的句柄；关闭之后返回 ErrClosed。
func (h *dbHandle) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	db, err := h.load()
	if err != nil {
		return nil, err
	}
	return db.QueryContext(ctx, query, args...)
}

// QueryRowContext 转发到真正的句柄；关闭之后返回一个必定报 ErrClosed 的行。
func (h *dbHandle) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	// 注意：这里**不能**在关闭时返回 nil（调用方会直接 .Scan() 空指针崩溃）。
	// 保留句柄转发出去，由 database/sql 返回 `sql: database is closed`。
	if db := h.ptr.Load(); db != nil {
		return db.QueryRowContext(ctx, query, args...)
	}
	// 从未 store 过（理论上只在构造失败路径上出现）：退回到一个必然报错的行。
	return (&sql.DB{}).QueryRowContext(ctx, query, args...)
}

// BeginTx 转发到真正的句柄；关闭之后返回 ErrClosed。
func (h *dbHandle) BeginTx(ctx context.Context, opts *sql.TxOptions) (*sql.Tx, error) {
	db, err := h.load()
	if err != nil {
		return nil, err
	}
	return db.BeginTx(ctx, opts)
}

// Store 是一个已打开的数据库。
//
// 句柄用原子指针保存：Close 与其余方法可以并发调用，不需要调用方加锁。
type Store struct {
	db  dbHandle
	dsn string
	// path 是宿主给的库文件路径。Open 之后要按它把三件套（.db/-wal/-shm）收紧到
	// 0600（见 perm.go），否则"关掉数据库再恢复"这类后续动作就没有路径可用。
	path string
	// workspace 是 Options.Workspace：Memory() 的工作区视图（空串 = 全部可见）。
	workspace string
}

// Schema 是数据库当前的结构：核心版本号、表与索引清单，以及 sessions 表的列。
// 宿主用它确认自己的扩展确实被应用了；测试用它钉住契约。
type Schema struct {
	Version         int
	Tables          []string
	Indexes         []string
	Extensions      []string
	SessionsColumns []string
}

// Open 打开（必要时创建）opts.Path 指向的数据库，并把表结构升到最新版本。
func Open(opts Options) (*Store, error) {
	path := strings.TrimSpace(opts.Path)
	if path == "" {
		return nil, ErrNoPath
	}
	if err := validateExtensions(opts.Extensions); err != nil {
		return nil, err
	}
	timeout := opts.BusyTimeout
	if timeout <= 0 {
		timeout = DefaultBusyTimeout
	}
	dsn := "file:" + path + "?_pragma=busy_timeout(" + fmt.Sprint(timeout.Milliseconds()) + ")" +
		"&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("sqlite: open %s: %w", path, err)
	}
	// 同一时刻只允许一个写者，避免并发运行时 WAL 无限增长；读者仍然并行。
	// 桌面流量小且突发，串行写不会成为瓶颈。
	db.SetMaxOpenConns(1)
	store := &Store{dsn: dsn, path: path, workspace: strings.TrimSpace(opts.Workspace)}
	store.db.store(db)
	if err := store.migrate(context.Background(), opts.Extensions); err != nil {
		_ = db.Close()
		store.db.markClosed()
		return nil, err
	}
	// 权限收紧必须在 migrate **之后**：-wal/-shm 是进入 WAL 模式、写第一条 DDL 时
	// 才出现的，早于它们存在的 chmod 会全部打空。收紧失败就让 Open 失败——库里有
	// 会话正文，"打不开"远比"打开了一个同机其他账户能读的文件"好。（Windows 上
	// chmodStateFiles 直接返回 nil，见 perm.go。）
	if err := chmodStateFiles(path); err != nil {
		_ = db.Close()
		store.db.markClosed()
		return nil, err
	}
	return store, nil
}

// validateExtensions 拒绝那些会"静默跑偏"的形状：空名字、重名、或者一条语句都没
// 有的扩展。
func validateExtensions(exts []Extension) error {
	seen := make(map[string]bool, len(exts))
	for _, ext := range exts {
		name := strings.TrimSpace(ext.Name)
		if name == "" {
			return errors.New("sqlite: extension name must not be blank")
		}
		if seen[name] {
			return fmt.Errorf("sqlite: duplicate extension %q", name)
		}
		seen[name] = true
		if len(ext.Statements) == 0 {
			return fmt.Errorf("sqlite: extension %q has no statements", name)
		}
	}
	return nil
}

// migrate 应用核心 schema，再应用账本里还没记录的每一个扩展。
func (s *Store) migrate(ctx context.Context, exts []Extension) error {
	db, err := s.handle()
	if err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite: begin schema: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	version, err := probeVersion(ctx, tx)
	if err != nil {
		return err
	}
	if version > coreVersion {
		// 回滚即砖头的老问题：这里仍然拒绝（安全第一，旧二进制不该写新库），但错误
		// 信息必须给出**可执行**的下一步，而不是只说一句"太新"。宿主在 Open 之前
		// 应该先调 CanOpen 拿三选一动作。
		return fmt.Errorf("sqlite: database schema v%d is newer than this build's v%d: "+
			"call sqlite.CanOpen to choose between upgrading the program, exporting a backup first, or opening read-only", version, coreVersion)
	}
	// 分段迁移：从当前版本逐段推进到 coreVersion，每段执行完才写 user_version。
	if _, err := applyCoreMigrations(ctx, tx, version); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, extensionLedger); err != nil {
		return fmt.Errorf("sqlite: create extension ledger: %w", err)
	}
	if len(exts) > 0 {
		applied := make(map[string]bool, len(exts))
		rows, err := tx.QueryContext(ctx, `SELECT name FROM rig_schema_extensions`)
		if err != nil {
			return fmt.Errorf("sqlite: read extension ledger: %w", err)
		}
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				_ = rows.Close()
				return fmt.Errorf("sqlite: read extension ledger: %w", err)
			}
			applied[name] = true
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return fmt.Errorf("sqlite: read extension ledger: %w", err)
		}
		_ = rows.Close()
		for _, ext := range exts {
			name := strings.TrimSpace(ext.Name)
			if applied[name] {
				continue
			}
			for _, stmt := range ext.Statements {
				if _, err := tx.ExecContext(ctx, stmt); err != nil {
					return fmt.Errorf("sqlite: apply extension %q: %w", name, err)
				}
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO rig_schema_extensions(name, applied_at) VALUES(?, ?)`,
				name, time.Now().UnixMilli()); err != nil {
				return fmt.Errorf("sqlite: record extension %q: %w", name, err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlite: commit schema: %w", err)
	}
	// 从 v6 之前的库升级上来时，老消息的解码列是空的：一次性补齐（此后 version
	// 已是 coreVersion，不再扫表）。放在提交之后用独立连接做，避免在 DDL 事务里
	// 遍历结果集；失败不影响打开（检索会退化成 payload 兜底，数据仍在），宿主也
	// 可以再手动调 BackfillSearchText。
	if version > 0 && version < 6 {
		if _, backfillErr := s.BackfillSearchText(ctx); backfillErr != nil {
			// 回填失败不该让 Open 失败（数据仍在，检索退化为 payload 兜底），但绝
			// 不能静默：宿主需要知道"这次升级之后检索是降级的"。
			slog.Default().Warn("sqlite: backfill search text after upgrade failed",
				"error", backfillErr)
		}
	}
	return nil
}

// handle 返回可用的数据库句柄；关闭之后返回 ErrClosed。
//
// 所有导出方法都必须先过这里：Close 会把原子指针置空，于是"关闭之后调用"得到的是
// 一个可判定的错误，而不是空指针解引用（后者在并发 CI 上是随机 SIGSEGV）。
func (s *Store) handle() (*sql.DB, error) {
	if s == nil {
		return nil, ErrClosed
	}
	return s.db.load()
}

// DSN 返回本 store 打开时用的连接串，供宿主再开一条带同样 pragma 的连接。
func (s *Store) DSN() string { return s.dsn }

// DB 返回底层句柄，供宿主特性执行本 store 没有提供的查询。
//
// 句柄同一时刻只允许一个写者，所以从多个 goroutine 使用是安全的。**关闭之后返回
// nil**：调用方要么在关闭前用完，要么自己判断 nil。
func (s *Store) DB() *sql.DB {
	db, err := s.handle()
	if err != nil {
		return nil
	}
	return db
}

// Schema 报告数据库当前的结构。
func (s *Store) Schema(ctx context.Context) (Schema, error) {
	var schema Schema
	if err := s.usable(); err != nil {
		return Schema{}, err
	}
	db, err := s.handle()
	if err != nil {
		return Schema{}, err
	}
	if err := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&schema.Version); err != nil {
		return Schema{}, fmt.Errorf("sqlite: read schema version: %w", err)
	}
	tables, indexes, err := s.objects(ctx)
	if err != nil {
		return Schema{}, err
	}
	schema.Tables, schema.Indexes = tables, indexes
	if columns, err := s.columns(ctx, "rig_sessions"); err == nil {
		schema.SessionsColumns = columns
	}
	rows, err := db.QueryContext(ctx, `SELECT name FROM rig_schema_extensions ORDER BY name`)
	if err == nil {
		for rows.Next() {
			var name string
			if scanErr := rows.Scan(&name); scanErr == nil {
				schema.Extensions = append(schema.Extensions, name)
			}
		}
		_ = rows.Close()
	}
	return schema, nil
}

// objects 从系统表里列出表名与索引名。
func (s *Store) objects(ctx context.Context) (tables, indexes []string, err error) {
	db, err := s.handle()
	if err != nil {
		return nil, nil, err
	}
	rows, err := db.QueryContext(ctx,
		`SELECT type, name FROM sqlite_master WHERE type IN ('table','index') AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		return nil, nil, fmt.Errorf("sqlite: list schema: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var kind, name string
		if err := rows.Scan(&kind, &name); err != nil {
			return nil, nil, fmt.Errorf("sqlite: list schema: %w", err)
		}
		switch kind {
		case "table":
			tables = append(tables, name)
		case "index":
			indexes = append(indexes, name)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("sqlite: list schema: %w", err)
	}
	sort.Strings(tables)
	sort.Strings(indexes)
	return tables, indexes, nil
}

// columns 列出一张表的列名。
func (s *Store) columns(ctx context.Context, table string) ([]string, error) {
	db, err := s.handle()
	if err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, `SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return nil, fmt.Errorf("sqlite: columns of %s: %w", table, err)
	}
	defer func() { _ = rows.Close() }()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("sqlite: columns of %s: %w", table, err)
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

// Close 释放数据库句柄。重复调用是安全的（幂等）。
//
// 关闭之后所有导出方法都返回 ErrClosed；已经发出去的查询会各自失败，不会被本方法
// 在锁上等住。
//
// # 关闭前先 checkpoint（顺序不可交换）
//
// Close 在真正关连接池之前执行 `PRAGMA wal_checkpoint(TRUNCATE)`：把 -wal 里已提交
// 的帧并回主库并截断 WAL。**没有这一步，用户"只拷 .db"（不带 -wal/-shm）拿到的是
// 缺最近提交的旧快照，拷贝落在写入中途时甚至是 malformed**——它看起来像备份，其实
// 不是（要一个"拷走就能用"的副本请用 Backup / VACUUM INTO）。
//
// 顺序是硬要求：checkpoint 必须在 markClosed() **之前**跑。markClosed 之后句柄只剩
// "已关闭"语义，任何通过 s.db 的调用都返回 ErrClosed，checkpoint 会被静默跳过，
// 而且不会有任何测试失败——所以两者写在同一处，并保持这个先后。
//
// checkpoint 失败不阻断关闭（数据还在 -wal 里，下次打开会重放），但错误会被返回：
// 宿主需要知道"这个库现在拷不得"。
func (s *Store) Close() error {
	if s == nil {
		return nil
	}
	var checkpointErr error
	if db, err := s.handle(); err == nil {
		// 用一条独立的、可被取消的语句执行 checkpoint：它要等所有并发读者/写者
		// 让出连接，不能无限期挂住关停路径。
		ctx, cancel := context.WithTimeout(context.Background(), DefaultBusyTimeout)
		var busy, logFrames, checkpointed int
		err := db.QueryRowContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`).
			Scan(&busy, &logFrames, &checkpointed)
		cancel()
		if err == nil && busy != 0 {
			err = ErrBusy
		}
		if err != nil {
			checkpointErr = fmt.Errorf("sqlite: close: wal_checkpoint: %w", err)
		}
	}
	db := s.db.markClosed()
	if db == nil {
		return checkpointErr
	}
	if err := db.Close(); err != nil {
		if checkpointErr != nil {
			return errors.Join(checkpointErr, fmt.Errorf("sqlite: close: %w", err))
		}
		return fmt.Errorf("sqlite: close: %w", err)
	}
	return checkpointErr
}
