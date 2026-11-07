package skill

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/tools/internal/toolutil"
)

// MaxSkillBytes 限制单个技能文件的读取字节数（更大的文件会截断正文）。
const MaxSkillBytes = 1 << 20

// FileStore 是只读的 rig.SkillStore，由磁盘目录支撑。
//
// 每个目录会被扫描出 <name>/SKILL.md 条目，以及（可选的）顶层 <name>.md 文件。不存在的
// 目录会被忽略；解析到配置目录之外的符号链接会被跳过。store 只保存配置的路径，可安全
// 并发使用。
type FileStore struct {
	dirs []string
}

var _ rig.SkillStore = (*FileStore)(nil)

// NewFileStore 在 dirs 上创建 store（允许目录不存在）。重复与空路径会被去掉；路径会被
// 规范化，但不要求存在。
func NewFileStore(dirs ...string) *FileStore {
	cleaned := make([]string, 0, len(dirs))
	seen := make(map[string]bool, len(dirs))
	for _, dir := range dirs {
		dir = strings.TrimSpace(dir)
		if dir == "" {
			continue
		}
		abs, err := filepath.Abs(dir)
		if err != nil {
			continue
		}
		abs = filepath.Clean(abs)
		if seen[abs] {
			continue
		}
		seen[abs] = true
		cleaned = append(cleaned, abs)
	}
	return &FileStore{dirs: cleaned}
}

// Dirs 返回配置目录的副本。
func (s *FileStore) Dirs() []string { return slices.Clone(s.dirs) }

// DefaultDirs 返回 root 的标准技能搜索路径：冒号分隔的 $ZEOKEEL_SKILLS 条目、
// <root>/.zeokeel/skills 与 ~/.zeokeel/skills。空条目会被跳过。
func DefaultDirs(root string) []string {
	var dirs []string
	if env := strings.TrimSpace(os.Getenv("ZEOKEEL_SKILLS")); env != "" {
		for _, dir := range strings.Split(env, string(os.PathListSeparator)) {
			if dir = strings.TrimSpace(dir); dir != "" {
				dirs = append(dirs, dir)
			}
		}
	}
	if root = strings.TrimSpace(root); root != "" {
		dirs = append(dirs, filepath.Join(root, ".zeokeel", "skills"))
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		dirs = append(dirs, filepath.Join(home, ".zeokeel", "skills"))
	}
	return dirs
}

// NewDefaultFileStore 在 DefaultDirs(root) 上创建 FileStore。
func NewDefaultFileStore(root string) *FileStore { return NewFileStore(DefaultDirs(root)...) }

// List 实现 rig.SkillStore。结果先按目录、再按文件名排序；重名技能保留先出现者。
func (s *FileStore) List(ctx context.Context) ([]rig.Skill, error) {
	skills := make([]rig.Skill, 0)
	seenNames := make(map[string]bool)
	for _, dir := range s.dirs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		found, err := scanSkillDirectory(dir)
		if err != nil {
			return nil, err
		}
		for _, entry := range found {
			if seenNames[entry.Name] {
				continue
			}
			seenNames[entry.Name] = true
			skills = append(skills, entry)
		}
	}
	return skills, nil
}

// Read 实现 rig.SkillStore。
func (s *FileStore) Read(ctx context.Context, name string) (rig.Skill, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return rig.Skill{}, errors.New("skill: name is required")
	}
	skills, err := s.List(ctx)
	if err != nil {
		return rig.Skill{}, err
	}
	for _, entry := range skills {
		if entry.Name == name {
			return entry, nil
		}
	}
	return rig.Skill{}, fmt.Errorf("skill: unknown skill %q (use the skill tool with action=list)", name)
}

// scanSkillDirectory 读取一个技能目录。目录不存在时返回空结果；其他文件系统错误会
// 被上报。
func scanSkillDirectory(dir string) ([]rig.Skill, error) {
	info, err := os.Stat(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("skill: scan %q: %w", dir, err)
	}
	if !info.IsDir() {
		return nil, nil
	}
	realDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return nil, fmt.Errorf("skill: scan %q: %w", dir, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("skill: scan %q: %w", dir, err)
	}
	skills := make([]rig.Skill, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() {
			filePath := filepath.Join(dir, name, "SKILL.md")
			if fileInfo, statErr := os.Stat(filePath); statErr != nil || !fileInfo.Mode().IsRegular() {
				continue
			}
			if !pathWithin(realDir, filePath) {
				continue
			}
			if parsed, ok := readSkillFile(name, filePath); ok {
				skills = append(skills, parsed)
			}
			continue
		}
		// 可选的顶层 README 式技能文件。
		if !entry.Type().IsRegular() {
			continue
		}
		lower := strings.ToLower(name)
		if !strings.HasSuffix(lower, ".md") {
			continue
		}
		filePath := filepath.Join(dir, name)
		if !pathWithin(realDir, filePath) {
			continue
		}
		baseName := name[:len(name)-len(".md")] // lower 已带 .md 后缀
		if baseName == "" {
			continue
		}
		if parsed, ok := readSkillFile(baseName, filePath); ok {
			skills = append(skills, parsed)
		}
	}
	return skills, nil
}

// readSkillFile 解析一份技能文档；不可读的文件会被跳过（ok 为 false），而不是让整个
// 列表失败。
func readSkillFile(name, filePath string) (rig.Skill, bool) {
	file, err := os.Open(filePath)
	if err != nil {
		return rig.Skill{}, false
	}
	defer file.Close()
	data, err := readCapped(file, MaxSkillBytes)
	if err != nil {
		return rig.Skill{}, false
	}
	return parseSkill(name, filePath, string(data)), true
}

// readCapped 最多读取 limit 字节，文件更大时追加截断提示。
func readCapped(file *os.File, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		data = data[:limit]
		data = append(data, []byte("\n[truncated: skill file is larger than the read limit]\n")...)
	}
	return data, nil
}

// parseSkill 从一份技能文档中解析描述与正文。
//
// 支持的描述来源（按顺序）：
//  1. YAML 风格 front matter（`---` ... `---`）中的 `description:`（或
//     `summary:`/`title:`）行；
//  2. 文档顶部附近的普通 `description: ...` 行；
//  3. 第一个 `# 标题`；
//  4. 第一行非空文本。
//
// 格式错误的 front matter 会当作普通文本处理（不剥离）；解析永不失败。
func parseSkill(name, filePath, content string) rig.Skill {
	content = strings.TrimPrefix(content, "\ufeff")
	lines := strings.Split(content, "\n")
	meta := make(map[string]string)
	bodyStart := 0
	if len(lines) > 0 && strings.TrimSpace(lines[0]) == "---" {
		for index := 1; index < len(lines); index++ {
			line := strings.TrimSpace(lines[index])
			if line == "---" {
				bodyStart = index + 1
				break
			}
			key, value, ok := strings.Cut(line, ":")
			if !ok {
				continue
			}
			key = strings.ToLower(strings.TrimSpace(key))
			value = strings.Trim(strings.TrimSpace(value), `"'`)
			if key != "" && value != "" {
				meta[key] = value
			}
		}
		// front matter 未闭合：不剥离任何内容。
		if bodyStart == 0 {
			meta = nil
		}
	}

	description := firstNonEmpty(meta["description"], meta["summary"], meta["title"])
	if description == "" {
		for index := bodyStart; index < len(lines) && index < bodyStart+40; index++ {
			line := strings.TrimSpace(lines[index])
			if _, ok := strings.CutPrefix(strings.ToLower(line), "description:"); ok {
				description = strings.TrimSpace(line[len("description:"):])
				if description != "" {
					break
				}
			}
		}
	}
	if description == "" {
		for index := bodyStart; index < len(lines) && index < bodyStart+40; index++ {
			line := strings.TrimSpace(lines[index])
			if rest, ok := strings.CutPrefix(line, "# "); ok {
				description = strings.TrimSpace(rest)
				break
			}
		}
	}
	if description == "" {
		for index := bodyStart; index < len(lines); index++ {
			line := strings.TrimSpace(lines[index])
			if line == "" || strings.HasPrefix(line, "#") || line == "---" {
				continue
			}
			description = line
			break
		}
	}
	description = toolutil.TruncateBytes(description, 200, "…")
	return rig.Skill{
		Name:        name,
		Description: description,
		Path:        filePath,
		Body:        content,
	}
}

// firstNonEmpty 返回第一个非空的去空白值。
func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

// pathWithin 判断 path（可能是符号链接）解析后是否位于 realDir 内。它是防止技能目录
// 暴露目录之外文件的包含性检查。
func pathWithin(realDir, path string) bool {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return false
	}
	resolved = filepath.Clean(resolved)
	realDir = filepath.Clean(realDir)
	if resolved == realDir {
		return true
	}
	return strings.HasPrefix(resolved, realDir+string(filepath.Separator))
}
