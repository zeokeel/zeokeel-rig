package filewatch

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

// Changed 返回"自 from 快照以来发生变化"的路径，按字典序排列。
//
// 它给写入点做冲突检测用：调用方在准备改文件时留一份 Snapshot，落盘前用
// Changed(root, from) 确认目标路径没有被别人动过。
//
// 语义：
//   - from 为 nil 等价于空快照（所有现存文件都是 created）；
//   - 包含新增、修改与删除（KindRemoved 的 Size/ModTime/Digest 是最后已知值）；
//   - 比较口径与 Watcher 一致：总是比 (size, mtime)；若 from 里任一条目带
//     摘要，则同时开启内容摘要比较（能发现"size 与 mtime 都没变"的改写）；
//   - 忽略规则用与 New 相同的默认列表（.git/node_modules/vendor/dist/build）；
//   - 与 Watcher 不同，这里**不设 MaxFiles 上限**：冲突检测不能因为文件多就
//     漏看目标以外的改动；
//   - from 的键先做 filepath.Clean 归一，因此不同拼法（"./a"、"a//b"）都能
//     正确比对。
//
// root 不存在或不是目录时返回错误。
func Changed(root string, from map[string]FileInfo) ([]Event, error) {
	abs, err := resolveRoot(root)
	if err != nil {
		return nil, err
	}

	// 归一化旧快照：把键 Clean 一遍，避免调用方用不同拼法导致"看起来全变了"。
	seen := make(map[string]FileInfo, len(from))
	digest := false
	for path, state := range from {
		cleaned := filepath.Clean(path)
		seen[cleaned] = state
		if state.Digest != "" {
			digest = true
		}
	}

	// 扫描口径与 Watcher 保持一致（同一份默认忽略列表），但不受 MaxFiles 限制。
	walker := &scanner{
		root:     abs,
		ignore:   defaultIgnore(),
		maxFiles: 0,
		digest:   digest,
	}
	cur, _, err := walker.scan()
	if err != nil {
		return nil, fmt.Errorf("filewatch: 扫描 %q: %w", abs, err)
	}
	return diffEvents(seen, cur, digest), nil
}

// ChangedPaths 是 Changed 的**单点版本**：只检查调用方点名的路径，
// 成本 O(len(paths))，不遍历全树。
//
// 语义：
//   - paths 里的相对路径按 root 解析，绝对路径必须仍在 root 之内；逃出 root
//     （"../x"、root 外的绝对路径）返回错误；
//   - from 为空（nil 或空 map）等价于"全部视为新增"；
//   - from 里有、盘上已没有 → KindRemoved（带最后已知 size/mtime/摘要）；
//   - 盘上没有、from 里也没有 → 不产事件，也不报错；
//   - 目录不参与（与 Watcher/Snapshot 一致）；符号链接按 lstat 比较且不带摘要；
//   - 摘要口径与 Snapshot 一致：**只有当 from 里该路径带摘要时**才为它计算摘要
//     （这是比对"同 size 同 mtime 的改写"所必需的，也能让没开 Digest 的调用方
//     零额外 IO）；
//   - 结果按 Path 字典序排列，重复路径（含不同拼法）只报一次；
//   - from 中未被点名的条目被忽略（比较集合就是 paths）。
func ChangedPaths(root string, paths []string, from map[string]FileInfo) ([]Event, error) {
	rootAbs, err := resolveRoot(root)
	if err != nil {
		return nil, err
	}

	// 归一化旧快照的键：只用于查表，不影响比较集合。
	seen := make(map[string]FileInfo, len(from))
	for path, state := range from {
		seen[filepath.Clean(path)] = state
	}

	events := make([]Event, 0, len(paths))
	checked := make(map[string]struct{}, len(paths))
	for _, requested := range paths {
		abs, resolveErr := resolveUnderRoot(rootAbs, requested)
		if resolveErr != nil {
			return nil, resolveErr
		}
		if _, dup := checked[abs]; dup {
			continue
		}
		checked[abs] = struct{}{}

		prev, existed := seen[abs]
		wantDigest := existed && prev.Digest != ""
		cur, status, statErr := statPath(abs, wantDigest)
		if statErr != nil {
			return nil, fmt.Errorf("filewatch: 读取 %q 状态: %w", abs, statErr)
		}
		if status == pathDirectory {
			continue
		}
		if ev, ok := eventForOne(abs, prev, existed, cur, status == pathEntry, wantDigest); ok {
			events = append(events, ev)
		}
	}
	sort.Slice(events, func(i, j int) bool { return events[i].Path < events[j].Path })
	return events, nil
}

// resolveUnderRoot 把请求路径解析成 root 之下的绝对路径；逃出 root 时报错。
//
// 只做**词法**包含检查（Clean + filepath.Rel）：不解析符号链接，与 WalkDir
// 的口径一致——快照里的路径本来就不会穿过符号链接。因此如果调用方把
// root/link/file（link 是指向外部目录的符号链接）传进来，这里会判为"在 root
// 内"，请只查询来自 Snapshot/Watcher 的路径。
func resolveUnderRoot(root, requested string) (string, error) {
	trimmed := strings.TrimSpace(requested)
	if trimmed == "" {
		return "", fmt.Errorf("filewatch: 路径不能为空")
	}
	if strings.ContainsRune(trimmed, 0) {
		return "", fmt.Errorf("filewatch: 路径含 NUL 字节")
	}
	var abs string
	if filepath.IsAbs(trimmed) {
		abs = filepath.Clean(trimmed)
	} else {
		abs = filepath.Clean(filepath.Join(root, filepath.FromSlash(trimmed)))
	}
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		return "", fmt.Errorf("filewatch: 路径 %q 无法相对 root 解析: %w", requested, err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("filewatch: 路径 %q 逃出 root %q", requested, root)
	}
	return abs, nil
}
