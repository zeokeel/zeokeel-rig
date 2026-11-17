package patch_test

// dry_run 结果里的 diff 预览：三份 golden（单文件 / 多文件 / 超阈值）+ 非 dry_run
// 路径的逐字节不变性钉住。

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/tools/patch"
)

// updateDryRunGolden 是 golden 的 -update 开关。
var updateDryRunGolden = flag.Bool("update", false, "重新生成 tools/patch/testdata 下的 golden")

// goldenPatch 比较输出与 testdata 下的快照。
func goldenPatch(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *updateDryRunGolden {
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
		t.Fatalf("%s 与 golden 不一致\n--- got ---\n%s\n--- want ---\n%s", name, got, want)
	}
}

// R1：单文件小改 → 汇总之后紧跟 diff。
func TestDryRunSingleFilePreview(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.txt", "one\ntwo\nthree\n")
	patchText := "*** Begin Patch\n*** Update File: a.txt\n@@\n one\n-two\n+TWO\n*** End Patch\n"

	content, isError := invokeApplyPatch(t, root, patchText)
	if isError {
		t.Fatalf("dry_run 报错: %s", content)
	}
	goldenPatch(t, "dryrun_single.txt", content)

	if !strings.Contains(content, "[dry-run] diff preview:") {
		t.Fatalf("缺少预览段:\n%s", content)
	}
	if !strings.Contains(content, "@@ -1,3 +1,3 @@") || !strings.Contains(content, "-two") || !strings.Contains(content, "+TWO") {
		t.Fatalf("diff 内容不对:\n%s", content)
	}
	// 预览必须在收尾行之前，收尾行仍是最后一行。
	previewAt := strings.Index(content, "[dry-run] diff preview:")
	tailAt := strings.Index(content, "[dry-run] no files were modified")
	if previewAt < 0 || tailAt < 0 || previewAt > tailAt {
		t.Fatalf("预览位置不对（应插在收尾行之前）:\n%s", content)
	}
	if !strings.HasSuffix(content, "[dry-run] no files were modified\n") {
		t.Fatalf("收尾行不再是最后一行:\n%q", content[maxInt(0, len(content)-120):])
	}
	// dry_run 仍然不落盘。
	raw, err := os.ReadFile(filepath.Join(root, "a.txt"))
	if err != nil || string(raw) != "one\ntwo\nthree\n" {
		t.Fatalf("dry_run 改动了文件: %q (%v)", raw, err)
	}
}

// R2：多文件（update + add + delete）→ 每个文件的 diff 都在。
func TestDryRunMultiFilePreview(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "README.md", "# Title\n\nbody\n")
	writeFile(t, root, "old.txt", "gone\n")
	patchText := strings.Join([]string{
		"*** Begin Patch",
		"*** Update File: README.md",
		"@@",
		" # Title",
		" ",
		"-body",
		"+body with more text.",
		"*** Add File: docs/new.md",
		"+# New",
		"+",
		"+text",
		"*** Delete File: old.txt",
		"*** End Patch",
		"",
	}, "\n")

	content, isError := invokeApplyPatch(t, root, patchText)
	if isError {
		t.Fatalf("dry_run 报错: %s", content)
	}
	goldenPatch(t, "dryrun_multi.txt", content)

	for _, want := range []string{
		"--- a/README.md", "+++ b/README.md",
		"--- /dev/null", "+++ b/docs/new.md",
		"--- a/old.txt", "+++ /dev/null",
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("缺少 %q:\n%s", want, content)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "docs")); !os.IsNotExist(err) {
		t.Fatalf("dry_run 创建了目录: %v", err)
	}
}

// R3：diff 超过 200 行 → 只给逐文件统计（不展示 diff 正文）。
func TestDryRunPreviewOverThreshold(t *testing.T) {
	root := t.TempDir()
	var builder strings.Builder
	for i := 0; i < 400; i++ {
		builder.WriteString("line")
		builder.WriteString(string(rune('a' + i%26)))
		builder.WriteString("\n")
	}
	patchText := "*** Begin Patch\n*** Add File: big.txt\n" + prefixLines("+", builder.String()) + "*** End Patch\n"

	content, isError := invokeApplyPatch(t, root, patchText)
	if isError {
		t.Fatalf("dry_run 报错: %s", content)
	}
	goldenPatch(t, "dryrun_truncated.txt", content)

	if !strings.Contains(content, "[dry-run] diff preview omitted: ") {
		t.Fatalf("超阈值时应说明省略:\n%s", content)
	}
	if !strings.Contains(content, "exceeds the 200 line limit") {
		t.Fatalf("阈值说明不对:\n%s", content)
	}
	if strings.Contains(content, "@@ -") {
		t.Fatalf("超阈值时不应展示 hunk:\n%s", content)
	}
	if !strings.Contains(content, "  add    big.txt (+400 -0)") {
		t.Fatalf("缺少逐文件统计:\n%s", content)
	}
}

// R4【回归钉住】：非 dry_run 的结果文本逐字节不变（不含任何预览）。
func TestNonDryRunOutputIsUnchanged(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.txt", "one\ntwo\n")
	patchText := "*** Begin Patch\n*** Update File: a.txt\n@@\n one\n-two\n+TWO\n*** Add File: b.txt\n+hi\n*** End Patch\n"

	payload, err := json.Marshal(map[string]any{"patch": patchText})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	tool := patch.New(root)
	result, err := tool.Invoke(context.Background(), rig.ToolCall{
		ID: "c1", Name: patch.ToolName, Arguments: string(payload),
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if result.IsError {
		t.Fatalf("Invoke 报错: %s", result.Content)
	}
	goldenPatch(t, "no_dryrun.txt", result.Content)

	if strings.Contains(result.Content, "diff preview") || strings.Contains(result.Content, "@@ ") {
		t.Fatalf("非 dry_run 结果里不该有预览:\n%s", result.Content)
	}
	// 真落盘了（与 dry_run 相反）。
	raw, err := os.ReadFile(filepath.Join(root, "a.txt"))
	if err != nil || string(raw) != "one\nTWO\n" {
		t.Fatalf("非 dry_run 应当落盘: %q (%v)", raw, err)
	}
}

// R5：超大单文件（内容 >1 MiB）由 diffpreview 降级成字节摘要，并且补上文件路径。
//
// 注意构造方式：补丁文本本身有 1 MiB 上限（DefaultMaxPatchBytes），所以这里用
// "多行大文件只改一行"——补丁很小，但变更前后的内容都超过 diffpreview.MaxBytes。
func TestDryRunPreviewBinaryOrOversizeDegrades(t *testing.T) {
	root := t.TempDir()
	var builder strings.Builder
	first := ""
	for i := 0; builder.Len() < (1<<20)+4096; i++ {
		// 每行都不同，否则 hunk 的上下文会有歧义（工具会拒绝）。
		line := fmt.Sprintf("%08d 0123456789abcdefghijklmnopqrstuvwxyzABCDEF", i)
		if i == 0 {
			first = line
		}
		builder.WriteString(line)
		builder.WriteString("\n")
	}
	writeFile(t, root, "big.txt", builder.String())
	patchText := "*** Begin Patch\n*** Update File: big.txt\n@@\n-" + first + "\n+CHANGED\n*** End Patch\n"

	content, isError := invokeApplyPatch(t, root, patchText)
	if isError {
		t.Fatalf("dry_run 报错: %s", content)
	}
	if !strings.Contains(content, "big.txt: binary file changed (") {
		t.Fatalf("超大文件应降级成带路径的字节摘要:\n%s", content)
	}
	if strings.Contains(content, "@@ -") {
		t.Fatalf("降级时不应展示 hunk:\n%s", content)
	}
}

// R6：纯移动（内容未变）也要在预览里说明，不能只剩一个空段。
func TestDryRunPreviewShowsPureMove(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.txt", "same\n")
	patchText := "*** Begin Patch\n*** Update File: a.txt\n*** Move to: dir/b.txt\n*** End Patch\n"

	content, isError := invokeApplyPatch(t, root, patchText)
	if isError {
		t.Fatalf("dry_run 报错: %s", content)
	}
	if !strings.Contains(content, "move a.txt -> dir/b.txt") {
		t.Fatalf("纯移动没有说明:\n%s", content)
	}
}

// maxInt 是 max 的小工具（Go 1.21 的 builtin max 不适用于切片下标表达式）。
func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// prefixLines 给多行文本的每一行加前缀。
func prefixLines(prefix, text string) string {
	lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	for i := range lines {
		lines[i] = prefix + lines[i]
	}
	return strings.Join(lines, "\n") + "\n"
}
