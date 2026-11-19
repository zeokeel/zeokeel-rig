package sqlite

// 打开之前的预检：在**不迁移、不写一个字节**的前提下回答"这个库能不能打开、
// 是什么版本、要不要升级、是不是比本程序新"。
//
// 为什么需要它：`Open` 对"库比程序新"只能拒绝，而拒绝之后宿主走投无路——
// 回滚到旧版本的用户既打不开也导不出，库就成了砖头。`CanOpen`
// 把判断提前，并且给出**可执行的下一步**，而不是一条错误信息。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"time"
)

// Action 是预检给出的一个**可执行动作**。它是给宿主 UI 用的：三选一的按钮。
type Action string

const (
	// ActionCreate：库还不存在，Open 会按当前 coreVersion 新建。
	ActionCreate Action = "create"
	// ActionOpen：版本一致，直接 Open。
	ActionOpen Action = "open"
	// ActionBackupFirst：先导出备份（`(*Store).Export` 或拷贝库文件），再升级。
	// 升级是**单向**的：v3 的库回不到 v2，所以先备份不是可选项。
	ActionBackupFirst Action = "backup-first"
	// ActionUpgradeBuild：库比程序新。换回/升级到更新的程序才能以读写方式打开。
	ActionUpgradeBuild Action = "upgrade-build"
	// ActionReadOnly：只读打开（或只读导出）。旧程序读已知列是安全的，写不是。
	ActionReadOnly Action = "read-only"
)

// Verdict 是一次预检的结论。
//
// 三个布尔位的组合就是 UI 要展示的四种状态，以及各自该给的按钮：
//
//	Readable=false                 → 「这个库读不出来」+ 显示 Reason；只给"重试/换个文件"
//	                                 （不提供 Open：点了也只会得到同一个错误）
//	TooNew=true                    → 「这个库比当前程序新」+ 三选一：
//	                                 ① 升级程序（ActionUpgradeBuild，推荐）
//	                                 ② 先导出备份（ActionBackupFirst → OpenReadOnly + Export）
//	                                 ③ 只读打开（ActionReadOnly；写操作会被 SQLite 拒绝）
//	NeedsUpgrade=true              → 「需要升级」+ 主按钮「备份并升级」（ActionBackupFirst），
//	                                 次按钮「直接打开」（ActionOpen；Open 会自动分段迁移）
//	NeedsCreate=true               → 「新建」（ActionCreate）
//	以上皆假                        → 「打开」（ActionOpen）
//
// TooNew 时**不要**把"直接打开（读写）"做成可点的按钮：旧二进制写新库会丢掉新
// 版本才有的字段，那是不可逆的数据损失。
type Verdict struct {
	// Path 是预检的那个路径（已去掉首尾空白）。
	Path string
	// Readable 表示"能不能读到这个库"：只读打开、读版本号、读表结构都成功了。
	//
	// 它与 TooNew 不冲突：库比程序新时**读仍然可以**（旧程序读自己认识的列不受
	// 新表结构影响），所以"先备份再升级"这条路才走得通。
	Readable bool
	// CoreVersion 是**本构建**的 core schema 版本。
	CoreVersion int
	// DatabaseVersion 是库里 `PRAGMA user_version` 的值；文件不存在时为 0。
	DatabaseVersion int
	// NeedsUpgrade 表示库版本低于本构建，Open 会执行分段迁移把它升上来。
	NeedsUpgrade bool
	// TooNew 表示库版本**高于**本构建：本构建不该以读写方式打开它。
	TooNew bool
	// NeedsCreate 表示库文件（或核心表）还不存在，Open 会建库。
	NeedsCreate bool
	// HasCoreTables 表示库里已经有 rig_sessions（用于识别"版本号说最新、表却不在"
	// 的畸形库）。
	HasCoreTables bool
	// Reason 是一句给人看的中文结论，包含下一步该做什么。
	Reason string
	// Actions 是宿主可以直接做成按钮的可执行动作，按推荐顺序排列。
	Actions []Action
}

// CanOpen 预检 path 指向的数据库，**不迁移、不创建、不写任何东西**。
//
// 返回的 error 只表示"预检本身没做成"（路径为空、指向目录、打不开文件）。
// 库内容的问题（损坏、版本太新）通过 Verdict 报告——调用方要据此给用户三选一，
// 那不是"函数调用失败"。
func CanOpen(path string) (Verdict, error) {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return Verdict{CoreVersion: coreVersion}, ErrNoPath
	}
	verdict := Verdict{Path: trimmed, CoreVersion: coreVersion}
	info, err := os.Stat(trimmed)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		verdict.Readable = true
		verdict.NeedsCreate = true
		verdict.Reason = fmt.Sprintf("库还不存在：Open 会按 v%d 新建（不需要迁移，也不会碰任何已有文件）", coreVersion)
		verdict.Actions = []Action{ActionCreate}
		return verdict, nil
	case err != nil:
		verdict.Reason = fmt.Sprintf("读不到库文件（%v）：先检查路径与权限，或换一个位置", err)
		return verdict, nil
	case info.IsDir():
		return verdict, fmt.Errorf("sqlite: %q is a directory, not a database file", trimmed)
	}

	version, hasTables, err := inspectDatabase(trimmed)
	if err != nil {
		verdict.Reason = fmt.Sprintf("这个文件读不出 SQLite 表结构（%v）：它可能损坏、不是数据库，或当前账户没有读权限；请先修复/恢复文件再打开", err)
		return verdict, nil
	}
	verdict.Readable = true
	verdict.DatabaseVersion = version
	verdict.HasCoreTables = hasTables

	switch {
	case version > coreVersion:
		verdict.TooNew = true
		verdict.Reason = fmt.Sprintf("库是 v%d，本程序只到 v%d：升级程序最稳；若暂时升不上，"+
			"先导出备份再从旧版本只读打开，不要用本程序写它（写会丢新字段）", version, coreVersion)
		verdict.Actions = []Action{ActionUpgradeBuild, ActionBackupFirst, ActionReadOnly}
	case !hasTables:
		// 空文件（0 字节）或一个还没有核心表的库：Open 会把核心表建出来。
		verdict.NeedsCreate = true
		verdict.Reason = fmt.Sprintf("库里还没有 Rig 的核心表：Open 会在其中建 v%d 的表（已有别的表不受影响）", coreVersion)
		verdict.Actions = []Action{ActionCreate}
	case version < coreVersion:
		verdict.NeedsUpgrade = true
		verdict.Reason = fmt.Sprintf("库是 v%d，本程序是 v%d：升级是单向的，先导出备份（Export 或拷贝库文件），再 Open（会自动分段迁移）", version, coreVersion)
		verdict.Actions = []Action{ActionBackupFirst, ActionOpen}
	default:
		verdict.Reason = fmt.Sprintf("库与程序同为 v%d：直接打开", version)
		verdict.Actions = []Action{ActionOpen}
	}
	return verdict, nil
}

// OpenReadOnly 以**只读**方式打开一个已存在的库，**不迁移、不建库**。
//
// 它是 CanOpen 三选一里"先导出备份"和"只读打开"的落地点：库比程序新时 Open 会
// 拒绝（旧二进制不该写新库），但用户仍然需要**把数据拿出来**。有了这个构造器，
// `Store.Export`/`List`/`Load` 在降级场景下依然可用，写方法则由 SQLite 自己报
// `attempt to write a readonly database`。
//
// 与 Open 的差别只有两点：不跑迁移（因此不会因为版本号而拒绝），以及路径必须
// 已经存在（只读打开一个不存在的文件没有意义，也不该顺手创建它）。
func OpenReadOnly(path string) (*ReadOnlyStore, error) {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return nil, ErrNoPath
	}
	if _, err := os.Stat(trimmed); err != nil {
		return nil, fmt.Errorf("sqlite: open read-only: %w", err)
	}
	dsn := "file:" + trimmed + "?mode=ro&_pragma=busy_timeout(" +
		fmt.Sprint(DefaultBusyTimeout.Milliseconds()) + ")&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("sqlite: open read-only %s: %w", trimmed, err)
	}
	db.SetMaxOpenConns(1)
	store := &Store{dsn: dsn, path: trimmed}
	store.db.store(db)
	// 用一次读确认连接真的能用（模式为 ro 时，锁/权限问题会在第一次查询才暴露）。
	ctx, cancel := context.WithTimeout(context.Background(), DefaultBusyTimeout)
	defer cancel()
	if _, err := probeVersion(ctx, db); err != nil {
		_ = db.Close()
		store.db.markClosed()
		return nil, fmt.Errorf("sqlite: open read-only %s: %w", trimmed, err)
	}
	return &ReadOnlyStore{Store: store}, nil
}

// ReadOnlyStore 是 OpenReadOnly 的返回类型：一个只能读、不能迁移的库句柄。
//
// 它内嵌 *Store，所以 `History`/`List`/`Load`/`Export`/`DSN`/`DB` 照常可用；
// 唯一的改写是 Close——`Store.Close` 会先做 `wal_checkpoint(TRUNCATE)`，而只读
// 连接上那条 PRAGMA 必然失败（只读连接改不了 WAL），于是关一个只读库会返回一个
// 与用户操作无关的错误。只读句柄关的只是连接本身。
type ReadOnlyStore struct {
	*Store
}

// Close 释放只读连接。重复调用是安全的。
func (r *ReadOnlyStore) Close() error {
	if r == nil || r.Store == nil {
		return nil
	}
	db := r.Store.db.markClosed()
	if db == nil {
		return nil
	}
	if err := db.Close(); err != nil {
		return fmt.Errorf("sqlite: close read-only: %w", err)
	}
	return nil
}

// inspectDatabase 用**只读**连接读版本号与核心表是否存在。
//
// 只读（`mode=ro`）是这条路径的全部安全性所在：预检绝不能顺手把 WAL 恢复、
// 建表或写版本号——那样"我先看看"就变成了"我已经改了"。
func inspectDatabase(path string) (version int, hasCoreTables bool, err error) {
	dsn := "file:" + path + "?mode=ro&_pragma=busy_timeout(" +
		fmt.Sprint((50 * time.Millisecond).Milliseconds()) + ")"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return 0, false, err
	}
	defer func() { _ = db.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), DefaultBusyTimeout)
	defer cancel()
	version, err = probeVersion(ctx, db)
	if err != nil {
		return 0, false, err
	}
	var count int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'rig_sessions'`).Scan(&count); err != nil {
		return 0, false, err
	}
	return version, count > 0, nil
}
