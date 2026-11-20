package search

// 隐藏路径不能被检索到。v0.24.0 之前这是 WithDeniedGlobs 负责的事（那时它在
// 策略层真的拦），对齐后策略不再拦、search 也没有任何过滤入口，于是"设置里勾了
// 隐藏 .env"之后，`mode=text` 仍然会把 `.env` 的内容以 `path:line:content` 吐给
// 模型。本文件钉住 search 侧的过滤入口（WithHiddenFunc）。

import (
	"strings"
	"testing"
)

// TestHiddenFilesAreNotSearchable 覆盖内容检索与文件名检索两条路，并且先证明
// "不过滤时确实能搜到"——否则用例可能因为别的原因（分词/上限）而假绿。
func TestHiddenFilesAreNotSearchable(t *testing.T) {
	files := map[string]string{
		".env":            "TOKEN=super-secret-value\n",
		"src/main.go":     "package main\n\n// super-secret-value is mentioned here too\n",
		"secrets/keys.md": "super-secret-value in a hidden directory\n",
	}
	root := writeTree(t, files)

	t.Run("unfiltered", func(t *testing.T) {
		// 反面证据：没有过滤时 .env 与 secrets/ 都会被命中。
		result := invoke(t, New(root), `{"mode":"text","query":"super-secret-value"}`)
		if result.IsError {
			t.Fatalf("未过滤搜索失败: %q", result.Content)
		}
		if !strings.Contains(result.Content, ".env") {
			t.Fatalf("前置条件不成立：未过滤时应当命中 .env\n%s", result.Content)
		}
		if !strings.Contains(result.Content, "secrets/keys.md") {
			t.Fatalf("前置条件不成立：未过滤时应当命中 secrets/keys.md\n%s", result.Content)
		}
	})

	hidden := func(abs string) bool {
		slashed := strings.ReplaceAll(abs, "\\", "/")
		return strings.HasSuffix(slashed, "/.env") || strings.Contains(slashed, "/secrets/")
	}

	t.Run("text mode", func(t *testing.T) {
		result := invoke(t, New(root, WithHiddenFunc(hidden)), `{"mode":"text","query":"super-secret-value"}`)
		if result.IsError {
			t.Fatalf("搜索失败: %q", result.Content)
		}
		if strings.Contains(result.Content, ".env") {
			t.Fatalf("内容检索命中了被隐藏的 .env\n%s", result.Content)
		}
		if strings.Contains(result.Content, "secrets/") {
			t.Fatalf("内容检索进入了被隐藏的目录\n%s", result.Content)
		}
		if !strings.Contains(result.Content, "src/main.go") {
			t.Fatalf("可见文件不该被一起过滤掉\n%s", result.Content)
		}
	})

	t.Run("files mode", func(t *testing.T) {
		result := invoke(t, New(root, WithHiddenFunc(hidden)), `{"mode":"files","query":"main"}`)
		if result.IsError {
			t.Fatalf("文件名检索失败: %q", result.Content)
		}
		if !strings.Contains(result.Content, "src/main.go") {
			t.Fatalf("文件名检索应命中 src/main.go\n%s", result.Content)
		}
	})

	t.Run("mentions mode", func(t *testing.T) {
		// @提及也要遵守同一套隐藏规则：命中被隐藏的路径时按"缺失"处理。
		result := invoke(t, New(root, WithHiddenFunc(hidden)), `{"mode":"mentions","query":"@.env"}`)
		if result.IsError {
			t.Fatalf("@提及解析失败: %q", result.Content)
		}
		if strings.Contains(result.Content, "TOKEN=") {
			t.Fatalf("@提及把隐藏文件的内容带出来了\n%s", result.Content)
		}
	})
}
