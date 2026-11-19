package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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
// 阈值、内容为空或落盘失败时返回空字符串，调用方保留原始结果。
func spillToolResult(policy rig.SpillPolicy, call rig.ToolCall, content string) (string, error) {
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
		ref, err := policy.Store.Save(context.Background(), call, content)
		if err != nil {
			return "", fmt.Errorf("store spilled result: %w", err)
		}
		// 回收：文件后端由 pruneSpillFiles 按 MaxFiles 完成，Store 后端必须在这里
		// 做同样的事，否则 policy.MaxFiles 在设了 Store 之后会**静默失效**（表无限
		// 增长，而宿主以为自己设过上限）。回收失败不影响本次结果：字节已经存好，
		// 清理是尽力而为的维护动作。
		if policy.MaxFiles > 0 {
			_, _ = policy.Store.GC(context.Background(), policy.MaxFiles)
		}
		preview := previewOf(content)
		return fmt.Sprintf("%s\nref: %s\nbytes: %d\npreview: %s",
			rig.SpillSummaryPrefix, ref.ID, ref.Bytes, preview), nil
	}
	dir := strings.TrimSpace(policy.Dir)
	if dir == "" {
		dir = filepath.Join(os.TempDir(), "zeokeel-rig-spill")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create spill directory: %w", err)
	}
	name := spillFileName(call)
	path := filepath.Join(dir, name)
	tmp, err := os.CreateTemp(dir, ".spill-*")
	if err != nil {
		return "", fmt.Errorf("create spill file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
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
		pruneSpillFiles(dir, policy.MaxFiles)
	}
	return fmt.Sprintf("%s\npath: %s\nbytes: %d\npreview: %s",
		rig.SpillSummaryPrefix, path, len(content), previewOf(content)), nil
}

func spillFileName(call rig.ToolCall) string {
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
	return fmt.Sprintf("%s-%s-%s-%s.txt", base, sanitize(id), time.Now().UTC().Format("20060102T150405"), hex.EncodeToString(random[:]))
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

// pruneSpillFiles 只保留最新的 keep 个 .txt 落盘文件。
func pruneSpillFiles(dir string, keep int) {
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
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".txt") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		files = append(files, fileInfo{path: filepath.Join(dir, entry.Name()), mod: info.ModTime()})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].mod.After(files[j].mod) })
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
