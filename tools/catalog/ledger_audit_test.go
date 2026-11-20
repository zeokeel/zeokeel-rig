package catalog_test

// 账本审计函数的表驱动测试：Check / AuditModern / AuditLegacy / Report，
// 以及 Static / Optional / ServerBound / DocumentedGaps 之间的不变式。
//
// 与 catalog_test.go 的分工：那个文件守 Optional 与 Required 的集合运算；这里守
// "拿一份构建产出（[]string）去比对时到底报什么"，以及几份清单彼此不能互相矛盾
// （例如一个名字不可能既"必现"又是 server-bound）。

import (
	"slices"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig/tools/catalog"
)

// without 返回去掉指定名字后的副本（保持原顺序）。
func without(names []string, drop ...string) []string {
	gone := make(map[string]bool, len(drop))
	for _, name := range drop {
		gone[name] = true
	}
	out := make([]string, 0, len(names))
	for _, name := range names {
		if !gone[name] {
			out = append(out, name)
		}
	}
	return out
}

// sorted 返回升序副本，便于和 Missing/Extra（已排序）直接比较。
func sorted(names []string) []string {
	out := slices.Clone(names)
	slices.Sort(out)
	return out
}

// nameSet 把名字列表折成集合。
func nameSet(names []string) map[string]bool {
	set := make(map[string]bool, len(names))
	for _, name := range names {
		set[name] = true
	}
	return set
}

// TestCheckReportsMissingAndExtra 覆盖 Check 的四种比对结果：
// 合法账本、清单里有但构建没有（Missing）、构建里有但清单没有（Extra）、两者都有。
func TestCheckReportsMissingAndExtra(t *testing.T) {
	complete := slices.Clone(catalog.Static)

	cases := []struct {
		name       string
		have       []string
		wantOK     bool
		wantMiss   []string
		wantExtra  []string
		wantString string // 空表示只校验集合、不校验摘要文案
	}{
		{
			name:       "合法账本（与 Static 一致）",
			have:       complete,
			wantOK:     true,
			wantString: "tool catalog: complete",
		},
		{
			name:       "静态清单里有但构建没有",
			have:       without(complete, "bash", "read_file"),
			wantOK:     false,
			wantMiss:   []string{"bash", "read_file"},
			wantString: "tool catalog: missing 2 (bash, read_file)",
		},
		{
			name:       "构建里有但静态清单没有",
			have:       append(slices.Clone(complete), "read_file_v2", "zeta_tool"),
			wantOK:     true,
			wantExtra:  []string{"read_file_v2", "zeta_tool"},
			wantString: "tool catalog: complete (+2 extra: read_file_v2, zeta_tool)",
		},
		{
			name:       "既缺又多（Missing 优先决定摘要）",
			have:       append(without(complete, "grep"), "grep_v2"),
			wantOK:     false,
			wantMiss:   []string{"grep"},
			wantExtra:  []string{"grep_v2"},
			wantString: "tool catalog: missing 1 (grep)",
		},
		{
			name:      "重复的已知名字不算多余",
			have:      append(slices.Clone(complete), "read_file", "bash"),
			wantOK:    true,
			wantExtra: nil,
		},
		{
			name:     "空构建：每个静态名字都缺",
			have:     nil,
			wantOK:   false,
			wantMiss: sorted(catalog.Static),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			report := catalog.Check(tc.have)
			if report.OK() != tc.wantOK {
				t.Errorf("OK() = %v, want %v（%s）", report.OK(), tc.wantOK, report.String())
			}
			if !slices.Equal(report.Missing, tc.wantMiss) {
				t.Errorf("Missing = %v, want %v", report.Missing, tc.wantMiss)
			}
			if !slices.Equal(report.Extra, tc.wantExtra) {
				t.Errorf("Extra = %v, want %v", report.Extra, tc.wantExtra)
			}
			if !slices.IsSorted(report.Missing) || !slices.IsSorted(report.Extra) {
				t.Errorf("Missing/Extra 未排序: %v / %v", report.Missing, report.Extra)
			}
			if tc.wantString != "" {
				if got := report.String(); got != tc.wantString {
					t.Errorf("String() = %q, want %q", got, tc.wantString)
				}
			}
		})
	}
}

// TestReportStringBranches 直接构造 Report，钉住 String 的三条分支与 OK 的口径：
// OK 只看 Missing，Extra 不影响"账本是否完整"。
func TestReportStringBranches(t *testing.T) {
	cases := []struct {
		name   string
		report catalog.Report
		wantOK bool
		want   string
	}{
		{"完整无多余", catalog.Report{}, true, "tool catalog: complete"},
		{"完整但有多余", catalog.Report{Extra: []string{"x"}}, true, "tool catalog: complete (+1 extra: x)"},
		{"完整且多个多余（按传入顺序渲染）", catalog.Report{Extra: []string{"a", "b"}}, true, "tool catalog: complete (+2 extra: a, b)"},
		{"缺失一条", catalog.Report{Missing: []string{"bash"}}, false, "tool catalog: missing 1 (bash)"},
		{"缺失多条", catalog.Report{Missing: []string{"bash", "grep"}}, false, "tool catalog: missing 2 (bash, grep)"},
		{"既缺又多：Extra 不进摘要", catalog.Report{Missing: []string{"bash"}, Extra: []string{"x"}}, false, "tool catalog: missing 1 (bash)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.report.OK(); got != tc.wantOK {
				t.Errorf("OK() = %v, want %v", got, tc.wantOK)
			}
			if got := tc.report.String(); got != tc.want {
				t.Errorf("String() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestAuditModern 覆盖现代工具面审计：齐备时不报、缺项时按名字排序报告、
// 空构建报全部、多余的未知名字不参与审计。
func TestAuditModern(t *testing.T) {
	complete := slices.Clone(catalog.Modern)
	cases := []struct {
		name string
		have []string
		want []string
	}{
		{"现代工具面齐备", complete, nil},
		{"缺三个（顺序被打乱）", without(complete, "bash", "memory", "skill"), []string{"bash", "memory", "skill"}},
		{"构建多出未知名字不影响审计", append(slices.Clone(complete), "not_a_tool"), nil},
		{"空构建", nil, sorted(catalog.Modern)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := catalog.AuditModern(tc.have)
			if !slices.Equal(got, tc.want) {
				t.Errorf("AuditModern = %v, want %v", got, tc.want)
			}
			if len(got) > 1 && !slices.IsSorted(got) {
				t.Errorf("AuditModern 结果未排序: %v", got)
			}
		})
	}
}

// TestAuditLegacy 覆盖旧版工具面审计（含 server-bound 形态的名字）。
func TestAuditLegacy(t *testing.T) {
	complete := slices.Clone(catalog.Legacy)
	cases := []struct {
		name string
		have []string
		want []string
	}{
		{"旧版工具面齐备", complete, nil},
		{"缺两个（含 server-bound 形态的名字）", without(complete, "lsp", "team_status"), []string{"lsp", "team_status"}},
		{"构建多出未知名字不影响审计", append(slices.Clone(complete), "always_there"), nil},
		{"空构建", nil, sorted(catalog.Legacy)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := catalog.AuditLegacy(tc.have)
			if !slices.Equal(got, tc.want) {
				t.Errorf("AuditLegacy = %v, want %v", got, tc.want)
			}
			if len(got) > 1 && !slices.IsSorted(got) {
				t.Errorf("AuditLegacy 结果未排序: %v", got)
			}
		})
	}
}

// TestOptionalEntriesLocateRealTools 守 Optional 的 Gate/Field 完整性，以及
// "每个可选名字都能在别的清单里找到出处"：在 Static 里（条件构造）、在
// ServerBound 里（宿主不提供外部组件就不存在），或是 kit 单独追加的 run_code。
func TestOptionalEntriesLocateRealTools(t *testing.T) {
	static := nameSet(catalog.Static)
	serverBound := nameSet(catalog.ServerBound)
	for _, entry := range catalog.Optional {
		if entry.Name == "" || entry.Field == "" || entry.Gate == "" {
			t.Errorf("Optional 条目字段不完整: %+v", entry)
		}
		// Field 必须能被定位到一个控制字段，而不是一句自由文本。
		if !strings.HasPrefix(entry.Field, "AllOptions.") && !strings.HasPrefix(entry.Field, "kit.Config.") {
			t.Errorf("Optional[%q].Field = %q：应以 AllOptions. 或 kit.Config. 开头", entry.Name, entry.Field)
		}
		if strings.TrimSpace(entry.Name) != entry.Name || strings.TrimSpace(entry.Gate) == "" {
			t.Errorf("Optional[%q] 的名字或 Gate 含多余空白: %+v", entry.Name, entry)
		}
		located := static[entry.Name] || serverBound[entry.Name] || entry.Name == "run_code"
		if !located {
			t.Errorf("Optional[%q] 在 Static / ServerBound 里都找不到，也不是 kit 追加的工具", entry.Name)
		}
	}
}

// TestServerBoundInvariants 守 server-bound 清单：非空、无重复、名字干净，
// 且与 Static / Required 不相交（一个名字不可能既"由 server 决定存在"又"必现"）。
func TestServerBoundInvariants(t *testing.T) {
	if len(catalog.ServerBound) == 0 {
		t.Fatal("ServerBound 为空：MCP/LSP 这类名字无法在账本里表达")
	}
	static := nameSet(catalog.Static)
	required := nameSet(catalog.Required())
	seen := make(map[string]bool, len(catalog.ServerBound))
	for _, name := range catalog.ServerBound {
		if strings.TrimSpace(name) == "" {
			t.Error("ServerBound 里有空名字")
			continue
		}
		if strings.ContainsAny(name, " \t\n") {
			t.Errorf("ServerBound 名字含空白: %q", name)
		}
		if seen[name] {
			t.Errorf("ServerBound 重复登记了 %q", name)
		}
		seen[name] = true
		if static[name] {
			t.Errorf("ServerBound 的 %q 同时出现在 Static 里：它就不该是 server-bound", name)
		}
		if required[name] {
			t.Errorf("ServerBound 的 %q 同时出现在 Required 里", name)
		}
	}
}

// TestDocumentedGapsInvariants 守"记录在案的缺口"清单：每个缺口都必须说明
// 自己不是别的清单里的静态名字，否则就是一个自相矛盾的条目。
//
// DocumentedGaps 当前是空的（见 AGENTS.md / docs/COVERAGE.md），所以下面的循环
// 只在有人新增缺口时才真正执行；这不是空测试——它守的是"新增缺口时不得与现有
// 清单冲突"这条不变式。
func TestDocumentedGapsInvariants(t *testing.T) {
	static := nameSet(catalog.Static)
	modern := nameSet(catalog.Modern)
	serverBound := nameSet(catalog.ServerBound)
	seen := make(map[string]bool, len(catalog.DocumentedGaps))
	for _, gap := range catalog.DocumentedGaps {
		if strings.TrimSpace(gap) == "" {
			t.Error("DocumentedGaps 里有空名字")
			continue
		}
		if seen[gap] {
			t.Errorf("DocumentedGaps 重复登记了 %q", gap)
		}
		seen[gap] = true
		if static[gap] {
			t.Errorf("DocumentedGaps 的 %q 已经是 Static 里的静态工具，不再是缺口", gap)
		}
		if modern[gap] {
			t.Errorf("DocumentedGaps 的 %q 同时出现在 Modern 里：现代工具面要求暴露它，与'暂不暴露'矛盾", gap)
		}
		if serverBound[gap] {
			t.Errorf("DocumentedGaps 的 %q 同时出现在 ServerBound 里：应归到那一类", gap)
		}
	}
}

// TestStaticAndModernLedgersAreClean 是几份清单的基础卫生检查：非空、无空名、
// 无重复、不含已被删除的工具名（AGENTS.md §6 的禁名单）。
func TestStaticAndModernLedgersAreClean(t *testing.T) {
	removed := nameSet([]string{"report", "settings_get", "settings_set", "present_artifact"})
	ledgers := map[string][]string{
		"Static":  catalog.Static,
		"Modern":  catalog.Modern,
		"Legacy":  catalog.Legacy,
		"Dynamic": catalog.Dynamic,
	}
	for ledgerName, names := range ledgers {
		if len(names) == 0 {
			t.Errorf("%s 为空", ledgerName)
		}
		seen := make(map[string]bool, len(names))
		for _, name := range names {
			if strings.TrimSpace(name) == "" {
				t.Errorf("%s 里有空名字", ledgerName)
				continue
			}
			if seen[name] {
				t.Errorf("%s 重复登记了 %q", ledgerName, name)
			}
			seen[name] = true
			if removed[name] {
				t.Errorf("%s 里出现了已删除的工具名 %q", ledgerName, name)
			}
		}
	}
}
