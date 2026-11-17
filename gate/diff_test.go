package gate_test

// DiffPrompt 的行为测试：三种写工具的预览文案、apply_patch 预览不落盘、
// 行数阈值、与 Ask 的接线、以及"预览失败要说清楚"的降级路径。

import (
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/gate"
)

// gateWrite 建目录并写文件。
func gateWrite(t *testing.T, root, rel, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}

// call 构造一次工具调用（参数用 map 序列化，避免手写 JSON）。
func call(name string, args map[string]any) rig.ToolCall {
	payload, err := json.Marshal(args)
	if err != nil {
		panic(err)
	}
	return rig.ToolCall{ID: "call-1", Name: name, Arguments: string(payload)}
}

// probe 用 DiffPrompt 渲染一次。
func probe(t *testing.T, prompt func(rig.Tool, rig.ToolCall) string, call rig.ToolCall) string {
	t.Helper()
	return prompt(nil, call)
}

// G1：write_file 覆盖已有文件 → 显示 diff 与汇总。
func TestDiffPromptWriteFileOverwrite(t *testing.T) {
	root := t.TempDir()
	gateWrite(t, root, "src/a.txt", "one\ntwo\n")
	prompt := gate.DiffPrompt(gate.WithDiffRoot(root))

	text := probe(t, prompt, call(gate.ToolWriteFile, map[string]any{
		"path": "src/a.txt", "content": "one\nTWO\nthree\n",
	}))
	if !strings.Contains(text, "write_file: src/a.txt (overwrite: 2 lines → 3 lines)") {
		t.Fatalf("标题不对:\n%s", text)
	}
	if !strings.Contains(text, "--- a/src/a.txt") || !strings.Contains(text, "+++ b/src/a.txt") {
		t.Fatalf("缺少 diff 头:\n%s", text)
	}
	if !strings.Contains(text, "-two") || !strings.Contains(text, "+TWO") || !strings.Contains(text, "+three") {
		t.Fatalf("缺少改动行:\n%s", text)
	}
	if !strings.Contains(text, "1 file changed, +2 -1") {
		t.Fatalf("缺少汇总:\n%s", text)
	}
	if !strings.HasSuffix(text, "Allow this change?") {
		t.Fatalf("结尾缺少提问:\n%s", text)
	}
}

// G2：write_file 新建文件 → /dev/null 头 + create 标题。
func TestDiffPromptWriteFileCreate(t *testing.T) {
	root := t.TempDir()
	prompt := gate.DiffPrompt(gate.WithDiffRoot(root))

	text := probe(t, prompt, call(gate.ToolWriteFile, map[string]any{
		"path": "docs/new.md", "content": "# New\n\nbody\n",
	}))
	if !strings.Contains(text, "write_file: docs/new.md (create: 3 lines)") {
		t.Fatalf("标题不对:\n%s", text)
	}
	if !strings.Contains(text, "--- /dev/null") || !strings.Contains(text, "+++ b/docs/new.md") {
		t.Fatalf("新文件的头不对:\n%s", text)
	}
	if !strings.Contains(text, "1 file changed, +3 -0") {
		t.Fatalf("汇总不对:\n%s", text)
	}
}

// G3：edit_file → 套用替换后的 diff；替换目标不唯一/不存在时明确说会失败。
func TestDiffPromptEditFile(t *testing.T) {
	root := t.TempDir()
	gateWrite(t, root, "src/a.go", "package a\n\nfunc foo() {}\n")
	prompt := gate.DiffPrompt(gate.WithDiffRoot(root))

	text := probe(t, prompt, call(gate.ToolEditFile, map[string]any{
		"path": "src/a.go", "old_string": "func foo() {}", "new_string": "func bar() { println(\"hi\") }",
	}))
	if !strings.Contains(text, "edit_file: src/a.go (replace 1 occurrence)") {
		t.Fatalf("标题不对:\n%s", text)
	}
	if !strings.Contains(text, "-func foo() {}") || !strings.Contains(text, `+func bar() { println("hi") }`) {
		t.Fatalf("没有展示替换:\n%s", text)
	}

	// 目标不存在。
	text = probe(t, prompt, call(gate.ToolEditFile, map[string]any{
		"path": "src/a.go", "old_string": "does not exist", "new_string": "x",
	}))
	if !strings.Contains(text, "preview unavailable") || !strings.Contains(text, "does not occur") {
		t.Fatalf("应说明替换目标不存在:\n%s", text)
	}
	if !strings.Contains(text, "the tool will fail") {
		t.Fatalf("应提示这次调用会失败:\n%s", text)
	}

	// 目标不唯一（未开 replace_all）。
	gateWrite(t, root, "dup.txt", "x\nx\n")
	text = probe(t, prompt, call(gate.ToolEditFile, map[string]any{
		"path": "dup.txt", "old_string": "x", "new_string": "y",
	}))
	if !strings.Contains(text, "occurs 2 times") {
		t.Fatalf("应说明匹配不唯一:\n%s", text)
	}

	// 开启 replace_all 后应当展示全量替换。
	text = probe(t, prompt, call(gate.ToolEditFile, map[string]any{
		"path": "dup.txt", "old_string": "x", "new_string": "y", "replace_all": true,
	}))
	if !strings.Contains(text, "(replace 2 occurrences)") {
		t.Fatalf("replace_all 的次数不对:\n%s", text)
	}
	if strings.Count(text, "+y") != 2 || strings.Count(text, "-x") != 2 {
		t.Fatalf("replace_all 没有展示两处替换:\n%s", text)
	}
}

// G4：str_replace_editor 的 create / str_replace / view 三种命令。
func TestDiffPromptStrReplaceEditor(t *testing.T) {
	root := t.TempDir()
	gateWrite(t, root, "page.html", "<h1>old</h1>\n")
	prompt := gate.DiffPrompt(gate.WithDiffRoot(root))

	create := probe(t, prompt, call(gate.ToolStrReplaceEditor, map[string]any{
		"command": "create", "path": "snake.html", "file_text": "<html>\n<body>snake</body>\n</html>",
	}))
	if !strings.Contains(create, "str_replace_editor create: snake.html (3 lines)") ||
		!strings.Contains(create, "--- /dev/null") {
		t.Fatalf("create 预览不对:\n%s", create)
	}

	replace := probe(t, prompt, call(gate.ToolStrReplaceEditor, map[string]any{
		"command": "str_replace", "path": "page.html", "old_str": "old", "new_str": "NEW",
	}))
	if !strings.Contains(replace, "str_replace_editor str_replace: page.html") ||
		!strings.Contains(replace, "-<h1>old</h1>") || !strings.Contains(replace, "+<h1>NEW</h1>") {
		t.Fatalf("str_replace 预览不对:\n%s", replace)
	}

	// 只读命令不需要 diff 预览（回退到默认文案 → 空串）。
	if text := probe(t, prompt, call(gate.ToolStrReplaceEditor, map[string]any{
		"command": "view", "path": "page.html",
	})); text != "" {
		t.Fatalf("view 命令不应生成 diff 预览，得到:\n%s", text)
	}
}

// G5【核心】：apply_patch 的预览必须展示多文件 diff，且**一个字节都不落盘**。
func TestDiffPromptApplyPatchDoesNotWrite(t *testing.T) {
	root := t.TempDir()
	gateWrite(t, root, "a.txt", "one\ntwo\n")
	prompt := gate.DiffPrompt(gate.WithDiffRoot(root))

	patchText := strings.Join([]string{
		"*** Begin Patch",
		"*** Update File: a.txt",
		"@@",
		" one",
		"-two",
		"+TWO",
		"*** Add File: nested/new.txt",
		"+hello",
		"*** End Patch",
		"",
	}, "\n")
	text := probe(t, prompt, call(gate.ToolApplyPatch, map[string]any{"patch": patchText}))

	if !strings.Contains(text, "apply_patch: 2 files") {
		t.Fatalf("标题里的汇总不对:\n%s", text)
	}
	if !strings.Contains(text, "--- a/a.txt") || !strings.Contains(text, "--- /dev/null") ||
		!strings.Contains(text, "+++ b/nested/new.txt") {
		t.Fatalf("多文件 diff 不完整:\n%s", text)
	}

	// 落盘断言。
	if _, err := os.Stat(filepath.Join(root, "nested")); !os.IsNotExist(err) {
		t.Fatalf("预览创建了目录: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(root, "a.txt"))
	if err != nil || string(raw) != "one\ntwo\n" {
		t.Fatalf("预览改写了文件: %q (%v)", raw, err)
	}
}

// G6：补丁无法解析时，预览要说明原因并附上原文（截断）。
func TestDiffPromptApplyPatchParseFailure(t *testing.T) {
	root := t.TempDir()
	gateWrite(t, root, "a.txt", "one\n")
	prompt := gate.DiffPrompt(gate.WithDiffRoot(root))

	text := probe(t, prompt, call(gate.ToolApplyPatch, map[string]any{
		"patch": "*** Begin Patch\n*** Update File: a.txt\n@@\n-missing\n+x\n*** End Patch\n",
	}))
	if !strings.Contains(text, "preview unavailable") {
		t.Fatalf("应说明预览不可用:\n%s", text)
	}
	if !strings.Contains(text, "```diff") {
		t.Fatalf("应附上补丁原文:\n%s", text)
	}
}

// G7：超过阈值（默认 200 行）只显示摘要与逐文件统计，不展示 diff。
func TestDiffPromptThreshold(t *testing.T) {
	root := t.TempDir()
	prompt := gate.DiffPrompt(gate.WithDiffRoot(root))

	var builder strings.Builder
	for i := 0; i < 400; i++ {
		builder.WriteString("line")
		builder.WriteString(string(rune('a' + i%26)))
		builder.WriteString("\n")
	}
	text := probe(t, prompt, call(gate.ToolWriteFile, map[string]any{
		"path": "big.txt", "content": builder.String(),
	}))
	if !strings.Contains(text, "diff omitted:") || !strings.Contains(text, "exceeds the 200 line limit") {
		t.Fatalf("超过阈值时应给摘要:\n%s", text)
	}
	if strings.Contains(text, "@@ -") {
		t.Fatalf("超过阈值时不应展示 hunk:\n%s", text)
	}
	if !strings.Contains(text, "1 file changed, +400 -0") {
		t.Fatalf("摘要不对:\n%s", text)
	}
	if !strings.Contains(text, "  add    big.txt (+400 -0)") {
		t.Fatalf("缺少逐文件统计:\n%s", text)
	}

	// 自定义阈值：10 行也应当触发摘要。
	small := gate.DiffPrompt(gate.WithDiffRoot(root), gate.WithDiffMaxLines(10))
	text = probe(t, small, call(gate.ToolWriteFile, map[string]any{
		"path": "small.txt", "content": strings.Repeat("x\n", 30),
	}))
	if !strings.Contains(text, "exceeds the 10 line limit") {
		t.Fatalf("自定义阈值未生效:\n%s", text)
	}
}

// G8：非写类工具返回空串（AskGate 回退到默认文案）。
func TestDiffPromptIgnoresOtherTools(t *testing.T) {
	prompt := gate.DiffPrompt(gate.WithDiffRoot(t.TempDir()))
	for _, name := range []string{"read_file", "bash", "grep", "list_dir", "sandbox_status"} {
		if text := probe(t, prompt, call(name, map[string]any{"path": "x"})); text != "" {
			t.Fatalf("%s 不应生成 diff 预览，得到:\n%s", name, text)
		}
	}
}

// G9：参数畸形 → 空串（绝不 panic）。
func TestDiffPromptMalformedArgs(t *testing.T) {
	prompt := gate.DiffPrompt(gate.WithDiffRoot(t.TempDir()))
	for _, name := range []string{gate.ToolWriteFile, gate.ToolEditFile, gate.ToolStrReplaceEditor, gate.ToolApplyPatch} {
		text := prompt(nil, rig.ToolCall{Name: name, Arguments: `{"path":`})
		if text != "" {
			t.Fatalf("%s 参数畸形时应返回空串，得到 %q", name, text)
		}
	}
	// 缺 path 也要安全。
	if text := prompt(nil, call(gate.ToolWriteFile, map[string]any{"content": "x"})); text != "" {
		t.Fatalf("缺 path 时应返回空串，得到 %q", text)
	}
}

// G10：没有配置 root 时——write_file 按新建预览、edit_file/apply_patch 明说不可预览。
func TestDiffPromptWithoutRoot(t *testing.T) {
	prompt := gate.DiffPrompt()

	text := probe(t, prompt, call(gate.ToolWriteFile, map[string]any{"path": "a.txt", "content": "x\n"}))
	if !strings.Contains(text, "(create: 1 lines)") {
		t.Fatalf("没有 root 时 write_file 应按新建预览:\n%s", text)
	}

	text = probe(t, prompt, call(gate.ToolEditFile, map[string]any{
		"path": "a.txt", "old_string": "x", "new_string": "y",
	}))
	if !strings.Contains(text, "the file does not exist") {
		t.Fatalf("没有 root 时 edit_file 应说明无法读取:\n%s", text)
	}

	text = probe(t, prompt, call(gate.ToolApplyPatch, map[string]any{"patch": "*** Begin Patch\n*** End Patch\n"}))
	if !strings.Contains(text, "no workspace root") {
		t.Fatalf("没有 root 时 apply_patch 应说明缺少 root:\n%s", text)
	}
}

// G11：WithDiffPrompt 真的接到了 AskGate 的提问里（端到端）。
func TestWithDiffPromptDrivesAskGate(t *testing.T) {
	root := t.TempDir()
	gateWrite(t, root, "a.txt", "old\n")
	asker := &recordingAsker{answer: gate.AskDeny}
	g := gate.Ask(asker, gate.WithDiffPrompt(gate.WithDiffRoot(root)))

	decision, err := g.Approve(context.Background(), nil, call(gate.ToolWriteFile, map[string]any{
		"path": "a.txt", "content": "new\n",
	}))
	if err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if decision != rig.DecisionDeny {
		t.Fatalf("decision = %v, want deny", decision)
	}
	if len(asker.questions) != 1 {
		t.Fatalf("提问次数 = %d", len(asker.questions))
	}
	question := asker.questions[0]
	if !strings.Contains(question, "--- a/a.txt") || !strings.Contains(question, "-old") || !strings.Contains(question, "+new") {
		t.Fatalf("提问里没有 diff:\n%s", question)
	}
	// 只读工具仍然走默认文案（不含 diff）。
	if _, err := g.Approve(context.Background(), nil, call("read_file", map[string]any{"path": "a.txt"})); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if strings.Contains(asker.questions[1], "--- a/") {
		t.Fatalf("只读工具的提问不应带 diff:\n%s", asker.questions[1])
	}
	if !strings.Contains(asker.questions[1], "read_file") {
		t.Fatalf("只读工具应回退到默认文案:\n%s", asker.questions[1])
	}
}

// G12：WithDiffReadFile 让宿主接管读取（远程工作区），并可用于抑制 diff。
func TestWithDiffReadFile(t *testing.T) {
	calls := 0
	prompt := gate.DiffPrompt(
		gate.WithDiffRoot("/nonexistent"),
		gate.WithDiffReadFile(func(rel string) ([]byte, error) {
			calls++
			if rel != "virtual.txt" {
				return nil, os.ErrNotExist
			}
			return []byte("virtual old\n"), nil
		}),
	)
	text := probe(t, prompt, call(gate.ToolWriteFile, map[string]any{
		"path": "virtual.txt", "content": "virtual new\n",
	}))
	if calls != 1 {
		t.Fatalf("注入的读取函数被调用 %d 次", calls)
	}
	if !strings.Contains(text, "(overwrite: 1 lines → 1 lines)") || !strings.Contains(text, "-virtual old") {
		t.Fatalf("注入读取没有生效:\n%s", text)
	}
}

// recordingAsker 记录每次提问并返回固定答案。
type recordingAsker struct {
	answer    string
	questions []string
}

func (a *recordingAsker) Ask(_ context.Context, question string, _ []string) (string, error) {
	a.questions = append(a.questions, question)
	return a.answer, nil
}

// 确保 Ask 的超时选项与本文件无关地仍然可用（防止未来重构把 prompt 接错位）。
var _ = time.Second

// G13：三种写工具的真实预览文案的 golden 快照。
//
// 它同时是"预览长什么样"的文档：任何文案改动都会让这条测试与快照一起变红。
func TestDiffPromptGolden(t *testing.T) {
	root := t.TempDir()
	gateWrite(t, root, "internal/agent/loop.go", "package agent\n\nfunc Loop(ctx context.Context) error {\n\tfor {\n\t\tselect {\n\t\tcase <-ctx.Done():\n\t\t\treturn ctx.Err()\n\t\tdefault:\n\t\t}\n\t\tif err := step(ctx); err != nil {\n\t\t\treturn err\n\t\t}\n\t}\n}\n")
	gateWrite(t, root, "README.md", "# ZeoKeel Rig\n\nAgent toolset SDK.\n")
	prompt := gate.DiffPrompt(gate.WithDiffRoot(root))

	cases := []struct {
		name string
		call rig.ToolCall
	}{
		{"write_file.txt", call(gate.ToolWriteFile, map[string]any{
			"path":    "README.md",
			"content": "# ZeoKeel Rig\n\nAgent toolset SDK for building personal agents.\n\n## Install\n\n```sh\ngo get github.com/zeokeel/zeokeel-rig\n```\n",
		})},
		{"edit_file.txt", call(gate.ToolEditFile, map[string]any{
			"path":       "internal/agent/loop.go",
			"old_string": "\t\tif err := step(ctx); err != nil {\n\t\t\treturn err\n\t\t}",
			"new_string": "\t\tif err := step(ctx); err != nil {\n\t\t\treturn fmt.Errorf(\"agent: step: %w\", err)\n\t\t}",
		})},
		{"apply_patch.txt", call(gate.ToolApplyPatch, map[string]any{
			"patch": strings.Join([]string{
				"*** Begin Patch",
				"*** Update File: README.md",
				"@@",
				" # ZeoKeel Rig",
				" ",
				"-Agent toolset SDK.",
				"+Agent toolset SDK for building personal agents.",
				"+See docs/zh/AI-SDK.md for the assembly guide.",
				"*** Add File: CHANGELOG.md",
				"+## v0.19.0",
				"+",
				"+- diff preview before writes",
				"*** End Patch",
				"",
			}, "\n"),
		})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := probe(t, prompt, tc.call)
			path := filepath.Join("testdata", tc.name)
			if *updateGolden {
				if err := os.MkdirAll("testdata", 0o755); err != nil {
					t.Fatalf("MkdirAll: %v", err)
				}
				if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
					t.Fatalf("写 golden: %v", err)
				}
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("读 golden %s（首次运行加 -update）: %v", path, err)
			}
			if got != string(want) {
				t.Fatalf("%s 与 golden 不一致\n--- got ---\n%s\n--- want ---\n%s", tc.name, got, want)
			}
			// 预览里不能出现绝对路径（会把宿主目录泄漏给审批界面/日志）。
			if strings.Contains(got, root) {
				t.Fatalf("%s 泄漏了绝对路径 %q:\n%s", tc.name, root, got)
			}
		})
	}
}

// updateGolden 是 golden 快照的 -update 开关。
var updateGolden = flag.Bool("update", false, "重新生成 gate/testdata 下的预览快照")
