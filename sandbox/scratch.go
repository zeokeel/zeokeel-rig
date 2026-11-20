package sandbox

// 私有 scratch 目录（A4）：read-only 档唯一可写的宿主目录。
//
// read-only 档的可写面以前只有 /dev/null，于是任何需要临时文件的命令
// （`go build`、`mktemp`、`git` 的锁文件、编辑器的 swap）都会失败，模型唯一能
// 用的临时目录是与其他进程共享的 /tmp。WithScratchDir 给这一档一个**私有**可写
// 目录，并把 TMPDIR/TMP/TEMP 指过去，使"只读"不再等于"连临时文件都不能写"。

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ScratchDirMode 是 scratch 目录的创建权限（0700：只有宿主用户自己可进）。
const ScratchDirMode os.FileMode = 0o700

// scratchEnvKeys 是被 scratch 覆盖的三个临时目录环境变量。
var scratchEnvKeys = []string{"TMPDIR", "TMP", "TEMP"}

// WithScratchDir 设置一个**私有可写**的临时目录（scratch）。
//
// 语义：
//   - 目录不存在就在 New 里 MkdirAll(0700) 建出来；建不出来不静默——错误保留在
//     Policy 上，经 ScratchDirError 读取（sandbox_status 也会报）；
//   - 相对路径按 os.Getwd() 解析成绝对路径（与 WithRoots 一致的口径），
//     ScratchDir 返回的永远是绝对路径；
//   - 设置之后：Policy.CommandEnv 把 TMPDIR/TMP/TEMP 三个键覆盖成它，
//     WritableRootsFor 把它并入可写面（read-only 档是 /dev/null + scratch），
//     OS runner 也会把它渲染成可写根。
//
// 它只加一个可写目录，不改变"读范围、网络、命令名单"等任何其它语义。
func WithScratchDir(dir string) Option {
	return func(policy *Policy) { policy.scratch = dir }
}

// ScratchDir 返回私有 scratch 目录的绝对路径；未设置时返回空串。
func (p *Policy) ScratchDir() string {
	if p == nil {
		return ""
	}
	return p.scratch
}

// ScratchDirError 返回构造期准备 scratch 目录时的错误（MkdirAll 失败、路径无法
// 绝对化），nil 表示目录可用。
//
// 它存在的理由：scratch 建不出来时命令会在运行期以 ENOENT/EACCES 的形式零散失败，
// 宿主需要一条能直接报出来的错误，而不是让模型自己猜。
func (p *Policy) ScratchDirError() error {
	if p == nil {
		return nil
	}
	return p.scratchErr
}

// prepareScratch 在 New 末尾把 scratch 落成"绝对路径 + 已存在目录 + 0700"。
//
// 它只在构造期跑一次（Policy 之后并发只读），因此不需要加锁。
func (p *Policy) prepareScratch() {
	dir := strings.TrimSpace(p.scratch)
	if dir == "" {
		p.scratch = ""
		return
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		// 绝对化失败时**不**保留相对路径：ScratchDir 的契约是"返回绝对路径"，返回
		// 一个相对路径会让下游（OS runner、TMPDIR、sandbox_status）各自再猜一遍。
		p.scratch = ""
		p.scratchErr = fmt.Errorf("sandbox: cannot resolve scratch dir %q to an absolute path: %w", dir, err)
		return
	}
	abs = filepath.Clean(abs)
	p.scratch = abs
	if err := os.MkdirAll(abs, ScratchDirMode); err != nil {
		// 建不出来就**不能**再宣称它可写：ScratchDir()/WritableRootsFor/Preflight/
		// sandbox_status 必须一致地说"没有可写临时目录"，错误留在 ScratchDirError。
		p.scratch = ""
		p.scratchErr = fmt.Errorf("sandbox: cannot create scratch dir %q: %w", abs, err)
		return
	}
	// "私有"是 scratch 的全部意义：已存在的目录可能带着 0755，MkdirAll 不会收紧它。
	// 收紧失败同样要报出来（说明它不是我们的目录，或者权限不够）。
	if err := os.Chmod(abs, ScratchDirMode); err != nil {
		p.scratch = ""
		p.scratchErr = fmt.Errorf("sandbox: cannot tighten scratch dir %q to %o: %w", abs, ScratchDirMode, err)
	}
}

// WritableRootsFor 返回该权限档位下**真正可写**的绝对路径列表，供 Preflight、
// UI 与 sandbox_status 如实展示。
//
// 口径与 sandbox/osrunner 的 profile 渲染一致：
//   - read-only：/dev/null（POSIX 设备节点）[+ scratch]；不含工作区、不含 /tmp；
//   - workspace-write：/dev/null + 平台临时目录（受 IncludeSlashTmp /
//     IncludeTmpdirEnv 控制）+ 工作区根 + 追加可写根与运行期授权 [+ scratch]；
//   - danger-full-access：nil（不包装，整个文件系统可写，没有"列表"可列）。
//
// mode 由调用方给出，UI 因此可以并排展示"如果切到某一档会怎样"。空串表示
// "用策略自己的模式"——把空串当成 read-only 会让没传模式的调用方（UI 初期、
// 诊断脚本）把 workspace-write 会话报成"只有 /dev/null 可写"（A4）。
func (p *Policy) WritableRootsFor(mode Mode) []string {
	if p == nil {
		return nil
	}
	if mode == "" {
		mode = p.Mode()
	}
	switch mode {
	case ModeDangerFullAccess:
		return nil
	case ModeReadOnly:
		out := []string{"/dev/null"}
		if scratch := p.ScratchDir(); scratch != "" {
			out = append(out, scratch)
		}
		return dedupePaths(out)
	default:
		out := []string{"/dev/null"}
		out = append(out, p.writableTempRoots()...)
		out = append(out, p.Roots()...)
		out = append(out, p.AdditionalWritableRoots()...)
		if scratch := p.ScratchDir(); scratch != "" {
			out = append(out, scratch)
		}
		return dedupePaths(out)
	}
}

// writableTempRoots 返回本策略放行的平台临时目录根（受两个开关控制）。
//
// 它是策略层对"临时目录"这一项的单一判定，osrunner 与 sandbox_status 都按它
// 渲染，避免三处各自推导。字面 /tmp 只在 POSIX 上存在，Windows 只有 os.TempDir()。
func (p *Policy) writableTempRoots() []string {
	if p == nil {
		return nil
	}
	var out []string
	if p.IncludeSlashTmp() && filepath.Separator == '/' {
		out = append(out, "/tmp")
	}
	if p.IncludeTmpdirEnv() {
		if dir := strings.TrimSpace(os.TempDir()); dir != "" {
			out = append(out, dir)
		}
	}
	return dedupePaths(out)
}

// dedupePaths 按平台路径语义去重并保序（PathEqual 归一大小写与尾分隔符）。
func dedupePaths(paths []string) []string {
	out := make([]string, 0, len(paths))
	for _, path := range paths {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		duplicate := false
		for _, existing := range out {
			if PathEqual(existing, path) {
				duplicate = true
				break
			}
		}
		if !duplicate {
			out = append(out, path)
		}
	}
	return out
}

// withScratchEnv 把 base 里的 TMPDIR/TMP/TEMP 覆盖成 scratch，并保证**只有**这三个
// 键各出现一次（base 里它们的旧值被丢弃，缺失的补在末尾）。
//
// 其它键一个都不动，包括重复项：`PATH=/a, PATH=/b` 是调用方有意给出的顺序，去重会
// 让子进程丢掉后半段搜索路径（A4 审计）。scratch 为空时**原样返回 base**（不做任何
// 复制或重排）：未设置 scratch 的策略的命令环境必须与以前逐字节一致。
func withScratchEnv(base []string, scratch string) []string {
	if scratch == "" {
		return base
	}
	overridden := make(map[string]struct{}, len(scratchEnvKeys))
	for _, key := range scratchEnvKeys {
		overridden[envKey(key)] = struct{}{}
	}
	out := make([]string, 0, len(base)+len(scratchEnvKeys))
	for _, key := range scratchEnvKeys {
		out = append(out, key+"="+scratch)
	}
	for _, entry := range base {
		key, _, ok := strings.Cut(entry, "=")
		if key == "" {
			out = append(out, entry)
			continue
		}
		if !ok {
			// 裸键（`TMPDIR` 没有 `=`）：若是三个临时键就丢掉，否则保留。
			if _, skip := overridden[envKey(key)]; skip {
				continue
			}
			out = append(out, entry)
			continue
		}
		if _, skip := overridden[envKey(key)]; skip {
			continue
		}
		out = append(out, entry)
	}
	return out
}
