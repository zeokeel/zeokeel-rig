package catalog_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig/tools/catalog"
)

// TestOptionalLedgerIsConsistent 守 catalog 可选清单的结构：每条都必须能说明
// "哪个字段控制它、打开时出现什么"，且 Static 与 Optional 的集合运算是确定的。
func TestOptionalLedgerIsConsistent(t *testing.T) {
	if len(catalog.Optional) == 0 {
		t.Fatal("catalog.Optional 为空：条件构造的工具无法与'永远构造不出来'区分")
	}
	seen := make(map[string]bool, len(catalog.Optional))
	for _, entry := range catalog.Optional {
		if entry.Name == "" || entry.Field == "" || entry.Gate == "" {
			t.Errorf("catalog.Optional 条目字段不完整: %+v", entry)
		}
		if !strings.Contains(entry.Field, ".") {
			t.Errorf("catalog.Optional[%q].Field = %q：必须写成'包.字段'形式，才能定位控制它的字段", entry.Name, entry.Field)
		}
		if seen[entry.Name] {
			t.Errorf("catalog.Optional 重复登记了 %q", entry.Name)
		}
		seen[entry.Name] = true
		if got, ok := catalog.OptionalByName(entry.Name); !ok || got != entry {
			t.Errorf("OptionalByName(%q) = %+v, %v；want %+v", entry.Name, got, ok, entry)
		}
	}
	if _, ok := catalog.OptionalByName("definitely-not-a-tool"); ok {
		t.Error("OptionalByName 对未登记的名字返回了 true")
	}

	names := catalog.OptionalNames()
	if len(names) != len(catalog.Optional) {
		t.Fatalf("OptionalNames 返回 %d 个名字，Optional 有 %d 条", len(names), len(catalog.Optional))
	}
	if !slices.IsSorted(names) {
		t.Errorf("OptionalNames 未排序: %v", names)
	}
	if len(slices.Compact(slices.Clone(names))) != len(names) {
		t.Errorf("OptionalNames 有重复: %v", names)
	}
}

// TestRequiredIsStaticMinusOptional 守"必须出现"的口径：Required 恰好是
// Static 去掉 Optional 中登记的名字。
func TestRequiredIsStaticMinusOptional(t *testing.T) {
	optional := make(map[string]bool, len(catalog.Optional))
	for _, entry := range catalog.Optional {
		optional[entry.Name] = true
	}
	var want []string
	for _, name := range catalog.Static {
		if !optional[name] {
			want = append(want, name)
		}
	}
	got := catalog.Required()
	if !slices.IsSorted(got) {
		t.Errorf("Required 未排序: %v", got)
	}
	if !slices.Equal(got, wantNamesSorted(want)) {
		t.Errorf("Required = %v；want Static − Optional = %v", got, wantNamesSorted(want))
	}
	for _, name := range got {
		if optional[name] {
			t.Errorf("Required 里出现了可选工具 %q", name)
		}
	}
	if len(got) == 0 || len(got) == len(catalog.Static) && len(catalog.Optional) > 0 {
		t.Errorf("Required 口径可疑: %d 个必现 / %d 个 Optional", len(got), len(catalog.Optional))
	}
}

// TestOptionalCoversConditionalConstructors 锁住两个不在 Static 里的条件构造
// 工具，它们是"名字在账本里但暂时构造不出来"的主要来源：
//
//   - spill_read 只在宿主注入 AllOptions.SpillStore 时出现（ServerBound）；
//   - run_code 只在 kit.Config.PTC=true 时由 kit 追加（full.All 不构造）。
func TestOptionalCoversConditionalConstructors(t *testing.T) {
	cases := map[string]string{
		"spill_read": "AllOptions.SpillStore",
		"run_code":   "kit.Config.PTC",
	}
	for name, field := range cases {
		entry, ok := catalog.OptionalByName(name)
		if !ok {
			t.Errorf("%q 未登记进 catalog.Optional：它是有条件构造的，不能当作必现工具", name)
			continue
		}
		if entry.Field != field {
			t.Errorf("catalog.Optional[%q].Field = %q；want %q", name, entry.Field, field)
		}
	}
}

func wantNamesSorted(names []string) []string {
	out := slices.Clone(names)
	slices.Sort(out)
	return out
}
