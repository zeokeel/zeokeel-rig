package sandbox

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ResolvePath validates requested against the policy and returns its real
// absolute path (symbolic links resolved on the longest existing prefix).
//
// Rules, all enforced fail-closed:
//   - no roots configured -> denied (no file access);
//   - NUL bytes and any ".." path component -> denied;
//   - relative paths resolve against the primary (first) root;
//   - absolute paths must fall inside one of the roots;
//   - the resolved path (including symlink targets) must stay inside a root;
//   - paths matching a WithDeniedGlobs pattern -> denied.
//
// The target itself does not have to exist (write_file creates files); only the
// existing ancestor chain is resolved, so a not-yet-created file cannot hide an
// escaping symlink.
func (p *Policy) ResolvePath(requested string) (string, error) {
	if err := p.modeDenial(); err != nil {
		return "", err
	}
	if p == nil || (len(p.roots) == 0 && !p.fullAccess()) {
		return "", deny("no roots configured")
	}
	requested = strings.TrimSpace(requested)
	if requested == "" {
		requested = "."
	}
	if strings.ContainsRune(requested, 0) {
		return "", deny("path contains a NUL byte")
	}
	if p.fullAccess() {
		return resolveFullAccessPath(p, requested)
	}
	if hasDotDotComponent(requested) {
		return "", deny("path %q contains a parent-directory component", requested)
	}

	var candidate string
	if filepath.IsAbs(requested) {
		candidate = filepath.Clean(requested)
	} else {
		candidate = filepath.Join(p.roots[0], filepath.FromSlash(requested))
	}
	resolved, err := resolveExistingPrefix(candidate)
	if err != nil {
		return "", deny("path %q: %v", requested, err)
	}
	resolved = filepath.Clean(resolved)
	if _, ok := p.containingRoot(resolved); !ok {
		return "", deny("path %q is outside the configured roots", requested)
	}
	if err := p.checkDeniedGlobs(resolved); err != nil {
		return "", err
	}
	return resolved, nil
}

// CheckPath reports whether an absolute path is allowed by the policy: it must
// be inside one of the roots and must not match a denied glob. It is the
// post-resolution check tool implementations use for values they computed
// themselves (for example a glob match found by a directory walk).
func (p *Policy) CheckPath(abs string) error {
	if err := p.modeDenial(); err != nil {
		return err
	}
	if p == nil || (len(p.roots) == 0 && !p.fullAccess()) {
		return deny("no roots configured")
	}
	if abs == "" || !filepath.IsAbs(abs) {
		return deny("path %q is not absolute", abs)
	}
	if strings.ContainsRune(abs, 0) {
		return deny("path contains a NUL byte")
	}
	abs = filepath.Clean(abs)
	if p.fullAccess() {
		return nil
	}
	if _, ok := p.containingRoot(abs); !ok {
		return deny("path %q is outside the configured roots", abs)
	}
	return p.checkDeniedGlobs(abs)
}

// OpenPath resolves requested like ResolvePath and opens an *os.Root for the
// root containing it. It returns the open root, the path relative to that root
// (OS-separated, "." when the root itself is requested) and the real absolute
// path. The caller owns the returned *os.Root and must Close it.
//
// Tools use this instead of joining the returned absolute path themselves: all
// I/O then goes through os.Root, which enforces the containment at the
// syscall-adjacent layer and rejects escaping symlinks even if the filesystem
// changes between resolution and use.
func (p *Policy) OpenPath(requested string) (root *os.Root, relative, abs, rootPath string, err error) {
	if err := p.modeDenial(); err != nil {
		return nil, "", "", "", err
	}
	if p == nil || (len(p.roots) == 0 && !p.fullAccess()) {
		return nil, "", "", "", deny("no roots configured")
	}
	abs, err = p.ResolvePath(requested)
	if err != nil {
		return nil, "", "", "", err
	}
	if p.fullAccess() {
		rootPath, relative, err = fullAccessRoot(abs)
		if err != nil {
			return nil, "", "", "", deny("path %q: %v", requested, err)
		}
		opened, err := os.OpenRoot(rootPath)
		if err != nil {
			return nil, "", "", "", deny("root %q is not accessible: %v", rootPath, err)
		}
		return opened, relative, abs, rootPath, nil
	}
	containing, ok := p.containingRoot(abs)
	if !ok {
		return nil, "", "", "", deny("path %q is outside the configured roots", requested)
	}
	rootPath = containing
	relative, err = filepath.Rel(rootPath, abs)
	if err != nil || escapesRelative(relative) {
		return nil, "", "", "", deny("path %q is outside the configured roots", requested)
	}
	opened, err := os.OpenRoot(rootPath)
	if err != nil {
		return nil, "", "", "", deny("root %q is not accessible: %v", rootPath, err)
	}
	return opened, relative, abs, rootPath, nil
}

// checkDeniedGlobs rejects abs when its path relative to the containing root (or
// its base name) matches a denied glob.
func (p *Policy) checkDeniedGlobs(abs string) error {
	rootPath, ok := p.containingRoot(abs)
	if !ok {
		return deny("path %q is outside the configured roots", abs)
	}
	relative, err := filepath.Rel(rootPath, abs)
	if err != nil {
		return deny("path %q cannot be made relative to its root", abs)
	}
	relative = filepath.ToSlash(relative)
	base := pathBase(relative)
	for _, pattern := range p.deniedGlobs {
		if matchGlob(pattern, relative) || (!strings.Contains(pattern, "/") && matchGlob(pattern, base)) {
			return deny("path %q matches denied pattern %q", relative, pattern)
		}
	}
	return nil
}

// containingRoot returns the longest configured root that contains abs.
func (p *Policy) containingRoot(abs string) (string, bool) {
	best := ""
	found := false
	for _, root := range p.roots {
		if !pathWithin(root, abs) {
			continue
		}
		if len(root) > len(best) {
			best = root
			found = true
		}
	}
	return best, found
}

// DeniedPath reports whether abs is visible under the policy (contained in a
// root and not denied). It is CheckPath without an error value, used by tools
// that filter directory-list/glob/grep results.
func (p *Policy) DeniedPath(abs string) bool {
	return p.CheckPath(abs) != nil
}

// pathWithin reports whether path equals root or is a descendant of it.
func pathWithin(root, path string) bool {
	if path == root {
		return true
	}
	if root == string(filepath.Separator) {
		return strings.HasPrefix(path, root)
	}
	return strings.HasPrefix(path, root+string(filepath.Separator))
}

// escapesRelative reports whether a cleaned relative path points outside its
// base (mirrors the built-in filesystem tools' check).
func escapesRelative(relative string) bool {
	if relative == "" || filepath.IsAbs(relative) {
		return true
	}
	if relative == ".." {
		return true
	}
	return strings.HasPrefix(relative, ".."+string(filepath.Separator)) ||
		strings.HasPrefix(relative, "../")
}

// hasDotDotComponent reports whether any separator-delimited component of the
// requested path is "..". Slash and OS separators are both considered, so a
// caller cannot smuggle a traversal through either spelling.
func hasDotDotComponent(requested string) bool {
	normalized := strings.ReplaceAll(requested, "/", string(filepath.Separator))
	for _, component := range strings.Split(normalized, string(filepath.Separator)) {
		if component == ".." {
			return true
		}
	}
	return false
}

// resolveExistingPrefix resolves the longest existing ancestor of path with
// EvalSymlinks and appends the remaining non-existent components. This lets a
// not-yet-created file be validated while still refusing a symlink that escapes
// the root.
func resolveExistingPrefix(path string) (string, error) {
	remaining := make([]string, 0, 4)
	current := path
	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			resolved = filepath.Clean(resolved)
			for i := len(remaining) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, remaining[i])
			}
			return resolved, nil
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", fmt.Errorf("no existing ancestor: %v", err)
		}
		remaining = append(remaining, filepath.Base(current))
		current = parent
	}
}

func pathBase(slashPath string) string {
	if index := strings.LastIndex(slashPath, "/"); index >= 0 {
		return slashPath[index+1:]
	}
	return slashPath
}

// resolveFullAccessPath resolves requested for danger-full-access: no root
// fence, no parent-component rejection, but NUL bytes are still refused.
func resolveFullAccessPath(p *Policy, requested string) (string, error) {
	var candidate string
	if filepath.IsAbs(requested) {
		candidate = filepath.Clean(requested)
	} else if len(p.roots) > 0 {
		candidate = filepath.Join(p.roots[0], filepath.FromSlash(requested))
	} else {
		abs, err := filepath.Abs(requested)
		if err != nil {
			return "", deny("path %q: %v", requested, err)
		}
		candidate = abs
	}
	resolved, err := resolveExistingPrefix(candidate)
	if err != nil {
		return "", deny("path %q: %v", requested, err)
	}
	return filepath.Clean(resolved), nil
}

// fullAccessRoot returns the filesystem root containing abs and abs relative to
// it, so OpenPath can still use os.Root's symlink-safe I/O.
func fullAccessRoot(abs string) (rootPath, relative string, err error) {
	volume := filepath.VolumeName(abs)
	if volume == "" {
		rootPath = string(filepath.Separator)
	} else {
		rootPath = volume + string(filepath.Separator)
	}
	relative, err = filepath.Rel(rootPath, abs)
	if err != nil || escapesRelative(relative) {
		return "", "", fmt.Errorf("outside filesystem root")
	}
	return rootPath, relative, nil
}
