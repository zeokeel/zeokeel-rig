package builtin

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/zeokeel/zeokeel-rig"
)

// errStopWalk stops a walk early (a cap was reached); it never reaches callers.
var errStopWalk = errors.New("builtin: stop walk")

// --- read_file -------------------------------------------------------------

type readFileArgs struct {
	Path   string `json:"path"`
	Offset int    `json:"offset"`
	Limit  int    `json:"limit"`
}

func newReadFile(root fsRoot) rig.Tool {
	return &builtinTool{
		name: ToolReadFile,
		description: "Read a UTF-8 text file inside the tool root and return numbered lines. " +
			"Use offset (1-based line) and limit (max lines) to page through large files. " +
			"The output is truncated: at most 2000 lines, 2000 bytes per line and 256 KiB total.",
		parameters: objectSchema(map[string]any{
			"path":   stringProp("File path relative to the tool root (an absolute path inside the root is accepted)."),
			"offset": integerProp("First line to return, 1-based (default 1)."),
			"limit":  integerProp("Maximum number of lines to return (default and maximum 2000)."),
		}, "path"),
		run: func(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
			var args readFileArgs
			if err := decodeArgs(call, &args); err != nil {
				return rig.ToolResult{}, err
			}
			if strings.TrimSpace(args.Path) == "" {
				return errorResult("read_file: path is required"), nil
			}
			if err := ctx.Err(); err != nil {
				return rig.ToolResult{}, err
			}
			offset := args.Offset
			if offset == 0 {
				offset = 1
			}
			if offset < 1 {
				return errorResult("read_file: offset must be >= 1, got %d", args.Offset), nil
			}
			limit := args.Limit
			if limit <= 0 {
				limit = MaxReadLines
			}
			if limit > MaxReadLines {
				limit = MaxReadLines
			}

			fsys, relative, _, err := root.openFor(args.Path)
			if err != nil {
				return errorResultErr(err), nil
			}
			defer fsys.Close()
			file, err := fsys.Open(relative)
			if err != nil {
				return errorResult("read_file: %v", err), nil
			}
			info, err := file.Stat()
			if err != nil {
				file.Close()
				return errorResult("read_file: %v", err), nil
			}
			if info.IsDir() {
				file.Close()
				return errorResult("read_file: %q is a directory", args.Path), nil
			}
			data, err := io.ReadAll(io.LimitReader(file, MaxReadBytes+1))
			file.Close()
			if err != nil {
				return errorResult("read_file: %v", err), nil
			}
			truncatedBytes := len(data) > MaxReadBytes
			if truncatedBytes {
				data = data[:MaxReadBytes]
			}
			lines := splitLines(string(data))
			if offset > len(lines) {
				if truncatedBytes {
					return textResult("[truncated: file is larger than 256 KiB]"), nil
				}
				return textResult(""), nil
			}
			end := offset - 1 + limit
			truncatedLines := false
			if end < len(lines) {
				truncatedLines = true
			} else {
				end = len(lines)
			}
			var builder strings.Builder
			for index := offset - 1; index < end; index++ {
				fmt.Fprintf(&builder, "%d\t%s\n", index+1, truncateLine(lines[index], MaxReadLineBytes))
			}
			switch {
			case truncatedBytes:
				builder.WriteString(truncationNotice("file is larger than 256 KiB; showing the first lines"))
			case truncatedLines:
				builder.WriteString(truncationNotice(fmt.Sprintf(
					"showing lines %d-%d of %d; use offset/limit to page", offset, end, len(lines))))
			}
			return textResult(builder.String()), nil
		},
	}
}

// splitLines splits text into lines, dropping the empty element a trailing
// newline produces (so line numbers match an editor).
func splitLines(text string) []string {
	lines := strings.Split(text, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// --- write_file ------------------------------------------------------------

type writeFileArgs struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

func newWriteFile(root fsRoot) rig.Tool {
	return &builtinTool{
		name: ToolWriteFile,
		description: "Create or overwrite a text file inside the tool root. " +
			"Missing parent directories are created. Prefer edit_file for small changes to an existing file.",
		parameters: objectSchema(map[string]any{
			"path":    stringProp("File path relative to the tool root (an absolute path inside the root is accepted)."),
			"content": stringProp("Full file content to write."),
		}, "path", "content"),
		run: func(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
			var args writeFileArgs
			if err := decodeArgs(call, &args); err != nil {
				return rig.ToolResult{}, err
			}
			if strings.TrimSpace(args.Path) == "" {
				return errorResult("write_file: path is required"), nil
			}
			if root.readOnly() {
				return errorResult("write_file: sandbox policy is read-only"), nil
			}
			if err := ctx.Err(); err != nil {
				return rig.ToolResult{}, err
			}
			fsys, relative, _, err := root.openFor(args.Path)
			if err != nil {
				return errorResultErr(err), nil
			}
			defer fsys.Close()
			if relative == "." {
				return errorResult("write_file: path must name a file, not the tool root"), nil
			}
			if parent := path.Dir(relative); parent != "." {
				if err := fsys.MkdirAll(parent, 0o755); err != nil {
					return errorResult("write_file: %v", err), nil
				}
			}
			if err := fsys.WriteFile(relative, []byte(args.Content), 0o644); err != nil {
				return errorResult("write_file: %v", err), nil
			}
			return textResult(fmt.Sprintf("wrote %d bytes to %s", len(args.Content), relative)), nil
		},
	}
}

// --- edit_file -------------------------------------------------------------

type editFileArgs struct {
	Path       string `json:"path"`
	OldString  string `json:"old_string"`
	NewString  string `json:"new_string"`
	ReplaceAll bool   `json:"replace_all"`
}

func newEditFile(root fsRoot) rig.Tool {
	return &builtinTool{
		name: ToolEditFile,
		description: "Replace exact text in an existing file inside the tool root. " +
			"old_string must occur exactly once unless replace_all is true; a missing or ambiguous match is an error.",
		parameters: objectSchema(map[string]any{
			"path":        stringProp("File path relative to the tool root (an absolute path inside the root is accepted)."),
			"old_string":  stringProp("Exact text to replace (must be unique unless replace_all is true)."),
			"new_string":  stringProp("Replacement text (may be empty to delete)."),
			"replace_all": booleanProp("Replace every occurrence (default false)."),
		}, "path", "old_string", "new_string"),
		run: func(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
			var args editFileArgs
			if err := decodeArgs(call, &args); err != nil {
				return rig.ToolResult{}, err
			}
			switch {
			case strings.TrimSpace(args.Path) == "":
				return errorResult("edit_file: path is required"), nil
			case args.OldString == "":
				return errorResult("edit_file: old_string must not be empty"), nil
			}
			if root.readOnly() {
				return errorResult("edit_file: sandbox policy is read-only"), nil
			}
			if err := ctx.Err(); err != nil {
				return rig.ToolResult{}, err
			}
			fsys, relative, _, err := root.openFor(args.Path)
			if err != nil {
				return errorResultErr(err), nil
			}
			defer fsys.Close()
			data, err := fsys.ReadFile(relative)
			if err != nil {
				return errorResult("edit_file: %v", err), nil
			}
			if len(data) > MaxEditFileBytes {
				return errorResult("edit_file: file is larger than %d bytes; write_file is safer for a full rewrite", MaxEditFileBytes), nil
			}
			content := string(data)
			count := strings.Count(content, args.OldString)
			switch {
			case count == 0:
				return errorResult("edit_file: old_string not found in %s", relative), nil
			case count > 1 && !args.ReplaceAll:
				return errorResult("edit_file: old_string occurs %d times in %s; set replace_all=true or make old_string unique", count, relative), nil
			}
			replacements := 1
			if args.ReplaceAll {
				replacements = -1
			}
			updated := strings.Replace(content, args.OldString, args.NewString, replacements)
			if err := fsys.WriteFile(relative, []byte(updated), 0o644); err != nil {
				return errorResult("edit_file: %v", err), nil
			}
			return textResult(fmt.Sprintf("edited %s: replaced %d occurrence(s)", relative, count)), nil
		},
	}
}

// --- list_dir --------------------------------------------------------------

type listDirArgs struct {
	Path string `json:"path"`
}

func newListDir(root fsRoot) rig.Tool {
	return &builtinTool{
		name: ToolListDir,
		description: "List one directory inside the tool root: directories first (with a trailing /), then files, " +
			"both sorted by name. The tool root itself is listed when path is omitted.",
		parameters: objectSchema(map[string]any{
			"path": stringProp("Directory path relative to the tool root (default: the root itself)."),
		}),
		run: func(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
			var args listDirArgs
			if err := decodeArgs(call, &args); err != nil {
				return rig.ToolResult{}, err
			}
			if err := ctx.Err(); err != nil {
				return rig.ToolResult{}, err
			}
			fsys, relative, rootDir, err := root.openFor(args.Path)
			if err != nil {
				return errorResultErr(err), nil
			}
			defer fsys.Close()
			directory, err := fsys.Open(relative)
			if err != nil {
				return errorResult("list_dir: %v", err), nil
			}
			entries, err := directory.ReadDir(-1)
			directory.Close()
			if err != nil {
				return errorResult("list_dir: %v", err), nil
			}
			if root.policy != nil {
				kept := entries[:0]
				for _, entry := range entries {
					if root.denied(absolutePath(rootDir, path.Join(relative, entry.Name()))) {
						continue
					}
					kept = append(kept, entry)
				}
				entries = kept
			}
			sort.SliceStable(entries, func(i, j int) bool {
				if entries[i].IsDir() != entries[j].IsDir() {
					return entries[i].IsDir()
				}
				return entries[i].Name() < entries[j].Name()
			})
			lines := make([]string, 0, len(entries))
			for _, entry := range entries {
				name := entry.Name()
				if entry.IsDir() {
					name += "/"
				}
				lines = append(lines, name)
			}
			if len(lines) > MaxDirEntries {
				rest := len(lines) - MaxDirEntries
				lines = lines[:MaxDirEntries]
				lines = append(lines, fmt.Sprintf("[truncated: %d more entries]", rest))
			}
			return textResult(strings.Join(lines, "\n")), nil
		},
	}
}

// --- glob ------------------------------------------------------------------

type globArgs struct {
	Pattern string `json:"pattern"`
	Root    string `json:"root"`
}

func newGlob(root fsRoot) rig.Tool {
	return &builtinTool{
		name: ToolGlob,
		description: "Find files inside the tool root whose path matches a glob pattern. " +
			"* and ? match within one path segment, ** matches any number of segments (e.g. **/*.go). " +
			"Results are sorted paths relative to the tool root and capped at 500.",
		parameters: objectSchema(map[string]any{
			"pattern": stringProp("Glob pattern relative to root, e.g. **/*.go or internal/**/*_test.go."),
			"root":    stringProp("Optional subdirectory to search from (default: the tool root)."),
		}, "pattern"),
		run: func(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
			var args globArgs
			if err := decodeArgs(call, &args); err != nil {
				return rig.ToolResult{}, err
			}
			pattern := filepath.ToSlash(strings.TrimSpace(args.Pattern))
			if pattern == "" {
				return errorResult("glob: pattern is required"), nil
			}
			if strings.ContainsRune(pattern, 0) {
				return errorResult("glob: pattern contains a NUL byte"), nil
			}
			if strings.HasPrefix(pattern, "/") || strings.HasPrefix(pattern, "../") || pattern == ".." {
				return errorResult("glob: pattern must be relative to the tool root, got %q", args.Pattern), nil
			}
			if err := ctx.Err(); err != nil {
				return rig.ToolResult{}, err
			}
			fsys, base, rootDir, err := root.openFor(args.Root)
			if err != nil {
				return errorResultErr(err), nil
			}
			defer fsys.Close()
			matches := make([]string, 0, 16)
			truncated := false
			walkErr := fs.WalkDir(fsys.FS(), base, func(current string, entry fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if err := ctx.Err(); err != nil {
					return err
				}
				if root.policy != nil && root.denied(absolutePath(rootDir, current)) {
					if entry.IsDir() {
						return fs.SkipDir
					}
					return nil
				}
				if entry.IsDir() || !entry.Type().IsRegular() {
					return nil
				}
				relativeToBase := relativeTo(base, current)
				if !matchPattern(pattern, relativeToBase) {
					return nil
				}
				if len(matches) >= MaxGlobMatches {
					truncated = true
					return errStopWalk
				}
				matches = append(matches, current)
				return nil
			})
			if walkErr != nil && !errors.Is(walkErr, errStopWalk) {
				if err := ctx.Err(); err != nil {
					return rig.ToolResult{}, err
				}
				return errorResult("glob: %v", walkErr), nil
			}
			sort.Strings(matches)
			if truncated {
				matches = append(matches, truncationNotice(fmt.Sprintf("more than %d matches", MaxGlobMatches)))
			}
			return textResult(strings.Join(matches, "\n")), nil
		},
	}
}

// relativeTo returns current relative to base using slash separators.
func relativeTo(base, current string) string {
	if base == "." {
		return current
	}
	if current == base {
		return "."
	}
	return strings.TrimPrefix(current, base+"/")
}

// matchPattern matches a slash-separated glob pattern against a slash-separated
// path. Supported wildcards: ** (any number of segments), * and ? (within one
// segment). Everything else is literal.
func matchPattern(pattern, name string) bool {
	return matchSegments(strings.Split(pattern, "/"), strings.Split(name, "/"))
}

func matchSegments(pattern, name []string) bool {
	if len(pattern) == 0 {
		return len(name) == 0
	}
	if pattern[0] == "**" {
		for index := 0; index <= len(name); index++ {
			if matchSegments(pattern[1:], name[index:]) {
				return true
			}
		}
		return false
	}
	if len(name) == 0 {
		return false
	}
	if !matchSegment(pattern[0], name[0]) {
		return false
	}
	return matchSegments(pattern[1:], name[1:])
}

// matchSegment matches one path segment against a pattern segment containing *
// and ? wildcards.
func matchSegment(pattern, name string) bool {
	patternIndex, nameIndex := 0, 0
	starIndex, starName := -1, 0
	for nameIndex < len(name) {
		switch {
		case patternIndex < len(pattern) && (pattern[patternIndex] == name[nameIndex] || pattern[patternIndex] == '?'):
			patternIndex++
			nameIndex++
		case patternIndex < len(pattern) && pattern[patternIndex] == '*':
			starIndex = patternIndex
			starName = nameIndex
			patternIndex++
		case starIndex >= 0:
			patternIndex = starIndex + 1
			starName++
			nameIndex = starName
		default:
			return false
		}
	}
	for patternIndex < len(pattern) && pattern[patternIndex] == '*' {
		patternIndex++
	}
	return patternIndex == len(pattern)
}

// --- grep ------------------------------------------------------------------

type grepArgs struct {
	Pattern string `json:"pattern"`
	Path    string `json:"path"`
	Include string `json:"include"`
}

func newGrep(root fsRoot) rig.Tool {
	return &builtinTool{
		name: ToolGrep,
		description: "Search file contents inside the tool root with a Go regular expression. " +
			"Returns matching lines as file:line:text. include filters file names with a glob (e.g. *.go). " +
			"Results are capped at 200 matching lines.",
		parameters: objectSchema(map[string]any{
			"pattern": stringProp("Go (RE2) regular expression to search for."),
			"path":    stringProp("File or directory relative to the tool root (default: the root)."),
			"include": stringProp("Optional file-name glob filter, e.g. *.go or *_test.go."),
		}, "pattern"),
		run: func(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
			var args grepArgs
			if err := decodeArgs(call, &args); err != nil {
				return rig.ToolResult{}, err
			}
			if args.Pattern == "" {
				return errorResult("grep: pattern is required"), nil
			}
			expression, err := regexp.Compile(args.Pattern)
			if err != nil {
				return errorResult("grep: invalid regular expression: %v", err), nil
			}
			include := strings.TrimSpace(args.Include)
			if include != "" {
				if _, err := filepath.Match(include, "x"); err != nil {
					return errorResult("grep: invalid include glob: %v", err), nil
				}
			}
			if err := ctx.Err(); err != nil {
				return rig.ToolResult{}, err
			}
			fsys, target, rootDir, err := root.openFor(args.Path)
			if err != nil {
				return errorResultErr(err), nil
			}
			defer fsys.Close()
			info, err := fsys.Stat(target)
			if err != nil {
				return errorResult("grep: %v", err), nil
			}
			accumulator := &grepAccumulator{}
			searchOne := func(filePath string) error {
				if root.policy != nil && root.denied(absolutePath(rootDir, filePath)) {
					return nil
				}
				if include != "" {
					matched, err := filepath.Match(include, path.Base(filePath))
					if err != nil || !matched {
						return nil
					}
				}
				return accumulator.searchFile(fsys, filePath, expression)
			}
			if !info.IsDir() {
				if err := searchOne(target); err != nil {
					if ctxErr := ctx.Err(); ctxErr != nil {
						return rig.ToolResult{}, ctxErr
					}
					return errorResult("grep: %v", err), nil
				}
				return textResult(accumulator.content()), nil
			}
			walkErr := fs.WalkDir(fsys.FS(), target, func(current string, entry fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if err := ctx.Err(); err != nil {
					return err
				}
				if root.policy != nil && root.denied(absolutePath(rootDir, current)) {
					if entry.IsDir() {
						return fs.SkipDir
					}
					return nil
				}
				if entry.IsDir() || !entry.Type().IsRegular() {
					return nil
				}
				if err := searchOne(current); err != nil {
					return err
				}
				if accumulator.full() {
					return errStopWalk
				}
				return nil
			})
			if walkErr != nil && !errors.Is(walkErr, errStopWalk) {
				if ctxErr := ctx.Err(); ctxErr != nil {
					return rig.ToolResult{}, ctxErr
				}
				return errorResult("grep: %v", walkErr), nil
			}
			return textResult(accumulator.content()), nil
		},
	}
}

// grepAccumulator bounds grep's model-facing output.
type grepAccumulator struct {
	lines     []string
	bytes     int
	truncated bool
}

func (a *grepAccumulator) full() bool {
	return len(a.lines) >= MaxGrepMatches || a.bytes >= MaxGrepMatches*MaxGrepLineBytes
}

func (a *grepAccumulator) searchFile(fsys *os.Root, filePath string, expression *regexp.Regexp) error {
	info, err := fsys.Stat(filePath)
	if err != nil {
		return nil // a file that disappeared mid-walk is not an error for a search
	}
	if info.Size() > MaxGrepFileBytes {
		return nil
	}
	data, err := fsys.ReadFile(filePath)
	if err != nil {
		return nil
	}
	if bytes.IndexByte(data, 0) >= 0 {
		return nil // binary; skip
	}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 0, 64*1024), MaxGrepFileBytes)
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := scanner.Text()
		if !expression.MatchString(line) {
			continue
		}
		if len(a.lines) >= MaxGrepMatches {
			a.truncated = true
			return nil
		}
		formatted := fmt.Sprintf("%s:%d:%s", filePath, lineNumber, truncateLine(line, MaxGrepLineBytes))
		a.lines = append(a.lines, formatted)
		a.bytes += len(formatted) + 1
		if a.bytes >= MaxGrepMatches*MaxGrepLineBytes {
			a.truncated = true
			return nil
		}
	}
	return scanner.Err()
}

func (a *grepAccumulator) content() string {
	if len(a.lines) == 0 {
		return ""
	}
	content := strings.Join(a.lines, "\n")
	if a.truncated || len(a.lines) >= MaxGrepMatches {
		content += truncationNotice(fmt.Sprintf("showing the first %d matching lines", len(a.lines)))
	}
	return content
}
