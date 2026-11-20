package undo

// C4-6：POSIX 合法文件名 `x:y.txt` 不能被当成 Windows 盘符绝对路径拒绝。

import "testing"

// TestColonFileNameIsRelative 守护 C4-6：只有真正的盘符形式（`X:\` / `X:/`）才算
// 绝对路径；`x:y.txt` 这类 POSIX 合法名字必须能用作回退的 path 过滤。
func TestColonFileNameIsRelative(t *testing.T) {
	got, err := resolveRelativePath("x:y.txt")
	if err != nil {
		t.Fatalf("resolveRelativePath(\"x:y.txt\") = %q, %v; want 允许（POSIX 合法文件名）", got, err)
	}
	if got != "x:y.txt" {
		t.Fatalf("resolveRelativePath = %q, want x:y.txt", got)
	}
	for _, absolute := range []string{"/x.txt", `C:\x.txt`, "C:/x.txt", `\\server\share\x.txt`} {
		if !isAbsoluteAnyOS(absolute) {
			t.Fatalf("isAbsoluteAnyOS(%q) = false, want true", absolute)
		}
	}
	for _, relative := range []string{"x:y.txt", "a/b.txt", "C:relative.txt"} {
		if isAbsoluteAnyOS(relative) {
			t.Fatalf("isAbsoluteAnyOS(%q) = true, want false", relative)
		}
	}
}
