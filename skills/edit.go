// The skills editor half: create/update/delete SKILL.md skills below the
// host data directory. Only that directory is writable.

package skills

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/zeokeel/zeokeel-rig"
)

// maxSkillNameRunes bounds a skill slug (its directory name).
const maxSkillNameRunes = 64

// Draft is one create/update request.
type Draft struct {
	Name        string
	Description string
	Body        string
}

// DataDirName is the skill directory inside the host data directory.
const DataDirName = "skills"

// CreateSkill implements rig.SkillsEditor: it validates the draft, writes
// <dataDir>/skills/<slug>/SKILL.md with name/description frontmatter and
// returns the created skill. The service rebuilds a rig FileStore per call
// (there is no cache), so the new skill is immediately visible to List/Read and
// the toolkit. An existing name is rejected instead of overwritten.
func (s *Store) CreateSkill(ctx context.Context, draft Draft) (rig.Skill, error) {
	if err := contextErr(ctx); err != nil {
		return rig.Skill{}, err
	}
	name, err := normalizeSkillName(draft.Name)
	if err != nil {
		return rig.Skill{}, err
	}
	body := strings.TrimSpace(draft.Body)
	if body == "" {
		return rig.Skill{}, errors.New("skill body is required")
	}
	description := normalizeSkillDescription(draft.Description)
	baseDir, err := s.dataDirSkillsDir()
	if err != nil {
		return rig.Skill{}, err
	}
	targetDir := filepath.Join(baseDir, name)
	if !pathWithinDir(baseDir, targetDir) {
		return rig.Skill{}, fmt.Errorf("skill %q escapes the data directory", name)
	}
	if _, err := os.Lstat(targetDir); err == nil {
		return rig.Skill{}, fmt.Errorf("skill %q already exists", name)
	} else if !errors.Is(err, os.ErrNotExist) {
		return rig.Skill{}, fmt.Errorf("stat skill directory: %w", err)
	}
	if err := os.MkdirAll(baseDir, 0o755); err != nil {
		return rig.Skill{}, fmt.Errorf("create skills directory: %w", err)
	}
	if err := os.Mkdir(targetDir, 0o755); err != nil {
		return rig.Skill{}, fmt.Errorf("create skill directory: %w", err)
	}
	path := filepath.Join(targetDir, "SKILL.md")
	content := fmt.Sprintf("---\nname: %s\ndescription: %s\n---\n\n%s", name, description, body)
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		_ = os.Remove(targetDir)
		return rig.Skill{}, fmt.Errorf("create %s: %w", path, err)
	}
	if _, err := file.WriteString(content); err != nil {
		_ = file.Close()
		_ = os.RemoveAll(targetDir)
		return rig.Skill{}, fmt.Errorf("write %s: %w", path, err)
	}
	if err := file.Close(); err != nil {
		_ = os.RemoveAll(targetDir)
		return rig.Skill{}, fmt.Errorf("close %s: %w", path, err)
	}
	// The umask may have narrowed the mode; the file's mode is part of the
	// skill-format contract.
	if err := os.Chmod(path, 0o644); err != nil {
		_ = os.RemoveAll(targetDir)
		return rig.Skill{}, fmt.Errorf("chmod %s: %w", path, err)
	}
	s.opts.Logger.Printf("skills: created skill %q at %s", name, path)
	return rig.Skill{Name: name, Description: description, Path: path, Body: body}, nil
}

// UpdateSkill implements rig.SkillsEditor: it rewrites an existing
// data-dir skill's SKILL.md atomically (temp file in the skill directory,
// chmod 0644, rename over the old file). The draft name must match the target
// name; configured and workspace skills stay read-only. The service has no
// cache, so List/Read see the new content immediately.
func (s *Store) UpdateSkill(ctx context.Context, name string, draft Draft) (rig.Skill, error) {
	if err := contextErr(ctx); err != nil {
		return rig.Skill{}, err
	}
	targetName, err := normalizeSkillName(name)
	if err != nil {
		return rig.Skill{}, err
	}
	draftName, err := normalizeSkillName(draft.Name)
	if err != nil {
		return rig.Skill{}, err
	}
	if draftName != targetName {
		return rig.Skill{}, fmt.Errorf("skill name cannot be changed (got %q, want %q)", draftName, targetName)
	}
	body := strings.TrimSpace(draft.Body)
	if body == "" {
		return rig.Skill{}, errors.New("skill body is required")
	}
	description := normalizeSkillDescription(draft.Description)
	baseDir, err := s.dataDirSkillsDir()
	if err != nil {
		return rig.Skill{}, err
	}
	targetDir := filepath.Join(baseDir, targetName)
	if !pathWithinDir(baseDir, targetDir) {
		return rig.Skill{}, fmt.Errorf("skill %q escapes the data directory", targetName)
	}
	info, err := os.Lstat(targetDir)
	if errors.Is(err, os.ErrNotExist) {
		return rig.Skill{}, fmt.Errorf("skill %q not found in the data directory", targetName)
	}
	if err != nil {
		return rig.Skill{}, fmt.Errorf("stat skill directory: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return rig.Skill{}, fmt.Errorf("skill %q is a symlink; refusing to update it", targetName)
	}
	if !info.IsDir() {
		return rig.Skill{}, fmt.Errorf("skill %q is not a skill directory", targetName)
	}
	path := filepath.Join(targetDir, "SKILL.md")
	if fileInfo, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return rig.Skill{}, fmt.Errorf("skill %q not found in the data directory", targetName)
	} else if err != nil {
		return rig.Skill{}, fmt.Errorf("stat skill file: %w", err)
	} else if fileInfo.Mode()&os.ModeSymlink != 0 {
		return rig.Skill{}, fmt.Errorf("skill %q is a symlink; refusing to update it", targetName)
	}
	// S-04: keep every unknown frontmatter key (allowed-tools, license, …) when
	// the product rewrites the file; only name/description/body are managed here.
	existingContent, err := os.ReadFile(path)
	if err != nil {
		return rig.Skill{}, fmt.Errorf("read %s: %w", path, err)
	}
	content := rewriteSkillDocument(string(existingContent), targetName, description, body)
	if err := writeSkillFileAtomic(path, content); err != nil {
		return rig.Skill{}, err
	}
	s.opts.Logger.Printf("skills: updated skill %q at %s", targetName, path)
	return rig.Skill{Name: targetName, Description: description, Path: path, Body: body}, nil
}

// rewriteSkillDocument rewrites one SKILL.md preserving the frontmatter lines
// this editor does not manage (S-04): the name/description lines are replaced in
// place, unknown keys keep their raw text and order, missing name/description
// lines are added.
func rewriteSkillDocument(existing, name, description, body string) string {
	pairs, _ := splitFrontmatter(existing)
	lines := make([]string, 0, len(pairs)+2)
	haveName, haveDescription := false, false
	for _, pair := range pairs {
		switch pair.key {
		case "name":
			lines = append(lines, "name: "+name)
			haveName = true
		case "description":
			if description != "" {
				lines = append(lines, "description: "+description)
			} else {
				lines = append(lines, "description:")
			}
			haveDescription = true
		default:
			lines = append(lines, pair.raw)
		}
	}
	if !haveName {
		lines = append([]string{"name: " + name}, lines...)
	}
	if !haveDescription {
		lines = append(lines, "description: "+description)
	}
	return "---\n" + strings.Join(lines, "\n") + "\n---\n\n" + body
}

// writeSkillFileAtomic writes content to a temporary file in the target
// directory, fixes its mode to 0644 and renames it over path so a concurrent
// reader never sees a partially written SKILL.md.
func writeSkillFileAtomic(path, content string) error {
	dir := filepath.Dir(path)
	temp, err := os.CreateTemp(dir, ".SKILL.md.tmp-*")
	if err != nil {
		return fmt.Errorf("create temporary skill file: %w", err)
	}
	tempPath := temp.Name()
	cleanup := func() { _ = os.Remove(tempPath) }
	if _, err := temp.WriteString(content); err != nil {
		_ = temp.Close()
		cleanup()
		return fmt.Errorf("write temporary skill file: %w", err)
	}
	if err := temp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("close temporary skill file: %w", err)
	}
	if err := os.Chmod(tempPath, 0o644); err != nil {
		cleanup()
		return fmt.Errorf("chmod temporary skill file: %w", err)
	}
	if err := os.Rename(tempPath, path); err != nil {
		cleanup()
		return fmt.Errorf("replace skill file: %w", err)
	}
	syncDir(filepath.Dir(path))
	return nil
}

// syncDir fsyncs a directory so a completed rename survives a power loss (best
// effort: some platforms/filesystems cannot sync directories).
func syncDir(directory string) {
	handle, err := os.Open(directory)
	if err != nil {
		return
	}
	_ = handle.Sync()
	_ = handle.Close()
}

// DeleteSkill implements rig.SkillsEditor: it deletes one skill directory
// below <dataDir>/skills. Names that are unsafe, absent, or that belong to
// another root (configured/workspace) are rejected with a readable error; a
// directory without SKILL.md is not treated as a skill and is left alone.
func (s *Store) DeleteSkill(ctx context.Context, name string) error {
	if err := contextErr(ctx); err != nil {
		return err
	}
	normalized, err := normalizeSkillName(name)
	if err != nil {
		return err
	}
	baseDir, err := s.dataDirSkillsDir()
	if err != nil {
		return err
	}
	targetDir := filepath.Join(baseDir, normalized)
	if !pathWithinDir(baseDir, targetDir) {
		return fmt.Errorf("skill %q escapes the data directory", normalized)
	}
	info, err := os.Lstat(targetDir)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("skill %q not found in the data directory", normalized)
	}
	if err != nil {
		return fmt.Errorf("stat skill directory: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("skill %q is a symlink; refusing to delete it", normalized)
	}
	if !info.IsDir() {
		return fmt.Errorf("skill %q is not a skill directory", normalized)
	}
	if _, err := os.Stat(filepath.Join(targetDir, "SKILL.md")); errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("skill %q not found in the data directory", normalized)
	} else if err != nil {
		return fmt.Errorf("stat skill file: %w", err)
	}
	// Defense in depth: resolve symlinked parents and confirm the real target
	// still lives below the real data-dir skills directory.
	if realBase, err := filepath.EvalSymlinks(baseDir); err == nil {
		if realTarget, err := filepath.EvalSymlinks(targetDir); err == nil && !pathWithinDir(realBase, realTarget) {
			return fmt.Errorf("skill %q resolves outside the data directory", normalized)
		}
	}
	if err := os.RemoveAll(targetDir); err != nil {
		return fmt.Errorf("delete skill directory: %w", err)
	}
	s.opts.Logger.Printf("skills: deleted skill %q", normalized)
	return nil
}

// contextErr reports a canceled context (nil contexts count as background).
func contextErr(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	return ctx.Err()
}

// dataDirSkillsDir returns the absolute <dataDir>/skills directory.
func (s *Store) dataDirSkillsDir() (string, error) {
	dataDir := strings.TrimSpace(s.opts.DataDir)
	if dataDir == "" {
		return "", errors.New("no data directory for the skills editor")
	}
	return filepath.Abs(filepath.Join(dataDir, DataDirName))
}

// normalizeSkillName validates/trims a skill slug: 1-64 characters of ASCII
// letters/digits, Chinese characters, '-' or '_'. Everything else (paths,
// dots, spaces, newlines) is rejected, so a name is always one safe directory
// component below <dataDir>/skills.
func normalizeSkillName(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	if name == "" {
		return "", errors.New("skill name is required")
	}
	if utf8.RuneCountInString(name) > maxSkillNameRunes {
		return "", fmt.Errorf("skill name is too long (max %d characters)", maxSkillNameRunes)
	}
	for _, char := range name {
		switch {
		case char >= 'a' && char <= 'z':
		case char >= 'A' && char <= 'Z':
		case char >= '0' && char <= '9':
		case char == '-' || char == '_':
		case unicode.Is(unicode.Han, char):
		default:
			return "", fmt.Errorf("skill name %q contains an unsupported character %q (use letters, digits, Chinese, '-' or '_')", name, char)
		}
	}
	return name, nil
}

// normalizeSkillDescription folds an optional description into one safe
// frontmatter line (no newlines that could inject YAML keys).
func normalizeSkillDescription(raw string) string {
	return strings.Join(strings.Fields(raw), " ")
}

// pathWithinDir reports whether path is dir itself or below it (after cleaning;
// both paths must already be absolute).
func pathWithinDir(dir, path string) bool {
	relative, err := filepath.Rel(filepath.Clean(dir), filepath.Clean(path))
	if err != nil {
		return false
	}
	if relative == "." {
		return true
	}
	return relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative)
}
