package full_test

// DSH 对齐后的写面语义：`full` 装配**不再**登记 `.git` 保护。
//
// 这条用例守的是"半开"回归：旧版 full 会调 sandbox.Policy.AddGitProtection，
// 把 `.git/config|hooks|info` 声明成可写根内的只读子路径；策略层的保护路径停用
// 之后，这一条成了唯一还拦着 `.git` 的进程内判定，与 DSH 的"只围栏可写根"
// 不一致。现在 full 不登记任何只读子路径，`.git` 与普通目录一样可写。

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/sandbox"
	"github.com/zeokeel/zeokeel-rig/tools/builtin"
	"github.com/zeokeel/zeokeel-rig/tools/full"
)

// TestGitControlPlaneIsWritableUnderFull 断言 full 装配下 `.git/config` 与
// `.git/hooks/*` 都能写成功，且策略里没有被登记任何只读子路径/保护路径。
func TestGitControlPlaneIsWritableUnderFull(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git", "hooks"), 0o755); err != nil {
		t.Fatalf("mkdir .git/hooks: %v", err)
	}
	configPath := filepath.Join(root, ".git", "config")
	if err := os.WriteFile(configPath, []byte("[core]\n\trepositoryformatversion = 0\n"), 0o644); err != nil {
		t.Fatalf("write .git/config: %v", err)
	}

	policy := sandbox.New(
		sandbox.WithRoots(root),
		sandbox.WithMode(sandbox.ModeWorkspaceWrite),
	)
	// 刻意把已停用的开关打开：它必须不再改变任何行为（这正是"半开"回归点）。
	tools := full.All(full.AllOptions{
		Policy:                   policy,
		ProtectWorkspaceMetadata: true,
	})
	if len(tools) == 0 {
		t.Fatal("full.All 没有返回任何工具")
	}

	invokeTool(t, tools, builtin.ToolWriteFile, `{"path":".git/config","content":"[core]\n\thooksPath = .evil-hooks\n"}`)
	invokeTool(t, tools, builtin.ToolWriteFile, `{"path":".git/hooks/pre-commit","content":"#!/bin/sh\nexit 0\n"}`)

	if data, err := os.ReadFile(configPath); err != nil {
		t.Fatalf("read .git/config: %v", err)
	} else if string(data) != "[core]\n\thooksPath = .evil-hooks\n" {
		t.Fatalf(".git/config 没有被改写：%q", string(data))
	}
	hookPath := filepath.Join(root, ".git", "hooks", "pre-commit")
	if data, err := os.ReadFile(hookPath); err != nil {
		t.Fatalf("read .git/hooks/pre-commit: %v", err)
	} else if len(data) == 0 {
		t.Fatal(".git/hooks/pre-commit 是空的")
	}

	// 装配过程不得再往策略里塞任何"根内只读"约束。
	for _, writable := range policy.WritableRoots() {
		if len(writable.ReadOnlySubpaths) > 0 {
			t.Fatalf("可写根 %q 仍有只读子路径 %v：full 又在登记 .git 保护了",
				writable.Root, writable.ReadOnlySubpaths)
		}
	}
	if paths := policy.ProtectedPaths(); len(paths) != 0 {
		t.Fatalf("Policy.ProtectedPaths() = %v，want empty（DSH 对齐后没有保护路径）", paths)
	}
}

// invokeTool 按名字调用一个工具，失败即 t.Fatal（写面被拒就是用例失败）。
func invokeTool(t *testing.T, tools []rig.Tool, name, arguments string) rig.ToolResult {
	t.Helper()
	for _, tool := range tools {
		if tool.Name() != name {
			continue
		}
		result, err := tool.Invoke(context.Background(), rig.ToolCall{ID: name, Name: name, Arguments: arguments})
		if err != nil {
			t.Fatalf("%s Invoke 返回 Go error: %v", name, err)
		}
		if result.IsError {
			t.Fatalf("%s 被拒绝：%s", name, result.Content)
		}
		return result
	}
	t.Fatalf("工具 %q 不在 full 装配结果里", name)
	return rig.ToolResult{}
}
