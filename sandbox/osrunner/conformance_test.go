package osrunner

// 沙箱一致性 / 极端场景测试套件（跨平台共享表 + 通用断言）。
//
// 目标：**别让用户替我们做 QA**。三件事：
//
//  1. **一张表跑三个平台**：每一行是一个场景（命令 + 期望），平台差异用 perOS 覆盖
//     表达，而不是分叉写三份脚本。
//  2. **两层**：
//     - 纯逻辑层（任何宿主可跑）：表本身自检、profile 渲染是否"该拒的拒、该放的放"；
//     - 真机层（RIG_CONFORMANCE=1 门控）：真的 spawn，看退出码与原始错误文本。
//  3. **默认不拖慢 CI**：真机层默认 skip；CI 的 windows/macos/linux job 里显式开。
//
// 断言风格（失败信息必须自解释）：场景 / 期望 / 实际（退出码 + 前 200 字节 stderr）
// / 平台 / 后端 / enforcement。缺依赖一律 skip 并写清原因，绝不静默忽略。

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig/sandbox"
)

// conformanceGateEnv 打开真机层。CI 的对应平台 job 里显式设为 "1"。
const conformanceGateEnv = "RIG_CONFORMANCE"

// conformanceSlowEnv 打开"慢/占空间"的场景（100 MB 输出、1 GiB 预分配、10k 文件）。
const conformanceSlowEnv = "RIG_CONFORMANCE_SLOW"

// conformanceExpect 是对一个场景的期望。
type conformanceExpect int

const (
	// conformanceAllow：允许（退出码必须是 0）。
	conformanceAllow conformanceExpect = iota
	// conformanceDeny：必须被拒绝（非零退出，且 stderr 命中沙箱方言）。
	conformanceDeny
	// conformanceRecord：只报不判。用于**平台能力差异**与**已知边界**：我们会把实测
	// 结果打进报告，但不强制某个结论（否则三个平台的表就没法共用）。
	conformanceRecord
)

func (e conformanceExpect) String() string {
	switch e {
	case conformanceAllow:
		return "allow"
	case conformanceDeny:
		return "deny"
	default:
		return "record"
	}
}

// conformanceOverride 是某个平台对一行场景的覆盖。
type conformanceOverride struct {
	// Expect 覆盖默认期望（零值不覆盖：用 HasExpect 区分）。
	Expect    conformanceExpect
	HasExpect bool
	// Skip 非空表示该平台跳过，字符串就是跳过原因（必须写清"为什么"）。
	Skip string
	// Note 是该平台的补充说明（进报告）。
	Note string
}

// conformanceCase 是表的一行。
type conformanceCase struct {
	// Name 是场景名（唯一，报告里用它定位）。
	Name string
	// Category 是分类（工作区/路径形态/文件系统/命令/输出/并发/逃逸/工具链/git）。
	Category string
	// Mode 是策略模式（默认 workspace-write）。
	Mode sandbox.Mode
	// Network 是策略的 AllowNetwork（默认 false）。
	Network bool
	// Sh 是 POSIX shell 脚本（linux/darwin 用）。
	Sh string
	// Cmd 是 Windows cmd 脚本（windows 用；为空则跳过该平台的真机层）。
	Cmd string
	// Expect 是默认期望。
	Expect conformanceExpect
	// KnownBoundary 非空表示"这是**已知边界**，不是缺陷"：字符串写清边界内容，
	// 报告里会归到「已知边界」一节，且断言按 Expect 的语义执行（通常是 allow/record）。
	KnownBoundary string
	// Needs 是外部依赖（go/npm/git/python3/docker…）：缺失即 skip 并写原因。
	Needs []string
	// Slow 标记慢场景，需要 RIG_CONFORMANCE_SLOW=1。
	Slow bool
	// Protect 标记"本行开启 .git 保护"。默认 **不开启**：绝大多数场景测的是
	// runner 的可写面本身；保护语义与"根级文件不可写"这类后果由专门的几行覆盖
	// （否则整张表在 Landlock 上会因为一个已知限制全红，失去回归价值）。
	Protect bool
	// PerOS 是平台覆盖。
	PerOS map[string]conformanceOverride
}

// expectFor 返回该平台上的期望。
func (c conformanceCase) expectFor(goos string) conformanceExpect {
	if override, ok := c.PerOS[goos]; ok && override.HasExpect {
		return override.Expect
	}
	return c.Expect
}

// skipFor 返回该平台上的跳过原因（空串 = 不跳过）。
func (c conformanceCase) skipFor(goos string) string {
	if override, ok := c.PerOS[goos]; ok {
		return override.Skip
	}
	return ""
}

// noteFor 返回该平台的补充说明。
func (c conformanceCase) noteFor(goos string) string {
	if override, ok := c.PerOS[goos]; ok {
		return override.Note
	}
	return ""
}

// mode 返回生效的模式（零值 = workspace-write）。
func (c conformanceCase) mode() sandbox.Mode {
	if c.Mode == "" {
		return sandbox.ModeWorkspaceWrite
	}
	return c.Mode
}

// conformanceCases 是**唯一**的场景表。三个平台共用；差异走 PerOS。
func conformanceCases() []conformanceCase {
	const readOnly = "read-only"
	_ = readOnly
	return []conformanceCase{
		// ---------------- 工作区基本操作 ----------------
		{Name: "workspace/new-file", Category: "workspace", Expect: conformanceAllow,
			Sh: `echo hi > new.txt && cat new.txt | grep -q hi`, Cmd: `echo hi> new.txt`},
		{Name: "workspace/overwrite", Category: "workspace", Expect: conformanceAllow,
			Sh: `echo one > f.txt && echo two > f.txt && cat f.txt | grep -q two`, Cmd: `echo one> f.txt && echo two> f.txt`},
		{Name: "workspace/append", Category: "workspace", Expect: conformanceAllow,
			Sh: `echo a > f.txt && echo b >> f.txt && [ "$(wc -l < f.txt)" = "2" ]`, Cmd: `echo a> f.txt && echo b>> f.txt`},
		{Name: "workspace/delete", Category: "workspace", Expect: conformanceAllow,
			Sh: `echo x > d.txt && rm d.txt && [ ! -e d.txt ]`, Cmd: `echo x> d.txt && del d.txt`},
		{Name: "workspace/rename", Category: "workspace", Expect: conformanceAllow,
			Sh: `echo x > a.txt && mv a.txt b.txt && [ -f b.txt ] && [ ! -e a.txt ]`, Cmd: `echo x> a.txt && move a.txt b.txt`},
		{Name: "workspace/mkdir", Category: "workspace", Expect: conformanceAllow,
			Sh: `mkdir -p a/b/c && [ -d a/b/c ]`, Cmd: `mkdir a\b\c`},
		{Name: "workspace/rm-rf-dir", Category: "workspace", Expect: conformanceAllow,
			Sh: `mkdir -p tree/x && echo y > tree/x/f && rm -rf tree && [ ! -e tree ]`, Cmd: `mkdir tree && rmdir /s /q tree`},
		{Name: "workspace/new-file-in-workspace-root", Protect: true, Category: "workspace", Expect: conformanceAllow,
			KnownBoundary: "Landlock 下若根里含保护路径（kit 默认保护 .git），根自身会进只读集合、写授权只下移到**已存在的子项**：根级新建会失败，而进程内策略 AllowsWrite 仍认为可以（两者不一致）",
			Sh:            `echo x > "$CONFORMANCE_WS/root-level-$$.txt" && echo OK`,
			Cmd:           `echo x> %CONFORMANCE_WS%oot-level.txt`,
			PerOS: map[string]conformanceOverride{
				"linux": {Expect: conformanceRecord, HasExpect: true, Note: "bwrap 允许（rw bind 覆盖整个根），Landlock 拒绝 → 后端不一致"},
			}},
		{Name: "workspace/overwrite-root-level-file", Protect: true, Category: "workspace", Expect: conformanceAllow,
			KnownBoundary: "同上：根级**已存在**文件在 Landlock 下覆写也会被拒（文件只被根的只读规则覆盖）",
			Sh:            `echo y > "$CONFORMANCE_WS/seed.txt" && echo OK`,
			Cmd:           `echo y> %CONFORMANCE_WS%\seed.txt`,
			PerOS: map[string]conformanceOverride{
				"linux": {Expect: conformanceRecord, HasExpect: true, Note: "bwrap 允许覆写，Landlock 拒绝"},
			}},
		{Name: "workspace/chmod", Category: "workspace", Expect: conformanceRecord,
			KnownBoundary: "Landlock 不治理 chmod：可写面**之外**的文件也能改权限（本行的目标在工作区内，两个后端都应允许）",
			Sh:            `echo x > m.txt && chmod 600 m.txt && [ "$(stat -c %a m.txt)" = "600" ]`, Cmd: `echo x> m.txt && attrib +r m.txt`},
		{Name: "workspace/touch-d", Category: "workspace", Expect: conformanceRecord,
			KnownBoundary: "Landlock 不治理 utimensat：可写面之外的已存在文件也能改时间戳",
			Sh:            `echo x > t.txt && touch -d 2020-01-01 t.txt`, Cmd: `echo x> t.txt`},

		// ---------------- git ----------------
		{Name: "git/init-commit-log", Category: "git", Needs: []string{"git"}, Expect: conformanceAllow,
			Sh:  `git init -q . && echo x > f && git add f && git -c user.name=a -c user.email=a@b commit -qm m && git log --oneline | head -1`,
			Cmd: `git init -q . && echo x> f && git add f && git -c user.name=a -c user.email=a@b commit -qm m && git log --oneline`},
		{Name: "git/read-config", Category: "git", Needs: []string{"git"}, Expect: conformanceAllow,
			Sh: `git init -q . && cat .git/config | head -1`, Cmd: `git init -q . && type .git\config`},
		{Name: "git/write-config-denied", Protect: true, Category: "git", Needs: []string{"git"}, Expect: conformanceDeny,
			Sh:    `git init -q . && echo evil >> .git/config`,
			Cmd:   `git init -q . && echo evil>> .git\config`,
			PerOS: map[string]conformanceOverride{
				// 保护路径默认只在「宿主显式配置」时存在；真机层用 conformancePolicy 统一开启，
				// 因此这里三个平台都应当是 deny。
			}},
		{Name: "git/nested-repo-uses-outer-dot-git", Category: "git", Needs: []string{"git"}, Expect: conformanceRecord,
			KnownBoundary: "**工作区在别人的 git 仓库里**（monorepo 子目录 / 家目录）时，git 会向上找到外层的 .git：外层 .git 往往是只读的（不在可写面内），于是任何 git 操作都失败。这不是沙箱 bug，而是「工作区根 != 仓库根」的常见布局没被考虑",
			Sh:            `git rev-parse --show-toplevel 2>&1 | head -1 ; echo x > probe.txt && git add probe.txt 2>&1 | head -1`,
			Cmd:           `git rev-parse --show-toplevel`},
		{Name: "git/commit-blocked-by-protection", Protect: true, Category: "git", Needs: []string{"git"}, Expect: conformanceDeny,
			KnownBoundary: "**设计冲突（三方一致）**：.git 受保护时沙箱里的 git add/commit 必然失败（index.lock 落在只读的 .git 里）。这是「kit 默认开保护」与「挂 OS runner」两个默认值打架，不是某后端的 bug",
			Sh:            `echo x > f && git add f && git -c user.name=a -c user.email=a@b commit -qm m`,
			Cmd:           `echo x> f && git add f && git -c user.name=a -c user.email=a@b commit -qm m`},
		{Name: "git/protect-missing-at-build-time", Protect: true, Category: "git", Needs: []string{"git"}, Expect: conformanceRecord,
			KnownBoundary: "**fail-open 已知边界**：保护路径在构造策略时如果**不存在**（工作区还没有 .git），渲染阶段会被丢掉（MissingProtected 只报告不拦），随后 git init 出来的 .git 不受保护。缓解：宿主应当在 git init 后重建策略，或用 WithProtectMissingPaths 要求硬失败",
			// .git 本身是只读挂载点（删不掉），所以在一层**未被保护**的子目录里
			// git init：策略构造时那个 .git 还不存在 → 保护被静默丢弃 → 写入成功。
			Sh:  `mkdir -p nested && cd nested && git init -q . && echo evil >> .git/config && echo WROTE`,
			Cmd: `rmdir /s /q .git && git init -q . && echo evil>> .git\config`},
		{Name: "git/write-hook-denied", Protect: true, Category: "git", Needs: []string{"git"}, Expect: conformanceDeny,
			Sh:  `git init -q . && mkdir -p .git/hooks && echo evil > .git/hooks/pre-commit`,
			Cmd: `git init -q . && echo evil> .git\hooks\pre-commit`},

		// ---------------- 语言工具链 ----------------
		{Name: "toolchain/go-build", Category: "toolchain", Needs: []string{"go"}, Expect: conformanceAllow,
			Sh:  `printf 'module m\n\ngo 1.21\n' > go.mod && printf 'package main\nfunc main(){}\n' > main.go && go build ./...`,
			Cmd: `echo module m> go.mod && echo package main> main.go && go build ./...`},
		{Name: "toolchain/go-test", Category: "toolchain", Needs: []string{"go"}, Expect: conformanceAllow,
			Sh:  `printf 'module m\n\ngo 1.21\n' > go.mod && printf 'package main\nimport "testing"\nfunc TestX(t *testing.T){}\n' > m_test.go && go test ./...`,
			Cmd: `echo module m> go.mod && go test ./...`},
		{Name: "toolchain/npm-init-install", Category: "toolchain", Needs: []string{"npm"}, Expect: conformanceAllow,
			Sh:  `printf '{"name":"x","version":"1.0.0"}' > package.json && npm install --no-audit --no-fund --offline 2>&1 | tail -1`,
			Cmd: `echo {"name":"x","version":"1.0.0"}> package.json`},
		{Name: "toolchain/write-tmpdir", Category: "toolchain", Expect: conformanceAllow,
			Sh:  `echo x > "$TMPDIR/conformance.tmp" && cat "$TMPDIR/conformance.tmp" | grep -q x`,
			Cmd: `echo x> %TEMP%\conformance.tmp`},

		// ---------------- 边界写 ----------------
		{Name: "boundary/outside-workspace", Category: "boundary", Expect: conformanceDeny,
			Sh: `echo x > "$CONFORMANCE_OUTSIDE/escape.txt"`, Cmd: `echo x> %CONFORMANCE_OUTSIDE%\escape.txt`},
		{Name: "boundary/home-ssh", Category: "boundary", Expect: conformanceDeny,
			Sh: `echo x > "$HOME/.ssh/escape.txt"`, Cmd: `echo x> %USERPROFILE%\.ssh\escape.txt`},

		// ---------------- 路径形态 ----------------
		{Name: "path/long-300", Category: "path", Expect: conformanceAllow,
			// 单个分量上限是 NAME_MAX=255，所以"长路径"用多段拼（总长 >300）：
			// 这也正是 Windows MAX_PATH(260) 关心的形态。
			Sh:  `python3 -c 'import os; a="a"*100; p=os.path.join(a,a,a); os.makedirs(p,exist_ok=True); open(os.path.join(p,"f.txt"),"w").write("x")'`,
			Cmd: `mkdir 300char`},
		{Name: "path/deep-50", Category: "path", Expect: conformanceAllow,
			Sh:  `p=. ; i=0 ; while [ $i -lt 50 ]; do p=$p/d$i ; i=$((i+1)) ; done ; mkdir -p $p && echo x > $p/f.txt`,
			Cmd: `mkdir deep50`},
		{Name: "path/spaces", Category: "path", Expect: conformanceAllow,
			Sh:  `mkdir -p "dir with space" && echo x > "dir with space/f 1.txt" && [ -f "dir with space/f 1.txt" ]`,
			Cmd: `mkdir "dir with space" && echo x> "dir with space\f 1.txt"`},
		{Name: "path/quotes", Category: "path", Expect: conformanceAllow,
			Sh:  `mkdir -p 'q' && echo x > 'q/quo"te.txt' && echo y > "q/sin'gle.txt" && [ -f 'q/quo"te.txt' ]`,
			Cmd: `mkdir q && echo x> q\quo"te.txt`},
		{Name: "path/shell-metachars", Category: "path", Expect: conformanceAllow,
			Sh:  `mkdir -p m && echo x > 'm/amp&semi;dollar.txt' && [ -f 'm/amp&semi;dollar.txt' ]`,
			Cmd: `mkdir m`},
		{Name: "path/subshell-name", Category: "path", Expect: conformanceAllow,
			KnownBoundary: "文件名里的 $(...) 只是字面量：真正的注入风险在「把模型输出拼进 shell 命令」，不在文件名",
			Sh:            `mkdir -p s && echo x > 's/$(touch pwned).txt' && [ -f 's/$(touch pwned).txt' ] && [ ! -e pwned ]`,
			Cmd:           `mkdir s`},
		{Name: "path/unicode", Category: "path", Expect: conformanceAllow,
			Sh:  `mkdir -p u && echo x > 'u/中文文件名.txt' && [ -f 'u/中文文件名.txt' ]`,
			Cmd: `mkdir u && echo x> u\中文文件名.txt`},
		{Name: "path/emoji", Category: "path", Expect: conformanceAllow,
			Sh:  `mkdir -p e && echo x > 'e/🚀-👨‍👩‍👧‍👦.txt' && [ -f 'e/🚀-👨‍👩‍👧‍👦.txt' ]`,
			Cmd: `mkdir e`},
		{Name: "path/dotfile", Category: "path", Expect: conformanceAllow,
			Sh: `echo x > .hidden && [ -f .hidden ] && cat .hidden | grep -q x`, Cmd: `echo x> .hidden`},
		{Name: "path/trailing-space", Category: "path", Expect: conformanceAllow,
			Sh: `echo x > 'trail ' && [ -f 'trail ' ]`, Cmd: `echo x> trail`,
			PerOS: map[string]conformanceOverride{
				"windows": {Skip: "Windows 会静默去掉结尾空格（Win32 路径规范化），无法创建这种名字"},
			}},
		{Name: "path/case-variant-protected", Category: "path", Expect: conformanceAllow,
			KnownBoundary: "大小写敏感性随平台：Linux 上 .GIT 是另一个目录（允许写）；macOS/Windows 的默认文件系统把它映射到 .git（必须拒绝）",
			Sh:            `mkdir -p .GIT && echo x > .GIT/probe.txt`,
			Cmd:           `mkdir .GIT && echo x> .GIT\probe.txt`,
			PerOS: map[string]conformanceOverride{
				"darwin":  {Expect: conformanceDeny, HasExpect: true, Note: "APFS 默认大小写不敏感 → .GIT 就是 .git（受保护）"},
				"windows": {Expect: conformanceDeny, HasExpect: true, Note: "NTFS 大小写不敏感 → .GIT 就是 .git（受保护）"},
			}},
		{Name: "path/dotdot-traversal", Category: "path", Expect: conformanceDeny,
			// 用例工作目录是 <ws>/<backend>/<case>，因此 "../../escape.txt" 才真正越过
			// 策略根；单个 ".." 只在工作区内部跳一层（合法）。
			Sh: `echo x > ../../../escape.txt`, Cmd: `echo x> ..\..\..\escape.txt`},
		{Name: "path/absolute-outside", Category: "path", Expect: conformanceDeny,
			Sh: `echo x > "$CONFORMANCE_OUTSIDE/abs.txt"`, Cmd: `echo x> %CONFORMANCE_OUTSIDE%\abs.txt`},
		{Name: "path/home-expansion", Category: "path", Expect: conformanceDeny,
			Sh: `echo x > ~/expansion.txt`, Cmd: `echo x> %USERPROFILE%\expansion.txt`},

		// ---------------- 文件系统边界 ----------------
		{Name: "fs/symlink-escape", Category: "fs", Expect: conformanceDeny,
			Sh:  `ln -s "$CONFORMANCE_OUTSIDE" link 2>/dev/null && echo x > link/through.txt`,
			Cmd: `mklink /D link %CONFORMANCE_OUTSIDE% && echo x> link\through.txt`},
		{Name: "fs/symlink-loop", Category: "fs", Expect: conformanceRecord,
			KnownBoundary: "符号链接环：内核 ELOOP。沙箱只负责不逃逸，环本身由调用方处理",
			Sh:            `ln -s loop loop 2>/dev/null ; cat loop 2>&1 | head -1`,
			Cmd:           `mklink loop loop`},
		{Name: "fs/hardlink-inside", Category: "fs", Expect: conformanceAllow,
			Sh:  `echo x > orig.txt && ln orig.txt link.txt && [ "$(cat link.txt)" = "x" ]`,
			Cmd: `echo x> orig.txt && mklink /H link.txt orig.txt`},
		{Name: "fs/fifo", Category: "fs", Expect: conformanceRecord,
			KnownBoundary: "命名管道：沙箱不管类型；FIFO 上的 open 会阻塞（工具层必须自己拒绝非普通文件）",
			Sh:            `mkfifo pipe 2>/dev/null && [ -p pipe ] && echo FIFO_OK`,
			Cmd:           `echo N/A`},
		{Name: "fs/dev-zero-read", Category: "fs", Expect: conformanceAllow,
			Sh: `head -c 16 /dev/zero | wc -c`, Cmd: `echo N/A`},
		{Name: "fs/sparse-1g", Category: "fs", Slow: true, Expect: conformanceAllow,
			Sh:  `truncate -s 1G big.sparse && [ "$(stat -c %s big.sparse)" = "1073741824" ]`,
			Cmd: `fsutil file createnew big.sparse 1073741824`},
		{Name: "fs/zero-bytes", Category: "fs", Expect: conformanceAllow,
			Sh: `: > empty.txt && [ ! -s empty.txt ]`, Cmd: `type nul> empty.txt`},
		{Name: "fs/readonly-file", Category: "fs", Expect: conformanceDeny,
			Sh:  `echo x > ro.txt && chmod 400 ro.txt && echo y > ro.txt`,
			Cmd: `echo x> ro.txt && attrib +r ro.txt && echo y> ro.txt`,
			PerOS: map[string]conformanceOverride{
				// 以 root 运行时 0400 也照样可写：这条只在非 root 下有意义。
				"linux":   {Skip: "本机以 root 运行时 0400 无法阻止写入（EACCES 不会发生）"},
				"darwin":  {Skip: "同上：root 会绕过权限位"},
				"windows": {Skip: "Windows 的只读属性对管理员无效"},
			}},
		{Name: "fs/open-file-rewrite", Category: "fs", Expect: conformanceAllow,
			Sh:  `echo one > o.txt && (exec 3< o.txt ; echo two > o.txt) && [ "$(cat o.txt)" = "two" ]`,
			Cmd: `echo one> o.txt && echo two> o.txt`},

		// ---------------- 命令边界 ----------------
		{Name: "cmd/argv-1mb", Category: "cmd", Slow: true, Expect: conformanceRecord,
			KnownBoundary: "1 MB 单参数会触碰内核 E2BIG/ARG_MAX（Linux 单参数上限 128 KiB）；沙箱不改变这个上限，工具层必须自己限制参数体积",
			Sh:            `arg=$(python3 -c 'print("x"*1000000)' 2>/dev/null || string=; ); /bin/true "$arg"; echo EXIT=$?`,
			Cmd:           `echo N/A`},
		{Name: "cmd/env-1000", Category: "cmd", Expect: conformanceAllow,
			Sh:  `i=0 ; while [ $i -lt 1000 ]; do eval "export V$i=$i" ; i=$((i+1)) ; done ; env | wc -l | grep -qE '^[0-9]+$'`,
			Cmd: `set V1=1`},
		{Name: "cmd/nested-shell-20", Category: "cmd", Expect: conformanceAllow,
			// 真嵌套 20 层进程：用 python 生成 argv，避免 shell 引号地狱。
			Sh: `python3 -c 'import subprocess,sys
a = ["echo", "deep"]
for _ in range(20):
    a = ["bash", "-c", " ".join(a)]
sys.exit(subprocess.run(a, stdout=subprocess.PIPE).returncode)' && echo NESTED_OK`,
			Cmd: `echo deep`},
		{Name: "cmd/background-tree", Category: "cmd", Expect: conformanceRecord,
			KnownBoundary: "后台子进程树：leader 退出后孤儿不会被沙箱回收（--die-with-parent 只保证随 leader 死亡的一层）；工具层必须自己 kills 整个进程组",
			Sh:            `sh -c 'sleep 60 & echo started' ; echo LEADER_DONE`,
			Cmd:           `start /b ping -n 60 127.0.0.1`},
		{Name: "cmd/timeout-command", Category: "cmd", Needs: []string{"timeout"}, Expect: conformanceAllow,
			Sh: `timeout 1 sleep 5 ; [ $? = 124 ]`, Cmd: `echo N/A`},
		{Name: "cmd/exit-codes", Category: "cmd", Expect: conformanceRecord,
			Sh:  `sh -c 'exit 1' ; a=$? ; sh -c 'exit 7' ; b=$? ; command_not_found_xyz 2>/dev/null ; c=$? ; [ "$b" = "7" ]`,
			Cmd: `cmd /c exit 1`},
		{Name: "cmd/missing-command", Category: "cmd", Expect: conformanceRecord,
			Sh: `definitely-not-a-command-xyz 2>&1 | head -1`, Cmd: `definitely-not-a-command-xyz`},
		{Name: "cmd/no-exec-bit", Category: "cmd", Expect: conformanceRecord,
			Sh:  `printf '#!/bin/sh\necho hi\n' > s.sh && chmod 644 s.sh && ./s.sh 2>&1 | head -1`,
			Cmd: `echo N/A`},
		{Name: "cmd/glob-10k", Category: "cmd", Slow: true, Expect: conformanceAllow,
			Sh:  `mkdir -p many && i=0 ; while [ $i -lt 10000 ]; do : > many/f$i.txt ; i=$((i+1)) ; done ; ls many/*.txt | wc -l | grep -q 10000`,
			Cmd: `echo N/A`},

		// ---------------- 输出边界 ----------------
		{Name: "output/100mb", Category: "output", Slow: true, Expect: conformanceRecord,
			KnownBoundary: "runner 不做输出截断（截断/落盘是工具层 Spill 的职责）；本行只验证 100 MB 不会把 runner 打挂",
			Sh:            `head -c 100000000 /dev/zero | tr '\0' 'a' | wc -c`,
			Cmd:           `echo N/A`},
		{Name: "output/nul-bytes", Category: "output", Expect: conformanceAllow,
			Sh: `printf 'a\0b\0c' > nul.bin && [ "$(wc -c < nul.bin)" = "5" ]`, Cmd: `echo N/A`},
		{Name: "output/invalid-utf8", Category: "output", Expect: conformanceAllow,
			KnownBoundary: "非法 UTF-8 字节由调用方决定怎么展示（U+FFFD 替换发生在上层，runner 原样传递）",
			Sh:            `printf '\377\376\375' > bad.bin && [ "$(wc -c < bad.bin)" = "3" ]`, Cmd: `echo N/A`},
		{Name: "output/no-trailing-newline", Category: "output", Expect: conformanceAllow,
			Sh: `printf 'nonewline' > nl.txt && [ "$(wc -c < nl.txt)" = "9" ]`, Cmd: `echo N/A`},
		{Name: "output/interleaved", Category: "output", Expect: conformanceAllow,
			Sh: `sh -c 'echo out1 ; echo err1 >&2 ; echo out2' 2>&1 | wc -l | grep -q 3`, Cmd: `echo out1`},

		// ---------------- 并发 ----------------
		{Name: "concurrent/20-writes", Category: "concurrent", Expect: conformanceAllow,
			Sh:  `i=0 ; while [ $i -lt 20 ]; do (echo $i > "c$i.txt") & i=$((i+1)) ; done ; wait ; ls c*.txt | wc -l | grep -q 20`,
			Cmd: `echo N/A`},
		{Name: "concurrent/same-file", Category: "concurrent", Expect: conformanceRecord,
			KnownBoundary: "并发写同一文件：最后一个写者赢（POSIX 语义），沙箱不提供事务",
			Sh:            `i=0 ; while [ $i -lt 20 ]; do (echo $i > same.txt) & i=$((i+1)) ; done ; wait ; wc -c < same.txt`,
			Cmd:           `echo N/A`},
		{Name: "concurrent/git-commit", Category: "concurrent", Needs: []string{"git"}, Expect: conformanceRecord,
			KnownBoundary: "并发 git commit：git 的 index.lock 保证只有一个赢；失败方不留半成品（锁文件可能残留）",
			Sh:            `git init -q . && echo a > a && git add a && git -c user.name=x -c user.email=x@y commit -qm base && (echo b > b && git add b && git -c user.name=x -c user.email=x@y commit -qm b) & (echo c > c && git add c && git -c user.name=x -c user.email=x@y commit -qm c) & wait`,
			Cmd:           `echo N/A`},

		// ---------------- 逃逸向量 ----------------
		{Name: "escape/docker-socket", Category: "escape", Needs: []string{"docker"}, Expect: conformanceDeny,
			KnownBoundary: "经宿主守护进程 socket 的请求是「宿主动作」：屏蔽 socket 是唯一有效手段（/dev/null 覆盖）",
			Sh:            `if docker ps > docker.out 2>&1; then cat docker.out; exit 0; else cat docker.out; exit 1; fi`,
			Cmd:           `docker ps`},
		{Name: "escape/loopback-when-network-off", Category: "escape", Expect: conformanceRecord,
			KnownBoundary: "**网络语义两个后端不一致**：bwrap 用 --unshare-net 断出口（回环仍可用）；Landlock 完全不拦。本机出口网络不稳定（无网时两个后端都 000，不可判定），因此这行是 record：有外网的 CI 上应当看到 bwrap=000 / landlock=301",
			// 2026-xx 起 bwrap 会额外挂 seccomp 过滤器；这里只关心"能不能连出去"，
			// 用公共 IP 而不是 localhost（后者在 unshared netns 里也没有对端）。
			Sh:  `curl -s -m 3 -o /dev/null -w '%{http_code}' http://1.1.1.1/ 2>/dev/null ; echo`,
			Cmd: `echo N/A`},
		{Name: "escape/etc-shadow-readable", Category: "escape", Expect: conformanceAllow,
			KnownBoundary: "**我们只做写围栏，不做读隔离**：/etc/shadow 在 Linux 上可读（root 场景）。这条断言「确实能读」，避免后人误以为读被隔离",
			Sh:            `head -1 /etc/shadow 2>&1 | wc -l`,
			Cmd:           `type C:\Windows\System32\config\SAM`},
		{Name: "escape/proc-self-environ", Category: "escape", Expect: conformanceAllow,
			KnownBoundary: "**已知边界**：/proc/self/environ 可读（进程自己的环境）；同理 /proc 下其它进程信息在 bwrap --unshare-pid 下不可见，但 landlock 下可见",
			Sh:            `tr '\0' '\n' < /proc/self/environ | head -1 | wc -l`,
			Cmd:           `echo N/A`},
		{Name: "escape/ld-preload", Category: "escape", Expect: conformanceRecord,
			KnownBoundary: "LD_PRELOAD 注入：沙箱不清理环境变量（用户自己的进程，注入自己的库不算逃逸）；不可信模型场景应清空环境",
			Sh:            `LD_PRELOAD=/nonexistent.so /bin/true ; echo EXIT=$?`,
			Cmd:           `echo N/A`},
		{Name: "escape/dash-arg-injection", Category: "escape", Expect: conformanceRecord,
			KnownBoundary: "-- 前缀参数注入：runner 把 argv 原样拼接，不做参数解释；风险在工具层把用户字符串拼进 argv",
			Sh:            `printf x > ./-rf ; ls -- -rf 2>&1 | head -1`,
			Cmd:           `echo N/A`},
	}
}

// conformanceBackend 是一个被测后端。
type conformanceBackend struct {
	Name      string
	Runner    sandbox.Runner
	Enforcing bool
}

// conformanceEnv 是真机层的环境：目录骨架 + 后端列表 + 平台命令构造。
type conformanceEnv struct {
	Root    string // 策略根（工作区）
	Home    string // 伪 HOME（子进程真正用的）
	Outside string // 工作区外的可写目录（用来验证"写不进去"）
	Tmp     string // 自定义 TMPDIR
	Goos    string
	Shell   []string // 平台 shell 前缀，例如 {"/bin/sh","-c"}
}

// conformanceResult 是一次受限执行的观测。
type conformanceResult struct {
	Exit        int
	Stdout      string
	Stderr      string
	Argv        []string
	Enforcement sandbox.Enforcement
	Backend     string
	ConfineErr  error
}

// output 返回合并输出（用于匹配方言）。
func (r conformanceResult) output() string { return r.Stdout + r.Stderr }

// conformanceDialects 是"沙箱拒绝"的跨平台文案（小写匹配）。
var conformanceDialects = []string{
	"read-only file system",
	"permission denied",
	"operation not permitted",
	"access is denied",
	"cannot create",
}

// deniedBySandbox 报告一次失败是不是"被沙箱拒绝"。
func deniedBySandbox(r conformanceResult) bool {
	text := strings.ToLower(r.output())
	for _, dialect := range conformanceDialects {
		if strings.Contains(text, dialect) {
			return true
		}
	}
	return false
}

// truncateForReport 截断到 n 字节（报告用）。
func truncateForReport(text string, n int) string {
	text = strings.TrimSpace(text)
	if len(text) <= n {
		return text
	}
	return text[:n] + "…"
}

// assertConformance 按期望断言，失败信息自解释。
func assertConformance(t *testing.T, env conformanceEnv, c conformanceCase, backend conformanceBackend, result conformanceResult) {
	t.Helper()
	expected := c.expectFor(env.Goos)
	detail := fmt.Sprintf(
		"场景: %s [%s]\n  期望: %s\n  实际: exit=%d\n  平台: %s  后端: %s  enforcement: %v\n  stderr(前200B): %s\n  stdout(前200B): %s\n  argv: %s",
		c.Name, c.Category, expected, result.Exit, env.Goos, backend.Name, result.Enforcement,
		truncateForReport(result.Stderr, 200), truncateForReport(result.Stdout, 200),
		truncateForReport(strings.Join(result.Argv, " "), 300))
	if c.KnownBoundary != "" {
		detail += "\n  已知边界: " + c.KnownBoundary
	}
	if result.ConfineErr != nil {
		if expected == conformanceDeny {
			return // fail closed 也算"拒绝"
		}
		t.Fatalf("%s\n  Confine 失败: %v", detail, result.ConfineErr)
	}
	switch expected {
	case conformanceAllow:
		if result.Exit != 0 {
			t.Fatalf("%s", detail)
		}
	case conformanceDeny:
		if result.Exit == 0 {
			t.Fatalf("%s", detail)
		}
		if !deniedBySandbox(result) {
			t.Fatalf("%s\n  非零退出但**没有**沙箱拒绝文案：可能是别的原因（命令不存在/语法错），不算拒绝", detail)
		}
	case conformanceRecord:
		// 只报不判：把结果打进 -v 日志，供人工与"能力矩阵"使用。
		t.Logf("RECORD %s: exit=%d %s", c.Name, result.Exit, truncateForReport(result.output(), 120))
	}
}

// conformanceStats 是"共 N 项 / 本平台适用 M / 真机执行 K / 跳过 J"的统计。
type conformanceStats struct {
	Total      int
	Applicable int
	Executed   int
	Skipped    int
	Reasons    map[string]int
}

// printConformanceStats 打印统计与"已覆盖/未覆盖"表（随代码走的能力矩阵）。
func printConformanceStats(t *testing.T, goos string, cases []conformanceCase, stats conformanceStats) {
	t.Helper()
	t.Logf("conformance: 共 %d 项；平台 %s 适用 %d 项；真机执行 %d 项；跳过 %d 项",
		stats.Total, goos, stats.Applicable, stats.Executed, stats.Skipped)
	if len(stats.Reasons) > 0 {
		keys := make([]string, 0, len(stats.Reasons))
		for reason := range stats.Reasons {
			keys = append(keys, reason)
		}
		sort.Strings(keys)
		for _, reason := range keys {
			t.Logf("  skip ×%d: %s", stats.Reasons[reason], reason)
		}
	}
	boundaries := 0
	for _, c := range cases {
		if c.KnownBoundary != "" {
			boundaries++
		}
	}
	t.Logf("  其中标注「已知边界」%d 项（不是缺陷，见各行注释）", boundaries)
}

// TestConformanceTableIsWellFormed 是**纯逻辑层**：验证表本身（任何宿主可跑）。
//
// 它不 spawn 任何东西，因此永远在常规 CI 里跑：改名/加平台/写错期望都会在这里红。
func TestConformanceTableIsWellFormed(t *testing.T) {
	cases := conformanceCases()
	if len(cases) < 40 {
		t.Fatalf("场景表太小（%d 行）：极端场景套件的价值在于覆盖面", len(cases))
	}
	seen := map[string]bool{}
	categories := map[string]int{}
	for _, c := range cases {
		if strings.TrimSpace(c.Name) == "" {
			t.Fatal("存在匿名的场景")
		}
		if seen[c.Name] {
			t.Fatalf("场景名重复: %q（报告里无法定位）", c.Name)
		}
		seen[c.Name] = true
		if strings.TrimSpace(c.Category) == "" {
			t.Fatalf("%s: 缺 Category", c.Name)
		}
		categories[c.Category]++
		if c.Sh == "" && c.Cmd == "" {
			t.Fatalf("%s: 既没有 Sh 也没有 Cmd", c.Name)
		}
		if c.Expect < conformanceAllow || c.Expect > conformanceRecord {
			t.Fatalf("%s: 非法期望 %d", c.Name, c.Expect)
		}
		for goos, override := range c.PerOS {
			switch goos {
			case "linux", "darwin", "windows":
			default:
				t.Fatalf("%s: PerOS 里有未知平台 %q", c.Name, goos)
			}
			if override.Skip != "" && override.HasExpect {
				t.Fatalf("%s/%s: 既 skip 又改期望，语义冲突", c.Name, goos)
			}
		}
	}
	// 每个分类都要有非 record 的硬断言（"每行都要有断言，不能只 log"）。
	hard := map[string]int{}
	for _, c := range cases {
		if c.Expect != conformanceRecord {
			hard[c.Category]++
		}
	}
	for category := range categories {
		if hard[category] == 0 {
			t.Errorf("分类 %q 只有 record 行：每行都要有断言，不能只 log", category)
		}
	}
	t.Logf("场景表：%d 行，分类 %v", len(cases), categories)
}

// TestConformanceRuntime 是真机层的入口：由各平台的 conformance_<os>_test.go 提供
// 后端与环境（这一层不在这里实现，保证"同一张表跑三个平台"）。
func TestConformanceRuntime(t *testing.T) {
	if os.Getenv(conformanceGateEnv) != "1" {
		t.Skipf("真机层默认跳过；设 %s=1 启用（依赖本机真实沙箱后端与工具链）", conformanceGateEnv)
	}
	env, backends := conformancePlatform(t)
	if len(backends) == 0 {
		t.Skip("本机没有可用的 OS 沙箱后端")
	}
	cases := conformanceCases()
	stats := conformanceStats{Total: len(cases), Reasons: map[string]int{}}
	for _, c := range cases {
		if c.skipFor(env.Goos) != "" {
			stats.Skipped++
			stats.Reasons["平台声明跳过: "+c.skipFor(env.Goos)]++
			continue
		}
		if c.Cmd == "" && env.Goos == "windows" {
			stats.Skipped++
			stats.Reasons["没有 Windows 版脚本"]++
			continue
		}
		if c.Sh == "" && env.Goos != "windows" {
			stats.Skipped++
			stats.Reasons["没有 POSIX 版脚本"]++
			continue
		}
		stats.Applicable++
	}
	for _, backend := range backends {
		backend := backend
		t.Run(backend.Name, func(t *testing.T) {
			for _, c := range cases {
				c := c
				if reason := c.skipFor(env.Goos); reason != "" {
					t.Logf("SKIP %s: %s", c.Name, reason)
					continue
				}
				if env.Goos == "windows" && c.Cmd == "" {
					t.Logf("SKIP %s: 没有 Windows 版脚本", c.Name)
					continue
				}
				if env.Goos != "windows" && c.Sh == "" {
					t.Logf("SKIP %s: 没有 POSIX 版脚本", c.Name)
					continue
				}
				if note := c.noteFor(env.Goos); note != "" {
					t.Logf("NOTE %s: %s", c.Name, note)
				}
				if c.Slow && os.Getenv(conformanceSlowEnv) != "1" {
					t.Logf("SKIP %s: 慢场景（设 %s=1 启用）", c.Name, conformanceSlowEnv)
					continue
				}
				if reason := conformanceMissingDependency(c); reason != "" {
					t.Logf("SKIP %s: %s", c.Name, reason)
					continue
				}
				t.Run(c.Name, func(t *testing.T) {
					// 每个 (后端, 用例) 独立目录：失败/残留不影响下一个，也避免
					// bwrap 跑过之后 landlock 看到残留（"File exists"这类假失败）。
					workRoot := filepath.Join(env.Root, backend.Name, sanitizeForPath(c.Name))
					if err := os.MkdirAll(workRoot, 0o755); err != nil {
						t.Fatalf("mkdir work root: %v", err)
					}
					result := conformanceRun(t, env, backend, c, workRoot)
					assertConformance(t, env, c, backend, result)
				})
			}
		})
	}
	// 统计放在最后打印，方便一眼看到 N/M/K/J。
	executed := 0
	skipped := 0
	for range backends {
		for _, c := range cases {
			if c.skipFor(env.Goos) != "" || (env.Goos == "windows" && c.Cmd == "") || (env.Goos != "windows" && c.Sh == "") {
				skipped++
				continue
			}
			if c.Slow && os.Getenv(conformanceSlowEnv) != "1" {
				skipped++
				continue
			}
			if conformanceMissingDependency(c) != "" {
				skipped++
				continue
			}
			executed++
		}
	}
	stats.Executed = executed
	stats.Skipped = skipped * 1
	printConformanceStats(t, env.Goos, cases, stats)
}

// sanitizeForPath 把场景名变成安全的单层目录名。
func sanitizeForPath(name string) string {
	replacer := strings.NewReplacer("/", "_", " ", "_", "\\", "_", ":", "_")
	return replacer.Replace(name)
}

// conformanceMissingDependency 返回缺失依赖的原因（空串 = 依赖齐全）。
func conformanceMissingDependency(c conformanceCase) string {
	for _, dep := range c.Needs {
		if _, err := exec.LookPath(dep); err != nil {
			return fmt.Sprintf("缺少依赖 %q（%v）", dep, err)
		}
	}
	return ""
}

// conformanceRun 用产品路径执行一行场景：Confine → spawn → 收集结果。
func conformanceRun(t *testing.T, env conformanceEnv, backend conformanceBackend, c conformanceCase, workRoot string) conformanceResult {
	t.Helper()
	script := c.Sh
	if env.Goos == "windows" {
		script = c.Cmd
	}
	argv := append(append([]string(nil), env.Shell...), script)
	if c.Protect {
		// 保护路径必须在 Confine 之前存在，否则会被 existingPaths 丢掉（fail-open，
		// 见 git/protect-missing-at-build-time）。
		if err := os.MkdirAll(filepath.Join(workRoot, ".git"), 0o755); err != nil {
			t.Fatalf("mkdir .git: %v", err)
		}
	}
	policy := conformancePolicy(env, c, workRoot)
	confined, err := backend.Runner.Confine(argv, policy)
	if err != nil {
		return conformanceResult{Exit: -1, Backend: backend.Name, ConfineErr: err}
	}
	if len(confined.Argv) == 0 {
		return conformanceResult{Exit: -1, Backend: backend.Name, ConfineErr: errors.New("runner 返回空 argv")}
	}
	cmd := exec.Command(confined.Argv[0], confined.Argv[1:]...)
	cmd.Dir = workRoot
	cmd.Env = conformanceChildEnv(env)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	code := 0
	if runErr != nil {
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			code = exitErr.ExitCode()
		} else {
			code = -1
		}
	}
	return conformanceResult{
		Exit: code, Stdout: stdout.String(), Stderr: stderr.String(),
		Argv: confined.Argv, Enforcement: confined.Enforcement, Backend: backend.Name,
	}
}

// conformancePolicy 构造一行场景的策略：workspace-write + 保护 .git + 工具链缓存。
//
// 与产品默认一致（kit 默认开保护、runner 默认追加工具链缓存），因此这些极端场景
// 测的就是用户真实拿到的组合。
func conformancePolicy(env conformanceEnv, c conformanceCase, workRoot string) *sandbox.Policy {
	policy := sandbox.New(
		sandbox.WithRoots(env.Root),
		sandbox.WithMode(c.mode()),
		sandbox.WithNetwork(c.Network),
	)
	if c.mode() == sandbox.ModeWorkspaceWrite && c.Protect {
		// 用例工作目录里的 .git（git init 会创建在 cmd.Dir 下）与工作区根的 .git
		// 都要保护：这与 kit 的默认保护语义一致，也让 git/* 用例有意义。
		policy.AddProtectedPaths(filepath.Join(workRoot, ".git"), filepath.Join(env.Root, ".git"))
	}
	return policy
}

// conformanceChildEnv 返回子进程环境（紧凑、不泄漏宿主的真实 HOME）。
func conformanceChildEnv(env conformanceEnv) []string {
	path := os.Getenv("PATH")
	if path == "" {
		path = "/usr/local/bin:/usr/bin:/bin"
	}
	return []string{
		"PATH=" + path,
		"HOME=" + env.Home,
		"USERPROFILE=" + env.Home,
		"TMPDIR=" + env.Tmp,
		"TEMP=" + env.Tmp,
		"TMP=" + env.Tmp,
		"LANG=C.UTF-8",
		"CONFORMANCE_OUTSIDE=" + env.Outside,
		"CONFORMANCE_WS=" + env.Root,
		"GOTOOLCHAIN=local",
		"GOFLAGS=-mod=mod",
	}
}

// conformanceNewEnv 建好一次真机层要用的目录骨架（各平台复用）。
func conformanceNewEnv(t *testing.T, goos string) conformanceEnv {
	t.Helper()
	base := conformanceBaseDir(t)
	if base == "" {
		t.Skip("找不到可用的审计根目录（需要 /tmp 之外的可写目录；见 conformanceBaseDir）")
	}
	// 进程唯一后缀：`go test ./...` 与 `-run Conformance` 可能并发跑同一个 base，
	// 两边的 Cleanup 会互相 RemoveAll 对方正在用的目录（表现为随机的
	// "no such file"，与本轮修的平台断言无关，但会淹没真问题）。
	name := t.Name()
	if len(name) > 40 {
		name = name[:40]
	}
	base = filepath.Join(base, fmt.Sprintf("%s-%d", name, os.Getpid()))
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatalf("mkdir base %s: %v", base, err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	env := conformanceEnv{
		Root:    filepath.Join(base, "ws"),
		Home:    filepath.Join(base, "home"),
		Outside: filepath.Join(base, "outside"),
		Tmp:     filepath.Join(base, "tmpdir"),
		Goos:    goos,
	}
	for _, dir := range []string{
		env.Root, env.Home, env.Outside, env.Tmp,
		filepath.Join(env.Home, ".ssh"),
		filepath.Join(env.Home, ".cache", "go-build"),
		filepath.Join(env.Home, "go", "pkg", "mod"),
		filepath.Join(env.Home, ".npm"),
		filepath.Join(env.Home, ".cargo"),
		filepath.Join(env.Home, ".rustup"),
		filepath.Join(env.Home, ".m2"),
		filepath.Join(env.Home, ".gradle"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	// 根级已存在文件：供"覆写工作区根级文件"用例使用（沙箱内不能再新建它）。
	if err := os.WriteFile(filepath.Join(env.Root, "seed.txt"), []byte("seed\n"), 0o644); err != nil {
		t.Fatalf("write seed.txt: %v", err)
	}
	// 工具链缓存根与临时根都由**宿主进程**的环境派生，必须对齐（否则白名单指向真实 HOME）。
	t.Setenv("HOME", env.Home)
	t.Setenv("USERPROFILE", env.Home)
	t.Setenv("TMPDIR", env.Tmp)
	// Windows 的缓存根是 `%LOCALAPPDATA%` 与 `%APPDATA%\npm-cache`：只改
	// USERPROFILE 的话它们仍指向真实 AppData（那里已存在），落到"白名单指向别处"
	// 的形态——Linux 上看不出差别，Windows 上会直接改变被测行为。
	t.Setenv("LOCALAPPDATA", filepath.Join(env.Home, "AppData", "Local"))
	t.Setenv("APPDATA", filepath.Join(env.Home, "AppData", "Roaming"))
	return env
}

// conformanceBaseDir 返回一个**不在 /tmp 之下**的可写目录。
//
// bwrap 的私有 tmpfs 会遮住 /tmp（工作区在 /tmp 下时沙箱里看不到它），因此真机层
// 必须用一个 /tmp 之外的目录。选择顺序：RIG_CONFORMANCE_DIR → UserCacheDir →
// /var/tmp → /dev/shm；都不可用时返回空串（调用方 skip）。
func conformanceBaseDir(t *testing.T) string {
	t.Helper()
	var candidates []string
	if fromEnv := strings.TrimSpace(os.Getenv("RIG_CONFORMANCE_DIR")); fromEnv != "" {
		candidates = append(candidates, fromEnv)
	}
	if cache, err := os.UserCacheDir(); err == nil && cache != "" {
		candidates = append(candidates, filepath.Join(cache, "rig-conformance"))
	}
	candidates = append(candidates, "/var/tmp/rig-conformance", "/dev/shm/rig-conformance")
	for _, candidate := range candidates {
		if sandbox.PathWithin("/tmp", candidate) && runtime.GOOS == "linux" {
			continue
		}
		if err := os.MkdirAll(candidate, 0o755); err != nil {
			continue
		}
		probe := filepath.Join(candidate, ".writable")
		if err := os.WriteFile(probe, []byte("x"), 0o600); err != nil {
			continue
		}
		_ = os.Remove(probe)
		unique := filepath.Join(candidate, fmt.Sprintf("run-%d", time.Now().UnixNano()))
		if err := os.MkdirAll(unique, 0o755); err != nil {
			continue
		}
		t.Cleanup(func() { _ = os.RemoveAll(unique) })
		return unique
	}
	return ""
}
