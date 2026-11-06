package skill

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/tools/internal/toolutil"
)

// MaxSkillBytes caps how much of one skill file is read (larger files are
// truncated in the body).
const MaxSkillBytes = 1 << 20

// FileStore is a read-only rig.SkillStore backed by directories on disk.
//
// Each directory is scanned for `<name>/SKILL.md` entries and, optionally,
// top-level `<name>.md` files. Directories that do not exist are ignored;
// symlinks that resolve outside a configured directory are skipped. The store
// holds only the configured paths and is safe for concurrent use.
type FileStore struct {
	dirs []string
}

var _ rig.SkillStore = (*FileStore)(nil)

// NewFileStore creates a store over dirs (missing directories are allowed).
// Duplicate and empty paths are removed; paths are cleaned but not required to
// exist.
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

// Dirs returns a copy of the configured directories.
func (s *FileStore) Dirs() []string { return append([]string(nil), s.dirs...) }

// DefaultDirs returns the standard skill search path for root: the
// colon-separated $ZEOKEEL_SKILLS entries, <root>/.zeokeel/skills and
// ~/.zeokeel/skills. Empty entries are skipped.
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

// NewDefaultFileStore creates a FileStore over DefaultDirs(root).
func NewDefaultFileStore(root string) *FileStore { return NewFileStore(DefaultDirs(root)...) }

// List implements rig.SkillStore. The result is ordered by directory and then by
// file name; duplicate skill names keep the first occurrence.
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

// Read implements rig.SkillStore.
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

// scanSkillDirectory reads one skills directory. A missing directory returns no
// skills; other filesystem errors are reported.
func scanSkillDirectory(dir string) ([]rig.Skill, error) {
	info, err := os.Stat(dir)
	if err != nil {
		if os.IsNotExist(err) {
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
		if os.IsNotExist(err) {
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
		// Optional top-level README-style skill files.
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
		baseName := name[:len(name)-len(".md")] // lower has the .md suffix
		if baseName == "" {
			continue
		}
		if parsed, ok := readSkillFile(baseName, filePath); ok {
			skills = append(skills, parsed)
		}
	}
	return skills, nil
}

// readSkillFile parses one skill document; unreadable files are skipped (ok is
// false) rather than failing the whole listing.
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

// readCapped reads up to limit bytes, appending a truncation notice when the
// file is larger.
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

// parseSkill extracts the description and body from one skill document.
//
// Supported description sources, in order:
//  1. YAML-ish front matter (`---` ... `---`) with a `description:` (or
//     `summary:`/`title:`) line;
//  2. a plain `description: ...` line near the top;
//  3. the first `# Heading`;
//  4. the first non-empty text line.
//
// A malformed front matter block is treated as ordinary text (it is not
// stripped); parsing never fails.
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
		// Unterminated front matter: do not strip anything.
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
			if strings.HasPrefix(line, "# ") {
				description = strings.TrimSpace(strings.TrimPrefix(line, "# "))
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

// firstNonEmpty returns the first non-empty trimmed value.
func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

// pathWithin reports whether path (which may be a symlink) resolves inside
// realDir. It is the containment check that keeps a skill directory from
// exposing files outside itself.
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
