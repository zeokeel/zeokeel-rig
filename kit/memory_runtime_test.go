package kit_test

// Runtime 下的记忆接线：同一工作区必须复用同一份 store；MemoryWiki 必须与工具
// 同源；RuntimeConfig.MemoryOptions 必须真正生效。

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig/gate"
	"github.com/zeokeel/zeokeel-rig/kit"
	"github.com/zeokeel/zeokeel-rig/memoryvfs"
	"github.com/zeokeel/zeokeel-rig/tools/memory"
)

func TestRuntimeSharesOneMemoryStorePerWorkspace(t *testing.T) {
	useStateDir(t)
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	rt, err := kit.NewRuntime(kit.RuntimeConfig{Provider: &scriptProvider{}, Model: "m"})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer func() { _ = rt.Close() }()

	makeAgent := func(root string) *kit.Agent {
		agent, agentErr := rt.NewAgent(kit.Config{Model: "m", Root: root, Gate: gate.AllowAll()})
		if agentErr != nil {
			t.Fatalf("NewAgent(%q): %v", root, agentErr)
		}
		return agent
	}
	first := makeAgent(".")
	second := makeAgent(cwd)
	if first.MemoryStore() == nil || second.MemoryStore() == nil {
		t.Fatalf("Runtime 没有打开 v2 store: %v / %v", first.MemoryStoreError(), second.MemoryStoreError())
	}
	if first.MemoryStore() != second.MemoryStore() {
		t.Fatal(`"." 与绝对路径应共享同一份 store（canonical 缓存键）`)
	}

	// 工具写入后，宿主的 MemoryWiki 兼容面必须看到同一条（同源）。
	tool, ok := first.Tool(memory.ToolName)
	if !ok {
		t.Fatal("memory 工具未注册")
	}
	result, err := tool.Invoke(context.Background(), kit.ToolCall{
		Name:      memory.ToolName,
		Arguments: `{"action":"remember","type":"preferences","summary":"runtime shared memory","content":"written by tool"}`,
	})
	if err != nil || result.IsError {
		t.Fatalf("remember: %v %+v", err, result)
	}
	metas, err := second.MemoryWiki().Index(context.Background())
	if err != nil {
		t.Fatalf("MemoryWiki.Index: %v", err)
	}
	if len(metas) == 0 || !strings.Contains(metas[0].Summary, "runtime shared memory") {
		t.Fatalf("MemoryWiki 与工具不同源: %+v", metas)
	}
}

func TestRuntimeMemoryOptionsApply(t *testing.T) {
	useStateDir(t)
	rt, err := kit.NewRuntime(kit.RuntimeConfig{
		Provider:      &scriptProvider{},
		Model:         "m",
		MemoryOptions: []memoryvfs.Option{memoryvfs.WithMaxEntries(1)},
	})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer func() { _ = rt.Close() }()
	agent, err := rt.NewAgent(kit.Config{Model: "m", Root: t.TempDir(), Gate: gate.AllowAll()})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	tool, _ := agent.Tool(memory.ToolName)
	remember := func(summary string) rigToolResult {
		result, invokeErr := tool.Invoke(context.Background(), kit.ToolCall{
			Name:      memory.ToolName,
			Arguments: `{"action":"remember","type":"preferences","summary":"` + summary + `","content":"x"}`,
		})
		if invokeErr != nil {
			t.Fatalf("Invoke: %v", invokeErr)
		}
		return rigToolResult{isError: result.IsError, content: result.Content}
	}
	if first := remember("first"); first.isError {
		t.Fatalf("first write failed: %s", first.content)
	}
	if second := remember("second different"); !second.isError || !strings.Contains(second.content, "quota") {
		t.Fatalf("RuntimeConfig.MemoryOptions 未生效: %+v", second)
	}
}

type rigToolResult struct {
	isError bool
	content string
}
