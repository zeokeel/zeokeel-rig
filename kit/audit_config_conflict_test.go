package kit_test

// 审计用例（已修）：`kit.Config` 里"注入一个组件就取代一组配置"的冲突必须在**构造期**
// 暴露出来，而不是静默吸收——用户以为还开着沙箱、其实没有，是这类问题里最严重的。

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig/kit"
	"github.com/zeokeel/zeokeel-rig/sandbox"
)

// captureSlog 把默认 logger 重定向到缓冲区，返回读取函数。
func captureSlog(t *testing.T) func() string {
	t.Helper()
	var buffer bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buffer, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return func() string { return buffer.String() }
}

// 审计 1（必修）：`Tools != nil` + `SandboxRunner != nil` → 必须**报错**。
// 自建工具集不会经过 OS 沙箱包装，用户却会以为"还在沙箱里"。
func TestAuditToolsAndRunnerConflictIsAnError(t *testing.T) {
	dir := t.TempDir()
	runner := &kitRunner{}
	_, err := kit.New(kit.Config{
		Model:         "m",
		Provider:      &scriptProvider{},
		Root:          dir,
		Tools:         []kit.Tool{},
		SandboxRunner: runner,
	})
	if err == nil {
		t.Fatal("Tools 与 SandboxRunner 同时设置却没有报错（用户会以为已沙箱）")
	}
	text := err.Error()
	for _, want := range []string{"kit:", "SandboxRunner", "Tools", "沙箱"} {
		if !strings.Contains(text, want) {
			t.Fatalf("错误文案缺少 %q: %s", want, text)
		}
	}
	t.Logf("冲突错误文案: %s", text)
}

// 审计 2：`Tools == nil` + `SandboxRunner != nil` 是**正常用法**，不能报错也不该告警。
func TestAuditRunnerWithDefaultToolsIsFine(t *testing.T) {
	dir := t.TempDir()
	warnings := captureSlog(t)
	_, err := kit.New(kit.Config{
		Model:         "m",
		Provider:      &scriptProvider{},
		Root:          dir,
		SandboxRunner: &kitRunner{},
	})
	if err != nil {
		t.Fatalf("默认工具集 + runner 不该报错: %v", err)
	}
	if text := warnings(); strings.Contains(text, "SandboxRunner") {
		t.Fatalf("正常用法不该告警: %s", text)
	}
}

// 审计 3：`Tools != nil` 且没有其它依赖 → 零告警。
func TestAuditCustomToolsAloneIsQuiet(t *testing.T) {
	dir := t.TempDir()
	warnings := captureSlog(t)
	if _, err := kit.New(kit.Config{
		Model:    "m",
		Provider: &scriptProvider{},
		Root:     dir,
		Tools:    []kit.Tool{},
	}); err != nil {
		t.Fatalf("kit.New: %v", err)
	}
	if text := warnings(); strings.TrimSpace(text) != "" {
		t.Fatalf("不该有告警: %s", text)
	}
}

// 审计 4：`Tools != nil` + 依赖项 → 一条 slog.Warn 列出"哪些不生效"。
func TestAuditCustomToolsWarnsIgnoredDependencies(t *testing.T) {
	dir := t.TempDir()
	warnings := captureSlog(t)
	if _, err := kit.New(kit.Config{
		Model:      "m",
		Provider:   &scriptProvider{},
		Root:       dir,
		Tools:      []kit.Tool{},
		Searcher:   &auditSearcher{},
		SessionLog: &auditSessionLog{},
		Asker:      &auditAsker{answer: "allow"},
		GitUndo:    true,
	}); err != nil {
		t.Fatalf("kit.New: %v", err)
	}
	text := warnings()
	for _, want := range []string{"Searcher", "SessionLog", "GitUndo", "ask_user"} {
		if !strings.Contains(text, want) {
			t.Fatalf("告警里没有列出 %q: %s", want, text)
		}
	}
	// 关键：Asker 仍然驱动审批门禁，不能在告警里说它"失效"。
	if strings.Contains(text, "Asker 不生效") {
		t.Fatalf("Asker 仍然用于审批门禁，不该被说成失效: %s", text)
	}
	t.Logf("告警内容: %s", strings.TrimSpace(text))
}

// 编译期：确保 sandbox.Runner 的 stub 仍然满足接口（kitRunner 在 runner_test.go）。
var _ sandbox.Runner = (*kitRunner)(nil)
