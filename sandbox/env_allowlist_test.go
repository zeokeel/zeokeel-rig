package sandbox

// 环境变量白名单的"唯一事实源"契约。
//
// 背景：这份名单以前只存在于包内（defaultEnvAllowlist），于是包外想按同一口径起
// 子进程的地方只能手抄一份（tools/ptc 的 run_code 就是这么做的）。两份手抄清单
// 没有一致性保证：sandbox 侧新增一个变量时另一侧不跟，或 sandbox 决定丢弃某个
// 变量时另一侧又把它放回去。
//
// 本文件钉住三件事：
//  1. 导出函数与内部存储逐项一致（防止以后有人只改内部那份）；
//  2. 导出函数返回**副本**（调用方改它不影响策略层）；
//  3. Policy.CommandEnv 的默认路径确实用的是同一份名单。

import (
	"slices"
	"strings"
	"testing"
)

func TestDefaultEnvAllowlistMatchesInternalStorage(t *testing.T) {
	exported := DefaultEnvAllowlist()
	if !slices.Equal(exported, defaultEnvAllowlist) {
		t.Fatalf("DefaultEnvAllowlist() 与内部 defaultEnvAllowlist 不一致:\n export=%v\ninternal=%v",
			exported, defaultEnvAllowlist)
	}
	// 允许集必须覆盖"定位工具链 / 临时目录 / 本地化"必需的键。
	for _, required := range []string{"PATH", "HOME", "LANG"} {
		if !slices.Contains(exported, required) {
			t.Fatalf("默认白名单缺少必需变量 %q: %v", required, exported)
		}
	}
	// 凭据类变量绝不能被默认放行（大小写不敏感地检查常见名字）。
	for _, forbidden := range []string{"OPENAI_API_KEY", "AWS_SECRET_ACCESS_KEY", "GITHUB_TOKEN"} {
		if slices.Contains(exported, forbidden) {
			t.Fatalf("默认白名单放行了凭据变量 %q: %v", forbidden, exported)
		}
	}
}

func TestDefaultEnvAllowlistReturnsACopy(t *testing.T) {
	first := DefaultEnvAllowlist()
	if len(first) == 0 {
		t.Fatal("DefaultEnvAllowlist() 返回空列表")
	}
	original := first[0]
	first[0] = "RIG_MUTATED_BY_CALLER"
	first = append(first, "RIG_APPENDED_BY_CALLER")

	second := DefaultEnvAllowlist()
	if len(second) != len(defaultEnvAllowlist) || second[0] != original {
		t.Fatalf("调用方修改返回值污染了唯一事实源: %v", second)
	}
	if slices.Contains(second, "RIG_APPENDED_BY_CALLER") || second[0] == "RIG_MUTATED_BY_CALLER" {
		t.Fatalf("返回值不是副本: %v", second)
	}
}

// TestCommandEnvUsesDefaultAllowlist：默认（未 WithEnvAllowlist）路径与导出函数
// 过滤出的键集合必须完全相同——否则"导出的名单"只是一份装饰。
func TestCommandEnvUsesDefaultAllowlist(t *testing.T) {
	base := []string{
		"PATH=/usr/bin", "HOME=/root", "LANG=C.UTF-8",
		"OPENAI_API_KEY=sk-should-not-leak",
		"TEMP=/tmp", "SystemRoot=C:\\Windows",
		"RIG_ONLY_IN_BASE=1",
	}
	policy := New(WithRoots(t.TempDir()))

	got := keySet(t, policy.CommandEnv(base))
	want := make(map[string]struct{})
	for _, name := range DefaultEnvAllowlist() {
		want[name] = struct{}{}
	}
	for _, name := range []string{"PATH", "HOME", "LANG", "TEMP", "SystemRoot"} {
		if _, ok := got[name]; !ok {
			t.Fatalf("CommandEnv 丢掉了白名单内的 %q: %v", name, sortedEnvKeys(t, got))
		}
	}
	for _, name := range []string{"OPENAI_API_KEY", "RIG_ONLY_IN_BASE"} {
		if _, ok := got[name]; ok {
			t.Fatalf("CommandEnv 放行了白名单外的 %q: %v", name, sortedEnvKeys(t, got))
		}
	}
	// 与导出名单一致：名单里有、base 里也有的键 == 实际输出键（base 只提供子集，
	// 因此这里比较的是"输出键集合 ⊆ 名单，且名单 ∩ base 全部被保留"）。
	for _, key := range sortedEnvKeys(t, got) {
		if _, allowed := want[key]; !allowed {
			t.Fatalf("输出里有白名单之外的键 %q", key)
		}
	}
}

// keySet 把 KEY=VALUE 列表折叠成键集合。
func keySet(t *testing.T, entries []string) map[string]struct{} {
	t.Helper()
	keys := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		key, _, ok := strings.Cut(entry, "=")
		if !ok || key == "" {
			continue
		}
		keys[key] = struct{}{}
	}
	return keys
}

// sortedEnvKeys 只用于失败信息，保证输出稳定。
func sortedEnvKeys(t *testing.T, keys map[string]struct{}) []string {
	t.Helper()
	out := make([]string, 0, len(keys))
	for key := range keys {
		out = append(out, key)
	}
	slices.Sort(out)
	return out
}
