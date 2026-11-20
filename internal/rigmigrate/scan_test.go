package rigmigrate_test

// 扫描器的行为测试：符号表形态、误报边界、import 别名。
//
// 用例刻意把"宿主真实会写成什么样"当输入：宿主升级时看到的是编译错误，
// 扫描器的职责是把这些错误一次列全，不多报也不要漏报。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig/internal/rigmigrate"
)

// writeHost 造一个最小宿主目录（只有 .go 文件，扫描器不需要 go.mod）。
func writeHost(t *testing.T, files map[string]string) string {
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

// TestTableEntriesAreWellFormed 钉住符号表本身的形状：扫描器与文档守护都按
// "至少两段、最后一段是成员名"解析 Old，写坏了会静默漏报。
func TestTableEntriesAreWellFormed(t *testing.T) {
	entries := rigmigrate.RemovedSymbols()
	if len(entries) < 10 {
		t.Fatalf("符号表条目 = %d, want >= 10（破坏性变更清单不该这么短）", len(entries))
	}
	seen := make(map[string]bool, len(entries))
	for i, entry := range entries {
		if entry.Old == "" || entry.New == "" || entry.Note == "" {
			t.Errorf("条目 %d (%q) 有空字段: %+v", i, entry.Old, entry)
		}
		if seen[entry.Old] {
			t.Errorf("条目重复: %s", entry.Old)
		}
		seen[entry.Old] = true
		parts := strings.Split(entry.Old, ".")
		if len(parts) < 2 || len(parts) > 3 {
			t.Errorf("条目 %q 的段数 = %d, want 2 或 3（pkg.Symbol / pkg.Type.Member）", entry.Old, len(parts))
		}
		for _, part := range parts {
			if part == "" {
				t.Errorf("条目 %q 有空段", entry.Old)
			}
		}
		if entry.Args < 0 {
			t.Errorf("条目 %q 的 Args = %d, want >= 0（0 表示不做参数个数匹配）", entry.Old, entry.Args)
		}
	}
	for _, removed := range rigmigrate.RemovedImports() {
		if removed.Path == "" || removed.Note == "" {
			t.Errorf("已删除包的条目有空字段: %+v", removed)
		}
	}
}

// TestScanDoesNotFlagRetainedSymbols 是误报边界：这些写法**现在仍然有效**，
// 扫描器必须一条都不报。
func TestScanDoesNotFlagRetainedSymbols(t *testing.T) {
	root := writeHost(t, map[string]string{
		"policy.go": `package host

import "github.com/zeokeel/zeokeel-rig/sandbox"

// 隐藏路径是保护路径的替代品，必须仍然有效。
func build() *sandbox.Policy {
	policy := sandbox.NewPolicy(sandbox.WithHidden("**/.env", "secrets/**"))
	_ = policy.Hidden()
	_ = policy.HiddenPatterns()
	_ = policy.DeniedPath("/tmp/x")
	_ = sandbox.GlobMatch("**/.env", ".env")
	_ = sandbox.WorkspaceRoot
	// 普通路径字符串里的 secrets 不是包引用。
	path := "secrets/creds.md"
	_ = path
	return policy
}
`,
	})
	findings, err := rigmigrate.Scan(root)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("仍然有效的写法被误报 %d 条:\n%s", len(findings), describe(findings))
	}
}

// TestScanResolvesImportAliases 断言别名 import 也能命中：宿主常写 `sb "…/sandbox"`。
func TestScanResolvesImportAliases(t *testing.T) {
	root := writeHost(t, map[string]string{
		"aliased.go": `package host

import (
	sb "github.com/zeokeel/zeokeel-rig/sandbox"
)

func build() {
	_ = sb.WithProtectedPaths("/root/.ssh")
}
`,
	})
	findings, err := rigmigrate.Scan(root)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("别名 import 命中 %d 条, want 1:\n%s", len(findings), describe(findings))
	}
	if findings[0].Old != "sandbox.WithProtectedPaths" {
		t.Errorf("Old = %q, want sandbox.WithProtectedPaths", findings[0].Old)
	}
	if findings[0].Line != 8 {
		t.Errorf("Line = %d, want 8", findings[0].Line)
	}
}

// TestScanIgnoresHostPackagesWithSameBaseName 钉住 import 的模块前缀判定：
// 包基名相同不等于同一个包。宿主自己的 `github.com/host/internal/kit`、
// `github.com/host/secrets` 与 rig 的 `kit`/`secrets` 同名，但一条都不该报——
// 否则"已经迁移干净的宿主"会收到一批假红，还要去改本来正确的代码。
func TestScanIgnoresHostPackagesWithSameBaseName(t *testing.T) {
	root := writeHost(t, map[string]string{
		"host.go": `package host

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
	})
	findings, err := rigmigrate.Scan(root)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("宿主自有的同名包被误报 %d 条:\n%s", len(findings), describe(findings))
	}
}

// TestScanMapsOnlyRigImports 断言映射只对真在本仓模块前缀下的 import 生效：
// 同一个文件里别名引入宿主的 kit 与 rig 的 kit 时，只有后者算命中。
func TestScanMapsOnlyRigImports(t *testing.T) {
	root := writeHost(t, map[string]string{
		"mixed.go": `package host

import (
	hk "github.com/host/internal/kit"
	kit "github.com/zeokeel/zeokeel-rig/kit"
)

func build() {
	_ = hk.HomeSecretPaths()
	_ = hk.Config{Redactor: nil}
	_ = kit.HomeSecretPaths()
}
`,
	})
	findings, err := rigmigrate.Scan(root)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("命中 %d 条, want 1（只有 rig 的 kit 算）:\n%s", len(findings), describe(findings))
	}
	if findings[0].Old != "kit.HomeSecretPaths" {
		t.Errorf("Old = %q, want kit.HomeSecretPaths", findings[0].Old)
	}
	if findings[0].Line != 11 {
		t.Errorf("Line = %d, want 11", findings[0].Line)
	}
}

// TestScanNarrowsThreePartEntriesToRigTypedReceivers 钉住三段条目的匹配口径：
// `pkg.Type.Member` 只报"接收者被声明成这个 rig 包限定类型"的选择器，不再按
// 成员名宽匹配——宿主自己的 `s.ProtectedPaths` / `m.Redactor` 不是 rig 的字段。
func TestScanNarrowsThreePartEntriesToRigTypedReceivers(t *testing.T) {
	t.Run("宿主本地同名成员不报", func(t *testing.T) {
		root := writeHost(t, map[string]string{
			"local.go": `package host

import "github.com/zeokeel/zeokeel-rig/sandbox"

type localPolicy struct {
	ProtectedPaths []string
	Redactor       any
}

func build(s *localPolicy, m any, r any) {
	_ = s.ProtectedPaths
	_ = m.Redactor
	_ = r.Redactor
	_ = sandbox.GlobMatch("**/.env", ".env")
}
`,
		})
		findings, err := rigmigrate.Scan(root)
		if err != nil {
			t.Fatalf("Scan: %v", err)
		}
		if len(findings) != 0 {
			t.Fatalf("宿主本地同名成员被误报 %d 条:\n%s", len(findings), describe(findings))
		}
	})

	t.Run("声明成 rig 类型的选择器仍然命中", func(t *testing.T) {
		root := writeHost(t, map[string]string{
			"typed.go": `package host

import (
	"github.com/zeokeel/zeokeel-rig/kit"
	"github.com/zeokeel/zeokeel-rig/sandbox"
)

func build(cfg kit.Config) {
	_ = cfg.Redactor
	var p sandbox.Policy
	_ = p.WriteDenyPaths
	q := &sandbox.Policy{}
	_ = q.ProtectedPaths
}
`,
		})
		findings, err := rigmigrate.Scan(root)
		if err != nil {
			t.Fatalf("Scan: %v", err)
		}
		want := map[string]bool{
			"kit.Config.Redactor":           false,
			"sandbox.Policy.WriteDenyPaths": false,
			"sandbox.Policy.ProtectedPaths": false,
		}
		for _, finding := range findings {
			if _, ok := want[finding.Old]; !ok {
				t.Errorf("意外的命中: %s", finding.String())
				continue
			}
			want[finding.Old] = true
		}
		for old, seen := range want {
			if !seen {
				t.Errorf("漏报 %s:\n%s", old, describe(findings))
			}
		}
		if len(findings) != 3 {
			t.Errorf("命中 %d 条, want 3:\n%s", len(findings), describe(findings))
		}
	})

	t.Run("同名参数在不同函数里不互相污染", func(t *testing.T) {
		root := writeHost(t, map[string]string{
			"shadow.go": `package host

import "github.com/zeokeel/zeokeel-rig/kit"

type config struct {
	Redactor any
}

func rigged(cfg kit.Config) {
	_ = cfg.Redactor
}

func local(cfg *config) {
	_ = cfg.Redactor
}
`,
		})
		findings, err := rigmigrate.Scan(root)
		if err != nil {
			t.Fatalf("Scan: %v", err)
		}
		if len(findings) != 1 {
			t.Fatalf("命中 %d 条, want 1（只有 rigged 里的 cfg 是 kit.Config）:\n%s", len(findings), describe(findings))
		}
		if findings[0].Line != 10 {
			t.Errorf("Line = %d, want 10（rigged 里的 cfg.Redactor）:\n%s", findings[0].Line, describe(findings))
		}
	})
}

// TestScanSkipsVendoredAndUnderscoreDirs 断言第三方副本与样例目录不参与扫描：
// 宿主的 vendor/ 里可能有旧版 SDK，报它只会制造噪音。
func TestScanSkipsVendoredAndUnderscoreDirs(t *testing.T) {
	root := writeHost(t, map[string]string{
		"vendor/old/old.go": `package old

import "github.com/zeokeel/zeokeel-rig/kit"

var cfg = kit.Config{Redactor: nil}
`,
		"_sample/broken.go": `package sample

import "github.com/zeokeel/zeokeel-rig/kit"

var cfg = kit.Config{Redactor: nil}
`,
		"ok.go": "package host\n",
	})
	findings, err := rigmigrate.Scan(root)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("vendor/_sample 里的旧写法被报 %d 条:\n%s", len(findings), describe(findings))
	}
}

// TestScanReportsParseError 断言语法错误不会被当成"干净"：宿主目录读不懂时
// 调用方必须看到错误（CLI 把它折成退出码 2）。
func TestScanReportsParseError(t *testing.T) {
	root := writeHost(t, map[string]string{
		"broken.go": "package host\n\nfunc {\n",
	})
	if _, err := rigmigrate.Scan(root); err == nil {
		t.Fatal("语法错误没有返回 error")
	}
}

// describe 把命中列表渲染成可读文本，用于失败信息。
func describe(findings []rigmigrate.Finding) string {
	var b strings.Builder
	for _, finding := range findings {
		b.WriteString(finding.String())
		b.WriteByte('\n')
	}
	return b.String()
}
