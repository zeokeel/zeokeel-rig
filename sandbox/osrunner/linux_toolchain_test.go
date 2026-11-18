//go:build linux

package osrunner

// Linux 侧"开发工具链能力对齐"审计（见 docs/audit/SANDBOX-TOOLCHAIN-MATRIX.md）。
//
// 这个文件是**可执行的证据**：每条结论都能用
//
//	RIG_SANDBOX_TOOLCHAIN_TEST=1 go test ./sandbox/osrunner/ -run Toolchain -v
//
// 复现。默认跳过（go test 不带门控时只 t.Skip），因为它依赖宿主机上的真实工具链
// （go/npm/pip/cargo/gcc/git）、bwrap/Landlock，以及一个可以随便写坏的临时 HOME。
//
// 走的是**产品路径**：sandbox.Policy → sandbox.Runner.Confine → exec 返回的 Argv，
// 与 bash 工具在实际运行时的调用形状完全一致（bwrap 与 Landlock 两个后端各跑一遍）。

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig/sandbox"
)

const toolchainGateEnv = "RIG_SANDBOX_TOOLCHAIN_TEST"

// toolchainSkipUnlessGated 让这一组用例默认不跑。
func toolchainSkipUnlessGated(t *testing.T) {
	t.Helper()
	if os.Getenv(toolchainGateEnv) != "1" {
		t.Skipf("开发工具链审计默认跳过；设 %s=1 启用（依赖真实工具链与 bwrap/Landlock）", toolchainGateEnv)
	}
	if runtime.GOOS != "linux" {
		t.Skip("仅 Linux")
	}
}

// toolchainRunner 是被测后端。
type toolchainRunner struct {
	name   string
	runner sandbox.Runner
}

// toolchainRunners 返回本机真实可用的后端（bwrap 优先，Landlock 其次）。
//
// 直接构造具体类型（而不是走 New）是为了**两个后端都测**：New 的语义是"选一个
// 最好的"，而能力对齐审计需要同一策略在两者上的差异。
func toolchainRunners(t *testing.T) []toolchainRunner {
	t.Helper()
	var runners []toolchainRunner
	if path, reason := probeBwrapDetailed(DefaultProbeTimeout); reason == "" {
		runners = append(runners, toolchainRunner{name: RunnerBwrap, runner: &bwrapRunner{path: path}})
	} else {
		t.Logf("[skip] bwrap 不可用: %s", reason)
	}
	if abi, ok := probeLandlock(DefaultProbeTimeout); ok {
		exe, err := os.Executable()
		if err == nil && exe != "" {
			runners = append(runners, toolchainRunner{name: RunnerLandlock, runner: &landlockRunner{exe: exe, abi: abi}})
		}
	} else {
		t.Log("[skip] Landlock 不可用")
	}
	if len(runners) == 0 {
		t.Skip("本机没有可用的 Linux OS runner")
	}
	return runners
}

// toolchainSandbox 是一次审计用的沙箱环境：工作区 + 伪 HOME + 自定义 TMPDIR。
type toolchainSandbox struct {
	Root string // 策略根（工作区）
	Home string // 伪 HOME（模型"用户的 $HOME"）
	Tmp  string // 自定义 TMPDIR
}

// toolchainBaseDir 选一个**不在 /tmp 之下**的可写目录作为审计根。
//
// 为什么不能直接用 t.TempDir()：t.TempDir() 落在 $TMPDIR（通常是 /tmp），而 bwrap
// 的私有 tmpfs 会整块遮住 /tmp —— 于是"工作区"在沙箱里根本不存在，所有探针都会
// 变成 "No such file or directory"，测出来的是伪装成缺口的假象。这个坑本身也是
// 一条结论（见 TestToolchainPrivateTmpMasksHostTmp）。
//
// 选择顺序：RIG_SANDBOX_TOOLCHAIN_DIR → UserCacheDir → /var/tmp → /dev/shm →
// os.TempDir()；第一个"可写且不在 /tmp 之下"的胜出。全都不行时返回空串，调用方
// 应当跳过 bwrap 侧的用例。
func toolchainBaseDir(t *testing.T) string {
	t.Helper()
	var candidates []string
	if fromEnv := strings.TrimSpace(os.Getenv("RIG_SANDBOX_TOOLCHAIN_DIR")); fromEnv != "" {
		candidates = append(candidates, fromEnv)
	}
	if cache, err := os.UserCacheDir(); err == nil && cache != "" {
		candidates = append(candidates, filepath.Join(cache, "rig-toolchain-audit"))
	}
	candidates = append(candidates, "/var/tmp/rig-toolchain-audit", "/dev/shm/rig-toolchain-audit")
	for _, candidate := range candidates {
		if pathWithinOS("linux", "/tmp", candidate) {
			continue // bwrap 的私有 tmpfs 会遮住它
		}
		if err := os.MkdirAll(candidate, 0o755); err != nil {
			continue
		}
		probe := filepath.Join(candidate, ".writable")
		if err := os.WriteFile(probe, []byte("x"), 0o600); err != nil {
			continue
		}
		_ = os.Remove(probe)
		return candidate
	}
	return ""
}

// newToolchainSandbox 建好目录骨架：HOME 下带上常见开发工具的缓存目录（模拟真实
// 用户的 `~`），但**不**把它们放进策略的可写面。
func newToolchainSandbox(t *testing.T) toolchainSandbox {
	t.Helper()
	base := toolchainBaseDir(t)
	if base == "" {
		t.Skip("找不到 /tmp 之外的可写目录（bwrap 的私有 tmpfs 会遮住 /tmp）；" +
			"请设 RIG_SANDBOX_TOOLCHAIN_DIR 指向一个真实目录")
	}
	base = filepath.Join(base, t.Name())
	_ = os.RemoveAll(base)
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatalf("mkdir base: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	box := toolchainSandbox{
		Root: filepath.Join(base, "ws"),
		Home: filepath.Join(base, "home"),
		Tmp:  filepath.Join(base, "tmpdir"),
	}
	for _, dir := range []string{
		box.Root,
		box.Tmp,
		filepath.Join(box.Home, ".cache", "go-build"),
		filepath.Join(box.Home, ".cache", "pip"),
		filepath.Join(box.Home, "go", "pkg", "mod"),
		filepath.Join(box.Home, ".npm"),
		filepath.Join(box.Home, ".cargo"),
		filepath.Join(box.Home, ".rustup"),
		filepath.Join(box.Home, ".m2"),
		filepath.Join(box.Home, ".gradle"),
		filepath.Join(box.Home, ".local", "lib"),
		filepath.Join(box.Home, ".ssh"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	// 一份最小 git 身份，让 `git commit` 不需要用户交互。
	gitconfig := "[user]\n\tname = Audit\n\temail = audit@example.com\n"
	if err := os.WriteFile(filepath.Join(box.Home, ".gitconfig"), []byte(gitconfig), 0o644); err != nil {
		t.Fatalf("write .gitconfig: %v", err)
	}
	if err := os.WriteFile(filepath.Join(box.Home, ".ssh", "id_ed25519"), []byte("fake-key\n"), 0o600); err != nil {
		t.Fatalf("write ssh key: %v", err)
	}
	// 关键：工具链缓存根是**用宿主进程的环境**算出来的（osrunner 侧 os.Getenv），
	// 而被约束进程用的是它继承的 HOME。产品里两者一致，测试里必须显式对齐，
	// 否则白名单会指向 /root/.cache 之类的真实 HOME → 这条用例会假红。
	t.Setenv("HOME", box.Home)
	t.Setenv("USERPROFILE", box.Home)
	// 同理：可写临时目录由平台临时根（os.TempDir()，读宿主 TMPDIR）派生，
	// 因此这里也要对齐——产品里被约束进程继承的正是宿主这个值。
	t.Setenv("TMPDIR", box.Tmp)
	return box
}

// policy 按模式构造策略；可写面严格按产品规则：workspace ∪ 平台临时目录 − protected。
func (b toolchainSandbox) policy(mode sandbox.Mode, network bool, extra ...sandbox.Option) *sandbox.Policy {
	options := []sandbox.Option{
		sandbox.WithRoots(b.Root),
		sandbox.WithMode(mode),
		sandbox.WithNetwork(network),
		sandbox.WithSlashTmp(true),
	}
	options = append(options, extra...)
	return sandbox.New(options...)
}

// env 返回子进程环境：紧凑、可测、不与宿主机的真实 $HOME 混淆。
func (b toolchainSandbox) env(t *testing.T) []string {
	t.Helper()
	path := os.Getenv("PATH")
	if path == "" {
		path = "/usr/local/bin:/usr/bin:/bin"
	}
	return []string{
		"PATH=" + path,
		"HOME=" + b.Home,
		"TMPDIR=" + b.Tmp,
		"LANG=C.UTF-8",
		"GIT_CONFIG_GLOBAL=" + filepath.Join(b.Home, ".gitconfig"),
		"GOTOOLCHAIN=local",
		"GOFLAGS=-mod=mod",
	}
}

// result 是一次受限执行的观测结果。
type result struct {
	Stdout string
	Stderr string
	Code   int
	Argv   []string
	Err    error
}

// output 返回合并输出（便于断言"原始错误"）。
func (r result) output() string { return r.Stdout + r.Stderr }

// runScript 通过产品路径执行一段 /bin/sh 脚本。
func (b toolchainSandbox) runScript(t *testing.T, r toolchainRunner, policy *sandbox.Policy, dir, script string) result {
	t.Helper()
	confined, err := r.runner.Confine([]string{"/bin/sh", "-c", script}, policy)
	if err != nil {
		return result{Code: -1, Err: err}
	}
	if len(confined.Argv) == 0 {
		return result{Code: -1, Err: errors.New("runner returned an empty argv")}
	}
	cmd := exec.Command(confined.Argv[0], confined.Argv[1:]...)
	cmd.Dir = dir
	cmd.Env = b.env(t)
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
	return result{Stdout: stdout.String(), Stderr: stderr.String(), Code: code, Argv: confined.Argv, Err: runErr}
}

// ---------------------------------------------------------------------------
// 能力矩阵报告（写入 docs/audit/SANDBOX-TOOLCHAIN-MATRIX.md 的原始数据）
// ---------------------------------------------------------------------------

// toolchainProbe 是一条能力探针。
type toolchainProbe struct {
	Name string
	// Script 是 /bin/sh 脚本；非空表示整段脚本一起跑（用于需要多步的场景）。
	Script string
	// Want 是 workspace-write 下的期望结果，仅用于报告里的比对列。
	Want string
}

// toolchainProbes 覆盖 A 组 10 类开发工具能力。
func toolchainProbes(b toolchainSandbox) []toolchainProbe {
	home := b.Home
	return []toolchainProbe{
		{Name: "写工作区文件", Want: "OK", Script: "touch " + quoteSh(b.Root+"/probe.txt") + " && echo OK"},
		{Name: "写 $HOME/.cache/go-build（GOCACHE）", Want: "?", Script: "touch " + quoteSh(home+"/.cache/go-build/probe") + " 2>&1 && echo OK"},
		{Name: "写 $HOME/go/pkg/mod（GOMODCACHE）", Want: "?", Script: "touch " + quoteSh(home+"/go/pkg/mod/probe") + " 2>&1 && echo OK"},
		{Name: "写 $HOME/.npm", Want: "?", Script: "touch " + quoteSh(home+"/.npm/probe") + " 2>&1 && echo OK"},
		{Name: "写 $HOME/.cache/pip", Want: "?", Script: "touch " + quoteSh(home+"/.cache/pip/probe") + " 2>&1 && echo OK"},
		{Name: "写 $HOME/.cargo", Want: "?", Script: "touch " + quoteSh(home+"/.cargo/probe") + " 2>&1 && echo OK"},
		{Name: "写 $HOME/.rustup", Want: "?", Script: "touch " + quoteSh(home+"/.rustup/probe") + " 2>&1 && echo OK"},
		{Name: "写 $HOME/.m2", Want: "?", Script: "touch " + quoteSh(home+"/.m2/probe") + " 2>&1 && echo OK"},
		{Name: "写 $HOME/.gradle", Want: "?", Script: "touch " + quoteSh(home+"/.gradle/probe") + " 2>&1 && echo OK"},
		{Name: "写 $HOME/.local/lib（pip --user）", Want: "?", Script: "touch " + quoteSh(home+"/.local/lib/probe") + " 2>&1 && echo OK"},
		{Name: "写 $TMPDIR（自定义）", Want: "OK", Script: "touch " + quoteSh(b.Tmp+"/probe") + " 2>&1 && echo OK"},
		{Name: "写字面 /tmp", Want: "OK(私有)", Script: "touch /tmp/probe && echo OK"},
		{Name: "写 /var/tmp", Want: "?", Script: "touch /var/tmp/probe 2>&1 && echo OK"},
		{Name: "写 /dev/shm", Want: "?", Script: "touch /dev/shm/probe 2>&1 && echo OK"},
		{Name: "写 /dev/null", Want: "OK", Script: "echo x > /dev/null && echo OK"},
		{Name: "读 /etc/hosts", Want: "OK", Script: "head -1 /etc/hosts"},
		{Name: "读 /etc/resolv.conf", Want: "OK", Script: "head -1 /etc/resolv.conf"},
		{Name: "读 /proc/self/exe", Want: "OK", Script: "readlink /proc/self/exe"},
		{Name: "读 /proc/version", Want: "OK", Script: "head -1 /proc/version"},
		{Name: "读 /sys 设备信息", Want: "OK", Script: "head -1 /sys/devices/system/cpu/online"},
		{Name: "读 $HOME/.ssh 私钥", Want: "OK", Script: "head -1 " + quoteSh(home+"/.ssh/id_ed25519")},
		{Name: "读 $HOME/.gitconfig", Want: "OK", Script: "head -1 " + quoteSh(home+"/.gitconfig")},
		{Name: "写 $HOME/.ssh 新文件", Want: "DENY", Script: "touch " + quoteSh(home+"/.ssh/evil") + " 2>&1 && echo OK"},
		{Name: "改工作区外的文件", Want: "DENY", Script: "touch " + quoteSh(home+"/outside.txt") + " 2>&1 && echo OK"},
		{Name: "chmod 工作区文件", Want: "OK", Script: "touch " + quoteSh(b.Root+"/m.txt") + " && chmod 600 " + quoteSh(b.Root+"/m.txt") + " && echo OK"},
		{Name: "回环监听（127.0.0.1）", Want: "?", Script: "python3 -c 'import socket;s=socket.socket();s.bind((\"127.0.0.1\",0));print(\"BIND_OK\",s.getsockname()[1]);s.close()' 2>&1"},
		{Name: "回环连接（curl 本机端口）", Want: "?", Script: "python3 -c 'import socket,threading,urllib.request,time\ns=socket.socket();s.bind((\"127.0.0.1\",0));s.listen(1);port=s.getsockname()[1]\ndef serve():\n    c,_=s.accept();c.sendall(b\"HTTP/1.1 200 OK\\r\\nContent-Length: 2\\r\\n\\r\\nhi\");c.close()\nthreading.Thread(target=serve,daemon=True).start()\nprint(urllib.request.urlopen(f\"http://127.0.0.1:{port}/\",timeout=3).read().decode())' 2>&1"},
		{Name: "DNS 解析（getent hosts localhost）", Want: "?", Script: "getent hosts localhost 2>&1 || echo getent-failed"},
		{Name: "出口网络（curl 1.1.1.1）", Want: "?", Script: "curl -s -m 3 -o /dev/null -w '%{http_code}' http://1.1.1.1/ 2>&1 || echo curl-failed"},
	}
}

// quoteSh 用单引号包住路径，避免空格/特殊字符干扰。
func quoteSh(path string) string { return "'" + strings.ReplaceAll(path, "'", `'\''`) + "'" }

// TestToolchainCapabilityMatrixReport 打印完整的能力矩阵（报告用，不做断言）。
func TestToolchainCapabilityMatrixReport(t *testing.T) {
	toolchainSkipUnlessGated(t)
	box := newToolchainSandbox(t)
	runners := toolchainRunners(t)
	probes := toolchainProbes(box)

	for _, r := range runners {
		for _, modeInfo := range []struct {
			mode    sandbox.Mode
			network bool
			label   string
		}{
			{sandbox.ModeReadOnly, false, "read-only"},
			{sandbox.ModeWorkspaceWrite, false, "workspace-write"},
			{sandbox.ModeWorkspaceWrite, true, "workspace-write+net"},
			{sandbox.ModeDangerFullAccess, false, "danger-full-access"},
		} {
			policy := box.policy(modeInfo.mode, modeInfo.network)
			for _, probe := range probes {
				got := box.runScript(t, r, policy, box.Root, probe.Script)
				t.Logf("MATRIX|%s|%s|%s|%s|code=%d|%s",
					r.name, modeInfo.label, probe.Name, verdict(got), got.Code,
					firstLine(strings.TrimSpace(got.output())))
			}
		}
	}
}

// verdict 把一次观测压成 OK / DENY / FAIL 三态（报告列）。
func verdict(r result) string {
	text := strings.ToLower(r.output())
	switch {
	case r.Err != nil && r.Code == -1:
		return "RUNNER-ERR"
	case r.Code != 0:
		return "FAIL"
	case strings.Contains(text, "read-only file system"),
		strings.Contains(text, "permission denied"),
		strings.Contains(text, "operation not permitted"):
		return "DENY"
	}
	return "OK"
}

// firstLine 截断多行输出，方便单行报告。
func firstLine(text string) string {
	if index := strings.IndexByte(text, '\n'); index >= 0 {
		text = text[:index]
	}
	if len(text) > 120 {
		text = text[:120] + "…"
	}
	return text
}

// ---------------------------------------------------------------------------
// 回归断言：这些是"必须成立"的契约（不管实现怎么改，都不该变）
// ---------------------------------------------------------------------------

// TestToolchainWorkspaceWriteAllowsWorkspaceOnly 断言 workspace-write 的写面就是
// 工作区 + 临时目录：工作区可写，$HOME 不可写（无论 HOME 下是什么目录）。
func TestToolchainWorkspaceWriteAllowsWorkspaceOnly(t *testing.T) {
	toolchainSkipUnlessGated(t)
	box := newToolchainSandbox(t)
	for _, r := range toolchainRunners(t) {
		t.Run(r.name, func(t *testing.T) {
			policy := box.policy(sandbox.ModeWorkspaceWrite, false)
			if got := box.runScript(t, r, policy, box.Root, "touch inner.txt && echo OK"); !strings.Contains(got.output(), "OK") {
				t.Fatalf("工作区写入应当成功: code=%d out=%q", got.Code, got.output())
			}
			got := box.runScript(t, r, policy, box.Root, "touch "+quoteSh(box.Home+"/escape.txt")+" 2>&1")
			if got.Code == 0 {
				t.Fatalf("$HOME 写入应当被拒绝: out=%q", got.output())
			}
			if !strings.Contains(strings.ToLower(got.output()), "read-only") &&
				!strings.Contains(strings.ToLower(got.output()), "permission denied") {
				t.Fatalf("拒绝原因不是可读的沙箱错误: %q", got.output())
			}
		})
	}
}

// TestToolchainReadOnlyDeniesWrites 断言 read-only 下连工作区都不可写。
func TestToolchainReadOnlyDeniesWrites(t *testing.T) {
	toolchainSkipUnlessGated(t)
	box := newToolchainSandbox(t)
	for _, r := range toolchainRunners(t) {
		t.Run(r.name, func(t *testing.T) {
			policy := box.policy(sandbox.ModeReadOnly, false)
			got := box.runScript(t, r, policy, box.Root, "touch denied.txt 2>&1")
			if got.Code == 0 {
				t.Fatalf("read-only 下工作区写入应当失败: out=%q", got.output())
			}
			// 读系统路径仍然允许。
			if read := box.runScript(t, r, policy, box.Root, "head -1 /etc/hosts"); read.Code != 0 {
				t.Fatalf("read-only 下读 /etc/hosts 应当成功: %q", read.output())
			}
		})
	}
}

// TestToolchainDevCacheDirsAreWritable 是**工具链缓存白名单**的回归：GOCACHE、
// GOMODCACHE、~/.npm、~/.cache/pip、~/.cargo、~/.rustup、~/.m2、~/.gradle 都必须
// 在 workspace-write 下可写。
//
// 修复之前这条是红的（那就是审计里"沙箱一开、开发全废"的根因）：
//
//	bwrap    : open …/.cache/go-build/…: read-only file system
//	landlock : open …/.cache/go-build/…: permission denied
//
// 它依赖宿主进程的 HOME 与子进程一致（产品默认如此）——缓存根由宿主侧 os.Getenv
// 计算，两者不一致时白名单会指向别处。
func TestToolchainDevCacheDirsAreWritable(t *testing.T) {
	toolchainSkipUnlessGated(t)
	box := newToolchainSandbox(t)
	caches := []struct{ name, dir string }{
		{"GOCACHE", filepath.Join(box.Home, ".cache", "go-build")},
		{"GOMODCACHE", filepath.Join(box.Home, "go", "pkg", "mod")},
		{"npm", filepath.Join(box.Home, ".npm")},
		{"pip", filepath.Join(box.Home, ".cache", "pip")},
		{"cargo", filepath.Join(box.Home, ".cargo")},
		{"rustup", filepath.Join(box.Home, ".rustup")},
		{"maven", filepath.Join(box.Home, ".m2")},
		{"gradle", filepath.Join(box.Home, ".gradle")},
	}
	for _, r := range toolchainRunners(t) {
		for _, cache := range caches {
			t.Run(r.name+"/"+cache.name, func(t *testing.T) {
				policy := box.policy(sandbox.ModeWorkspaceWrite, false)
				target := filepath.Join(cache.dir, "probe-$$")
				got := box.runScript(t, r, policy, box.Root, "echo x > "+quoteSh(target)+" 2>&1")
				if got.Code != 0 {
					t.Fatalf("%s 不可写（工具链缓存白名单没生效）: code=%d %s", cache.name, got.Code, strings.TrimSpace(got.output()))
				}
			})
		}
	}
}

// TestToolchainCacheCredentialFilesStayReadOnly 是**安全回归**：缓存目录可写不等于
// 里面的凭据文件可改。~/.npm/.npmrc（registry token）、~/.cargo/credentials.toml、
// ~/.m2/settings.xml（服务器密码）、~/.gradle/gradle.properties 一旦可写，就等于
// 给沙箱一个"改配置 → 下次构建执行任意代码/外泄凭据"的持久化通道。
func TestToolchainCacheCredentialFilesStayReadOnly(t *testing.T) {
	toolchainSkipUnlessGated(t)
	box := newToolchainSandbox(t)
	credentials := []string{
		filepath.Join(box.Home, ".npm", ".npmrc"),
		filepath.Join(box.Home, ".cargo", "credentials.toml"),
		filepath.Join(box.Home, ".m2", "settings.xml"),
		filepath.Join(box.Home, ".gradle", "gradle.properties"),
	}
	for _, path := range credentials {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(path, []byte("# placeholder\n"), 0o600); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	for _, r := range toolchainRunners(t) {
		for _, path := range credentials {
			t.Run(r.name+"/"+filepath.Base(filepath.Dir(path))+"/"+filepath.Base(path), func(t *testing.T) {
				policy := box.policy(sandbox.ModeWorkspaceWrite, false)
				got := box.runScript(t, r, policy, box.Root, "echo x > "+quoteSh(path)+" 2>&1")
				if got.Code == 0 {
					t.Fatalf("%s 可被沙箱改写（凭据持久化通道）", path)
				}
			})
		}
	}
}

// TestToolchainPrivateTmpMasksHostTmp 记录一个**会在建测试时把人坑惨**的语义：
// bwrap 的 `--tmpfs /tmp` 会把宿主 `/tmp` 整块遮住，因此
//   - 工作区放在 /tmp 下时，沙箱里根本看不到它（所有探针变成 "No such file"）；
//   - 写字面 /tmp 成功，但落在私有 tmpfs，宿主看不到（不落宿主）。
//
// Landlock 没有私有 tmpfs：它的 /tmp 就是宿主 /tmp。
func TestToolchainPrivateTmpMasksHostTmp(t *testing.T) {
	toolchainSkipUnlessGated(t)
	box := newToolchainSandbox(t)
	for _, r := range toolchainRunners(t) {
		t.Run(r.name, func(t *testing.T) {
			policy := box.policy(sandbox.ModeWorkspaceWrite, false)
			name := "/tmp/rig-toolchain-private-" + r.name + "-" + "probe"
			got := box.runScript(t, r, policy, box.Root, "echo hi > "+quoteSh(name)+" && echo OK")
			if got.Code != 0 {
				t.Fatalf("往 /tmp 写应当成功（bwrap 私有 tmpfs / landlock 宿主 /tmp）: %q", got.output())
			}
			_, statErr := os.Stat(name)
			visibleOnHost := statErr == nil
			t.Logf("%s: /tmp 写入对宿主可见 = %v（bwrap 期望 false，landlock 期望 true）", r.name, visibleOnHost)
			if visibleOnHost {
				_ = os.Remove(name)
			}
			if r.name == RunnerBwrap && visibleOnHost {
				t.Fatal("bwrap 的 /tmp 应当是私有 tmpfs，写入不应落到宿主 /tmp")
			}
		})
	}
}

// TestToolchainTmpDirIdentity 断言临时目录语义：自定义 $TMPDIR 可写；bwrap 的字面
// /tmp 是私有 tmpfs（写成功但宿主的 /tmp 看不到），Landlock 没有私有 tmpfs。
func TestToolchainTmpDirIdentity(t *testing.T) {
	toolchainSkipUnlessGated(t)
	box := newToolchainSandbox(t)
	for _, r := range toolchainRunners(t) {
		t.Run(r.name, func(t *testing.T) {
			policy := box.policy(sandbox.ModeWorkspaceWrite, false)
			custom := box.runScript(t, r, policy, box.Root, "touch "+quoteSh(box.Tmp+"/probe")+" && echo OK")
			if custom.Code != 0 {
				t.Fatalf("自定义 $TMPDIR 应当可写: %q", custom.output())
			}
			literal := box.runScript(t, r, policy, box.Root, "echo hi > /tmp/rig-audit-tmp && echo OK")
			t.Logf("字面 /tmp 写入: code=%d out=%q", literal.Code, strings.TrimSpace(literal.output()))
			// bwrap 的私有 tmpfs：宿主上不应存在该文件；Landlock 直接拒绝。
			if _, err := os.Stat("/tmp/rig-audit-tmp"); err == nil {
				t.Logf("注意：/tmp 写入落到了宿主 /tmp（backend=%s）", r.name)
				_ = os.Remove("/tmp/rig-audit-tmp")
			}
		})
	}
}

// TestToolchainDangerFullAccessPassesThrough 断言 danger-full-access 完全不包装。
func TestToolchainDangerFullAccessPassesThrough(t *testing.T) {
	toolchainSkipUnlessGated(t)
	box := newToolchainSandbox(t)
	for _, r := range toolchainRunners(t) {
		confined, err := r.runner.Confine([]string{"/bin/true"}, box.policy(sandbox.ModeDangerFullAccess, true))
		if err != nil {
			t.Fatalf("%s: danger-full-access 不应返回错误: %v", r.name, err)
		}
		if confined.Enforcement != sandbox.EnforcementNone {
			t.Fatalf("%s: Enforcement = %v, want EnforcementNone", r.name, confined.Enforcement)
		}
		if len(confined.Argv) != 1 || confined.Argv[0] != "/bin/true" {
			t.Fatalf("%s: danger-full-access 应当透传 argv, got %v", r.name, confined.Argv)
		}
	}
}

// TestToolchainNoNetworkByDefault 断言默认策略（AllowNetwork=false）下回环也不通：
// bwrap 用 --unshare-net 创建一个全新的 net namespace，lo 是 DOWN 的。
func TestToolchainNoNetworkByDefault(t *testing.T) {
	toolchainSkipUnlessGated(t)
	box := newToolchainSandbox(t)
	for _, r := range toolchainRunners(t) {
		t.Run(r.name, func(t *testing.T) {
			policy := box.policy(sandbox.ModeWorkspaceWrite, false)
			got := box.runScript(t, r, policy, box.Root,
				"python3 -c 'import socket;s=socket.socket();s.settimeout(2);s.connect((\"127.0.0.1\",1))' 2>&1 || echo CONNECT-FAILED")
			t.Logf("回环连接（默认策略）: code=%d out=%q", got.Code, firstLine(strings.TrimSpace(got.output())))
			// 允许网络时应当能连上本机监听端口（bwrap 不 unshare net；Landlock 只管文件）。
			withNet := box.policy(sandbox.ModeWorkspaceWrite, true)
			loop := box.runScript(t, r, withNet, box.Root,
				"python3 -c 'import socket,threading,urllib.request\ns=socket.socket();s.bind((\"127.0.0.1\",0));s.listen(1);port=s.getsockname()[1]\ndef serve():\n    c,_=s.accept();c.sendall(b\"HTTP/1.1 200 OK\\r\\nContent-Length: 2\\r\\n\\r\\nhi\");c.close()\nthreading.Thread(target=serve,daemon=True).start()\nprint(urllib.request.urlopen(f\"http://127.0.0.1:{port}/\",timeout=3).read().decode())' 2>&1")
			t.Logf("回环连接（AllowNetwork=true）: code=%d out=%q", loop.Code, firstLine(strings.TrimSpace(loop.output())))
		})
	}
}

// TestToolchainToolVersions 打印审计时的工具链版本，便于把结论钉在具体版本上。
func TestToolchainToolVersions(t *testing.T) {
	toolchainSkipUnlessGated(t)
	box := newToolchainSandbox(t)
	policy := box.policy(sandbox.ModeDangerFullAccess, true)
	runner := toolchainRunner{name: "passthrough", runner: passthroughRunner{}}
	for _, command := range []string{
		"go version", "node --version", "npm --version", "python3 --version",
		"cargo --version", "git --version", "gcc --version | head -1", "make --version | head -1",
	} {
		got := box.runScript(t, runner, policy, box.Root, command)
		t.Logf("VERSION|%s|%s", command, firstLine(strings.TrimSpace(got.output())))
	}
	start := time.Now()
	t.Logf("审计时间: %s", start.Format(time.RFC3339))
}

// passthroughRunner 是 danger-full-access 的等价 runner（把 argv 原样返回）。
type passthroughRunner struct{}

func (passthroughRunner) Confine(argv []string, policy *sandbox.Policy) (sandbox.ConfinedCommand, error) {
	return sandbox.ConfinedCommand{Argv: append([]string(nil), argv...), Enforcement: sandbox.EnforcementNone}, nil
}
