package mcp

// 宿主可复刻的命名规则 + v0.24.0 旧名迁移助手的契约测试。
//
// 现象（修复前）：清洗规则 sanitizeName 未导出，宿主为 disabledTools 白名单/持久化
// 复刻工具名只能抄一份规则（随上游漂移）；v0.24.0 把 `mcp_list_resources` 一类的旧名
// 改成了 `mcp__<server>__list_resources`，但没有任何迁移助手，旧配置只能让用户重选。

import (
	"strings"
	"testing"
)

// TestSanitizeNameMatchesExistingToolNaming 钉住"导出的清洗规则 == 现有工具名里
// 实际用的那份规则"：样例全部来自真实构造器产出的名字，逐字节比较。
func TestSanitizeNameMatchesExistingToolNaming(t *testing.T) {
	// 普通工具：server "srv.a" + tool "read.file" → mcp__srv_a__read_file。
	tools := NewTools(&Client{}, "srv.a", []ToolInfo{{Name: "read.file"}}, "mcp__")
	if len(tools) != 1 {
		t.Fatalf("NewTools = %d tools, want 1", len(tools))
	}
	want := "mcp__" + SanitizeName("srv.a") + "__" + SanitizeName("read.file")
	if got := tools[0].Name(); got != want {
		t.Fatalf("tool name = %q, SanitizeName-derived = %q", got, want)
	}

	// 资源工具：server "a.b/c d" → mcp__a_b_c_d__list_resources。
	client := &Client{}
	client.serverName = "a.b/c d"
	wantResource := "mcp__" + SanitizeName("a.b/c d") + "__" + ToolListResources
	if got := ResourceToolName(client, "mcp__", ToolListResources); got != wantResource {
		t.Fatalf("resource tool name = %q, SanitizeName-derived = %q", got, wantResource)
	}

	cases := map[string]string{
		"srv.a":     "srv_a",
		"a.b/c d":   "a_b_c_d",
		"read.file": "read_file",
		"a b":       "a_b",
		"a_b-c":     "a_b-c",
		"-edge-":    "-edge-",
		"_edge_":    "edge",
		"__":        "tool",
		"":          "tool",
		"clé":       "cl", // é 变下划线后又在末尾，被 trim 掉
		"cléx":      "cl_x",
	}
	for input, want := range cases {
		if got := SanitizeName(input); got != want {
			t.Fatalf("SanitizeName(%q) = %q, want %q", input, got, want)
		}
	}
}

// TestMigrateLegacyNames 覆盖三种情况：给了 server 能迁移、没给 server 全部进
// needsUser、未知名字原样保留（不静默丢掉）。
func TestMigrateLegacyNames(t *testing.T) {
	legacy := []string{
		"mcp_list_resources",
		"mcp_read_resource",
		"mcp_list_resource_templates",
		"mcp_list_prompts",
		"mcp_get_prompt",
	}
	wantMigrated := []string{
		"mcp__srv_a__list_resources",
		"mcp__srv_a__read_resource",
		"mcp__srv_a__list_resource_templates",
		"mcp__srv_a__list_prompts",
		"mcp__srv_a__get_prompt",
	}

	// 1. 给定 server：五个旧名全部迁移成新形状，needsUser 为空。
	migrated, needsUser := MigrateLegacyNames(legacy, "srv.a")
	if strings.Join(migrated, ",") != strings.Join(wantMigrated, ",") {
		t.Fatalf("migrated = %v, want %v", migrated, wantMigrated)
	}
	if len(needsUser) != 0 {
		t.Fatalf("needsUser = %v, want empty", needsUser)
	}

	// 2. server 为空（含只有空白的字符串）：全部进 needsUser，且原样保留旧名。
	for _, emptyServer := range []string{"", "   "} {
		migrated, needsUser = MigrateLegacyNames(legacy, emptyServer)
		if len(migrated) != 0 {
			t.Fatalf("server=%q: migrated = %v, want empty", emptyServer, migrated)
		}
		if strings.Join(needsUser, ",") != strings.Join(legacy, ",") {
			t.Fatalf("server=%q: needsUser = %v, want %v", emptyServer, needsUser, legacy)
		}
	}

	// 3. 未知名字原样进 needsUser，已知名字仍然迁移。
	migrated, needsUser = MigrateLegacyNames([]string{
		"mcp_list_resources", "search_web", "mcp_bogus", "mcp__srv_a__read_resource",
	}, "srv.a")
	if strings.Join(migrated, ",") != "mcp__srv_a__list_resources" {
		t.Fatalf("mixed: migrated = %v", migrated)
	}
	if strings.Join(needsUser, ",") != "search_web,mcp_bogus,mcp__srv_a__read_resource" {
		t.Fatalf("mixed: needsUser = %v", needsUser)
	}

	// 空的输入不产生任何结果（nil 与空切片都行，长度必须为 0）。
	if m, n := MigrateLegacyNames(nil, "srv.a"); len(m) != 0 || len(n) != 0 {
		t.Fatalf("nil input: migrated=%v needsUser=%v", m, n)
	}
}
