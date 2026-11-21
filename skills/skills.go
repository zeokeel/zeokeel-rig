// Package skills implements filesystem-backed agent skills: discovery of
// SKILL.md roots, bounded listing, full reads and the frontmatter parsing rules.
//
// It is host-agnostic: the host supplies the ordered roots (workspace roots,
// configured extra directories, data-dir and user directories) through Options;
// this package owns the scanning, shadowing warnings and parsing.
package skills

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/zeokeel/zeokeel-rig"
)

const (
	// MaxListedSkills caps one List result; extra skills are reported through
	// Warnings() instead of silently growing the model-facing list.
	MaxListedSkills = 500
	// maxSkillsScannedPerRoot caps the directory entries examined per root.
	maxSkillsScannedPerRoot = 2000
	// maxListReadBytes is how much of one SKILL.md List reads.
	maxListReadBytes = 8 << 10
	// MaxSkillBytes caps the full Read/load file read.
	MaxSkillBytes = 1 << 20
	// maxWarnings caps Warnings() output.
	maxWarnings = 20
)

// Options configures one store.
type Options struct {
	// Roots returns the ordered skill roots (highest precedence first). It is a
	// function so a workspace switch or newly created directory is visible on
	// the next call without cache invalidation.
	Roots func() []string
	// Logger receives scan warnings (nil discards them).
	Logger rig.Logger
}

// Store is the skill discovery/read implementation.
type Store struct {
	opts Options

	mu       sync.Mutex
	warnings []string
}

// New builds a store.
func New(opts Options) *Store {
	if opts.Logger == nil {
		opts.Logger = discardLogger{}
	}
	return &Store{opts: opts}
}

// Roots returns the current root list.
func (s *Store) Roots() []string {
	if s == nil || s.opts.Roots == nil {
		return nil
	}
	return s.opts.Roots()
}

func (s *Store) addWarning(warnings *[]string, message string) {
	if s.opts.Logger != nil {
		s.opts.Logger.Printf("skills: %s", message)
	}
	*warnings = append(*warnings, message)
}

// setWarnings replaces the warning snapshot.
func (s *Store) setWarnings(warnings []string) {
	if len(warnings) > maxWarnings {
		warnings = warnings[:maxWarnings]
	}
	s.mu.Lock()
	s.warnings = warnings
	s.mu.Unlock()
}

// Warnings returns the last List scan's warnings.
func (s *Store) Warnings() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.warnings))
	copy(out, s.warnings)
	return out
}

// firstNonEmptyTrimmed returns the first trimmed non-empty value.
func firstNonEmptyTrimmed(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

// discardLogger drops skill log lines.
type discardLogger struct{}

func (discardLogger) Printf(string, ...any) {}

type skillCandidate struct {
	name string
	path string
}

// scanRoot lists the skill candidates of one root, applying the containment and
// regular-file rules. Missing roots and unreadable entries are skipped (never
// fail the whole list); a broken root is reported as a warning.
func (s *Store) scanRoot(root string, warnings *[]string) []skillCandidate {
	info, err := os.Stat(root)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			s.addWarning(warnings, fmt.Sprintf("skill root %s is unreadable: %v", root, err))
		}
		return nil
	}
	if !info.IsDir() {
		return nil
	}
	realDir, err := filepath.EvalSymlinks(root)
	if err != nil {
		s.addWarning(warnings, fmt.Sprintf("skill root %s cannot be resolved: %v", root, err))
		return nil
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			s.addWarning(warnings, fmt.Sprintf("skill root %s cannot be listed: %v", root, err))
		}
		return nil
	}
	candidates := make([]skillCandidate, 0, len(entries))
	for index, entry := range entries {
		if index >= maxSkillsScannedPerRoot {
			s.addWarning(warnings, fmt.Sprintf("skill root %s has more than %d entries; the rest was not scanned", root, maxSkillsScannedPerRoot))
			break
		}
		name := entry.Name()
		if entry.IsDir() {
			filePath := filepath.Join(root, name, "SKILL.md")
			fileInfo, statErr := os.Stat(filePath)
			if statErr != nil || !fileInfo.Mode().IsRegular() {
				continue
			}
			if !pathWithin(realDir, filePath) {
				continue
			}
			candidates = append(candidates, skillCandidate{name: name, path: filePath})
			continue
		}
		// Optional top-level Markdown skill (<name>.md).
		if !entry.Type().IsRegular() {
			continue
		}
		lower := strings.ToLower(name)
		if !strings.HasSuffix(lower, ".md") {
			continue
		}
		filePath := filepath.Join(root, name)
		if !pathWithin(realDir, filePath) {
			continue
		}
		baseName := name[:len(name)-len(".md")]
		if baseName == "" {
			continue
		}
		candidates = append(candidates, skillCandidate{name: baseName, path: filePath})
	}
	return candidates
}

// List implements rig.SkillStore: name + description + a bounded body excerpt
// for every skill, root precedence first. Reads only maxListReadBytes per file
// so a large skill library does not block the frame or the tool call (S-05).
func (s *Store) List(ctx context.Context) ([]rig.Skill, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	roots := s.Roots()
	warnings := make([]string, 0)
	skills := make([]rig.Skill, 0)
	seenNames := make(map[string]bool)
	for _, root := range roots {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		for _, candidate := range s.scanRoot(root, &warnings) {
			if seenNames[candidate.name] {
				s.addWarning(&warnings, fmt.Sprintf("skill %q from %s is shadowed by a higher-precedence root", candidate.name, filepath.Dir(candidate.path)))
				continue
			}
			seenNames[candidate.name] = true
			if len(skills) >= MaxListedSkills {
				s.addWarning(&warnings, fmt.Sprintf("more than %d skills discovered; the rest was not listed", MaxListedSkills))
				break
			}
			excerpt, truncated, err := readSkillPrefix(candidate.path, maxListReadBytes)
			if err != nil {
				s.addWarning(&warnings, fmt.Sprintf("skill %q cannot be read: %v", candidate.name, err))
				continue
			}
			skills = append(skills, parseSkill(candidate.name, candidate.path, excerpt, truncated))
		}
	}
	s.setWarnings(warnings)
	s.opts.Logger.Printf("skills: listed %d skills from %d roots", len(skills), len(roots))
	return skills, nil
}

// Read implements rig.SkillStore: the full SKILL.md of one skill (first root
// wins, matching List), capped at MaxSkillBytes.
func (s *Store) Read(ctx context.Context, name string) (rig.Skill, error) {
	if err := ctx.Err(); err != nil {
		return rig.Skill{}, err
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return rig.Skill{}, errors.New("skill: name is required")
	}
	for _, root := range s.Roots() {
		if err := ctx.Err(); err != nil {
			return rig.Skill{}, err
		}
		warnings := make([]string, 0)
		for _, candidate := range s.scanRoot(root, &warnings) {
			if candidate.name != name {
				continue
			}
			content, truncated, err := readSkillPrefix(candidate.path, MaxSkillBytes)
			if err != nil {
				return rig.Skill{}, fmt.Errorf("skill: read %q: %w", name, err)
			}
			return parseSkill(name, candidate.path, content, truncated), nil
		}
	}
	return rig.Skill{}, fmt.Errorf("skill: unknown skill %q (use the skill tool with action=list)", name)
}

// parseSkill parses one skill document: the rig rule for the frontmatter block
// (S-06), the description sources (frontmatter description/summary/title, a
// top-of-document description: line, the first heading, the first text line) and
// the Markdown body without the frontmatter.
func parseSkill(name, filePath, content string, truncated bool) rig.Skill {
	body := skillBody(content)
	description := skillDescription(content, body)
	description = toolutilTruncate(description, 200, "…")
	if truncated {
		body += "\n[truncated: skill file is larger than the read limit]\n"
	}
	return rig.Skill{
		Name:        name,
		Description: description,
		Path:        filePath,
		Body:        body,
	}
}

// skillBody returns the Markdown body without its frontmatter block.
func skillBody(content string) string {
	_, body := splitFrontmatter(content)
	return body
}

// frontmatterPair is one raw frontmatter line, preserved for rewrite.
type frontmatterPair struct {
	raw   string
	key   string
	value string
}

// splitFrontmatter splits a SKILL.md document into its frontmatter pairs and
// the Markdown body. It mirrors the rig rule exactly (S-06): the block only
// exists when the first line is exactly "---" and a later line is exactly
// "---"; otherwise the whole content is body and the frontmatter is nil.
func splitFrontmatter(content string) (pairs []frontmatterPair, body string) {
	content = strings.TrimPrefix(content, "\ufeff")
	lines := strings.Split(content, "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return nil, content
	}
	closing := -1
	for index := 1; index < len(lines); index++ {
		if strings.TrimSpace(lines[index]) == "---" {
			closing = index
			break
		}
	}
	if closing < 0 {
		// Unclosed frontmatter is ordinary text (same as rig).
		return nil, content
	}
	pairs = make([]frontmatterPair, 0, closing-1)
	for _, line := range lines[1:closing] {
		key, value, _ := strings.Cut(line, ":")
		pairs = append(pairs, frontmatterPair{
			raw:   line,
			key:   strings.ToLower(strings.TrimSpace(key)),
			value: strings.Trim(strings.TrimSpace(value), `"'`),
		})
	}
	// The guaranteed blank separator line after the closing delimiter is not
	// part of the body (the editor always writes one).
	return pairs, strings.TrimLeft(strings.Join(lines[closing+1:], "\n"), "\r\n")
}

// skillDescription resolves the description with the rig fallback chain.
func skillDescription(content, body string) string {
	pairs, _ := splitFrontmatter(content)
	meta := make(map[string]string, len(pairs))
	for _, pair := range pairs {
		if pair.key != "" && pair.value != "" {
			meta[pair.key] = pair.value
		}
	}
	if description := firstNonEmptyTrimmed(meta["description"], meta["summary"], meta["title"]); description != "" {
		return description
	}
	lines := strings.Split(body, "\n")
	if description := descriptionLine(lines); description != "" {
		return description
	}
	if description := headingLine(lines); description != "" {
		return description
	}
	return firstTextLine(lines)
}

// descriptionLine finds a top-of-body "description:" line (rig fallback 2).
func descriptionLine(lines []string) string {
	for index := 0; index < len(lines) && index < 40; index++ {
		line := strings.TrimSpace(lines[index])
		if rest, ok := strings.CutPrefix(strings.ToLower(line), "description:"); ok {
			if description := strings.TrimSpace(line[len(line)-len(rest):]); description != "" {
				return description
			}
		}
	}
	return ""
}

// headingLine finds the first "# " heading (rig fallback 3).
func headingLine(lines []string) string {
	for index := 0; index < len(lines) && index < 40; index++ {
		line := strings.TrimSpace(lines[index])
		if rest, ok := strings.CutPrefix(line, "# "); ok {
			if description := strings.TrimSpace(rest); description != "" {
				return description
			}
		}
	}
	return ""
}

// firstTextLine returns the first non-empty, non-heading, non-delimiter line
// (rig fallback 4).
func firstTextLine(lines []string) string {
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || trimmed == "---" {
			continue
		}
		return trimmed
	}
	return ""
}

// readSkillPrefix reads at most limit bytes from one skill file and reports
// whether the file is larger than that.
func readSkillPrefix(path string, limit int64) (string, bool, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", false, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return "", false, err
	}
	truncated := int64(len(data)) > limit
	if truncated {
		data = data[:limit]
	}
	return string(data), truncated, nil
}

// toolutilTruncate truncates s to at most limit bytes on a rune boundary and
// appends suffix when it truncated.
func toolutilTruncate(s string, limit int, suffix string) string {
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + suffix
}

// utf8RuneStart reports whether b is not a UTF-8 continuation byte.
func utf8RuneStart(b byte) bool { return b&0xC0 != 0x80 }

// addWarning appends one unique discovery warning (bounded).
// setWarnings publishes this scan's warnings for the settings page.
// Warnings implements contracts.SkillWarnings.
// pathWithin reports whether path (which may be a symlink) resolves inside
// realDir. It is the containment check that keeps a scan from exposing files
// outside a skill root.
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
