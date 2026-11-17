package instructions_test

// 第二轮审计（2H）：AGENTS.md 层级在"长会话 / 大仓库"里的表现。
//
// 关注点：
//   - 5 层链（含 @import 链）的渲染顺序、字节预算与"被丢弃的要有提示"；
//   - CRLF、非法 UTF-8、NUL 等"真实文件"里的脏字节不能进 prompt；
//   - 超大文档的截断尾注与总预算占位；
//   - import 循环/超深链不会挂死。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/zeokeel/zeokeel-rig/instructions"
)

// auditRepo 建一个带 .git 的仓库根，返回根目录。
func auditRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	mkdir(t, filepath.Join(root, ".git"))
	return root
}

// TestAudit2HFiveLevelChainOrderAndBudget 建 5 层 AGENTS.md（root → 3 ancestor →
// local），断言渲染顺序是"从远到近"、来源标签正确、且总字节仍在预算内。
func TestAudit2HFiveLevelChainOrderAndBudget(t *testing.T) {
	root := auditRepo(t)
	// 5 层：root、root/a、root/a/b、root/a/b/c、root/a/b/c/d（每层一份 AGENTS.md）。
	dirs := []string{root}
	current := root
	for _, name := range []string{"a", "b", "c", "d"} {
		current = filepath.Join(current, name)
		dirs = append(dirs, current)
	}
	for index, dir := range dirs {
		mkdir(t, dir)
		writeFile(t, filepath.Join(dir, "AGENTS.md"), "level-"+string(rune('0'+index))+"\n")
	}
	cwd := current
	set := mustLoad(t, cwd, instructions.WithUserPath(""))
	if len(set.Documents) != 5 {
		t.Fatalf("发现 %d 份文档，want 5: %+v", len(set.Documents), set.Documents)
	}
	// 顺序：root 在最前，local（d）在最后。
	wantSources := []instructions.Source{
		instructions.SourceRoot,
		instructions.SourceAncestor,
		instructions.SourceAncestor,
		instructions.SourceAncestor,
		instructions.SourceLocal,
	}
	for index, doc := range set.Documents {
		if doc.Source != wantSources[index] {
			t.Fatalf("第 %d 份文档来源 = %v, want %v", index, doc.Source, wantSources[index])
		}
	}
	rendered := set.Render(root)
	// 渲染里 "level-4"（local）必须出现在 "level-0"（root）之后。
	rootAt := strings.Index(rendered, "level-0")
	localAt := strings.Index(rendered, "level-4")
	if rootAt < 0 || localAt < 0 || rootAt > localAt {
		t.Fatalf("层级顺序错误: rootAt=%d localAt=%d\n%s", rootAt, localAt, rendered)
	}
	if set.SkippedDocuments() != 0 {
		t.Fatalf("小文档不该被预算丢弃: %d", set.SkippedDocuments())
	}
	if len(rendered) > instructions.DefaultTotalBytes {
		t.Fatalf("渲染 %d 字节超过了总预算 %d", len(rendered), instructions.DefaultTotalBytes)
	}
}

// TestAudit2HImportChainExpands 覆盖 @import 链（root → docs/base.md → docs/more.md）。
func TestAudit2HImportChainExpands(t *testing.T) {
	root := auditRepo(t)
	writeFile(t, filepath.Join(root, "AGENTS.md"), "root rules\n@docs/base.md\n")
	writeFile(t, filepath.Join(root, "docs", "base.md"), "base rules\n@more.md\n")
	writeFile(t, filepath.Join(root, "docs", "more.md"), "more rules\n")
	set := mustLoad(t, root, instructions.WithUserPath(""))
	rendered := set.Render(root)
	for _, want := range []string{"root rules", "base rules", "more rules"} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("渲染缺少 %q:\n%s", want, rendered)
		}
	}
	doc := findDoc(t, set, filepath.Join(root, "AGENTS.md"))
	if len(doc.Imports) == 0 {
		t.Fatal("Imports 记录为空")
	}
}

// TestAudit2HImportCycleDoesNotHang 覆盖 A→B→A 的循环引用。
func TestAudit2HImportCycleDoesNotHang(t *testing.T) {
	root := auditRepo(t)
	writeFile(t, filepath.Join(root, "AGENTS.md"), "root\n@a.md\n")
	writeFile(t, filepath.Join(root, "a.md"), "a\n@b.md\n")
	writeFile(t, filepath.Join(root, "b.md"), "b\n@a.md\n")
	set := mustLoad(t, root, instructions.WithUserPath(""))
	rendered := set.Render(root)
	if !strings.Contains(rendered, "cycle") {
		t.Fatalf("循环引用应留下注释，而不是静默消失:\n%s", rendered)
	}
	if strings.Count(rendered, "a\n") > 4 {
		t.Fatalf("循环可能被重复展开（a 出现太多次）:\n%s", rendered)
	}
}

// TestAudit2HImportDepthLimit 覆盖超深 import 链（默认最多 4 层）。
func TestAudit2HImportDepthLimit(t *testing.T) {
	root := auditRepo(t)
	writeFile(t, filepath.Join(root, "AGENTS.md"), "root\n@l1.md\n")
	for index := 1; index <= 8; index++ {
		next := ""
		if index < 8 {
			next = "\n@l" + string(rune('0'+index+1)) + ".md"
		}
		writeFile(t, filepath.Join(root, "l"+string(rune('0'+index))+".md"), "layer"+string(rune('0'+index))+next+"\n")
	}
	set := mustLoad(t, root, instructions.WithUserPath(""))
	rendered := set.Render(root)
	if !strings.Contains(rendered, "max depth") {
		t.Fatalf("超深链应留下 max depth 注释:\n%s", rendered)
	}
	if len(rendered) > instructions.DefaultTotalBytes {
		t.Fatalf("渲染 %d 字节超过总预算", len(rendered))
	}
}

// TestAudit2HOversizeDocumentTruncated 覆盖单文档超过 DefaultMaxBytes 的情形。
func TestAudit2HOversizeDocumentTruncated(t *testing.T) {
	root := auditRepo(t)
	huge := strings.Repeat("A", 100<<10) // 100 KiB > DefaultMaxBytes(64 KiB)
	writeFile(t, filepath.Join(root, "AGENTS.md"), huge)
	set := mustLoad(t, root, instructions.WithUserPath(""))
	if len(set.Documents) != 1 {
		t.Fatalf("docs = %d", len(set.Documents))
	}
	content := set.Documents[0].Content
	if len(content) > instructions.DefaultMaxBytes+128 {
		t.Fatalf("单文档内容 %d 字节超过上限 %d", len(content), instructions.DefaultMaxBytes)
	}
	if !strings.Contains(content, "truncated:") {
		t.Fatalf("缺少截断尾注: %q", content[len(content)-80:])
	}
	rendered := set.Render(root)
	if len(rendered) > instructions.DefaultTotalBytes {
		t.Fatalf("渲染后 %d 字节超过总预算", len(rendered))
	}
}

// TestAudit2HTotalBudgetDropsLowestPriority 覆盖总预算不足时的取舍：
// 最具体（local）的指令必须保留，最泛化（root）的可以被丢，且必须有提示。
func TestAudit2HTotalBudgetDropsLowestPriority(t *testing.T) {
	root := auditRepo(t)
	big := strings.Repeat("R", 400)
	writeFile(t, filepath.Join(root, "AGENTS.md"), big)
	mkdir(t, filepath.Join(root, "sub"))
	writeFile(t, filepath.Join(root, "sub", "AGENTS.md"), "local-rule\n")
	set := mustLoad(t, filepath.Join(root, "sub"),
		instructions.WithUserPath(""),
		instructions.WithTotalBytes(200)) // 只够放 local
	rendered := set.Render(root)
	if !strings.Contains(rendered, "local-rule") {
		t.Fatalf("最具体的 local 指令被丢了:\n%s", rendered)
	}
	if !strings.Contains(rendered, "document skipped: total budget exceeded") {
		t.Fatalf("被丢弃的文档应留占位:\n%s", rendered)
	}
	if !strings.Contains(rendered, "[instructions truncated:") {
		t.Fatalf("末尾应有截断提示:\n%s", rendered)
	}
	if set.SkippedDocuments() == 0 {
		t.Fatal("SkippedDocuments() = 0")
	}
	if len(rendered) > 200+200 { // 预算 + 少量结构性开销
		t.Fatalf("渲染 %d 字节，明显超过预算 200", len(rendered))
	}
}

// TestAudit2HDirtyBytesNeverReachPrompt 覆盖 CRLF / NUL / 非法 UTF-8。
func TestAudit2HDirtyBytesNeverReachPrompt(t *testing.T) {
	root := auditRepo(t)
	raw := "line one\r\nline two\r\n\x00nul byte\n" + string([]byte{0xff, 0xfe, 0x41}) + "\n"
	writeFile(t, filepath.Join(root, "AGENTS.md"), raw)
	set := mustLoad(t, root, instructions.WithUserPath(""))
	rendered := set.Render(root)
	if strings.ContainsRune(rendered, 0) {
		t.Fatal("渲染结果里有 NUL 字节")
	}
	if !utf8.ValidString(rendered) {
		t.Fatal("渲染结果不是合法 UTF-8")
	}
}

// TestAudit2HCRLFNormalized 覆盖 2H 审计 P7 的修复：AGENTS.md 的 CRLF 与孤立 CR
// 都被归一成 LF，不再把 \r 带进 prompt（省 token、字节确定、prompt 缓存友好）。
func TestAudit2HCRLFNormalized(t *testing.T) {
	root := auditRepo(t)
	// 第一份：CRLF；import 的第二份：孤立 CR。两者都必须被归一。
	writeFile(t, filepath.Join(root, "AGENTS.md"), "line one\r\nline two\r\n@more.md\r\n")
	writeFile(t, filepath.Join(root, "more.md"), "third\rfourth\r")
	set := mustLoad(t, root, instructions.WithUserPath(""))
	rendered := set.Render(root)
	if strings.Contains(rendered, "\r") {
		t.Fatalf("prompt 里仍有 \\r: %q", rendered)
	}
	for _, want := range []string{"line one\nline two", "third\nfourth"} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("归一后的换行不符合预期（want %q）:\n%s", want, rendered)
		}
	}
}

// TestAudit2HRenderIsAlwaysValidUTF8 用随机字节确认"任何输入都不会让 prompt 崩坏"。
func TestAudit2HRenderIsAlwaysValidUTF8(t *testing.T) {
	root := auditRepo(t)
	payload := make([]byte, 0, 512)
	for i := 0; i < 512; i++ {
		payload = append(payload, byte(i*37+11)) // 覆盖 0x00、0x80-0xFF 等
	}
	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), payload, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	set := mustLoad(t, root, instructions.WithUserPath(""))
	rendered := set.Render(root)
	if !utf8.ValidString(rendered) || strings.ContainsRune(rendered, 0) {
		t.Fatal("随机字节没有被净化成合法 UTF-8")
	}
}

// TestAudit2HImportOutsideRepoRootIsBlocked 确认仓库内文档不能 import 根外文件
// （否则 prompt 变成了任意文件读取通道）。
func TestAudit2HImportOutsideRepoRootIsBlocked(t *testing.T) {
	root := auditRepo(t)
	outside := filepath.Join(t.TempDir(), "secret.md")
	writeFile(t, outside, "TOP-SECRET\n")
	writeFile(t, filepath.Join(root, "AGENTS.md"), "root\n@"+outside+"\n")
	set := mustLoad(t, root, instructions.WithUserPath(""))
	rendered := set.Render(root)
	if strings.Contains(rendered, "TOP-SECRET") {
		t.Fatalf("根外文件被 import 进了 prompt:\n%s", rendered)
	}
}
