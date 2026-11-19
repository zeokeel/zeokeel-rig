package ptc

// 一致性测试：`sandbox.DefaultEnvAllowlist()` 是**收紧环境时的推荐清单**，
// 两侧（sandbox 的过滤路径 / ptc 的解释器）必须按同一份清单过滤。
//
// 历史：解释器以前在"没有策略"时也会套一份最小白名单（手抄 sandbox 的名单），
// 而且默认行为是过滤。现在默认改为**原样继承**（DSH 对齐，见 env_inherit_test.go
// 与 SECURITY.md「凭据」）；这份清单只在宿主显式配置时生效——但"显式配置时两边
// 口径一致"仍然是必须守的契约，否则同一份配置会让 bash 与 run_code 看到不同的
// 环境。

import (
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig/sandbox"
)

// envAuditSetenvKey 是"绝不该出现在解释器环境里"的哨兵（模拟宿主凭据）。
const envAuditSetenvKey = "RIG_AUDIT_SECRET"

// envKeys 把 KEY=VALUE 列表折叠成键集合。
func envKeys(entries []string) map[string]struct{} {
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

// diffEnvKeys 返回 got 与 want 的对称差（便于失败时直接看到漂移的那个键）。
func diffEnvKeys(got, want map[string]struct{}) []string {
	var diff []string
	for key := range got {
		if _, ok := want[key]; !ok {
			diff = append(diff, "extra:"+key)
		}
	}
	for key := range want {
		if _, ok := got[key]; !ok {
			diff = append(diff, "missing:"+key)
		}
	}
	sort.Strings(diff)
	return diff
}

// TestDefaultAllowlistIsTheSharedNarrowingContract 钉住"显式收紧时两侧口径一致"。
func TestDefaultAllowlistIsTheSharedNarrowingContract(t *testing.T) {
	allowlist := sandbox.DefaultEnvAllowlist()
	for _, name := range allowlist {
		t.Setenv(name, "audit-value")
	}
	t.Setenv(envAuditSetenvKey, "s3cr3t-value")

	base := os.Environ()
	// 收紧后的口径：策略显式配这份清单 → policy.CommandEnv；解释器走同一个策略。
	policy := sandbox.New(sandbox.WithEnvAllowlist(allowlist...))
	got := envKeys(interpreterEnv(policy))
	want := envKeys(policy.CommandEnv(base))
	if diff := diffEnvKeys(got, want); len(diff) > 0 {
		t.Fatalf("显式白名单下解释器环境与 policy.CommandEnv 不一致: %v\n got=%v\nwant=%v",
			diff, sortedKeys(got), sortedKeys(want))
	}

	// 清单里的键都在、哨兵不在。
	for _, required := range []string{"PATH", "HOME"} {
		if _, ok := got[required]; !ok {
			t.Fatalf("白名单里的 %s 被丢掉了: %v", required, sortedKeys(got))
		}
	}
	if _, leaked := got[envAuditSetenvKey]; leaked {
		t.Fatalf("显式白名单没能挡住 %s: %v", envAuditSetenvKey, sortedKeys(got))
	}

	// 清单本身逐项相等：任一侧新增/删除一个名字都会在这里失败。
	allowSet := make(map[string]struct{}, len(allowlist))
	for _, name := range allowlist {
		allowSet[name] = struct{}{}
	}
	if diff := diffEnvKeys(got, allowSet); len(diff) > 0 {
		t.Fatalf("显式白名单的可允许键集与 sandbox.DefaultEnvAllowlist() 不一致: %v (allowlist=%v)",
			diff, allowlist)
	}

	// 反面对照：默认（不配白名单）时解释器原样继承，哨兵必须在。
	lax := envKeys(interpreterEnv(nil))
	if _, ok := lax[envAuditSetenvKey]; !ok {
		t.Fatalf("默认应当原样继承宿主环境，却丢了 %s: %v", envAuditSetenvKey, sortedKeys(lax))
	}
}

// sortedKeys 只用于失败信息，保证输出稳定。
func sortedKeys(keys map[string]struct{}) []string {
	out := make([]string, 0, len(keys))
	for key := range keys {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}
