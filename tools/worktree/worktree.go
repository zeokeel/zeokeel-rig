// Package worktree provides the enter_worktree, exit_worktree and
// list_worktrees tools: git worktree management confined by a sandbox.Policy.
//
// The primary sandbox root must be the Git repository root. All Git mutations
// go through the system `git` executable (never manual file manipulation), so
// Git remains the single source of truth for worktree metadata. Tool paths are
// resolved through Policy.ResolvePath, so traversal, symlink escapes and denied
// globs are refused before a command runs.
//
// Worktrees created without an explicit path live under
// <repository>/.rig/worktrees/<branch>. exit_worktree removes the checkout and
// keeps its branch by default; pass keep_branch=false to delete the branch too.
//
// If `git` is not installed, a path is outside the sandbox, or the workspace is
// not a Git repository, the tools return readable model-facing errors
// (ToolResult.IsError=true) instead of Go errors.
package worktree

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
	"unicode/utf8"

	v1 "github.com/zeokeel/zeokeel-rig/contracts/v1"
	"github.com/zeokeel/zeokeel-rig/sandbox"
)

const (
	// ToolEnter is the create tool name.
	ToolEnter = "enter_worktree"
	// ToolExit is the remove tool name.
	ToolExit = "exit_worktree"
	// ToolList is the list tool name.
	ToolList = "list_worktrees"
	// DefaultGitTimeout bounds one git invocation when the policy has no
	// timeout.
	DefaultGitTimeout = 30 * time.Second
	// maxGitTimeout caps the git timeout derived from the policy.
	maxGitTimeout = 2 * time.Minute
	// maxGitOutputBytes caps one captured git stream.
	maxGitOutputBytes = 64 << 10
	// maxGitErrorBytes caps the git stderr quoted in an error.
	maxGitErrorBytes = 4 << 10
	// maxOutputBytes caps one tool response.
	maxOutputBytes = 32 << 10
	// managedDir is the repository-relative directory for default worktrees.
	managedDir = ".rig/worktrees"
)

// branchPattern keeps a branch name to one safe Git branch component (no path
// separators, no leading '-', no traversal, no whitespace).
var branchPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// Tools returns enter_worktree, exit_worktree and list_worktrees bound to policy.
// A nil policy fails closed (sandbox.New denies every path).
func Tools(policy *sandbox.Policy) []v1.Tool {
	if policy == nil {
		policy = sandbox.New()
	}
	ops := &operations{policy: policy, timeout: clampTimeout(policy.Timeout())}
	return []v1.Tool{
		&enterTool{ops: ops},
		&exitTool{ops: ops},
		&listTool{ops: ops},
	}
}

// operations carries the immutable configuration shared by the three tools.
type operations struct {
	policy  *sandbox.Policy
	timeout time.Duration
}

// clampTimeout bounds the policy timeout used for git invocations.
func clampTimeout(timeout time.Duration) time.Duration {
	if timeout <= 0 {
		return DefaultGitTimeout
	}
	if timeout > maxGitTimeout {
		return maxGitTimeout
	}
	return timeout
}

// workspaceRoot returns the primary sandbox root, which must be the Git
// repository root.
func (o *operations) workspaceRoot() (string, error) {
	roots := o.policy.Roots()
	if len(roots) == 0 {
		return "", errors.New("sandbox policy has no roots; set the repository root as the primary root")
	}
	root := roots[0]
	if err := o.policy.CheckPath(root); err != nil {
		return "", err
	}
	info, err := os.Stat(root)
	if err != nil {
		return "", fmt.Errorf("inspect workspace root: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("workspace root %q is not a directory", root)
	}
	return root, nil
}

// repository is one validated repository view. workspace is the policy's
// primary root; top is Git's toplevel (the same directory when configured
// correctly).
type repository struct {
	workspace string
	top       string
}

// repository resolves and validates the Git repository containing the primary
// root. The repository root must be inside the sandbox (normally it is the
// primary root itself).
func (o *operations) repository(ctx context.Context) (repository, error) {
	workspace, err := o.workspaceRoot()
	if err != nil {
		return repository{}, err
	}
	if _, err := exec.LookPath("git"); err != nil {
		return repository{}, fmt.Errorf("git executable not found: %w", err)
	}
	top, err := o.gitOutput(ctx, workspace, "rev-parse", "--show-toplevel")
	if err != nil {
		return repository{}, fmt.Errorf("workspace is not inside a git repository: %w", err)
	}
	top = strings.TrimSpace(top)
	if top == "" {
		return repository{}, errors.New("workspace is not inside a git repository: git returned no toplevel")
	}
	if real, err := filepath.EvalSymlinks(top); err == nil {
		top = filepath.Clean(real)
	}
	if err := o.policy.CheckPath(top); err != nil {
		return repository{}, fmt.Errorf("git repository root %q is outside the sandbox roots; make the repository root the primary sandbox root: %w", top, err)
	}
	return repository{workspace: workspace, top: top}, nil
}

// gitResult is the captured output of one git invocation. A non-zero exit is
// not an error here; callers decide whether the status is meaningful.
type gitResult struct {
	stdout   string
	stderr   string
	exitCode int
}

// runGit runs git in dir with a bounded timeout and a scrubbed environment.
func (o *operations) runGit(ctx context.Context, dir string, args ...string) (gitResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	runCtx, cancel := context.WithTimeout(ctx, o.timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, "git", args...)
	cmd.Dir = dir
	cmd.Env = gitEnv()
	stdout := &limitedBuffer{max: maxGitOutputBytes}
	stderr := &limitedBuffer{max: maxGitOutputBytes}
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	runErr := cmd.Run()
	result := gitResult{stdout: stdout.String(), stderr: stderr.String()}
	if err := runCtx.Err(); err != nil {
		command := "git " + strings.Join(args, " ")
		if errors.Is(err, context.DeadlineExceeded) {
			return result, fmt.Errorf("%s timed out after %s", command, o.timeout)
		}
		return result, fmt.Errorf("%s canceled: %w", command, err)
	}
	if runErr != nil {
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			result.exitCode = exitErr.ExitCode()
			return result, nil
		}
		return result, fmt.Errorf("run git: %w", runErr)
	}
	return result, nil
}

// gitOutput runs git and returns trimmed stdout, converting a non-zero exit to
// a descriptive error.
func (o *operations) gitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	result, err := o.runGit(ctx, dir, args...)
	if err != nil {
		return "", err
	}
	if result.exitCode != 0 {
		return "", gitError(result, args)
	}
	return strings.TrimSpace(result.stdout), nil
}

// gitError renders a failed git invocation with a bounded stderr excerpt.
func gitError(result gitResult, args []string) error {
	detail := strings.TrimSpace(result.stderr)
	if detail == "" {
		detail = strings.TrimSpace(result.stdout)
	}
	detail = truncateText(detail, maxGitErrorBytes)
	command := "git " + strings.Join(args, " ")
	if detail == "" {
		return fmt.Errorf("%s failed with exit status %d", command, result.exitCode)
	}
	return fmt.Errorf("%s failed with exit status %d: %s", command, result.exitCode, detail)
}

// gitRefExists reports whether ref exists (exit 0) or not (exit 1); other exit
// codes are operational errors.
func (o *operations) gitRefExists(ctx context.Context, dir, ref string) (bool, error) {
	result, err := o.runGit(ctx, dir, "show-ref", "--verify", "--quiet", ref)
	if err != nil {
		return false, err
	}
	switch result.exitCode {
	case 0:
		return true, nil
	case 1:
		return false, nil
	default:
		return false, gitError(result, []string{"show-ref", "--verify", "--quiet", ref})
	}
}

// gitEnv returns an environment with Git directory overrides removed, so git
// always operates on the checkout selected by the command directory.
func gitEnv() []string {
	env := make([]string, 0, len(os.Environ())+2)
	for _, entry := range os.Environ() {
		key, _, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		switch strings.ToUpper(key) {
		case "GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY",
			"GIT_COMMON_DIR", "GIT_NAMESPACE", "GIT_PREFIX":
			continue
		}
		env = append(env, entry)
	}
	env = append(env, "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	return env
}

// limitedBuffer keeps at most max bytes written to it and reports how many
// bytes were discarded.
type limitedBuffer struct {
	max       int
	buf       bytes.Buffer
	truncated bool
}

// Write implements io.Writer.
func (b *limitedBuffer) Write(data []byte) (int, error) {
	remaining := b.max - b.buf.Len()
	if remaining > 0 {
		if len(data) > remaining {
			_, _ = b.buf.Write(data[:remaining])
			b.truncated = true
		} else {
			_, _ = b.buf.Write(data)
		}
	} else if len(data) > 0 {
		b.truncated = true
	}
	return len(data), nil
}

// String returns the retained output with a truncation notice when needed.
func (b *limitedBuffer) String() string {
	out := b.buf.String()
	if b.truncated {
		out += "\n[truncated: git output exceeded the capture limit]"
	}
	return out
}

// validateBranch rejects branch names that are unsafe as a path element or as a
// Git option argument.
func validateBranch(branch string) error {
	if branch == "" {
		return errors.New("branch is empty")
	}
	if !branchPattern.MatchString(branch) {
		return fmt.Errorf("invalid branch name %q: use 1-64 letters, digits, '.', '_' or '-' (must start with a letter or digit; no spaces, slashes or leading '-')", branch)
	}
	return nil
}

// generatedBranch returns a unique safe branch name ("rig-wt-<random>").
func generatedBranch() string {
	return "rig-wt-" + strconv.FormatInt(time.Now().UnixNano(), 36)
}

// branchFromPath derives a safe branch name from a worktree path's base name.
func branchFromPath(target string) string {
	base := filepath.Base(filepath.Clean(target))
	var builder strings.Builder
	for _, char := range base {
		switch {
		case char >= 'a' && char <= 'z',
			char >= 'A' && char <= 'Z',
			char >= '0' && char <= '9',
			char == '.', char == '_', char == '-':
			builder.WriteRune(char)
		default:
			if builder.Len() > 0 {
				builder.WriteByte('-')
			}
		}
	}
	candidate := strings.Trim(builder.String(), "._-")
	if !branchPattern.MatchString(candidate) {
		return generatedBranch()
	}
	return candidate
}

// enterTool implements enter_worktree.
type enterTool struct{ ops *operations }

var _ v1.Tool = (*enterTool)(nil)

// Name implements v1.Tool.
func (t *enterTool) Name() string { return ToolEnter }

// Description implements v1.Tool.
func (t *enterTool) Description() string {
	return `Create a Git worktree on a new branch off the current HEAD.

Optional "path" selects the checkout directory (an absolute or root-relative
path inside the sandbox; it must not exist yet). Optional "branch" names the new
branch; when omitted it is derived from the path's base name, or generated when
no path is given. Without "path" the worktree is created at
<repository>/.rig/worktrees/<branch>.

The primary sandbox root must be the Git repository root, and the repository
must have at least one commit. Uncommitted files stay in the main checkout. The
result reports the created path, branch and base commit. Use exit_worktree to
remove it.`
}

// Schema implements v1.Tool.
func (t *enterTool) Schema() map[string]any {
	return objectSchema(map[string]any{
		"path":   stringProp("Worktree checkout directory inside the sandbox (default .rig/worktrees/<branch>)."),
		"branch": stringProp("New branch name (default derived from the path or generated)."),
	})
}

// enterArgs is the decoded enter_worktree argument object.
type enterArgs struct {
	Path   string `json:"path"`
	Branch string `json:"branch"`
}

// Invoke implements v1.Tool.
func (t *enterTool) Invoke(ctx context.Context, call v1.ToolCall) (v1.ToolResult, error) {
	if err := ctx.Err(); err != nil {
		return v1.ToolResult{}, err
	}
	var a enterArgs
	if err := decodeArgs(call, &a); err != nil {
		return v1.ToolResult{}, err
	}
	if t.ops.policy.ReadOnly() {
		return errorResult("%s: sandbox policy is read-only", ToolEnter), nil
	}
	repo, err := t.ops.repository(ctx)
	if err != nil {
		return errorResult("%s: %v", ToolEnter, err), nil
	}

	branch := strings.TrimSpace(a.Branch)
	pathArg := strings.TrimSpace(a.Path)
	var target string
	if pathArg != "" {
		target, err = t.ops.policy.ResolvePath(pathArg)
		if err != nil {
			return errorResult("%s: path %q: %v", ToolEnter, pathArg, err), nil
		}
		if branch == "" {
			branch = branchFromPath(target)
		}
	} else {
		if branch == "" {
			branch = generatedBranch()
		}
		if err := validateBranch(branch); err != nil {
			return errorResult("%s: %v", ToolEnter, err), nil
		}
		target = filepath.Join(repo.top, filepath.FromSlash(managedDir), branch)
		target, err = t.ops.policy.ResolvePath(target)
		if err != nil {
			return errorResult("%s: default worktree path %q: %v", ToolEnter, target, err), nil
		}
	}
	if err := validateBranch(branch); err != nil {
		return errorResult("%s: %v", ToolEnter, err), nil
	}
	if samePath(target, repo.top) || samePath(target, repo.workspace) {
		return errorResult("%s: worktree path must differ from the repository root", ToolEnter), nil
	}
	if _, err := os.Lstat(target); err == nil {
		return errorResult("%s: worktree path %q already exists", ToolEnter, target), nil
	} else if !os.IsNotExist(err) {
		return errorResult("%s: inspect %q: %v", ToolEnter, target, err), nil
	}
	exists, err := t.ops.gitRefExists(ctx, repo.workspace, "refs/heads/"+branch)
	if err != nil {
		return errorResult("%s: check branch %q: %v", ToolEnter, branch, err), nil
	}
	if exists {
		return errorResult("%s: branch %q already exists", ToolEnter, branch), nil
	}
	baseCommit, err := t.ops.gitOutput(ctx, repo.workspace, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return errorResult("%s: repository has no usable HEAD commit: %v", ToolEnter, err), nil
	}

	// Create the parent directories through the sandbox root so the checkout
	// location is validated by the same policy as the path itself.
	root, relative, _, _, err := t.ops.policy.OpenPath(target)
	if err != nil {
		return errorResult("%s: %v", ToolEnter, err), nil
	}
	parent := filepath.Dir(relative)
	if parent != "." && parent != "" {
		if err := root.MkdirAll(parent, 0o755); err != nil {
			_ = root.Close()
			return errorResult("%s: create worktree parent directory: %v", ToolEnter, err), nil
		}
	}
	_ = root.Close()

	if _, err := t.ops.gitOutput(ctx, repo.workspace, "worktree", "add", "-b", branch, target, "HEAD"); err != nil {
		// The target did not exist before this call; clean up the partial
		// checkout so a retry is not blocked by a half-created directory.
		_ = os.RemoveAll(target)
		return errorResult("%s: %v", ToolEnter, err), nil
	}
	return textResult(truncateOutput(fmt.Sprintf("created worktree %s on branch %s (base %s)",
		target, branch, shortCommit(baseCommit)), maxOutputBytes)), nil
}

// exitTool implements exit_worktree.
type exitTool struct{ ops *operations }

var _ v1.Tool = (*exitTool)(nil)

// Name implements v1.Tool.
func (t *exitTool) Name() string { return ToolExit }

// Description implements v1.Tool.
func (t *exitTool) Description() string {
	return `Remove a Git worktree created by enter_worktree.

"path" is required (the checkout path returned by enter_worktree; absolute or
root-relative inside the sandbox). The path must be a registered worktree of
the repository; the main checkout cannot be removed. The branch is kept by
default; set "keep_branch" to false to delete it as well (a branch that cannot
be deleted is reported as a warning after the checkout is removed).

Runs "git worktree remove --force"; uncommitted changes in the worktree are
discarded.`
}

// Schema implements v1.Tool.
func (t *exitTool) Schema() map[string]any {
	return objectSchema(map[string]any{
		"path":        stringProp("Worktree path returned by enter_worktree (required)."),
		"keep_branch": boolProp("Keep the branch after removing the worktree (default true)."),
	}, "path")
}

// exitArgs is the decoded exit_worktree argument object.
type exitArgs struct {
	Path       string `json:"path"`
	KeepBranch *bool  `json:"keep_branch"`
}

// Invoke implements v1.Tool.
func (t *exitTool) Invoke(ctx context.Context, call v1.ToolCall) (v1.ToolResult, error) {
	if err := ctx.Err(); err != nil {
		return v1.ToolResult{}, err
	}
	var a exitArgs
	if err := decodeArgs(call, &a); err != nil {
		return v1.ToolResult{}, err
	}
	pathArg := strings.TrimSpace(a.Path)
	if pathArg == "" {
		return errorResult("%s: path is required", ToolExit), nil
	}
	if t.ops.policy.ReadOnly() {
		return errorResult("%s: sandbox policy is read-only", ToolExit), nil
	}
	repo, err := t.ops.repository(ctx)
	if err != nil {
		return errorResult("%s: %v", ToolExit, err), nil
	}
	target, err := t.ops.policy.ResolvePath(pathArg)
	if err != nil {
		return errorResult("%s: path %q: %v", ToolExit, pathArg, err), nil
	}
	worktrees, err := t.ops.listWorktrees(ctx, repo.workspace)
	if err != nil {
		return errorResult("%s: %v", ToolExit, err), nil
	}
	entry, found := findWorktree(worktrees, target)
	if !found {
		return errorResult("%s: %q is not a registered worktree of %s", ToolExit, target, repo.top), nil
	}
	if samePath(entry.path, repo.top) || samePath(entry.path, repo.workspace) {
		return errorResult("%s: refusing to remove the main working tree", ToolExit), nil
	}
	if _, err := t.ops.gitOutput(ctx, repo.workspace, "worktree", "remove", "--force", entry.path); err != nil {
		return errorResult("%s: %v", ToolExit, err), nil
	}

	keepBranch := true
	if a.KeepBranch != nil {
		keepBranch = *a.KeepBranch
	}
	result := fmt.Sprintf("removed worktree %s", entry.path)
	switch {
	case entry.branch == "":
		// Detached worktree: nothing to delete.
	case keepBranch:
		result += fmt.Sprintf(" (branch %s kept)", entry.branch)
	default:
		if _, err := t.ops.gitOutput(ctx, repo.workspace, "branch", "-D", entry.branch); err != nil {
			result += fmt.Sprintf(" (warning: branch %s was not deleted: %v)", entry.branch, err)
		} else {
			result += fmt.Sprintf(" (branch %s deleted)", entry.branch)
		}
	}
	return textResult(truncateOutput(result, maxOutputBytes)), nil
}

// listTool implements list_worktrees.
type listTool struct{ ops *operations }

var _ v1.Tool = (*listTool)(nil)

// Name implements v1.Tool.
func (t *listTool) Name() string { return ToolList }

// Description implements v1.Tool.
func (t *listTool) Description() string {
	return `List the repository's Git worktrees as a table with columns PATH, BRANCH, HEAD and STATE.

Reads "git worktree list --porcelain"; the main checkout is marked "main" and
linked worktrees "managed" when they live under .rig/worktrees. Detached, bare,
locked and prunable checkouts are marked accordingly. The call is read-only and
takes no arguments.`
}

// Schema implements v1.Tool.
func (t *listTool) Schema() map[string]any {
	return objectSchema(map[string]any{})
}

// Invoke implements v1.Tool.
func (t *listTool) Invoke(ctx context.Context, call v1.ToolCall) (v1.ToolResult, error) {
	if err := ctx.Err(); err != nil {
		return v1.ToolResult{}, err
	}
	var a struct{}
	if err := decodeArgs(call, &a); err != nil {
		return v1.ToolResult{}, err
	}
	repo, err := t.ops.repository(ctx)
	if err != nil {
		return errorResult("%s: %v", ToolList, err), nil
	}
	worktrees, err := t.ops.listWorktrees(ctx, repo.workspace)
	if err != nil {
		return errorResult("%s: %v", ToolList, err), nil
	}
	var builder strings.Builder
	writer := tabwriter.NewWriter(&builder, 0, 4, 2, ' ', 0)
	fmt.Fprintln(writer, "PATH\tBRANCH\tHEAD\tSTATE")
	for _, worktree := range worktrees {
		fmt.Fprintf(writer, "%s\t%s\t%s\t%s\n",
			worktree.path, displayBranch(worktree.branch), shortCommit(worktree.head), worktreeState(worktree, repo.top))
	}
	_ = writer.Flush()
	return textResult(truncateOutput(strings.TrimSuffix(builder.String(), "\n"), maxOutputBytes)), nil
}

// worktree is one parsed "git worktree list --porcelain" record.
type worktree struct {
	path           string
	head           string
	branch         string
	branchRef      string
	detached       bool
	bare           bool
	locked         bool
	lockReason     string
	prunable       bool
	prunableReason string
}

// listWorktrees runs and parses "git worktree list --porcelain".
func (o *operations) listWorktrees(ctx context.Context, dir string) ([]worktree, error) {
	raw, err := o.gitOutput(ctx, dir, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	return parseWorktreePorcelain(raw), nil
}

// parseWorktreePorcelain parses the block format emitted by
// "git worktree list --porcelain".
func parseWorktreePorcelain(raw string) []worktree {
	worktrees := make([]worktree, 0, 4)
	var current *worktree
	flush := func() {
		if current != nil && current.path != "" {
			worktrees = append(worktrees, *current)
		}
		current = nil
	}
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSuffix(line, "\r")
		if strings.TrimSpace(line) == "" {
			flush()
			continue
		}
		if current == nil {
			current = &worktree{}
		}
		switch {
		case strings.HasPrefix(line, "worktree "):
			current.path = strings.TrimPrefix(line, "worktree ")
		case strings.HasPrefix(line, "HEAD "):
			current.head = strings.TrimPrefix(line, "HEAD ")
		case strings.HasPrefix(line, "branch "):
			ref := strings.TrimPrefix(line, "branch ")
			current.branchRef = ref
			current.branch = strings.TrimPrefix(ref, "refs/heads/")
		case line == "detached":
			current.detached = true
		case line == "bare":
			current.bare = true
		case strings.HasPrefix(line, "locked"):
			current.locked = true
			current.lockReason = strings.TrimSpace(strings.TrimPrefix(line, "locked"))
		case strings.HasPrefix(line, "prunable"):
			current.prunable = true
			current.prunableReason = strings.TrimSpace(strings.TrimPrefix(line, "prunable"))
		}
	}
	flush()
	return worktrees
}

// findWorktree returns the registered worktree whose path matches target.
func findWorktree(worktrees []worktree, target string) (worktree, bool) {
	want := canonicalPath(target)
	for _, worktree := range worktrees {
		if canonicalPath(worktree.path) == want {
			return worktree, true
		}
	}
	return worktree{}, false
}

// canonicalPath returns a comparison-friendly absolute path: symlinks are
// resolved when possible and the result is cleaned (case-folded on Windows).
func canonicalPath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	if real, err := filepath.EvalSymlinks(path); err == nil {
		path = real
	}
	path = filepath.Clean(path)
	if runtime.GOOS == "windows" {
		path = strings.ToLower(path)
	}
	return path
}

// samePath reports whether two paths name the same location.
func samePath(a, b string) bool {
	return canonicalPath(a) != "" && canonicalPath(a) == canonicalPath(b)
}

// displayBranch renders a detached worktree branch as "(detached)".
func displayBranch(branch string) string {
	if strings.TrimSpace(branch) == "" {
		return "(detached)"
	}
	return branch
}

// shortCommit truncates one commit hash for table output.
func shortCommit(commit string) string {
	commit = strings.TrimSpace(commit)
	if commit == "" {
		return "-"
	}
	if len(commit) > 12 {
		return commit[:12]
	}
	return commit
}

// worktreeState renders the flags of one worktree for table output.
func worktreeState(worktree worktree, repoTop string) string {
	states := make([]string, 0, 4)
	if samePath(worktree.path, repoTop) {
		states = append(states, "main")
	} else if strings.Contains(filepath.ToSlash(worktree.path), "/"+managedDir+"/") {
		states = append(states, "managed")
	}
	if worktree.bare {
		states = append(states, "bare")
	}
	if worktree.detached {
		states = append(states, "detached")
	}
	if worktree.locked {
		states = append(states, "locked")
	}
	if worktree.prunable {
		states = append(states, "prunable")
	}
	if len(states) == 0 {
		return "linked"
	}
	return strings.Join(states, ",")
}

// truncateOutput clamps model-facing output to max bytes without splitting a
// UTF-8 rune.
func truncateOutput(content string, max int) string {
	if max <= 0 || len(content) <= max {
		return content
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(content[cut]) {
		cut--
	}
	return content[:cut] + "\n[truncated: output exceeded the worktree tool limit]"
}

// truncateText truncates a git error excerpt without splitting a UTF-8 rune.
func truncateText(content string, max int) string {
	if max <= 0 || len(content) <= max {
		return content
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(content[cut]) {
		cut--
	}
	return content[:cut] + "…"
}

// objectSchema builds one tool argument JSON Schema.
func objectSchema(properties map[string]any, required ...string) map[string]any {
	schema := map[string]any{
		"type":                 "object",
		"properties":           properties,
		"additionalProperties": false,
	}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

// stringProp builds one string property schema.
func stringProp(description string) map[string]any {
	return map[string]any{"type": "string", "description": description}
}

// boolProp builds one boolean property schema.
func boolProp(description string) map[string]any {
	return map[string]any{"type": "boolean", "description": description}
}

// decodeArgs decodes one call's argument JSON. Missing arguments decode as an
// empty object.
func decodeArgs(call v1.ToolCall, target any) error {
	raw := strings.TrimSpace(call.Arguments)
	if raw == "" {
		raw = "{}"
	}
	if err := json.Unmarshal([]byte(raw), target); err != nil {
		return fmt.Errorf("invalid arguments JSON: %w", err)
	}
	return nil
}

// textResult builds a successful tool result.
func textResult(content string) v1.ToolResult { return v1.ToolResult{Content: content} }

// errorResult builds a model-facing tool failure.
func errorResult(format string, args ...any) v1.ToolResult {
	return v1.ToolResult{Content: fmt.Sprintf(format, args...), IsError: true}
}
