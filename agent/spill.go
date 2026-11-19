package agent

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/zeokeel/zeokeel-rig"
)

// defaultSpillThresholdBytes 是未配置阈值时触发落盘的最小字节数。
const defaultSpillThresholdBytes = 16 * 1024

// spillToolResult 按策略把过大的工具结果写入文件，返回模型可见摘要。未超过
// 阈值时返回空字符串，调用方保留原始结果；落盘失败返回错误（调用方记日志并
// 保留原始结果）。
//
// # 目录必须由调用方给出（审计 P1）
//
// 旧实现默认写到 `os.TempDir()/zeokeel-rig-spill`，有两个后果：① 那是**全局**目录，
// 所有工作区/会话的溢出结果混在一起；② 它不在工作区可写根内，模型拿到 path 之后
// 是否还能读回取决于读围栏的实现（DSH 对齐后读不再围栏才侥幸可读，但那不是承诺）。
// 现在 agent 层**不再自己挑位置**：`SpillPolicy.Dir` 为空时直接报错。`kit` 会把
// 空目录解析成 `<Root>/.rig/spill`（工作区内 → 天然 per-workspace、可写、可读回）。
//
// # 回收按 owner 分区
//
// 文件名带 `<session>` 作用域前缀，`pruneSpillFiles` 只删**本作用域**的文件。否则
// 两个会话（或主 agent 与子 agent）共用目录时，一个 policy 的 MaxFiles 会删掉别人
// 的句柄——句柄失效就是数据丢失。
//
// # ctx
//
// 取消后不再写盘：`ctx` 一路传到 Store/GC 与写文件之前的检查点。
func spillToolResult(ctx context.Context, policy rig.SpillPolicy, call rig.ToolCall, content string) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("spill tool %q result: %w", call.Name, err)
	}
	threshold := policy.ThresholdBytes
	if threshold <= 0 {
		threshold = defaultSpillThresholdBytes
	}
	if len(content) <= threshold {
		return "", nil
	}
	// 宿主把状态放在自己的存储里时（数据库行、对象存储），模型拿到的是句柄而不是
	// 路径；其余部分（阈值、预览、前缀）完全一样。
	if policy.Store != nil {
		ref, err := policy.Store.Save(ctx, call, content)
		if err != nil {
			return "", fmt.Errorf("store spilled result: %w", err)
		}
		// 回收：文件后端由 pruneSpillFiles 按 MaxFiles 完成，Store 后端必须在这里
		// 做同样的事，否则 policy.MaxFiles 在设了 Store 之后会**静默失效**（表无限
		// 增长，而宿主以为自己设过上限）。回收失败不影响本次结果：字节已经存好，
		// 清理是尽力而为的维护动作。
		if policy.MaxFiles > 0 {
			_, _ = policy.Store.GC(ctx, policy.MaxFiles)
		}
		preview := previewOf(content)
		return fmt.Sprintf("%s\nref: %s\nbytes: %d\npreview: %s",
			rig.SpillSummaryPrefix, ref.ID, ref.Bytes, preview), nil
	}
	dir := strings.TrimSpace(policy.Dir)
	if dir == "" {
		// 不再回落到系统临时目录：那是一个全局位置，既无法按工作区隔离，也不在
		// 模型可读的范围内。位置是宿主的选择（kit 默认给 <Root>/.rig/spill）。
		return "", errors.New("spill: SpillPolicy.Dir is required; kit sets <Root>/.rig/spill, " +
			"a bare agent.New host must choose a directory inside its sandbox roots")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create spill directory: %w", err)
	}
	scope := spillScope(ctx)
	name := spillFileName(scope, call)
	path := filepath.Join(dir, name)
	tmp, err := os.CreateTemp(dir, ".spill-*")
	if err != nil {
		return "", fmt.Errorf("create spill file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := ctx.Err(); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("spill tool %q result: %w", call.Name, err)
	}
	if _, err := tmp.WriteString(content); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("write spill file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("close spill file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return "", fmt.Errorf("rename spill file: %w", err)
	}
	if policy.MaxFiles > 0 {
		pruneSpillFiles(dir, spillFilePrefix(scope), policy.MaxFiles)
	}
	return fmt.Sprintf("%s\npath: %s\nbytes: %d\npreview: %s",
		rig.SpillSummaryPrefix, path, len(content), previewOf(content)), nil
}

// spillFilePrefix 返回某个作用域在目录里的文件名前缀。
//
// 前缀以 "." 结尾作为分隔符，因此 "session-a" 的前缀不会误匹配 "session-a-b" 的
// 文件（后者前缀是 "spill.session-a-b."）。
func spillFilePrefix(scope string) string {
	return "spill." + scope + "."
}

// spillScope 把会话/owner 变成一个适合进文件名的标签。
//
// 取 ctx 里的 sessionID（agent 在每次 Run 里都会注入），空则用 "default"。标签
// 只保留 [a-z0-9_-]，再拼上原始值的短哈希，避免不同 sessionID 清洗后撞在一起
// （撞了就会互删对方的句柄）。
func spillScope(ctx context.Context) string {
	raw := strings.TrimSpace(ToolSessionID(ctx))
	if raw == "" {
		raw = "default"
	}
	tag := sanitize(raw)
	if len(tag) > 32 {
		tag = tag[:32]
	}
	sum := sha256.Sum256([]byte(raw))
	return tag + "-" + hex.EncodeToString(sum[:4])
}

func spillFileName(scope string, call rig.ToolCall) string {
	base := strings.TrimSpace(call.Name)
	if base == "" {
		base = "tool"
	}
	base = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		default:
			return '_'
		}
	}, base)
	if len(base) > 40 {
		base = base[:40]
	}
	id := strings.TrimSpace(call.ID)
	if len(id) > 24 {
		id = id[:24]
	}
	var random [4]byte
	_, _ = rand.Read(random[:])
	// 时间戳带纳秒：同一秒内连续写入时，文件名本身就是"谁更新"的确定顺序
	// （prune 的 mtime 排序会并列，光靠 mtime 无法区分）。
	return fmt.Sprintf("%s%s-%s-%s-%s.txt", spillFilePrefix(scope), base, sanitize(id),
		time.Now().UTC().Format("20060102T150405.000000000"), hex.EncodeToString(random[:]))
}

func sanitize(value string) string {
	if value == "" {
		return "call"
	}
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		default:
			return '_'
		}
	}, value)
}

// pruneSpillFiles 在**本作用域内**只保留最新的 keep 个文件。
//
// prefix 由 spillFilePrefix 给出：只删自己写的那些文件，别的作用域（另一个会话、
// 主 agent 与子 agent）的句柄不受影响。旧实现扫整个目录，多会话下会互删句柄。
func pruneSpillFiles(dir, prefix string, keep int) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	type fileInfo struct {
		path string
		mod  time.Time
	}
	files := make([]fileInfo, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".txt") || !strings.HasPrefix(entry.Name(), prefix) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		files = append(files, fileInfo{path: filepath.Join(dir, entry.Name()), mod: info.ModTime()})
	}
	sort.Slice(files, func(i, j int) bool {
		if !files[i].mod.Equal(files[j].mod) {
			return files[i].mod.After(files[j].mod)
		}
		// 同一毫秒写入的两份结果：按文件名（末段是纳秒时间戳）倒序，保证
		// "保留最新的 keep 个"是确定的而不是取决于目录遍历顺序。
		return files[i].path > files[j].path
	})
	for i := keep; i < len(files); i++ {
		_ = os.Remove(files[i].path)
	}
}

// previewOf 是模型在完整结果位置上看到的一行预览。
//
// 按 **rune** 截断：预览是模型可见文案，切在字节中点会把一个汉字劈成两半，
// 模型读到的就是乱码。
func previewOf(content string) string {
	preview := content
	if len(preview) > 240 {
		cut := 240
		for cut > 0 && !utf8.RuneStart(preview[cut]) {
			cut--
		}
		preview = preview[:cut]
	}
	return strings.ReplaceAll(preview, "\n", " ")
}
