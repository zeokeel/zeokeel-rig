package sqlite

// Export 是 ImportDir 的镜像：把库里的会话写回 NDJSON 目录（session/store 的
// 格式，一行一条 rig.Message 的 JSON），外加一份版本化的会话元数据清单。
//
// 为什么这条出口必须存在：schema 升级是**单向**的，而升级之后的库旧版本读不了。
// 没有导出，用户"先备份再升级"这条路就断了。所以导出不是锦上
// 添花，而是改 schema 的唯一安全网。
//
// 导出面（版本化格式见 export_faces.go）：
//   - 会话与消息：`<id>.ndjson`（始终导出，session/store 的格式）；
//   - 会话元数据：`sessions.meta.jsonl`（始终导出：标题/工作区/时间戳）；
//   - 事件、花费、检查点：<events|costs|checkpoints>/<stem>.jsonl，
//     由 ExportOptions 的三个开关决定，**默认关闭**（保持既有行为）。
//
// 默认关闭的原因不是省事：花哨的备份容易让宿主以为"导完就是完整备份"。开关关闭
// 时报告里的 NotExported 会如实列出没导出的面，打开一个就少列一个。
//
// 入口：`(*Store).Export`（整库、覆盖）、`ExportToDir`（可选面/预览/增量）、
// `(*Store).VerifyExport`（把目录重新导入一个临时库并逐会话比对）。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	rig "github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/session/store"
)

// ExportSource 是 Export 需要的**最小读面**。
//
// 用接口而不是 `*Store`：导出的方向是从"某个存储"读出来，内存实现、未来的
// 实现、甚至一个只读打开的适配器都应该能导。写成具体类型会把这个出口锁死在
// 一种实现上（`kit.Config.SessionStore` 曾有的具体类型病）。
type ExportSource interface {
	List(ctx context.Context, filter Filter) ([]Meta, error)
	Load(ctx context.Context, id string) (Meta, []rig.Message, error)
}

// ExportExtras 是导出**可选面**需要的读面：事件、花费、检查点。
//
// 它故意与 ExportSource 分开，并且是可选的（`ExportToDir` 用类型断言探测）：
// 一个只实现 ExportSource 的适配器仍然能导出会话与消息，只是打不开这三个开关——
// 打开时会得到一个明确的错误，而不是一份"看起来成功了、其实什么都没有"的备份。
type ExportExtras interface {
	// SessionEvents 返回一个会话的全部事件（按 seq 升序）。
	SessionEvents(ctx context.Context, id string) ([]rig.SessionEvent, error)
	// SessionCosts 返回一个会话的全部花费行（按结算时间升序）。
	SessionCosts(ctx context.Context, id string) ([]CostEntry, error)
	// SessionCheckpoint 返回一个会话的检查点；没有时返回 nil。
	SessionCheckpoint(ctx context.Context, id string) (*rig.Checkpoint, error)
}

// 可选面的名字（报告里的 NotExported 用它们说话）。
const (
	exportFaceEvents      = "events"
	exportFaceCosts       = "costs"
	exportFaceCheckpoints = "checkpoints"
)

// notExportedFacesFor 返回本次导出**没有**覆盖的可选面。
func notExportedFacesFor(opts ExportOptions) []string {
	var faces []string
	if !opts.IncludeEvents {
		faces = append(faces, exportFaceEvents)
	}
	if !opts.IncludeCosts {
		faces = append(faces, exportFaceCosts)
	}
	if !opts.IncludeCheckpoints {
		faces = append(faces, exportFaceCheckpoints)
	}
	return faces
}

// ExportOptions 配置 ExportToDir。它是 ImportOptions 的镜像。
type ExportOptions struct {
	// Dir 是导出目标目录。不存在时会被创建（0700）；导出是"写一个新地方"，
	// 不像导入那样要求目录已存在。
	Dir string
	// Sessions 限定只导出这些会话 ID；空表示全部。
	Sessions []string
	// DryRun 只数不写：宿主可以先给用户看"将导出 N 个会话、M 条消息"。
	DryRun bool
	// Overwrite 决定"目标目录里已有同名会话"怎么办：
	//   - true：用 Replace 原子覆盖（`(*Store).Export` 用这个，重跑导出是幂等的）；
	//   - false：跳过并记进 Skipped（预览/增量导出用）。
	Overwrite bool
	// IncludeEvents 把事件日志写进 `<Dir>/events/<stem>.jsonl`（版本化格式见
	// export_faces.go）。默认 false：既有行为不变。
	IncludeEvents bool
	// IncludeCosts 把花费行写进 `<Dir>/costs/<stem>.jsonl`。默认 false。
	IncludeCosts bool
	// IncludeCheckpoints 把检查点写进 `<Dir>/checkpoints/<stem>.jsonl`。默认 false。
	IncludeCheckpoints bool
}

// ExportReport 说明这次导出发现了什么、做了什么。字段与 ImportReport 对齐，
// 便于宿主把两个报告混着显示。
type ExportReport struct {
	// Sessions 是这次选中的会话数（含跳过与失败的）。
	Sessions int
	// Messages 是读到的消息总数（试运行也计入）。
	Messages int
	// Exported 是真正写到目录里的会话数。
	Exported int
	// Events / Costs / Checkpoints 是这次写出的可选面行数（开关关闭时为 0）。
	Events      int
	Costs       int
	Checkpoints int
	// Skipped 列出目标已有、且 Overwrite=false 因而被跳过的会话。
	Skipped []string
	// Failed 列出读不出来或写不进去的会话及原因。一个会话坏掉不中断整次导出。
	Failed []string
	// NotExported 是本次**没有**导出的面（未打开开关的那些）：导出默认不是完整
	// 备份，报告必须说清楚。
	NotExported []string
}

// errExportSkip 是"目标已有、且不允许覆盖"的内部信号。
var errExportSkip = errors.New("sqlite: export skipped an existing session")

// Export 把整个库导出成一个 NDJSON 目录（`ImportDir` 可以直接把它读回来）。
//
// 目标目录里已有的同名会话会被**原子覆盖**（重跑一次导出不会追加出第二份历史）。
// 任何会话导出失败都会让整次调用返回错误——`error` 是唯一出口，所以不能静默
// 跳过；需要"跳过并报告"的宿主请用 ExportToDir + ExportOptions.Overwrite=false。
func (s *Store) Export(ctx context.Context, dir string) error {
	if err := s.usable(); err != nil {
		return err
	}
	report, err := ExportToDir(ctx, s, ExportOptions{Dir: dir, Overwrite: true})
	if err != nil {
		return err
	}
	if len(report.Failed) > 0 {
		return fmt.Errorf("sqlite: export to %q: %d of %d sessions failed: %s",
			dir, len(report.Failed), report.Sessions, strings.Join(report.Failed, "; "))
	}
	return nil
}

// ExportToDir 把 source 里的会话写到 opts.Dir。
//
// 与 ImportDir 相同的取舍：单个会话失败不中断整次导出（记进 Failed 继续），
// 这样用户不会因为一条坏记录丢掉全部其它会话。目录里的排序按 CreatedAt 最旧
// 优先，与导入一致。
//
// 元数据清单（sessions.meta.jsonl）在**所有会话处理完**之后写一次：Overwrite=false
// 跳过的会话保留它原来那一行，所以增量导出不会把清单越写越小。
func ExportToDir(ctx context.Context, source ExportSource, opts ExportOptions) (ExportReport, error) {
	report := ExportReport{NotExported: notExportedFacesFor(opts)}
	if source == nil {
		return report, errors.New("sqlite: export source is nil")
	}
	dir := strings.TrimSpace(opts.Dir)
	if dir == "" {
		return report, errors.New("sqlite: ExportOptions.Dir is required")
	}
	var extras ExportExtras
	if opts.IncludeEvents || opts.IncludeCosts || opts.IncludeCheckpoints {
		extras, _ = source.(ExportExtras)
		if extras == nil {
			return report, fmt.Errorf(
				"sqlite: export source %T cannot export %v: it does not implement ExportExtras",
				source, report.NotExported)
		}
	}
	if !opts.DryRun {
		// 导出目录是**新写的**地方：自动创建是友好的；导入方则要求目录已存在
		// （那是为了拦住"配置写错、静默导入 0 个会话"）。
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return report, fmt.Errorf("sqlite: create export directory %q: %w", dir, err)
		}
	}
	metas, err := source.List(ctx, Filter{})
	if err != nil {
		return report, fmt.Errorf("sqlite: list sessions to export: %w", err)
	}
	sort.SliceStable(metas, func(i, j int) bool { return metas[i].CreatedAt.Before(metas[j].CreatedAt) })

	selected := make(map[string]bool, len(opts.Sessions))
	for _, id := range opts.Sessions {
		if trimmed := strings.TrimSpace(id); trimmed != "" {
			selected[trimmed] = true
		}
	}

	manifest := map[string]sessionMetaRecord{}
	// writeManifest 决定最后要不要写清单：目标目录里已有一份读不懂的清单时**不写**
	// ——那可能是另一次导出的资产，把它覆盖成"只剩这一次的会话"是数据丢失。
	writeManifest := !opts.DryRun
	if !opts.DryRun {
		existing, readErr := readExportManifest(dir)
		if readErr != nil {
			report.Failed = append(report.Failed, fmt.Sprintf("%s: %v", exportManifestFile, readErr))
			writeManifest = false
		} else {
			for id, record := range existing {
				manifest[id] = record
			}
		}
	}

	var dest *store.Dir
	if !opts.DryRun {
		dest, err = store.NewDir(dir)
		if err != nil {
			return report, fmt.Errorf("sqlite: open export directory: %w", err)
		}
		defer func() { _ = dest.Close() }()
	}

	seen := make(map[string]bool, len(metas))
	for _, meta := range metas {
		if len(selected) > 0 && !selected[meta.ID] {
			continue
		}
		seen[meta.ID] = true
		report.Sessions++
		loaded, messages, loadErr := source.Load(ctx, meta.ID)
		if loadErr != nil {
			report.Failed = append(report.Failed, fmt.Sprintf("%s: %v", meta.ID, loadErr))
			continue
		}
		report.Messages += len(messages)
		if opts.DryRun {
			continue
		}
		writeErr := writeExportedSession(ctx, dest, meta, messages, opts.Overwrite)
		switch {
		case errors.Is(writeErr, errExportSkip):
			report.Skipped = append(report.Skipped, meta.ID)
			continue
		case writeErr != nil:
			report.Failed = append(report.Failed, fmt.Sprintf("%s: %v", meta.ID, writeErr))
			continue
		}
		report.Exported++
		// 元数据以 source 的读面为准（Load 比 List 更接近"这个会话到底是什么"）。
		title, workspace := meta.Title, meta.Workspace
		if loaded.ID != "" {
			title, workspace = loaded.Title, loaded.Workspace
		}
		manifest[meta.ID] = sessionMetaRecord{
			ID: meta.ID, Title: title, Workspace: workspace,
			CreatedAt: meta.CreatedAt.UnixMilli(), UpdatedAt: meta.UpdatedAt.UnixMilli(),
		}
		if extras == nil {
			continue
		}
		if err := writeSessionExtras(ctx, extras, dir, opts, meta.ID, &report); err != nil {
			// 消息文件已经写出去了：这里不是"整个会话丢了"，而是"备份不完整"。
			// 报告必须说清楚（Failed 是宿主唯一能看见的告警面）。
			report.Failed = append(report.Failed, fmt.Sprintf("%s: %v", meta.ID, err))
		}
	}
	// 点名要导、库里却没有的会话：必须报出来，否则"导了 3 个"是个错觉。
	for id := range selected {
		if !seen[id] {
			report.Failed = append(report.Failed, fmt.Sprintf("%s: %v", id, ErrNotFound))
		}
	}
	if writeManifest {
		if err := writeExportManifest(dir, manifest); err != nil {
			return report, fmt.Errorf("sqlite: write export manifest in %q: %w", dir, err)
		}
	}
	return report, nil
}

// writeSessionExtras 按开关写出一个会话的可选面，并把行数累加进报告。
func writeSessionExtras(ctx context.Context, extras ExportExtras, dir string,
	opts ExportOptions, id string, report *ExportReport) error {
	if opts.IncludeEvents {
		events, err := extras.SessionEvents(ctx, id)
		if err != nil {
			return fmt.Errorf("read events: %w", err)
		}
		if err := writeExportEvents(dir, id, events); err != nil {
			return fmt.Errorf("write events: %w", err)
		}
		report.Events += len(events)
	}
	if opts.IncludeCosts {
		costs, err := extras.SessionCosts(ctx, id)
		if err != nil {
			return fmt.Errorf("read costs: %w", err)
		}
		if err := writeExportCosts(dir, id, costs); err != nil {
			return fmt.Errorf("write costs: %w", err)
		}
		report.Costs += len(costs)
	}
	if opts.IncludeCheckpoints {
		checkpoint, err := extras.SessionCheckpoint(ctx, id)
		if err != nil {
			return fmt.Errorf("read checkpoint: %w", err)
		}
		if err := writeExportCheckpoint(dir, id, checkpoint); err != nil {
			return fmt.Errorf("write checkpoint: %w", err)
		}
		if checkpoint != nil {
			report.Checkpoints++
		}
	}
	return nil
}

// writeExportedSession 把一个会话写进目录。
//
// 已存在的会话用 Replace（整文件临时文件 + rename）：导出重跑一次结果相同，
// 而不是往旧文件尾部追加一份历史。
func writeExportedSession(ctx context.Context, dest *store.Dir, meta Meta, messages []rig.Message, overwrite bool) error {
	_, _, err := dest.Load(ctx, meta.ID)
	switch {
	case err == nil:
		if !overwrite {
			return errExportSkip
		}
		if _, err := dest.Replace(ctx, meta.ID, messages); err != nil {
			return fmt.Errorf("replace: %w", err)
		}
		return nil
	case errors.Is(err, ErrNotFound):
		created, err := dest.Create(ctx, Meta{
			ID:        meta.ID,
			Title:     meta.Title,
			Workspace: meta.Workspace,
			CreatedAt: meta.CreatedAt,
			UpdatedAt: meta.UpdatedAt,
		})
		if err != nil {
			return fmt.Errorf("create: %w", err)
		}
		if len(messages) > 0 {
			if _, err := dest.AppendBatch(ctx, created.ID, messages...); err != nil {
				return fmt.Errorf("append: %w", err)
			}
		}
		return nil
	default:
		return fmt.Errorf("probe destination: %w", err)
	}
}

// Mismatch 是一处往返差异。
type Mismatch struct {
	// SessionID 是有差异的会话。
	SessionID string
	// Field 是差异的维度：session / title / messages / bytes。
	Field string
	// Want / Got 用字符串表达，便于直接进日志与 UI。
	Want string
	Got  string
}

// RoundTripReport 是一次导出往返校验的结果。
//
// Sessions/Messages/Bytes 描述**源库**的规模（也就是"应该还原成什么样"），
// Mismatches 为空才算往返成功。
type RoundTripReport struct {
	Sessions   int
	Messages   int
	Bytes      int
	Mismatches []Mismatch
}

// messageBytes 是逐条 JSON 序列化后的总字节数——与库里的 payload 是同一个口径。
func messageBytes(messages []rig.Message) int {
	total := 0
	for _, message := range messages {
		if payload, err := json.Marshal(message); err == nil {
			total += len(payload)
		}
	}
	return total
}

// VerifyExport 把 dir 重新导入一个**全新的临时库**，然后逐会话比对消息条数与
// 字节、以及标题。宿主在升级/迁移之后跑一次，就能回答"这份备份到底能不能还原"。
//
// 为什么用临时库而不是和源库比：`ImportDir(Export(dir))` 才是往返的定义。拿
// 源库和目录比只能证明"写出去的文件读得回来"，证明不了"能被导入器吃下去"。
func (s *Store) VerifyExport(ctx context.Context, dir string) (RoundTripReport, error) {
	var report RoundTripReport
	if err := s.usable(); err != nil {
		return report, err
	}
	workspace, err := os.MkdirTemp("", "rig-export-verify-")
	if err != nil {
		return report, fmt.Errorf("sqlite: verify export: %w", err)
	}
	defer func() { _ = os.RemoveAll(workspace) }()
	target, err := Open(Options{Path: filepath.Join(workspace, "verify.sqlite")})
	if err != nil {
		return report, fmt.Errorf("sqlite: verify export: open scratch database: %w", err)
	}
	defer func() { _ = target.Close() }()
	if _, err := ImportDir(ctx, target, ImportOptions{Dir: dir}); err != nil {
		return report, fmt.Errorf("sqlite: verify export: import %q: %w", dir, err)
	}
	return CompareSessions(ctx, s, target)
}

// CompareSessions 逐会话比对两个存储：会话是否存在、标题、消息条数与字节。
//
// 它**不**比对 Workspace/Model：那些字段的丢失是上游目录后端的已知行为
// （session/store 只存消息与派生标题），把它们算成往返失败只会让门禁永远红。
// 标题则在口径内——它是宿主显式设置过的东西，而"往返之后标题变了"正是用户能
// 直接看见的那种数据丢失（导出目录的 sessions.meta.jsonl 就是为它存在的）。
func CompareSessions(ctx context.Context, want, got ExportSource) (RoundTripReport, error) {
	var report RoundTripReport
	if want == nil || got == nil {
		return report, errors.New("sqlite: CompareSessions needs two sources")
	}
	wantMetas, err := want.List(ctx, Filter{})
	if err != nil {
		return report, fmt.Errorf("sqlite: compare sessions: list source: %w", err)
	}
	gotMetas, err := got.List(ctx, Filter{})
	if err != nil {
		return report, fmt.Errorf("sqlite: compare sessions: list destination: %w", err)
	}
	gotByID := make(map[string]bool, len(gotMetas))
	for _, meta := range gotMetas {
		gotByID[meta.ID] = true
	}
	for _, meta := range wantMetas {
		_, wantMessages, err := want.Load(ctx, meta.ID)
		if err != nil {
			return report, fmt.Errorf("sqlite: compare sessions: load source %q: %w", meta.ID, err)
		}
		report.Sessions++
		report.Messages += len(wantMessages)
		report.Bytes += messageBytes(wantMessages)
		gotMeta, gotMessages, err := got.Load(ctx, meta.ID)
		if errors.Is(err, ErrNotFound) {
			report.Mismatches = append(report.Mismatches, Mismatch{
				SessionID: meta.ID, Field: "session", Want: "present", Got: "missing",
			})
			continue
		}
		if err != nil {
			return report, fmt.Errorf("sqlite: compare sessions: load destination %q: %w", meta.ID, err)
		}
		if gotMeta.Title != meta.Title {
			report.Mismatches = append(report.Mismatches, Mismatch{
				SessionID: meta.ID, Field: "title", Want: meta.Title, Got: gotMeta.Title,
			})
		}
		if len(gotMessages) != len(wantMessages) {
			report.Mismatches = append(report.Mismatches, Mismatch{
				SessionID: meta.ID, Field: "messages",
				Want: fmt.Sprint(len(wantMessages)), Got: fmt.Sprint(len(gotMessages)),
			})
		}
		if wantBytes, gotBytes := messageBytes(wantMessages), messageBytes(gotMessages); wantBytes != gotBytes {
			report.Mismatches = append(report.Mismatches, Mismatch{
				SessionID: meta.ID, Field: "bytes",
				Want: fmt.Sprint(wantBytes), Got: fmt.Sprint(gotBytes),
			})
		}
		delete(gotByID, meta.ID)
	}
	// 目标里多出来的会话也算差异（导出目录被别的会话污染 / 清空没清干净）。
	for id := range gotByID {
		report.Mismatches = append(report.Mismatches, Mismatch{
			SessionID: id, Field: "session", Want: "absent", Got: "present",
		})
	}
	sort.SliceStable(report.Mismatches, func(i, j int) bool {
		if report.Mismatches[i].SessionID != report.Mismatches[j].SessionID {
			return report.Mismatches[i].SessionID < report.Mismatches[j].SessionID
		}
		return report.Mismatches[i].Field < report.Mismatches[j].Field
	})
	return report, nil
}
