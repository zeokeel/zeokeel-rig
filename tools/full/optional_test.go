package full_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/spillfile"
	"github.com/zeokeel/zeokeel-rig/tools/catalog"
	"github.com/zeokeel/zeokeel-rig/tools/full"
	"github.com/zeokeel/zeokeel-rig/tools/ptc"
)

// TestRequiredToolsAreAlwaysConstructed 断言"必现"口径：不打开任何可选开关时，
// Static − Optional 的每个名字都必须由 full.All 构造出来；多出来的名字必须是
// Optional 登记过的条件工具，否则就是账本漏记。
func TestRequiredToolsAreAlwaysConstructed(t *testing.T) {
	built := toolNameSet(full.All(full.AllOptions{Root: t.TempDir()}))
	if len(built) == 0 {
		t.Fatal("full.All 没有构造任何工具")
	}

	optional := make(map[string]bool, len(catalog.Optional))
	for _, entry := range catalog.Optional {
		optional[entry.Name] = true
	}
	static := make(map[string]bool, len(catalog.Static))
	for _, name := range catalog.Static {
		static[name] = true
	}

	for _, name := range catalog.Required() {
		if !built[name] {
			t.Errorf("必现工具 %q 没有被 full.All(AllOptions{Root}) 构造出来", name)
		}
	}
	for name := range built {
		if static[name] || optional[name] {
			continue
		}
		t.Errorf("full.All 构造了账本外的工具 %q：请登记进 catalog.Static 或 catalog.Optional", name)
	}
}

// TestRequiredToolsExistWithoutRoot 断言"必现"承诺在**不带任何 Root/Policy** 的
// 最简构造下也成立：catalog.Required() 的每个名字都必须由
// full.All(full.AllOptions{}) 构造出来。
//
// 只有这条对撞能发现"新增了一个只在 workspaceRoot != \"\" 时构造的工具，却忘记
// 登记 catalog.Optional"：带 Root 的用例会把它构造出来并全绿，而模型在默认
// （无根）配置下其实拿不到它，账本对"必现"的承诺就是假的。
func TestRequiredToolsExistWithoutRoot(t *testing.T) {
	built := toolNameSet(full.All(full.AllOptions{}))
	if len(built) == 0 {
		t.Fatal("full.All(full.AllOptions{}) 没有构造任何工具")
	}
	for _, name := range catalog.Required() {
		if !built[name] {
			t.Errorf("必现工具 %q 没有被 full.All(full.AllOptions{}) 构造出来：它是条件工具，请登记 catalog.Optional；否则就是接线漏了", name)
		}
	}
}

// TestOptionalToolGates 逐个打开条件字段，断言对应工具出现、关闭时缺席。
//
// run_code 是由 kit 在 PTC=true 时用 ptc.New 追加的（full.All 不构造它），
// 因此这里直接调用同一个构造器：门禁打开时它必须叫 run_code，并且登记在
// catalog.Optional 里。
func TestOptionalToolGates(t *testing.T) {
	root := t.TempDir()
	store, err := spillfile.New(t.TempDir())
	if err != nil {
		t.Fatalf("spillfile.New: %v", err)
	}

	// PTC 分支：full.All 不认识 PTC，用 kit 的构造器验证同一个名字。
	ptcTools := []rig.Tool{ptc.New(ptc.Config{})}

	cases := []struct {
		field string
		open  func() []rig.Tool
		want  []string
	}{
		{
			field: "AllOptions.Root",
			open:  func() []rig.Tool { return full.All(full.AllOptions{Root: root}) },
			want:  []string{"apply_patch", "search", "undo"},
		},
		{
			field: "AllOptions.SandboxGrants",
			open: func() []rig.Tool {
				return full.All(full.AllOptions{Root: root, SandboxGrants: true})
			},
			want: []string{"sandbox_status", "request_sandbox_access"},
		},
		{
			field: "AllOptions.SpillStore",
			open: func() []rig.Tool {
				return full.All(full.AllOptions{Root: root, SpillStore: store})
			},
			want: []string{"spill_read"},
		},
		{
			field: "kit.Config.PTC",
			open:  func() []rig.Tool { return ptcTools },
			want:  []string{"run_code"},
		},
	}

	covered := make(map[string]bool, len(cases))
	for _, tc := range cases {
		covered[tc.field] = true
		openSet := toolNameSet(tc.open())
		for _, name := range tc.want {
			if !openSet[name] {
				t.Errorf("%s 打开后缺少工具 %q", tc.field, name)
			}
			entry, ok := catalog.OptionalByName(name)
			if !ok {
				t.Errorf("%q 有条件构造却没有登记进 catalog.Optional", name)
			} else if entry.Field != tc.field {
				t.Errorf("catalog.Optional[%q].Field = %q；want %q", name, entry.Field, tc.field)
			}
		}
	}

	// 条件关闭时对应工具必须缺席：Root 缺失时工作区工具不构造。
	closed := toolNameSet(full.All(full.AllOptions{}))
	for _, name := range []string{"apply_patch", "search", "undo"} {
		if closed[name] {
			t.Errorf("没有 Root/Policy 时仍构造了 %q", name)
		}
	}
	// SandboxGrants 默认关闭。
	baseline := toolNameSet(full.All(full.AllOptions{Root: root}))
	for _, name := range []string{"sandbox_status", "request_sandbox_access", "spill_read"} {
		if baseline[name] {
			t.Errorf("默认配置（未打开 %s）却构造了 %q", mustOptionalField(t, name), name)
		}
	}

	// Optional 里每个由 AllOptions 字段控制的条目都必须被上表覆盖；
	// kit.Config.PTC 这类跨包门禁也必须显式覆盖，避免清单里出现"没人验证"的条件。
	for _, entry := range catalog.Optional {
		if covered[entry.Field] {
			continue
		}
		t.Errorf("catalog.Optional[%q] 的字段 %q 没有被 TestOptionalToolGates 验证", entry.Name, entry.Field)
	}
}

// TestOptionalFieldsExistOnAllOptions 断言 Optional 里以 AllOptions. 开头的字段
// 真实存在于 AllOptions 上：字段改名后清单必须跟着改，否则门禁是空的。
func TestOptionalFieldsExistOnAllOptions(t *testing.T) {
	known := map[string]bool{}
	typ := reflect.TypeOf(full.AllOptions{})
	for i := 0; i < typ.NumField(); i++ {
		known["AllOptions."+typ.Field(i).Name] = true
	}
	for _, entry := range catalog.Optional {
		if !strings.HasPrefix(entry.Field, "AllOptions.") {
			continue // 外部门禁（例如 kit.Config.PTC）由各自的构造器守护
		}
		if !known[entry.Field] {
			t.Errorf("catalog.Optional[%q] 指向不存在的字段 %q", entry.Name, entry.Field)
		}
	}
}

func mustOptionalField(t *testing.T, name string) string {
	t.Helper()
	entry, ok := catalog.OptionalByName(name)
	if !ok {
		return "<未登记>"
	}
	return entry.Field
}

func toolNameSet(tools []rig.Tool) map[string]bool {
	set := make(map[string]bool, len(tools))
	for _, tool := range tools {
		set[tool.Name()] = true
	}
	return set
}
