package ptc

// 一致性测试：run_code 在**没有沙箱策略**时使用的环境变量白名单，必须与
// sandbox 包导出的唯一事实源逐项一致。
//
// 为什么需要它：解释器进程的白名单以前是 ptc 手抄的一份 `minimalInterpreterEnv`
// （注释里写着"必须与 sandbox 的 defaultEnvAllowlist 保持一致"，但没有任何机制
// 保证）。sandbox 侧以后新增/删除一个变量（例如给它加 `SSL_CERT_FILE`）时，
// 手抄的那份不会跟着变：少一个 → 解释器缺变量跑不起来；多一个 → 可能把刚被
// sandbox 决定丢掉的敏感变量又放回给模型可见的子进程。
//
// 本测试同时从两个方向钉住"同一份清单"：
//  1. 键集合相等：对 os.Environ() 过滤后，interpreterEnv(nil) 与
//     sandbox.New().CommandEnv(...) 的键集合必须完全相同；
//  2. 清单本身相等：测试会为 sandbox.DefaultEnvAllowlist() 里的每个名字都设一个
//     值再调用 interpreterEnv(nil)，因此"任一侧多/少一个名字"都会失败（不依赖
//     宿主环境里恰好存在哪些变量）。

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

// TestInterpreterEnvMatchesSandboxAllowlist 是本包与 sandbox 的唯一事实源的契约。
func TestInterpreterEnvMatchesSandboxAllowlist(t *testing.T) {
	// 让清单里的每个名字都真实存在于宿主环境里：这样"某一侧少了这个名字"会以
	// missing 键的形式暴露，而不是被 os.Environ() 的随机内容掩盖。
	allowlist := sandbox.DefaultEnvAllowlist()
	for _, name := range allowlist {
		t.Setenv(name, "audit-value")
	}
	t.Setenv(envAuditSetenvKey, "s3cr3t-value")

	base := os.Environ()

	// 1) 与"有策略"的路径（policy.CommandEnv）必须给出同一组键——这是 DSH 对齐后
	//    bash/pwsh/jobs/terminal/run_code 共用的口径。
	got := envKeys(interpreterEnv(nil))
	want := envKeys(sandbox.New().CommandEnv(base))
	if diff := diffEnvKeys(got, want); len(diff) > 0 {
		t.Fatalf("无策略解释器环境与 sandbox 白名单不一致: %v\n got=%v\nwant=%v",
			diff, sortedKeys(got), sortedKeys(want))
	}

	// 2) 关键键必须在、哨兵必须不在。
	for _, required := range []string{"PATH", "HOME"} {
		if _, ok := got[required]; !ok {
			t.Fatalf("白名单里的 %s 被丢掉了: %v", required, sortedKeys(got))
		}
	}
	if _, leaked := got[envAuditSetenvKey]; leaked {
		t.Fatalf("解释器环境泄漏了 %s（模型可见）: %v", envAuditSetenvKey, sortedKeys(got))
	}

	// 3) 清单本身逐项相等：任何一侧新增/删除一个名字都会在这里失败。
	allowSet := make(map[string]struct{}, len(allowlist))
	for _, name := range allowlist {
		allowSet[name] = struct{}{}
	}
	if diff := diffEnvKeys(got, allowSet); len(diff) > 0 {
		t.Fatalf("interpreterEnv(nil) 的可允许键集与 sandbox.DefaultEnvAllowlist() 不一致: %v (allowlist=%v)",
			diff, allowlist)
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
