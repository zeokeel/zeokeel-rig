package main

// rigmigrate CLI 的表驱动测试：退出码、单条命中、-json。
//
// 造出来的"宿主目录"只用于解析、不参与编译——宿主升级时的真实状态就是
// "编译器报错、代码还不能 build"，扫描器必须能在这种状态下工作。

import (
	"bytes"
	"encoding/json"
	"os"
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

// TestRunCleanJSONIsEmptyList 断言干净目录在 -json 下也是合法 JSON。
func TestRunCleanJSONIsEmptyList(t *testing.T) {
	dir := writeDir(t, cleanFixture)
	var stdout, stderr bytes.Buffer
	if exit := run([]string{"-C", dir, "-json"}, &stdout, &stderr); exit != 0 {
		t.Fatalf("exit = %d, want 0\nstderr:\n%s", exit, stderr.String())
	}
	var report struct {
		Findings []json.RawMessage `json:"findings"`
		Count    int               `json:"count"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("JSON 解析失败: %v\n%s", err, stdout.String())
	}
	if report.Count != 0 || len(report.Findings) != 0 {
		t.Errorf("count = %d / len = %d, want 0", report.Count, len(report.Findings))
	}
}
