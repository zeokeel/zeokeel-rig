package workspace

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// resolveRelative resolves dir/path relative to root and rejects absolute
// paths, ".." escapes and symbolic links pointing outside the root. The result
// is the canonical (symlink-resolved) absolute path, which the caller must use
// for the actual access.
func resolveRelative(root, requested string) (string, error) {
	requested = strings.TrimSpace(requested)
	if requested == "" || requested == "." {
		return root, nil
	}
	if strings.ContainsRune(requested, 0) {
		return "", fmt.Errorf("workspace: path contains a NUL byte")
	}
	if filepath.IsAbs(requested) {
		return "", fmt.Errorf("%w: %q is absolute", ErrPathOutsideRoot, requested)
	}
	cleaned := filepath.Clean(filepath.FromSlash(requested))
	if filepath.VolumeName(cleaned) != "" {
		return "", fmt.Errorf("%w: %q names a volume", ErrPathOutsideRoot, requested)
	}
	if cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: %q contains a parent-directory escape", ErrPathOutsideRoot, requested)
	}
	candidate := filepath.Join(root, cleaned)
	if !withinRoot(root, candidate) {
		return "", fmt.Errorf("%w: %q", ErrPathOutsideRoot, requested)
	}
	resolved, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		return "", fmt.Errorf("workspace: path %q: %w", requested, err)
	}
	if !withinRoot(root, resolved) {
		return "", fmt.Errorf("%w: %q resolves outside the root", ErrPathOutsideRoot, requested)
	}
	return resolved, nil
}

// withinRoot reports whether path is root or below it.
func withinRoot(root, path string) bool {
	relative, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	if relative == "." {
		return true
	}
	if relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return false
	}
	return !filepath.IsAbs(relative)
}

// Files lists one directory of the current workspace. The result is ordered
// directories first, then files, each by case-insensitive name (exact name
// breaks ties). Paths are relative to the workspace root and slash separated.
func (s *Service) Files(ctx context.Context, dir string) ([]FileEntry, error) {
	if err := contextErr(ctx); err != nil {
		return nil, err
	}
	_, root, err := s.currentWorkspaceRoot()
	if err != nil {
		return nil, err
	}
	directory, err := resolveRelative(root, dir)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(directory)
	if err != nil {
		return nil, fmt.Errorf("workspace: directory %q: %w", dir, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%w: %q", ErrNotDirectory, dir)
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, fmt.Errorf("workspace: list %q: %w", dir, err)
	}
	relativeDir, err := filepath.Rel(root, directory)
	if err != nil {
		return nil, fmt.Errorf("workspace: relative directory: %w", err)
	}
	if relativeDir == "." {
		relativeDir = ""
	}
	result := make([]FileEntry, 0, len(entries))
	for _, entry := range entries {
		if err := contextErr(ctx); err != nil {
			return nil, err
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		path := filepath.ToSlash(filepath.Join(relativeDir, entry.Name()))
		result = append(result, FileEntry{
			Name:    entry.Name(),
			Path:    path,
			Dir:     info.IsDir(),
			Size:    info.Size(),
			ModTime: info.ModTime(),
		})
	}
	sortFileEntries(result)
	return result, nil
}

// sortFileEntries orders directories first, then files, each by
// case-insensitive name.
func sortFileEntries(entries []FileEntry) {
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].Dir != entries[j].Dir {
			return entries[i].Dir
		}
		left, right := strings.ToLower(entries[i].Name), strings.ToLower(entries[j].Name)
		if left != right {
			return left < right
		}
		return entries[i].Name < entries[j].Name
	})
}

// ReadFile reads a regular file of the current workspace. The path must be
// relative to the root (no absolute paths, no ".." and no symlink leaving the
// root). A zero limit uses the configured default; at most that many bytes are
// read, so a large file is truncated rather than loaded whole. A negative limit
// is ErrInvalidLimit.
func (s *Service) ReadFile(ctx context.Context, path string, limit int) ([]byte, error) {
	if err := contextErr(ctx); err != nil {
		return nil, err
	}
	if limit < 0 {
		return nil, fmt.Errorf("%w: %d", ErrInvalidLimit, limit)
	}
	_, root, err := s.currentWorkspaceRoot()
	if err != nil {
		return nil, err
	}
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, errors.New("workspace: file path is required")
	}
	file, err := resolveRelative(root, path)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(file)
	if err != nil {
		return nil, fmt.Errorf("workspace: file %q: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: %q", ErrNotFile, path)
	}
	if limit == 0 {
		limit = s.readLimit
	}
	handle, err := os.Open(file)
	if err != nil {
		return nil, fmt.Errorf("workspace: open %q: %w", path, err)
	}
	defer handle.Close()
	data, err := io.ReadAll(io.LimitReader(handle, int64(limit)))
	if err != nil {
		return nil, fmt.Errorf("workspace: read %q: %w", path, err)
	}
	return data, nil
}

// Search performs a case-insensitive file-name search below the current
// workspace root. Hidden directories (a leading dot) are skipped, unreadable
// directories do not fail the search, and the walk is bounded by the configured
// depth and by limit (0 = DefaultSearchLimit). Results are sorted by path
// (case-insensitive, then exact) and only regular entries are returned.
func (s *Service) Search(ctx context.Context, query string, limit int) ([]FileEntry, error) {
	if err := contextErr(ctx); err != nil {
		return nil, err
	}
	query = strings.ToLower(strings.TrimSpace(query))
	if query == "" {
		return []FileEntry{}, nil
	}
	if limit < 0 {
		return nil, fmt.Errorf("%w: %d", ErrInvalidLimit, limit)
	}
	if limit == 0 {
		limit = DefaultSearchLimit
	}
	_, root, err := s.currentWorkspaceRoot()
	if err != nil {
		return nil, err
	}
	depthLimit := s.searchDepth
	// A host-supplied limit is a result cap, not an allocation size: cap the
	// initial capacity so a huge limit cannot allocate unbounded memory.
	capacity := limit
	if capacity > DefaultSearchLimit {
		capacity = DefaultSearchLimit
	}
	results := make([]FileEntry, 0, capacity)
	var walk func(directory string, depth int) error
	walk = func(directory string, depth int) error {
		if err := contextErr(ctx); err != nil {
			return err
		}
		if len(results) >= limit || depth > depthLimit {
			return nil
		}
		entries, err := os.ReadDir(directory)
		if err != nil {
			return nil // unreadable subdirectories do not fail the search
		}
		for _, entry := range entries {
			if err := contextErr(ctx); err != nil {
				return err
			}
			if len(results) >= limit {
				return nil
			}
			name := entry.Name()
			if entry.IsDir() {
				if strings.HasPrefix(name, ".") {
					continue
				}
				if err := walk(filepath.Join(directory, name), depth+1); err != nil {
					return err
				}
				continue
			}
			if !strings.Contains(strings.ToLower(name), query) {
				continue
			}
			info, err := entry.Info()
			if err != nil {
				continue
			}
			relative, err := filepath.Rel(root, filepath.Join(directory, name))
			if err != nil {
				continue
			}
			results = append(results, FileEntry{
				Name:    name,
				Path:    filepath.ToSlash(relative),
				Dir:     false,
				Size:    info.Size(),
				ModTime: info.ModTime(),
			})
		}
		return nil
	}
	if err := walk(root, 0); err != nil {
		return nil, err
	}
	sort.SliceStable(results, func(i, j int) bool {
		left, right := strings.ToLower(results[i].Path), strings.ToLower(results[j].Path)
		if left != right {
			return left < right
		}
		return results[i].Path < results[j].Path
	})
	return results, nil
}
