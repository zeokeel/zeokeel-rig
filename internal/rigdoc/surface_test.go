package rigdoc

// 产品面守护测试：Rig 的定位是**个人 agent 客户端 SDK**。企业/多租户/自建服务形态
// 的三块能力已经从发布面移除，这条用例把"不得回来"钉死：
//
//   - tools/escalate（请求宿主权限执行的通道；个人客户端用 danger-full-access 或
//     审批即可）
//   - mcpserver + cmd/rigmcp（把自己变成 MCP server；产品本身是客户端）
//   - internal/filewatch（轮询冲突检测；单 agent 场景由 git 覆盖）
//   - sandbox/escalate（提权协议本体；产品面不需要"把命令送回宿主执行"的旁路）
//
// 断言分三层：源码里不再有 import / 目录；对外账本（catalog.Static / presets /
// full.All）里不再有相关工具名；kit.Config 不再有提权配置字段。

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig/kit"
	"github.com/zeokeel/zeokeel-rig/tools/catalog"
	"github.com/zeokeel/zeokeel-rig/tools/full"
	"github.com/zeokeel/zeokeel-rig/tools/presets"
)

// removedPackages 是被移除的产品面模块（import 路径后缀 + 目录）。
var removedPackages = []struct {
	importPath string
	dir        string
}{
	{"github.com/zeokeel/zeokeel-rig/mcpserver", "mcpserver"},
	{"github.com/zeokeel/zeokeel-rig/cmd/rigmcp", "cmd/rigmcp"},
	{"github.com/zeokeel/zeokeel-rig/tools/escalate", "tools/escalate"},
	{"github.com/zeokeel/zeokeel-rig/internal/filewatch", "internal/filewatch"},
	// 提权协议整包移除：产品面 0 引用、含 os/exec 的面，留着只是维护成本。
	{"github.com/zeokeel/zeokeel-rig/sandbox/escalate", "sandbox/escalate"},
}

// removedToolNameFragments 是禁止出现在对外工具账本里的名字片段。
// 工具名是模型可见接口，"escalat" 能同时兜住 escalate / request_command_escalation
// 之类的变体；"mcp_server"/"filewatch" 同理兜住"把自己变成服务"的重举。
var removedToolNameFragments = []string{"escalat", "mcp_server", "rigmcp", "filewatch"}

// moduleRoot 返回仓库根（测试的工作目录是 internal/rigdoc）。
func moduleRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve module root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("module root %q has no go.mod: %v", root, err)
	}
	return root
}

// TestRemovedPackagesAreGone 断言四个模块的目录与 import 彻底消失。
func TestRemovedPackagesAreGone(t *testing.T) {
	root := moduleRoot(t)

	for _, removed := range removedPackages {
		dir := filepath.Join(root, filepath.FromSlash(removed.dir))
		if _, err := os.Stat(dir); err == nil {
			t.Errorf("目录仍然存在：%s（个人客户端定位下不应发布）", removed.dir)
		}
	}

	var offenders []string
	walkErr := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := entry.Name()
		if entry.IsDir() {
			if path == root {
				return nil
			}
			// 跳过隐藏目录（VCS、审计/消费者 scratch、编辑器缓存）、依赖与测试数据：
			// 它们不是产品源码，里面的 import 不代表发布面。
			if strings.HasPrefix(name, ".") || name == "node_modules" || name == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(name, ".go") || name == "surface_test.go" {
			// surface_test.go 是本守护用例自身：它的注释与清单里必然写着这些路径。
			return nil
		}
		content, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		text := string(content)
		for _, removed := range removedPackages {
			if strings.Contains(text, `"`+removed.importPath+`"`) {
				relative, relErr := filepath.Rel(root, path)
				if relErr != nil {
					relative = path
				}
				offenders = append(offenders, relative+" → "+removed.importPath)
			}
		}
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walk module: %v", walkErr)
	}
	if len(offenders) > 0 {
		t.Fatalf("仍有代码 import 已移除的模块：\n  %s", strings.Join(offenders, "\n  "))
	}
}

// TestRemovedToolNamesAbsentFromLedgers 断言对外工具账本里没有这些名字。
func TestRemovedToolNamesAbsentFromLedgers(t *testing.T) {
	ledgers := map[string][]string{
		"catalog.Static": catalog.Static,
		"catalog.Modern": catalog.Modern,
		"presets.all":    mustPreset(t, presets.PresetAll),
	}
	names := make([]string, 0, 64)
	for _, tool := range full.All(full.AllOptions{Root: t.TempDir(), SandboxGrants: true}) {
		names = append(names, tool.Name())
	}
	ledgers["full.All"] = names

	for ledger, list := range ledgers {
		for _, toolName := range list {
			lower := strings.ToLower(toolName)
			for _, fragment := range removedToolNameFragments {
				if strings.Contains(lower, fragment) {
					t.Errorf("%s 里仍有被移除能力的工具 %q（匹配片段 %q）", ledger, toolName, fragment)
				}
			}
		}
	}
}

// mustPreset 取某个预设的工具名列表。
func mustPreset(t *testing.T, preset string) []string {
	t.Helper()
	names, ok := presets.Names(preset)
	if !ok {
		t.Fatalf("presets.Names(%q) 不存在", preset)
	}
	return names
}

// TestKitConfigHasNoEscalation 断言 kit 的产品面不再有提权通道配置。
func TestKitConfigHasNoEscalation(t *testing.T) {
	fields := reflect.TypeOf(kit.Config{})
	for index := 0; index < fields.NumField(); index++ {
		name := fields.Field(index).Name
		if strings.Contains(strings.ToLower(name), "escalat") {
			t.Fatalf("kit.Config.%s 仍然存在：个人客户端不使用宿主权限提权通道", name)
		}
	}
}
