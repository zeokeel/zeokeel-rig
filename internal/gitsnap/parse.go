package gitsnap

import (
	"fmt"
	"strconv"
	"strings"
)

// splitNUL 把 `-z` 输出切成 token（丢弃末尾的空 token，以及空 token 本身）。
//
// git 的 `-z` 输出用 NUL 分隔，文件名里可以包含换行与其它控制字符，因此绝不能按行切。
func splitNUL(data []byte) []string {
	if len(data) == 0 {
		return nil
	}
	parts := strings.Split(string(data), "\x00")
	if last := len(parts) - 1; last >= 0 && parts[last] == "" {
		parts = parts[:last]
	}
	return parts
}

// joinNUL 把路径列表拼成 NUL 分隔的字节串（供 `--pathspec-from-file` 与 blob 使用）。
func joinNUL(paths []string) []byte {
	if len(paths) == 0 {
		return nil
	}
	builder := make([]byte, 0, len(paths)*16)
	for _, path := range paths {
		builder = append(builder, path...)
		builder = append(builder, 0)
	}
	return builder
}

// rawEntry 是 `git diff-tree --raw -z` 的一条记录。
type rawEntry struct {
	oldMode string
	newMode string
	oldSHA  string
	newSHA  string
	status  byte
	// path 是变更后的路径（删除时就是被删的路径）。
	path string
	// oldPath 只在 rename/copy 时非空。
	oldPath string
}

// parseRaw 解析 `--raw -z` 输出。
//
// 格式：`:100644 100644 <oldsha> <newsha> M\0<path>\0`；rename/copy 是
// `... R075\0<old>\0<new>\0`。
func parseRaw(data []byte) ([]rawEntry, error) {
	tokens := splitNUL(data)
	entries := make([]rawEntry, 0, len(tokens)/2)
	for index := 0; index < len(tokens); {
		header := tokens[index]
		if !strings.HasPrefix(header, ":") {
			return nil, fmt.Errorf("gitsnap: malformed raw diff header %q", header)
		}
		fields := strings.Fields(header[1:])
		if len(fields) != 5 {
			return nil, fmt.Errorf("gitsnap: malformed raw diff header %q", header)
		}
		index++
		if index >= len(tokens) {
			return nil, fmt.Errorf("gitsnap: raw diff header %q without a path", header)
		}
		entry := rawEntry{
			oldMode: fields[0],
			newMode: fields[1],
			oldSHA:  fields[2],
			newSHA:  fields[3],
			status:  fields[4][0],
			path:    tokens[index],
		}
		index++
		if entry.status == 'R' || entry.status == 'C' {
			if index >= len(tokens) {
				return nil, fmt.Errorf("gitsnap: rename entry %q without a destination path", header)
			}
			entry.oldPath = entry.path
			entry.path = tokens[index]
			index++
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

// numStat 是 `--numstat -z` 的一条记录。
type numStat struct {
	added   int
	removed int
	path    string
	oldPath string
}

// parseNumstat 解析 `--numstat -z` 输出；二进制文件的增删行数是 "-"，记为 0。
func parseNumstat(data []byte) []numStat {
	tokens := splitNUL(data)
	stats := make([]numStat, 0, len(tokens)/2)
	for index := 0; index < len(tokens); index++ {
		fields := strings.SplitN(tokens[index], "\t", 3)
		if len(fields) != 3 {
			continue
		}
		stat := numStat{
			added:   atoiOrZero(fields[0]),
			removed: atoiOrZero(fields[1]),
			path:    fields[2],
		}
		if stat.path == "" && index+2 < len(tokens) {
			// rename/copy：`<add>\t<del>\t\0<old>\0<new>\0`
			stat.oldPath = tokens[index+1]
			stat.path = tokens[index+2]
			index += 2
		}
		stats = append(stats, stat)
	}
	return stats
}

// atoiOrZero 解析数字，"-" 与非法值都返回 0。
func atoiOrZero(text string) int {
	value, err := strconv.Atoi(text)
	if err != nil {
		return 0
	}
	return value
}

// modeKind 把 git 的文件模式归类（用于识别类型变化）。
func modeKind(mode string) string {
	switch mode {
	case "120000":
		return "symlink"
	case "160000":
		return "submodule"
	case "100644", "100755":
		return "file"
	}
	return "other"
}

// typeChangeOf 报告两侧模式是否构成"路径类型变化"，返回可读描述（空串表示没有）。
func typeChangeOf(oldMode, newMode string) string {
	oldKind, newKind := modeKind(oldMode), modeKind(newMode)
	switch {
	case oldKind == newKind:
		if oldKind == "symlink" {
			// 两侧都是链接但内容（目标文本）变了。
			return "symlink->symlink"
		}
		return ""
	default:
		return oldKind + "->" + newKind
	}
}
