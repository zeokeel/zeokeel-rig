package sandbox

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
)

// ErrWritableRootNotFound 表示要撤销的路径不在这份策略的可写面里（未知路径、空路径，
// 或者它是不可撤销的主根——那种情况同时包装 ErrWritableRootIsPrimary）。
var ErrWritableRootNotFound = errors.New("sandbox: path is not a writable root of this policy")

// ErrWritableRootIsPrimary 表示要撤销的根是策略主根（Roots()[0]）：它同时是相对
// 路径的解析基准，撤销它等于让策略失去全部文件访问，因此永远拒绝。
//
// 它是与 ErrWritableRootNotFound **可区分**的哨兵：撤销主根的错误同时包装两个哨兵
// （保持既有调用方的分支不变），所以判断顺序必须是**先** errors.Is(err,
// ErrWritableRootIsPrimary) 再 errors.Is(err, ErrWritableRootNotFound)。
//
// 典型调用方（kit.Runtime.RemoveRoot）原本把 NotFound 当成"这个 Agent 本来就没有
// 这根"直接跳过，于是"主根不可撤销"和"未知路径"混为一谈，调用方拿到 nil 却什么
// 都没撤销。有了这个哨兵，调用方能把前者当成需要报给用户的错误。
var ErrWritableRootIsPrimary = errors.New("sandbox: writable root is the primary root and cannot be removed")

// 可写面模型：一个可写根 + 根内必须保持只读的子路径。
//
// 这是"策略承诺的可写面"的**唯一**表达。它存在的理由是一次真实的跨平台不一致：
// 以前 `.git` 被整体放进 ProtectedPaths，于是三个 OS 后端都拦掉了 `git commit`
// （index/objects/refs 都写不了），而进程内的文件工具根本不知道 `.git` 被保护
// （CheckPath 只看根）。同一个策略在两个执行路径上给出不同结论，正
// `roots.ts` 指出的坑——**可写面必须只有一个函数决定**。
//
// 现在：
//   - Policy.WritableRoots() 返回模型；
//   - BuildProfile 从它派生四个 OS 后端的渲染输入（bwrap/Landlock/Seatbelt/ACL）；
//   - Policy.AllowsWrite 用同一份模型判定进程内写入；
//   - 工具链缓存（沙箱内 go build/npm ci 需要的宿主缓存）作为**runner 级追加**
//     由 osrunner 合并（见 sandbox/osrunner/toolchain_caches.go），不进策略模型：
//     它们是"给子进程的构建缓存"，不是"给 agent 的文件写入面"。
type WritableRoot struct {
	// Root 是可写根（规范化的绝对路径）。
	Root string
	// ReadOnlySubpaths 是 Root 之内必须保持只读的子路径（绝对路径；构造
	// WritableRoot 时传相对路径会按 Root 解析）。它们可以指向文件或目录，
	// 目录表示整棵子树只读。
	ReadOnlySubpaths []string
	// DegradeOnUnionBackends 表示：在**无法表达"根可写但子路径只读"**的后端上
	// （目前只有 Landlock：它的 path-beneath 规则是并集语义，没有否定规则），
	// 允许把整根降级为可写，而不是让整根变成不可写。
	//
	// 只有 git 元数据需要它：`git commit` 必须在 `.git` 目录里新建
	// `index.lock` 再改名覆盖 `index`（同一个目录里的 create+write+rename），
	// 而并集语义下只要 `.git` 自身不给 MAKE_*/WRITE_FILE 就做不到。降级后
	// `.git/config`、`.git/hooks` 在 Landlock 下变成可写——这条损失由
	// RunnerLimits() 如实上报，进程内围栏与 bwrap/Seatbelt/ACL 仍然拦住它们。
	DegradeOnUnionBackends bool
}

// WithWritableRoot 声明一个可写根，并可选地标出根内只读的子路径。
//
// root 命中已有根（策略根或之前追加的可写根）时，只登记子路径；否则先追加该根。
// 相对子路径按 root 解析。
func WithWritableRoot(root string, readOnlySubpaths ...string) Option {
	return func(policy *Policy) {
		policy.DeclareWritableRoot(root, readOnlySubpaths...)
	}
}

// WithReadOnlySubpaths 在某个已存在的可写根内标出只读子路径（等价于
// WithWritableRoot 的"只加子路径"用法）。
func WithReadOnlySubpaths(root string, subpaths ...string) Option {
	return WithWritableRoot(root, subpaths...)
}

// WithToolchainCaches 控制 OS runner 是否追加平台工具链缓存根（默认 **true**）。
//
// 关掉它 = 回到"纯白名单"姿态：沙箱里的 `go build`、`npm ci`、`pip install`、
// `cargo build` 会因为 GOCACHE/GOMODCACHE/~/.npm/pip cache 不可写而失败。个人
// 客户端里这些是日常动作，所以默认开；需要最严格的隔离时再关。
func WithToolchainCaches(include bool) Option {
	return func(policy *Policy) { policy.toolchainCaches = include }
}

// IncludeToolchainCaches 报告 runner 是否应追加工具链缓存根。
func (p *Policy) IncludeToolchainCaches() bool {
	if p == nil {
		return false
	}
	return p.toolchainCaches
}

// DeclareWritableRoot 在构造之后追加可写根/只读子路径。
//
// 名字里没有 "Add" 前缀是为了与运行期授权 AddWritableRoot(path, reason) 区分：
// 那个是会记录审计、受预算与审批约束的**授权**；这个只是策略的静态声明。
func (p *Policy) DeclareWritableRoot(root string, readOnlySubpaths ...string) {
	if p == nil {
		return
	}
	root = strings.TrimSpace(root)
	if root == "" {
		return
	}
	resolved, ok := p.canonicalWritableRoot(root)
	p.mu.Lock()
	defer p.mu.Unlock()
	if !ok {
		p.droppedWritableRoots = append(p.droppedWritableRoots, root)
		return
	}
	if !slices.Contains(p.extraWritableRoots, resolved) {
		p.extraWritableRoots = append(p.extraWritableRoots, resolved)
	}
	p.addReadOnlySubpathsLocked(resolved, readOnlySubpaths...)
}

// AddReadOnlySubpaths 在 root 内登记只读子路径；root 必须已经可写（策略根或
// 追加可写根），否则按 root 先追加一个可写根。
func (p *Policy) AddReadOnlySubpaths(root string, subpaths ...string) {
	if p == nil {
		return
	}
	p.DeclareWritableRoot(root, subpaths...)
}

// DeclareDegradableWritableRoot 声明一个"在并集语义后端允许降级为整根可写"的
// 可写根（见 WritableRoot.DegradeOnUnionBackends）。git 元数据用它。
func (p *Policy) DeclareDegradableWritableRoot(root string, readOnlySubpaths ...string) {
	if p == nil {
		return
	}
	root = strings.TrimSpace(root)
	if root == "" {
		return
	}
	resolved, ok := p.canonicalWritableRoot(root)
	if !ok {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !slices.Contains(p.extraWritableRoots, resolved) {
		p.extraWritableRoots = append(p.extraWritableRoots, resolved)
	}
	if p.degradableRoots == nil {
		p.degradableRoots = make(map[string]struct{})
	}
	p.degradableRoots[resolved] = struct{}{}
	p.addReadOnlySubpathsLocked(resolved, readOnlySubpaths...)
}

// addReadOnlySubpathsLocked 是内部实现：把子路径规范化并挂在 root 下。
//
// 调用方**必须**已持有 p.mu 的写锁（见 DeclareWritableRoot /
// DeclareDegradableWritableRoot）。它读写的 readOnlySubpaths 是并发读侧
// （WritableRoots → AllowsWrite）直接遍历的 map。
func (p *Policy) addReadOnlySubpathsLocked(root string, subpaths ...string) {
	for _, sub := range subpaths {
		resolved := canonicalSubpath(root, sub)
		if resolved == "" {
			continue
		}
		if slices.Contains(p.readOnlySubpaths[root], resolved) {
			continue
		}
		if p.readOnlySubpaths == nil {
			p.readOnlySubpaths = make(map[string][]string)
		}
		p.readOnlySubpaths[root] = append(p.readOnlySubpaths[root], resolved)
	}
}

// RemoveWritableRoot 撤销一个可写根：策略根、显式追加的可写根（WithWritableRoot /
// DeclareWritableRoot）与运行期授权（AddWritableRoot）都在撤销范围内。
//
// 语义：
//   - 撤销立即生效，所有工具（含 OS runner 渲染的可写面）看到的是同一份结果——
//     AllowsWrite / WritableRootsFor 与这里共用一份可写面模型；
//   - 主根（Roots()[0]）**不可**撤销：它同时是相对路径的解析基准，删掉会让策略
//     失去全部文件访问，因此报 ErrWritableRootIsPrimary（同时包装
//     ErrWritableRootNotFound，见该哨兵的说明）且可写面不变；
//   - 撤销策略根同时把它从读围栏里移除（撤销是收紧，不是"只读化"）；
//   - 路径不在可写面里时返回 ErrWritableRootNotFound，让调用方能区分"撤销成功"
//     与"本来就没有"。
func (p *Policy) RemoveWritableRoot(path string) error {
	if p == nil {
		return ErrWritableRootNotFound
	}
	path = strings.TrimSpace(path)
	if path == "" {
		return ErrWritableRootNotFound
	}
	abs := canonicalRemovableRoot(path)
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.roots) > 0 && PathEqual(p.roots[0], abs) {
		// 同时包装两个哨兵：ErrWritableRootIsPrimary 让调用方能区分"主根不可撤销"
		// 与"本来就没有这个根"；ErrWritableRootNotFound 保持既有调用方（kit 目前
		// 把它当成"这个 Agent 没有这根"跳过）的分支不变。区分时先判 IsPrimary。
		return fmt.Errorf("%w: %w: %q",
			ErrWritableRootNotFound, ErrWritableRootIsPrimary, abs)
	}
	removed := false
	if kept := withoutPath(p.roots, abs); len(kept) != len(p.roots) {
		p.roots = kept
		removed = true
	}
	if kept := withoutPath(p.writableRoots, abs); len(kept) != len(p.writableRoots) {
		p.writableRoots = kept
		removed = true
	}
	if kept := withoutPath(p.extraWritableRoots, abs); len(kept) != len(p.extraWritableRoots) {
		p.extraWritableRoots = kept
		removed = true
	}
	// 授权撤掉之后不再占可写面；预算不返还（批准过一次就是一次）。
	if p.grants.revoke(abs) {
		removed = true
	}
	// 该根上的只读子路径随根一起消失，否则同一个路径再次被加回来时会莫名只读。
	delete(p.readOnlySubpaths, abs)
	if p.degradableRoots != nil {
		delete(p.degradableRoots, abs)
	}
	if !removed {
		return ErrWritableRootNotFound
	}
	return nil
}

// canonicalRemovableRoot 把待撤销的路径规范成与可写面同一套写法（绝对、清理、
// 尽可能解析符号链接）。它**不**要求路径存在：目录可能已经被删掉。
func canonicalRemovableRoot(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	abs = filepath.Clean(abs)
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		abs = filepath.Clean(real)
	}
	return abs
}

// withoutPath 返回去掉 path（按平台路径语义比较）之后的副本，保序。
func withoutPath(paths []string, path string) []string {
	out := make([]string, 0, len(paths))
	for _, existing := range paths {
		if PathEqual(existing, path) {
			continue
		}
		out = append(out, existing)
	}
	return out
}

// WritableRoots 返回策略的可写面模型：策略根（含运行期授权）在前，显式追加的
// 可写根在后，每个根带上它的只读子路径。
func (p *Policy) WritableRoots() []WritableRoot {
	if p == nil {
		return nil
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.writableRootsLocked()
}

// writableRootsLocked 是 WritableRoots 的实现，调用方必须持有 p.mu 的读锁（或写锁）。
func (p *Policy) writableRootsLocked() []WritableRoot {
	out := make([]WritableRoot, 0, len(p.roots)+len(p.extraWritableRoots)+len(p.grants.paths()))
	appendRoot := func(root string) {
		if root == "" {
			return
		}
		for _, existing := range out {
			if PathEqual(existing.Root, root) {
				return
			}
		}
		_, degradable := p.degradableRoots[root]
		out = append(out, WritableRoot{
			Root:                   root,
			ReadOnlySubpaths:       slices.Clone(p.readOnlySubpaths[root]),
			DegradeOnUnionBackends: degradable,
		})
	}
	for _, root := range p.roots {
		appendRoot(root)
	}
	for _, root := range p.writableRoots {
		appendRoot(root)
	}
	for _, root := range p.extraWritableRoots {
		appendRoot(root)
	}
	for _, root := range p.grants.paths() {
		appendRoot(root)
	}
	return out
}

// readOnlySubpathsFlat 返回所有可写根内部的只读子路径（扁平、去重）。
//
// 这是"根内只读"的全部来源：WritableRoot.ReadOnlySubpaths（例如 .git/config、
// .git/hooks）。
func (p *Policy) readOnlySubpathsFlat() []string {
	if p == nil {
		return nil
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.readOnlySubpathsFlatLocked()
}

// readOnlySubpathsFlatLocked 是 readOnlySubpathsFlat 的实现，调用方必须持有
// p.mu 的读锁（或写锁）。它走 writableRootsLocked 而不是再取一次锁：RWMutex
// 的读锁不可重入（有写者等待时会死锁）。
func (p *Policy) readOnlySubpathsFlatLocked() []string {
	var out []string
	for _, root := range p.writableRootsLocked() {
		out = append(out, root.ReadOnlySubpaths...)
	}
	return dedupeStrings(out)
}

// AllowsWrite 报告进程内写入 abs 是否被策略允许。它是可写面的**单一判定**：
// OS 后端渲染的规则与这里必须同源（WritableRoots + 各根的 ReadOnlySubpaths）。
//
// 语义（DSH 对齐）：
//   - danger-full-access：放行（它就是透传）；
//   - read-only：拒绝（进程内工具没有"设备节点"这种特例）；
//   - workspace-write：必须落在某个可写根内，且不能落在该根声明的只读子路径之下。
//
// 判定只看可写根与各根声明的只读子路径。
func (p *Policy) AllowsWrite(abs string) bool {
	if p == nil {
		return false
	}
	if p.fullAccess() {
		return true
	}
	if p.readOnly || p.mode == ModeReadOnly {
		return false
	}
	abs = strings.TrimSpace(abs)
	if abs == "" || strings.ContainsRune(abs, 0) || !filepath.IsAbs(abs) {
		return false
	}
	abs = filepath.Clean(abs)
	// 一次读锁内取"只读子路径 + 可写根"的**同一份快照**：分两次取锁会看到
	// 声明前后不一致的组合（例如子路径已登记、根还没追加）。
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, deny := range p.readOnlySubpathsFlatLocked() {
		if PathWithin(deny, abs) {
			return false
		}
	}
	for _, root := range p.writableRootsLocked() {
		if PathWithin(root.Root, abs) {
			return true
		}
	}
	return false
}

// canonicalWritableRoot 把一个可写根规范化；不在敏感位置且可解析时 ok=true。
func (p *Policy) canonicalWritableRoot(root string) (string, bool) {
	if !filepath.IsAbs(root) {
		abs, err := filepath.Abs(root)
		if err != nil {
			return "", false
		}
		root = abs
	}
	root = filepath.Clean(root)
	if real, err := filepath.EvalSymlinks(root); err == nil {
		root = filepath.Clean(real)
	}
	return root, true
}

// canonicalSubpath 把 root 内的子路径（相对或绝对）规范成绝对路径；越界的相对
// 路径（含 ".." 逃逸）返回空串。
func canonicalSubpath(root, sub string) string {
	sub = strings.TrimSpace(sub)
	if sub == "" {
		return ""
	}
	// 先把 **root** 规范化，再拼接：root 本身可能是符号链接写法（macOS 的
	// `/var` → `/private/var`），用未规范化的 root 拼出来的子路径会带着错误的
	// 前缀，后续"解析已存在的最深祖先"只能从子路径往上找，一旦中途遇到不存在的
	// 分量就会停在符号链接写法上——同一个 root 下的两个子项于是出现两种写法。
	canonicalRoot := root
	if resolvedRoot, err := resolveExistingPrefix(root); err == nil && resolvedRoot != "" {
		canonicalRoot = filepath.Clean(resolvedRoot)
	}
	if !filepath.IsAbs(sub) {
		sub = filepath.Join(canonicalRoot, sub)
	}
	sub = filepath.Clean(sub)
	if !PathWithin(canonicalRoot, sub) && !PathEqual(canonicalRoot, sub) {
		return ""
	}
	// 只读子路径允许尚不存在（`git init` 之后才出现），所以**不能**直接
	// EvalSymlinks（路径不存在时会失败）。但也不能原样返回：macOS 上
	// `/var/folders/...` 与 `/private/var/folders/...` 是同一个目录的两种写法，
	// 同一个切片里混着两种写法会让"字符串比较"的下游（诊断、测试、日志）判不出
	// 是同一个路径。折中做法：解析**已存在的最深祖先**，再把剩下的分量接回去。
	// 这样存在的部分统一到真实路径，不存在的尾部保持原样。
	if resolved, err := resolveExistingPrefix(sub); err == nil && resolved != "" {
		return filepath.Clean(resolved)
	}
	return sub
}
