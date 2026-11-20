package main

// rigmigrate CLI 的表驱动测试：退出码、单条命中、-json。
//
// 造出来的"宿主目录"只用于解析、不参与编译——宿主升级时的真实状态就是
// "编译器报错、代码还不能 build"，扫描器必须能在这种状态下工作。

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// hostFixture 是"带旧写法的宿主目录"：模拟从 v0.24 升到 v0.25 的现场。
var hostFixture = map[string]string{
	"go.mod": "module example.com/host\n\ngo 1.27\n",
	"main.go": `package main

import (
	"bytes"

	"github.com/zeokeel/zeokeel-rig/kit"
	"github.com/zeokeel/zeokeel-rig/sandbox"
)

func build(r any) {
	var buf bytes.Buffer
	// 已删除的配置字段（键值字面量）。
	_ = kit.Config{Redactor: nil}
	// 已删除的保护路径接口。
	_ = sandbox.WithProtectedPaths("/root/.ssh")
	// 已收紧的事件桥签名：旧调用是 2 个实参。
	_ = kit.NewEventSink(&buf, r)
	// 已收紧的授权校验签名：旧调用是 5 个实参。
	_, _ = sandbox.ValidateWritableGrantPath("/x", []string{"/"}, nil, nil, nil)
}
`,
}

// cleanFixture 是"已迁移完的宿主目录"：包含仍然有效的隐藏路径与普通路径字符串。
var cleanFixture = map[string]string{
	"go.mod": "module example.com/host\n\ngo 1.27\n",
	"main.go": `package main

import "github.com/zeokeel/zeokeel-rig/sandbox"

func build() *sandbox.Policy {
	policy := sandbox.WithHidden("**/.env")
	_ = policy.DeniedPath("/tmp/x")
	_ = "secrets/creds.md"
	return policy
}
`,
}

// hostSameNameFixture 是"包基名与 rig 相同、但其实不是 rig"的宿主目录：
// `github.com/host/internal/kit`、`github.com/host/secrets` 是宿主自己的包。
// 扫描器必须一条不报、退出码 0，否则迁移干净的宿主会收到假红。
var hostSameNameFixture = map[string]string{
	"go.mod": "module example.com/host\n\ngo 1.27\n",
	"main.go": `package main

import (
	"github.com/host/internal/kit"
	"github.com/host/secrets"
)

func build(r any) {
	_ = kit.Config{Redactor: nil}
	_ = kit.NewEventSink(nil, nil)
	_ = kit.HomeSecretPaths()
	_ = secrets.RedactForExport(r)
}
`,
}

// writeDir 把 fixture 落到临时目录。
func writeDir(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, body := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", path, err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	return root
}

// TestRunExitCodes 覆盖三种退出码与命中数量。
func TestRunExitCodes(t *testing.T) {
	cases := []struct {
		name      string
		files     map[string]string
		subdir    string   // -C 相对临时目录的子路径（空表示临时目录本身）
		rawArgs   []string // 非 nil 时直接当命令行（用于用法错误）
		wantExit  int
		wantHits  int
		wantInOut []string
		wantInErr string
	}{
		{
			name:      "带旧写法的宿主目录",
			files:     hostFixture,
			wantExit:  1,
			wantHits:  4,
			wantInOut: []string{"main.go:", "kit.Config.Redactor", "sandbox.WithProtectedPaths", "kit.NewEventSink", "sandbox.ValidateWritableGrantPath"},
		},
		{
			name:     "已迁移干净的宿主目录",
			files:    cleanFixture,
			wantExit: 0,
			wantHits: 0,
		},
		{
			name:     "宿主自有同名包（host/internal/kit、host/secrets）",
			files:    hostSameNameFixture,
			wantExit: 0,
			wantHits: 0,
		},
		{
			name:      "只写选择器形态（cfg.Redactor）",
			files:     map[string]string{"main.go": "package host\n\nimport \"github.com/zeokeel/zeokeel-rig/kit\"\n\nfunc f(cfg kit.Config) {\n\t_ = cfg.Redactor\n}\n"},
			wantExit:  1,
			wantHits:  1,
			wantInOut: []string{"kit.Config.Redactor"},
		},
		{
			name:      "目录不存在",
			subdir:    "no-such-dir",
			wantExit:  2,
			wantInErr: "no-such-dir",
		},
		{
			name:      "未知参数",
			files:     cleanFixture,
			rawArgs:   []string{"-nope"},
			wantExit:  2,
			wantInErr: "flag provided but not defined",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var dir string
			if tc.files != nil {
				dir = writeDir(t, tc.files)
			} else {
				dir = t.TempDir()
			}
			target := dir
			if tc.subdir != "" {
				target = filepath.Join(dir, tc.subdir)
			}
			args := append([]string{}, tc.rawArgs...)
			if tc.rawArgs == nil {
				args = append(args, "-C", target)
			}
			var stdout, stderr bytes.Buffer
			exit := run(args, &stdout, &stderr)
			if exit != tc.wantExit {
				t.Fatalf("exit = %d, want %d\nstdout:\n%s\nstderr:\n%s", exit, tc.wantExit, stdout.String(), stderr.String())
			}
			out := stdout.String()
			for _, want := range tc.wantInOut {
				if !strings.Contains(out, want) {
					t.Errorf("stdout 缺少 %q:\n%s", want, out)
				}
			}
			if tc.wantInErr != "" && !strings.Contains(stderr.String(), tc.wantInErr) {
				t.Errorf("stderr 缺少 %q:\n%s", tc.wantInErr, stderr.String())
			}
			if hits := strings.Count(out, "main.go:"); hits != tc.wantHits {
				t.Errorf("命中行数 = %d, want %d:\n%s", hits, tc.wantHits, out)
			}
		})
	}
}

// TestRunJSON 断言 -json 输出可解析且带机器可读字段。
func TestRunJSON(t *testing.T) {
	dir := writeDir(t, hostFixture)
	var stdout, stderr bytes.Buffer
	if exit := run([]string{"-C", dir, "-json"}, &stdout, &stderr); exit != 1 {
		t.Fatalf("exit = %d, want 1\nstderr:\n%s", exit, stderr.String())
	}
	var report struct {
		Findings []struct {
			File string `json:"file"`
			Line int    `json:"line"`
			Old  string `json:"old"`
			New  string `json:"new"`
			Note string `json:"note"`
		} `json:"findings"`
		Count int `json:"count"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("JSON 解析失败: %v\n%s", err, stdout.String())
	}
	if report.Count != 4 || len(report.Findings) != 4 {
		t.Fatalf("count = %d / len = %d, want 4", report.Count, len(report.Findings))
	}
	for _, finding := range report.Findings {
		if finding.File == "" || finding.Line <= 0 || finding.Old == "" || finding.New == "" {
			t.Errorf("字段缺失: %+v", finding)
		}
	}
}

// TestRunCleanJSONIsEmptyList 断言干净目录在 -json 下输出的是 `[]` 而不是 `null`。
//
// 光把 findings 解到切片里没法区分两者（`null` 与 `[]` 都是 len==0），所以这里
// 直接看原始输出的字面量：脚本消费 -json 时按数组迭代，`null` 会炸。
func TestRunCleanJSONIsEmptyList(t *testing.T) {
	dir := writeDir(t, cleanFixture)
	var stdout, stderr bytes.Buffer
	if exit := run([]string{"-C", dir, "-json"}, &stdout, &stderr); exit != 0 {
		t.Fatalf("exit = %d, want 0\nstderr:\n%s", exit, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, `"findings": []`) {
		t.Errorf("-json 的 findings 不是空数组字面量（不能是 null）:\n%s", out)
	}
	if strings.Contains(out, "null") {
		t.Errorf("-json 输出里出现了 null:\n%s", out)
	}
	var report struct {
		Findings []json.RawMessage `json:"findings"`
		Count    int               `json:"count"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("JSON 解析失败: %v\n%s", err, out)
	}
	if report.Count != 0 || len(report.Findings) != 0 {
		t.Errorf("count = %d / len = %d, want 0", report.Count, len(report.Findings))
	}
}

// TestRunHelp 覆盖 -h / --help：帮助不是用法错误，退出码 0，用法写到 stderr。
func TestRunHelp(t *testing.T) {
	for _, arg := range []string{"-h", "--help"} {
		t.Run(arg, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if exit := run([]string{arg}, &stdout, &stderr); exit != exitClean {
				t.Fatalf("exit = %d, want %d：帮助不是用法错误", exit, exitClean)
			}
			if stdout.Len() != 0 {
				t.Errorf("帮助不该写 stdout:\n%s", stdout.String())
			}
			usage := stderr.String()
			for _, want := range []string{"usage: rigmigrate", "-C", "-json", "Exit codes"} {
				if !strings.Contains(usage, want) {
					t.Errorf("stderr 缺少 %q:\n%s", want, usage)
				}
			}
		})
	}
}

// TestRunDefaultDirectory 覆盖无参数：-C 默认 "."，扫的就是当前工作目录。
func TestRunDefaultDirectory(t *testing.T) {
	t.Run("干净目录", func(t *testing.T) {
		t.Chdir(writeDir(t, cleanFixture))
		var stdout, stderr bytes.Buffer
		if exit := run(nil, &stdout, &stderr); exit != exitClean {
			t.Fatalf("exit = %d, want %d\nstdout:\n%s\nstderr:\n%s", exit, exitClean, stdout.String(), stderr.String())
		}
		if stdout.Len() != 0 {
			t.Errorf("干净目录不该有输出:\n%s", stdout.String())
		}
	})
	t.Run("有命中目录", func(t *testing.T) {
		t.Chdir(writeDir(t, hostFixture))
		var stdout, stderr bytes.Buffer
		if exit := run(nil, &stdout, &stderr); exit != exitFound {
			t.Fatalf("exit = %d, want %d\nstdout:\n%s\nstderr:\n%s", exit, exitFound, stdout.String(), stderr.String())
		}
		if !strings.Contains(stderr.String(), "removed API use(s) found") {
			t.Errorf("stderr 缺少命中摘要:\n%s", stderr.String())
		}
	})
}

// TestRunRejectsPositionalArguments 覆盖"多给了位置参数"：打印提示与用法，退出 2。
func TestRunRejectsPositionalArguments(t *testing.T) {
	dir := writeDir(t, cleanFixture)
	var stdout, stderr bytes.Buffer
	if exit := run([]string{"-C", dir, "host"}, &stdout, &stderr); exit != exitUsage {
		t.Fatalf("exit = %d, want %d\nstderr:\n%s", exit, exitUsage, stderr.String())
	}
	errOut := stderr.String()
	for _, want := range []string{"unexpected arguments", "host", "usage: rigmigrate"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("stderr 缺少 %q:\n%s", want, errOut)
		}
	}
	if stdout.Len() != 0 {
		t.Errorf("用法错误不该写 stdout:\n%s", stdout.String())
	}
}

// TestRunTargetErrors 覆盖"读不懂的 -C 目标"：不存在、不是目录、源码语法错误、
// 以及读不进来的 .go 文件（自身符号链接）。它们都必须退出 2，绝不能当成干净。
func TestRunTargetErrors(t *testing.T) {
	cases := []struct {
		name      string
		prepare   func(t *testing.T) string
		wantInErr string
	}{
		{
			name: "目录不存在",
			prepare: func(t *testing.T) string {
				return filepath.Join(t.TempDir(), "no-such-dir")
			},
			wantInErr: "no-such-dir",
		},
		{
			name: "目标是文件而不是目录",
			prepare: func(t *testing.T) string {
				path := filepath.Join(t.TempDir(), "host.go")
				if err := os.WriteFile(path, []byte("package host\n"), 0o644); err != nil {
					t.Fatalf("write %s: %v", path, err)
				}
				return path
			},
			wantInErr: "not a directory",
		},
		{
			name: "源码语法错误",
			prepare: func(t *testing.T) string {
				return writeDir(t, map[string]string{
					"main.go": "package host\n\nfunc broken( {\n",
				})
			},
			wantInErr: "main.go",
		},
		{
			name: "读不进来的 .go 文件（符号链接自环）",
			prepare: func(t *testing.T) string {
				dir := t.TempDir()
				link := filepath.Join(dir, "loop.go")
				if err := os.Symlink("loop.go", link); err != nil {
					t.Skipf("当前平台造不出符号链接自环: %v", err)
				}
				return dir
			},
			wantInErr: "loop.go",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			target := tc.prepare(t)
			var stdout, stderr bytes.Buffer
			if exit := run([]string{"-C", target}, &stdout, &stderr); exit != exitUsage {
				t.Fatalf("exit = %d, want %d\nstdout:\n%s\nstderr:\n%s", exit, exitUsage, stdout.String(), stderr.String())
			}
			if !strings.Contains(stderr.String(), tc.wantInErr) {
				t.Errorf("stderr 缺少 %q:\n%s", tc.wantInErr, stderr.String())
			}
			if stdout.Len() != 0 {
				t.Errorf("读取/解析错误不该写 stdout:\n%s", stdout.String())
			}
		})
	}
}

// TestRunUnreadableDirectory 覆盖"目录存在但读不进去"（权限位）。
//
// 以 root 运行时 DAC 被绕过（chmod 000 的目录照样能遍历），本进程里造不出这个
// 失败；这里显式跳过而不是假装通过。同一条错误出口（Scan 返回错误 → 退出 2）
// 已由 TestRunTargetErrors 里的其他目标覆盖，跳过不影响覆盖目标。
func TestRunUnreadableDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root 绕过目录权限位，无法构造不可读目录")
	}
	dir := writeDir(t, cleanFixture)
	if err := os.Chmod(dir, 0o000); err != nil {
		t.Fatalf("chmod %s: %v", dir, err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	var stdout, stderr bytes.Buffer
	if exit := run([]string{"-C", dir}, &stdout, &stderr); exit != exitUsage {
		t.Fatalf("exit = %d, want %d\nstderr:\n%s", exit, exitUsage, stderr.String())
	}
	if !strings.Contains(stderr.String(), "permission denied") {
		t.Errorf("stderr 缺少 permission denied:\n%s", stderr.String())
	}
}

// TestRunSortsFindingsStably 覆盖多文件多命中的排序与可重复性：
// 顺序是 文件 → 行 → 列 → 旧符号，两次运行的输出必须逐字节一致。
func TestRunSortsFindingsStably(t *testing.T) {
	dir := writeDir(t, map[string]string{
		"z_last.go": `package host

import "github.com/zeokeel/zeokeel-rig/sandbox"

func z() { _ = sandbox.WithProtectedPaths("/root/.ssh") }

func z2() { _ = sandbox.WithProtectedPaths("/root/.aws") }
`,
		"a_first.go": `package host

import "github.com/zeokeel/zeokeel-rig/kit"

func a() { _ = kit.HomeSecretPaths() }
`,
	})

	first := runCLI(t, dir, false)
	if first.exit != exitFound {
		t.Fatalf("exit = %d, want %d\nstderr:\n%s", first.exit, exitFound, first.stderr)
	}
	locs := findingLocations(t, first.stdout)
	want := []string{"a_first.go:5:", "z_last.go:5:", "z_last.go:7:"}
	if len(locs) != len(want) {
		t.Fatalf("命中 %d 条，want %d:\n%s", len(locs), len(want), first.stdout)
	}
	for i, prefix := range want {
		if !strings.HasPrefix(locs[i], prefix) {
			t.Errorf("第 %d 条 = %q，want 前缀 %q（排序应为 文件→行→列）", i+1, locs[i], prefix)
		}
	}

	second := runCLI(t, dir, false)
	if second.stdout != first.stdout || second.stderr != first.stderr {
		t.Errorf("两次运行输出不一致:\n第一次 stdout:\n%s\n第二次 stdout:\n%s\n第一次 stderr:\n%s\n第二次 stderr:\n%s",
			first.stdout, second.stdout, first.stderr, second.stderr)
	}

	// -json 下 findings 的顺序必须与人读输出一致。
	jsonRun := runCLI(t, dir, true)
	var report struct {
		Findings []struct {
			File string `json:"file"`
			Line int    `json:"line"`
		} `json:"findings"`
	}
	if err := json.Unmarshal([]byte(jsonRun.stdout), &report); err != nil {
		t.Fatalf("JSON 解析失败: %v\n%s", err, jsonRun.stdout)
	}
	if len(report.Findings) != len(want) {
		t.Fatalf("-json findings = %d 条，want %d", len(report.Findings), len(want))
	}
	for i, finding := range report.Findings {
		if !strings.HasPrefix(locs[i], finding.File+":"+itoa(finding.Line)+":") {
			t.Errorf("-json 第 %d 条 (%s:%d) 与人读顺序不一致（%s）", i+1, finding.File, finding.Line, locs[i])
		}
	}
}

// TestRunJSONRoot 断言 -json 报告里的 root 就是本次 -C 的值，干净时 count=0。
func TestRunJSONRoot(t *testing.T) {
	dir := writeDir(t, cleanFixture)
	result := runCLI(t, dir, true)
	if result.exit != exitClean {
		t.Fatalf("exit = %d, want %d\nstderr:\n%s", result.exit, exitClean, result.stderr)
	}
	var report struct {
		Root  string `json:"root"`
		Count int    `json:"count"`
	}
	if err := json.Unmarshal([]byte(result.stdout), &report); err != nil {
		t.Fatalf("JSON 解析失败: %v\n%s", err, result.stdout)
	}
	if report.Root != dir {
		t.Errorf("root = %q, want %q", report.Root, dir)
	}
	if report.Count != 0 {
		t.Errorf("count = %d, want 0", report.Count)
	}
}

// cliResult 是一次 run 的输出快照。
type cliResult struct {
	exit   int
	stdout string
	stderr string
}

// runCLI 用给定目标跑一次 run，返回输出快照。
func runCLI(t *testing.T, dir string, asJSON bool) cliResult {
	t.Helper()
	args := []string{"-C", dir}
	if asJSON {
		args = append(args, "-json")
	}
	var stdout, stderr bytes.Buffer
	exit := run(args, &stdout, &stderr)
	return cliResult{exit: exit, stdout: stdout.String(), stderr: stderr.String()}
}

// findingLocations 从人读输出里取每行的 "文件:行:列" 前缀。
func findingLocations(t *testing.T, out string) []string {
	t.Helper()
	var locs []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, ":", 4)
		if len(parts) < 3 {
			t.Fatalf("输出行不是 file:line:col 形态: %q", line)
		}
		locs = append(locs, parts[0]+":"+parts[1]+":"+parts[2]+":")
	}
	return locs
}

// itoa 是 strconv.Itoa 的本地小包装，避免为一行格式化再引一个 import。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

// 子进程模式的标记环境变量：父测试把本测试二进制当成 rigmigrate 命令重新执行。
const (
	mainEntryDirEnv  = "RIGMIGRATE_TEST_MAIN_DIR"
	mainEntryJSONEnv = "RIGMIGRATE_TEST_MAIN_JSON"
)

// TestMainEntrypoint 覆盖 main() 本身——它只有一句 os.Exit(run(...))，而 os.Exit
// 会结束测试进程，所以用标准做法：父测试重新执行本测试二进制，子进程在测试函数里
// 换掉 os.Args 后调用 main()。
//
// 子进程必须继承 -test.gocoverdir，否则 main 的执行不会被计入覆盖率；这不是
// 为了凑数：main 的 os.Exit 语义（把 run 的退出码原样交给 shell）只有真的起一个
// 进程才能验证。
func TestMainEntrypoint(t *testing.T) {
	if dir := os.Getenv(mainEntryDirEnv); dir != "" {
		args := []string{"rigmigrate", "-C", dir}
		if os.Getenv(mainEntryJSONEnv) == "1" {
			args = append(args, "-json")
		}
		os.Args = args
		main() // 内部 os.Exit，不会返回
		return
	}

	cases := []struct {
		name     string
		files    map[string]string
		subdir   string
		asJSON   bool
		wantExit int
		wantOut  string
		wantErr  string
	}{
		{
			name:     "干净目录（-json，exit 0）",
			files:    cleanFixture,
			asJSON:   true,
			wantExit: exitClean,
			wantOut:  `"count": 0`,
		},
		{
			name:     "有命中（人读，exit 1）",
			files:    hostFixture,
			wantExit: exitFound,
			wantOut:  "kit.Config.Redactor",
			wantErr:  "removed API use(s) found",
		},
		{
			name:     "目标不存在（exit 2）",
			subdir:   "missing-dir",
			wantExit: exitUsage,
			wantErr:  "missing-dir",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.files != nil {
				dir = writeDir(t, tc.files)
			}
			target := dir
			if tc.subdir != "" {
				target = filepath.Join(dir, tc.subdir)
			}

			args := []string{"-test.run=^TestMainEntrypoint$"}
			for _, arg := range os.Args[1:] {
				if strings.HasPrefix(arg, "-test.gocoverdir=") {
					args = append(args, arg)
				}
			}
			cmd := exec.Command(os.Args[0], args...)
			cmd.Env = append(os.Environ(), mainEntryDirEnv+"="+target)
			if tc.asJSON {
				cmd.Env = append(cmd.Env, mainEntryJSONEnv+"=1")
			}
			var stdout, stderr bytes.Buffer
			cmd.Stdout = &stdout
			cmd.Stderr = &stderr
			err := cmd.Run()

			exit := 0
			if err != nil {
				var exitErr *exec.ExitError
				if !errors.As(err, &exitErr) {
					t.Fatalf("子进程启动失败: %v\nstderr:\n%s", err, stderr.String())
				}
				exit = exitErr.ExitCode()
			}
			if exit != tc.wantExit {
				t.Fatalf("exit = %d, want %d\nstdout:\n%s\nstderr:\n%s", exit, tc.wantExit, stdout.String(), stderr.String())
			}
			if tc.wantOut != "" && !strings.Contains(stdout.String(), tc.wantOut) {
				t.Errorf("stdout 缺少 %q:\n%s", tc.wantOut, stdout.String())
			}
			if tc.wantErr != "" && !strings.Contains(stderr.String(), tc.wantErr) {
				t.Errorf("stderr 缺少 %q:\n%s", tc.wantErr, stderr.String())
			}
		})
	}
}
