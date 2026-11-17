package sandbox

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// 运行期可写授权（path-scoped write grant）。
//
// 沙箱策略的可写面是"工作区 + 平台临时目录"这样的小集合；但真实工具经常把状态
// 写在固定路径（凭据、缓存、套接字）。与其为每个工具硬编码目录，不如让被约束的
// agent 在遇到拒绝时**申请一个具体目录**，由宿主审批后加入可写集。本文件定义
// 该授权的数据、校验规则与运行时存储。
//
// 设计要点：
//
//   - 授权是路径作用域，不是命令作用域：批准一次，随后所有工具都能写该目录；
//    这比"这条命令脱沙箱跑"窄得多，也便于审计。
//   - 校验在 sandbox 包内完成（纯标准库）：绝对路径、禁止 ".."、符号链接逃逸、
//     敏感系统目录、祖先包含工作区根、与保护区冲突、重复申请、次数上限。
//   - 本包只负责"能不能授予"；**是否授予由宿主 gate 决定**，工具在审批通过后
//     才调用 Policy.AddWritableRoot。

// DefaultGrantBudget 是单个策略默认允许的运行期授权次数上限。
const DefaultGrantBudget = 5

var (
	// ErrGrantRedundant 表示请求的路径已经在可写集内（无需授权）。
	ErrGrantRedundant = errors.New("sandbox: path is already writable")
	// ErrGrantBudgetExceeded 表示运行期授权次数已达上限。
	ErrGrantBudgetExceeded = errors.New("sandbox: writable-root grant budget exceeded")
	// ErrGrantSensitivePath 表示请求的路径属于禁止放宽的敏感目录。
	ErrGrantSensitivePath = errors.New("sandbox: refusing to grant a sensitive path")
	// ErrGrantNotAbsolute 表示请求的路径不是绝对路径。
	ErrGrantNotAbsolute = errors.New("sandbox: writable-root grant requires an absolute path")
	// ErrGrantProtected 表示请求的路径与保护路径（只读）冲突。
	ErrGrantProtected = errors.New("sandbox: path is protected read-only")
	// ErrGrantTooBroad 表示请求的路径会连带放开工作区根。
	ErrGrantTooBroad = errors.New("sandbox: path is too broad for a writable-root grant")
)

// Grant 是一次已批准的运行期可写授权。
type Grant struct {
	// Path 是规范化后的绝对路径。
	Path string
	// Reason 是申请方给出的理由（面向用户与审计）。
	Reason string
	// At 是批准时间。
	At time.Time
}

// grantSet 是 Policy 内部的可变授权集合，并发安全。
type grantSet struct {
	mu     sync.Mutex
	max    int
	grants []Grant
}

func newGrantSet(max int) *grantSet {
	if max <= 0 {
		max = DefaultGrantBudget
	}
	return &grantSet{max: max}
}

func (g *grantSet) budget() int {
	if g == nil {
		return DefaultGrantBudget
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.max
}

func (g *grantSet) paths() []string {
	if g == nil {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]string, 0, len(g.grants))
	for _, grant := range g.grants {
		out = append(out, grant.Path)
	}
	return out
}

func (g *grantSet) list() []Grant {
	if g == nil {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]Grant(nil), g.grants...)
}

// add 追加一次授权。调用方必须已完成 ValidateWritableGrantPath。
func (g *grantSet) add(path, reason string) (Grant, error) {
	if g == nil {
		return Grant{}, fmt.Errorf("%w: no grant store", ErrGrantBudgetExceeded)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, existing := range g.grants {
		if existing.Path == path {
			return Grant{}, fmt.Errorf("%w: %s", ErrGrantRedundant, path)
		}
	}
	if len(g.grants) >= g.max {
		return Grant{}, fmt.Errorf("%w: %d grants already approved (limit %d)", ErrGrantBudgetExceeded, len(g.grants), g.max)
	}
	grant := Grant{Path: path, Reason: strings.TrimSpace(reason), At: time.Now()}
	g.grants = append(g.grants, grant)
	return grant, nil
}

// ValidateWritableGrantPath 校验一个运行期可写授权请求，返回规范化后的绝对路径。
//
// 校验是 fail closed 的：任何异常输入都返回错误，而不是退化为"按字面授予"。
// 已存在路径会解析符号链接后再判定；尚不存在的路径要求父目录存在且是目录（因此
// 可以用于"工具首次运行才会创建自己的配置目录"的场景）。
func ValidateWritableGrantPath(path string, roots, configured, protected, granted []string) (string, error) {
	raw := strings.TrimSpace(path)
	if raw == "" {
		return "", fmt.Errorf("%w: empty path", ErrGrantNotAbsolute)
	}
	if hasDotDotSegment(raw) {
		return "", fmt.Errorf("%w: %q contains a %q segment", ErrGrantNotAbsolute, raw, "..")
	}
	if !filepath.IsAbs(raw) {
		return "", fmt.Errorf("%w: %q (relative paths have no stable identity)", ErrGrantNotAbsolute, raw)
	}
	resolved, exists, err := resolvePathForGrant(raw)
	if err != nil {
		return "", err
	}
	if isFilesystemRoot(resolved) {
		return "", fmt.Errorf("%w: %q is a filesystem root", ErrGrantSensitivePath, resolved)
	}
	if isSensitivePath(resolved) {
		return "", fmt.Errorf("%w: %q is a system directory", ErrGrantSensitivePath, resolved)
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if canonical, cerr := filepath.EvalSymlinks(home); cerr == nil {
			home = canonical
		}
		if samePath(resolved, home) {
			return "", fmt.Errorf("%w: %q is the home directory itself; request the specific subdirectory the tool uses", ErrGrantTooBroad, resolved)
		}
	}
	for _, root := range roots {
		if isSameOrInside(resolved, root) {
			return "", fmt.Errorf("%w: %q is already inside the workspace root %q", ErrGrantRedundant, resolved, root)
		}
		if isSameOrInside(root, resolved) {
			return "", fmt.Errorf("%w: %q contains the workspace root %q; request a narrower directory", ErrGrantTooBroad, resolved, root)
		}
	}
	for _, writable := range configured {
		if isSameOrInside(resolved, writable) {
			return "", fmt.Errorf("%w: %q is already writable via %q", ErrGrantRedundant, resolved, writable)
		}
	}
	for _, existing := range granted {
		if isSameOrInside(resolved, existing) {
			return "", fmt.Errorf("%w: %q is already writable via %q", ErrGrantRedundant, resolved, existing)
		}
	}
	for _, protectedPath := range protected {
		if isSameOrInside(resolved, protectedPath) || isSameOrInside(protectedPath, resolved) {
			return "", fmt.Errorf("%w: %q overlaps the protected path %q", ErrGrantProtected, resolved, protectedPath)
		}
	}
	if exists {
		info, statErr := os.Stat(resolved)
		if statErr != nil {
			return "", fmt.Errorf("sandbox: stat %q: %w", resolved, statErr)
		}
		if !info.IsDir() {
			return "", fmt.Errorf("%w: %q is not a directory (request its parent directory)", ErrGrantNotAbsolute, resolved)
		}
		return resolved, nil
	}
	parent := filepath.Dir(resolved)
	parentInfo, statErr := os.Stat(parent)
	if statErr != nil || !parentInfo.IsDir() {
		return "", fmt.Errorf("sandbox: cannot grant %q: parent directory %q is not usable", resolved, parent)
	}
	return resolved, nil
}

// resolvePathForGrant 规范化路径：对最深的已存在祖先解析符号链接，把尚不存在的
// 尾部原样拼回。返回规范化路径与"整条路径是否已存在"。
func resolvePathForGrant(path string) (string, bool, error) {
	cleaned := filepath.Clean(path)
	existing := cleaned
	rest := []string(nil)
	for {
		if _, err := os.Lstat(existing); err == nil {
			break
		}
		parent := filepath.Dir(existing)
		if parent == existing {
			break
		}
		rest = append([]string{filepath.Base(existing)}, rest...)
		existing = parent
	}
	resolved, err := filepath.EvalSymlinks(existing)
	if err != nil {
		return "", false, fmt.Errorf("sandbox: resolve %q: %w", path, err)
	}
	exists := len(rest) == 0
	for _, segment := range rest {
		resolved = filepath.Join(resolved, segment)
	}
	return resolved, exists, nil
}

// hasDotDotSegment 报告路径（按各平台分隔符切分）是否含 ".." 分量。
func hasDotDotSegment(path string) bool {
	for _, segment := range strings.FieldsFunc(path, func(r rune) bool { return r == '/' || r == '\\' }) {
		if segment == ".." {
			return true
		}
	}
	return false
}

// isFilesystemRoot 判断路径是否为文件系统根（"/" 或 "C:\\"）。
func isFilesystemRoot(path string) bool {
	cleaned := filepath.Clean(path)
	return filepath.Dir(cleaned) == cleaned
}

// sensitivePaths 返回禁止被授权为可写根的系统目录（按平台）。
func sensitivePaths() []string {
	switch runtime.GOOS {
	case "windows":
		drive := strings.TrimRight(os.Getenv("SystemDrive"), `\/`)
		if drive == "" {
			drive = `C:`
		}
		return []string{
			filepath.Join(drive, `\Windows`),
			filepath.Join(drive, `\Program Files`),
			filepath.Join(drive, `\Program Files (x86)`),
			filepath.Join(drive, `\ProgramData`),
			filepath.Join(drive, `\Recovery`),
		}
	case "darwin":
		return []string{
			"/etc", "/usr", "/bin", "/sbin", "/dev", "/System", "/Library/System",
			"/private/etc", "/private/var/db", "/private/var/root",
		}
	default:
		return []string{
			"/etc", "/usr", "/bin", "/sbin", "/lib", "/lib64", "/boot",
			"/dev", "/proc", "/sys", "/run", "/var/lib",
		}
	}
}

// isSensitivePath 报告路径是否落在敏感系统目录内（含相等）。
func isSensitivePath(path string) bool {
	for _, sensitive := range sensitivePaths() {
		if isSameOrInside(path, sensitive) {
			return true
		}
	}
	return false
}

// samePath 比较两个已规范化的路径是否指向同一位置（Windows 上忽略大小写）。
func samePath(a, b string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
	}
	return filepath.Clean(a) == filepath.Clean(b)
}

// isSameOrInside 报告 child 是否等于 parent 或位于其下（按路径分量比较，避免
// "/a/bc" 被误判为在 "/a/b" 之下）。
func isSameOrInside(child, parent string) bool {
	child = filepath.Clean(child)
	parent = filepath.Clean(parent)
	if parent == "" || parent == "." {
		return false
	}
	if samePath(child, parent) {
		return true
	}
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// ensureGrantDirectory 确保授权目录存在。父目录必须已经存在且是目录（校验阶段
// 已确认），因此这里只会创建最后一层；权限取 0700（凭据/缓存目录的默认最小面）。
func ensureGrantDirectory(path string) error {
	info, err := os.Stat(path)
	if err == nil {
		if !info.IsDir() {
			return fmt.Errorf("%w: %q is not a directory", ErrGrantNotAbsolute, path)
		}
		return nil
	}
	if !os.IsNotExist(err) {
		return fmt.Errorf("sandbox: stat %q: %w", path, err)
	}
	if mkErr := os.Mkdir(path, 0o700); mkErr != nil {
		return fmt.Errorf("sandbox: cannot create writable-root directory %q (the parent must exist and be writable): %w", path, mkErr)
	}
	return nil
}

// normalizeWritableRoots 规范化静态配置的可写根：保留存在且不敏感的目录，返回
// 保留项与被拒绝项（供宿主启动时 fail loud）。
func normalizeWritableRoots(paths []string) (kept, dropped []string) {
	seen := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		abs, err := filepath.Abs(path)
		if err != nil {
			dropped = append(dropped, path)
			continue
		}
		abs = filepath.Clean(abs)
		if real, err := filepath.EvalSymlinks(abs); err == nil {
			abs = filepath.Clean(real)
		}
		info, err := os.Stat(abs)
		if err != nil || !info.IsDir() {
			// 启动时显式配置的目录常常还没被工具创建（例如凭据目录）。父目录
			// 存在就创建它，让配置在首次运行时就生效；否则记为 dropped 让宿主
			// 报警，而不是静默失效。
			if err == nil || os.IsNotExist(err) {
				if ensureGrantDirectory(abs) == nil && isSafeStartupRoot(abs) {
					kept = append(kept, abs)
					continue
				}
			}
			dropped = append(dropped, path)
			continue
		}
		if isFilesystemRoot(abs) || isSensitivePath(abs) {
			dropped = append(dropped, path)
			continue
		}
		if _, exists := seen[abs]; exists {
			continue
		}
		seen[abs] = struct{}{}
		kept = append(kept, abs)
	}
	return kept, dropped
}

// isSafeStartupRoot 复检启动期自动创建的目录：不允许它落在敏感系统目录或
// 文件系统根上（校验也必须覆盖"刚刚被我们创建出来"的路径）。
func isSafeStartupRoot(path string) bool {
	return !isFilesystemRoot(path) && !isSensitivePath(path)
}

// normalizeProtectedPaths 规范化保护路径：相对路径按主根解析，绝对路径按字面
// 使用。**保留不存在的路径**——"保护一个尚不存在的目录"是有意义的（例如会话
// 中途 git init 出来的 .git），静默丢弃会让保护变成空承诺。能否渲染由后端决定：
// Seatbelt 的 subpath 规则对不存在的路径同样生效；bwrap/Landlock 需要真实路径，
// 因此 Profile 会把它们放进 MissingProtected 供调用方报警。
func normalizeProtectedPaths(roots, paths []string) []string {
	out := make([]string, 0, len(paths))
	seen := make(map[string]struct{}, len(paths))
	primary := ""
	if len(roots) > 0 {
		primary = roots[0]
	}
	for _, path := range paths {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		abs := path
		if !filepath.IsAbs(abs) {
			if primary == "" {
				continue
			}
			abs = filepath.Join(primary, abs)
		}
		abs = filepath.Clean(abs)
		if real, err := filepath.EvalSymlinks(abs); err == nil {
			abs = filepath.Clean(real)
		}
		if _, exists := seen[abs]; exists {
			continue
		}
		seen[abs] = struct{}{}
		out = append(out, abs)
	}
	return out
}

// dedupeStrings 保序去重。
func dedupeStrings(values []string) []string {
	out := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}
