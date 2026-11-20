package sqlite

// 导出目录里**消息之外**的文件面，以及它们的版本化格式。
//
//	sessions.meta.jsonl        会话元数据清单（id / title / workspace / 时间戳）
//	events/<stem>.jsonl        一个会话的事件日志
//	costs/<stem>.jsonl         一个会话的花费行
//	checkpoints/<stem>.jsonl   一个会话的检查点（一行，覆盖式）
//
// 为什么不把它们塞进 `<id>.ndjson`：那一层是 session/store 的格式——一行一条
// `rig.Message` 的 JSON，外部工具逐行解析就能读。混进别的记录会让"这是消息文件"
// 不再成立（`TestExportIsReadableByExternalTool` 也钉住了这一点）。所以可选面走
// 独立目录、独立文件。
//
// # 版本
//
// 每个文件的第一行是**格式头**：`{"format":"…","version":1}`（事件/花费/检查点还带
// `session_id`）。只有形状不兼容地改变时才 +1。读取方遇到不认识的 format 或 version
// 必须跳过整个文件并把它记进导入报告的 Failed——猜着解析的代价是把别人的历史写成
// 自己的历史。
//
// # 为什么元数据清单不是开关
//
// 标题/工作区不属于"可选面"：它们是会话自身的元数据，而目录后端（session/store）
// 不持久化它们（重开时按首条用户消息重扫）。清单让 `Export → ImportDir` 之后
// 标题原样回来，`CompareSessions` 才有意义。清单缺席时（老导出目录）行为与从前
// 一致：标题由首条用户消息派生。
//
// # 文件名
//
// stem 由会话 ID 派生（exportStem）：只保留 `[A-Za-z0-9._-]`，替换过的 ID 再拼一段
// 短哈希，保证不同 ID 不会落到同一个文件。真正的会话 ID 写在每个文件的第一行。

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	rig "github.com/zeokeel/zeokeel-rig"
)

// exportFaceVersion 是上面四个文件面的格式版本。
const exportFaceVersion = 1

// 文件名与格式标识。
const (
	exportManifestFile     = "sessions.meta.jsonl"
	exportEventsDir        = "events"
	exportCostsDir         = "costs"
	exportCheckpointsDir   = "checkpoints"
	exportFaceExt          = ".jsonl"
	exportFormatMeta       = "rig-session-meta"
	exportFormatEvents     = "rig-session-events"
	exportFormatCosts      = "rig-session-costs"
	exportFormatCheckpoint = "rig-session-checkpoint"
)

// exportHeader 是每个文件面的第一行。
type exportHeader struct {
	Format    string `json:"format"`
	Version   int    `json:"version"`
	SessionID string `json:"session_id,omitempty"`
	// Faces 只在会话元数据清单里出现：记录这次导出带了哪些可选面
	// （events/costs/checkpoints）。VerifyExport 靠它判断"该有的面是不是丢了"。
	Faces []string `json:"faces,omitempty"`
}

// sessionMetaRecord 是清单里的一行：一个会话的元数据。
type sessionMetaRecord struct {
	ID        string `json:"id"`
	Title     string `json:"title,omitempty"`
	Workspace string `json:"workspace,omitempty"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
}

// exportEventRecord 是事件文件里的一行。
type exportEventRecord struct {
	Seq     int64           `json:"seq"`
	Type    string          `json:"type"`
	At      int64           `json:"at"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// exportCostRecord 是花费文件里的一行。
type exportCostRecord struct {
	Model            string  `json:"model,omitempty"`
	WorkspaceID      string  `json:"workspace_id,omitempty"`
	RunID            string  `json:"run_id,omitempty"`
	PromptTokens     int     `json:"prompt_tokens"`
	CompletionTokens int     `json:"completion_tokens"`
	CachedTokens     int     `json:"cached_tokens"`
	CostUSD          float64 `json:"cost_usd"`
	At               int64   `json:"at"`
}

// exportCheckpointRecord 是检查点文件里（除头之外唯一的）一行。
//
// payload 存的是 `rig.Checkpoint` 的 JSON 原文：检查点的形状属于 rig 契约，这里
// 不做第二套映射，否则每次给 Checkpoint 加字段都要改一次导出格式。
type exportCheckpointRecord struct {
	Payload   json.RawMessage `json:"payload"`
	UpdatedAt int64           `json:"updated_at"`
}

// exportStem 把会话 ID 变成一个安全、且不会互相碰撞的文件名。
func exportStem(id string) string {
	var builder strings.Builder
	changed := false
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			builder.WriteRune(r)
		default:
			builder.WriteRune('_')
			changed = true
		}
	}
	stem := builder.String()
	if stem == "" || stem == "." || stem == ".." {
		stem = "session"
		changed = true
	}
	if len(stem) > 64 {
		stem = stem[:64]
		changed = true
	}
	if changed {
		// 替换掉字符的 ID 之间可能撞名（`a/b` 与 `a_b`）：拼一段 ID 的哈希。
		sum := sha256.Sum256([]byte(id))
		stem += "-" + hex.EncodeToString(sum[:4])
	}
	return stem
}

// writeExportJSONL 写一个 `头 + 行` 的 JSONL 文件（0600、整文件覆盖）。
func writeExportJSONL(path string, header exportHeader, lines []any) error {
	var buffer bytes.Buffer
	encoded, err := json.Marshal(header)
	if err != nil {
		return fmt.Errorf("encode header: %w", err)
	}
	buffer.Write(encoded)
	buffer.WriteByte('\n')
	for _, line := range lines {
		encoded, err := json.Marshal(line)
		if err != nil {
			return fmt.Errorf("encode record: %w", err)
		}
		buffer.Write(encoded)
		buffer.WriteByte('\n')
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create %q: %w", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, buffer.Bytes(), 0o600); err != nil {
		return fmt.Errorf("write %q: %w", path, err)
	}
	return nil
}

// readExportJSONL 读一个 JSONL 文件，返回头与逐行原文。
func readExportJSONL(path string, wantFormat string) (exportHeader, [][]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return exportHeader{}, nil, err
	}
	defer func() { _ = file.Close() }()
	var (
		header exportHeader
		lines  [][]byte
	)
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 16<<20)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		if len(lines) == 0 && header.Format == "" {
			if err := json.Unmarshal(line, &header); err != nil {
				return exportHeader{}, nil, fmt.Errorf("第一行不是格式头: %w", err)
			}
			if header.Format != wantFormat {
				return exportHeader{}, nil, fmt.Errorf("格式是 %q，want %q", header.Format, wantFormat)
			}
			if header.Version != exportFaceVersion {
				return exportHeader{}, nil, fmt.Errorf("格式版本 %d，本构建只认 %d", header.Version, exportFaceVersion)
			}
			continue
		}
		lines = append(lines, append([]byte(nil), line...))
	}
	if err := scanner.Err(); err != nil {
		return exportHeader{}, nil, err
	}
	if header.Format == "" {
		return exportHeader{}, nil, fmt.Errorf("空文件（缺少格式头）")
	}
	return header, lines, nil
}

// readExportManifest 读会话元数据清单。文件不存在时返回空 map（老导出目录）。
// readExportManifestFaces 只读清单头，返回这次导出带了哪些可选面（老导出目录返回 nil）。
func readExportManifestFaces(dir string) []string {
	path := filepath.Join(dir, exportManifestFile)
	header, _, err := readExportJSONL(path, exportFormatMeta)
	if err != nil {
		return nil
	}
	return header.Faces
}

func readExportManifest(dir string) (map[string]sessionMetaRecord, error) {
	path := filepath.Join(dir, exportManifestFile)
	_, lines, err := readExportJSONL(path, exportFormatMeta)
	if os.IsNotExist(err) {
		return map[string]sessionMetaRecord{}, nil
	}
	if err != nil {
		return nil, err
	}
	records := make(map[string]sessionMetaRecord, len(lines))
	for index, line := range lines {
		var record sessionMetaRecord
		if err := json.Unmarshal(line, &record); err != nil {
			return nil, fmt.Errorf("清单第 %d 行: %w", index+2, err)
		}
		if strings.TrimSpace(record.ID) == "" {
			return nil, fmt.Errorf("清单第 %d 行没有 id", index+2)
		}
		records[record.ID] = record
	}
	return records, nil
}

// writeExportManifest 覆盖写会话元数据清单（按 ID 排序，重跑结果稳定）。
func writeExportManifest(dir string, records map[string]sessionMetaRecord, faces []string) error {
	ids := make([]string, 0, len(records))
	for id := range records {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	lines := make([]any, 0, len(ids))
	for _, id := range ids {
		lines = append(lines, records[id])
	}
	return writeExportJSONL(filepath.Join(dir, exportManifestFile),
		exportHeader{Format: exportFormatMeta, Version: exportFaceVersion, Faces: faces}, lines)
}

// writeExportEvents 写一个会话的事件文件。
func writeExportEvents(dir, sessionID string, events []rig.SessionEvent) error {
	lines := make([]any, 0, len(events))
	for _, event := range events {
		at := event.Time
		if at.IsZero() {
			at = time.Now().UTC()
		}
		lines = append(lines, exportEventRecord{
			Seq: event.Seq, Type: event.Type, At: at.UnixMilli(), Payload: event.Payload,
		})
	}
	return writeExportJSONL(filepath.Join(dir, exportEventsDir, exportStem(sessionID)+exportFaceExt),
		exportHeader{Format: exportFormatEvents, Version: exportFaceVersion, SessionID: sessionID}, lines)
}

// writeExportCosts 写一个会话的花费文件。
func writeExportCosts(dir, sessionID string, costs []CostEntry) error {
	lines := make([]any, 0, len(costs))
	for _, entry := range costs {
		at := entry.At
		if at.IsZero() {
			at = time.Now().UTC()
		}
		lines = append(lines, exportCostRecord{
			Model: entry.Model, WorkspaceID: entry.WorkspaceID, RunID: entry.RunID,
			PromptTokens: entry.PromptTokens, CompletionTokens: entry.CompletionTokens,
			CachedTokens: entry.CachedTokens, CostUSD: entry.CostUSD, At: at.UnixMilli(),
		})
	}
	return writeExportJSONL(filepath.Join(dir, exportCostsDir, exportStem(sessionID)+exportFaceExt),
		exportHeader{Format: exportFormatCosts, Version: exportFaceVersion, SessionID: sessionID}, lines)
}

// writeExportCheckpoint 写一个会话的检查点文件。
func writeExportCheckpoint(dir, sessionID string, checkpoint *rig.Checkpoint) error {
	path := filepath.Join(dir, exportCheckpointsDir, exportStem(sessionID)+exportFaceExt)
	if checkpoint == nil {
		// 没有检查点 = 没有文件（覆盖导出时要把上一次的旧文件删掉，否则删掉的
		// 检查点会在下一次导入里"复活"）。
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	payload, err := json.Marshal(checkpoint)
	if err != nil {
		return fmt.Errorf("encode checkpoint: %w", err)
	}
	updated := checkpoint.UpdatedAt
	if updated.IsZero() {
		updated = time.Now().UTC()
	}
	return writeExportJSONL(path,
		exportHeader{Format: exportFormatCheckpoint, Version: exportFaceVersion, SessionID: sessionID},
		[]any{exportCheckpointRecord{Payload: payload, UpdatedAt: updated.UnixMilli()}})
}

// readExportEvents 读一个事件文件。
func readExportEvents(path string) (string, []rig.SessionEvent, error) {
	header, lines, err := readExportJSONL(path, exportFormatEvents)
	if err != nil {
		return "", nil, err
	}
	if header.SessionID == "" {
		return "", nil, fmt.Errorf("格式头缺少 session_id")
	}
	events := make([]rig.SessionEvent, 0, len(lines))
	for index, line := range lines {
		var record exportEventRecord
		if err := json.Unmarshal(line, &record); err != nil {
			return "", nil, fmt.Errorf("第 %d 行: %w", index+2, err)
		}
		if record.Type == "" {
			return "", nil, fmt.Errorf("第 %d 行没有 type", index+2)
		}
		events = append(events, rig.SessionEvent{
			Seq: record.Seq, SessionID: header.SessionID, Type: record.Type,
			Time: time.UnixMilli(record.At).UTC(), Payload: record.Payload,
		})
	}
	return header.SessionID, events, nil
}

// readExportCosts 读一个花费文件。
func readExportCosts(path string) (string, []CostEntry, error) {
	header, lines, err := readExportJSONL(path, exportFormatCosts)
	if err != nil {
		return "", nil, err
	}
	if header.SessionID == "" {
		return "", nil, fmt.Errorf("格式头缺少 session_id")
	}
	costs := make([]CostEntry, 0, len(lines))
	for index, line := range lines {
		var record exportCostRecord
		if err := json.Unmarshal(line, &record); err != nil {
			return "", nil, fmt.Errorf("第 %d 行: %w", index+2, err)
		}
		costs = append(costs, CostEntry{
			Model: record.Model, WorkspaceID: record.WorkspaceID, RunID: record.RunID,
			PromptTokens: record.PromptTokens, CompletionTokens: record.CompletionTokens,
			CachedTokens: record.CachedTokens, CostUSD: record.CostUSD,
			At: time.UnixMilli(record.At).UTC(),
		})
	}
	return header.SessionID, costs, nil
}

// readExportCheckpoint 读一个检查点文件；文件里没有记录时返回 nil。
func readExportCheckpoint(path string) (string, *rig.Checkpoint, error) {
	header, lines, err := readExportJSONL(path, exportFormatCheckpoint)
	if err != nil {
		return "", nil, err
	}
	if header.SessionID == "" {
		return "", nil, fmt.Errorf("格式头缺少 session_id")
	}
	if len(lines) == 0 {
		return header.SessionID, nil, nil
	}
	var record exportCheckpointRecord
	if err := json.Unmarshal(lines[0], &record); err != nil {
		return "", nil, fmt.Errorf("第 2 行: %w", err)
	}
	var checkpoint rig.Checkpoint
	if err := json.Unmarshal(record.Payload, &checkpoint); err != nil {
		return "", nil, fmt.Errorf("检查点载荷: %w", err)
	}
	if checkpoint.SessionID == "" {
		checkpoint.SessionID = header.SessionID
	}
	if checkpoint.UpdatedAt.IsZero() {
		checkpoint.UpdatedAt = time.UnixMilli(record.UpdatedAt).UTC()
	}
	return header.SessionID, &checkpoint, nil
}
