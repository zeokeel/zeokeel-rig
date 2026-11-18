package sandbox

import (
	"path/filepath"
	"slices"
	"strings"
)

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
	if !ok {
		p.droppedWritableRoots = append(p.droppedWritableRoots, root)
		return
	}
	if !slices.Contains(p.extraWritableRoots, resolved) {
		p.extraWritableRoots = append(p.extraWritableRoots, resolved)
	}
	p.addReadOnlySubpaths(resolved, readOnlySubpaths...)
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
	if !slices.Contains(p.extraWritableRoots, resolved) {
		p.extraWritableRoots = append(p.extraWritableRoots, resolved)
	}
	if p.degradableRoots == nil {
		p.degradableRoots = make(map[string]struct{})
	}
	p.degradableRoots[resolved] = struct{}{}
	p.addReadOnlySubpaths(resolved, readOnlySubpaths...)
}

// addReadOnlySubpaths 是内部实现：把子路径规范化并挂在 root 下。
func (p *Policy) addReadOnlySubpaths(root string, subpaths ...string) {
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

// WritableRoots 返回策略的可写面模型：策略根（含运行期授权）在前，显式追加的
// 可写根在后，每个根带上它的只读子路径。
func (p *Policy) WritableRoots() []WritableRoot {
	if p == nil {
		return nil
	}
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

// WriteDenyPaths 返回"即使在可写根内也必须只读"的**扁平**路径列表：全局保护路径
// 加上所有根的只读子路径。四个 OS 后端都消费这一份列表（bwrap 在 --bind 之后用
// --ro-bind 覆盖、Seatbelt 在 allow 之后 deny、Landlock 展开、Windows ACL 加
// deny ACE），因此不存在"某个后端忘了某类保护"的可能。
func (p *Policy) WriteDenyPaths() []string {
	if p == nil {
		return nil
	}
	out := append([]string(nil), p.ProtectedPaths()...)
	for _, root := range p.WritableRoots() {
		out = append(out, root.ReadOnlySubpaths...)
	}
	return dedupeStrings(out)
}

// AllowsWrite 报告进程内写入 abs 是否被策略允许。它是可写面的**单一判定**：
// OS 后端渲染的规则与这里必须同源（WritableRoots + WriteDenyPaths）。
//
// 语义：
//   - danger-full-access：放行（它就是透传）；
//   - read-only：拒绝（进程内工具没有"设备节点"这种特例）；
//   - workspace-write：必须落在某个可写根内，且不能落在任何只读子路径或全局
//     保护路径之下。
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
	for _, deny := range p.WriteDenyPaths() {
		if PathWithin(deny, abs) {
			return false
		}
	}
	for _, root := range p.WritableRoots() {
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
	if !filepath.IsAbs(sub) {
		sub = filepath.Join(root, sub)
	}
	sub = filepath.Clean(sub)
	if !PathWithin(root, sub) && !PathEqual(root, sub) {
		return ""
	}
	// 只读子路径允许尚不存在（`git init` 之后才出现），因此不做 EvalSymlinks：
	// 解析会因路径不存在而失败，也会把"将来的位置"变成"当前的真实位置"。
	return sub
}
