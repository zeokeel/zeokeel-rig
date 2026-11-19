package kit_test

// 本文件是 spill 的端到端回归（审计 P1）：默认 `SpillPolicy{}` 的溢出结果必须
// **能读回来**。
//
// 旧行为：默认目录是 `os.TempDir()/zeokeel-rig-spill`，不在 read_file 的 roots
// 里 → 模型拿到 `path:` 之后每次读回都是
// `denied: outside the configured roots`，等于结果永久丢失。
//
// 现在：kit 把默认目录解析成 `<Root>/.rig/spill`（workspace 内 → 天然 per-workspace
// 分区、落在可写根内、可读回）。

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/kit"
)

// spillReadbackProvider 是一个"看到溢出摘要就去读它"的假 provider：
//
//	Stream#1 → read_file(big.txt)            （结果超过阈值 → 被 spill）
//	Stream#2 → read_file(<摘要里的 path>, 前 3 行)
//	Stream#3 → 文本收尾
type spillReadbackProvider struct {
	mu         sync.Mutex
	streams    int
	spillPaths []string
	spillReads []string
}

func (p *spillReadbackProvider) Name() string { return "spill-readback" }

func (p *spillReadbackProvider) Stream(_ context.Context, request kit.Request) (kit.Stream, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.streams++
	switch p.streams {
	case 1:
		return &chunkStream{chunks: toolCallScript(kit.ToolCall{
			ID: "call-big", Name: "read_file", Arguments: `{"path":"big.txt"}`,
		})}, nil
	case 2:
		path := spillPathFrom(request.Messages)
		if path == "" {
			return nil, errNoSpillPath
		}
		p.spillPaths = append(p.spillPaths, path)
		return &chunkStream{chunks: toolCallScript(kit.ToolCall{
			ID:        "call-readback",
			Name:      "read_file",
			Arguments: `{"path":` + spillQuoteJSON(path) + `,"offset":1,"limit":3}`,
		})}, nil
	default:
		return &chunkStream{chunks: textScript("done")}, nil
	}
}

// spillPathFrom 在历史里找最后一条溢出摘要并取出 `path:` 行。
func spillPathFrom(messages []kit.Message) string {
	for index := len(messages) - 1; index >= 0; index-- {
		message := messages[index]
		if message.Role != kit.RoleTool {
			continue
		}
		for _, line := range strings.Split(message.Text(), "\n") {
			if path, ok := strings.CutPrefix(line, "path: "); ok {
				return strings.TrimSpace(path)
			}
		}
	}
	return ""
}

func spillQuoteJSON(value string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	return `"` + replacer.Replace(value) + `"`
}

type errNoSpillPathType struct{}

func (errNoSpillPathType) Error() string { return "no spill path in history" }

var errNoSpillPath error = errNoSpillPathType{}

// TestSpillDefaultDirIsReadableFromWorkspace 覆盖"溢出 → 按句柄读回"的完整闭环。
func TestSpillDefaultDirIsReadableFromWorkspace(t *testing.T) {
	root := t.TempDir()
	const marker = "SPILLED-MARKER-LINE-1"
	lines := []string{marker}
	for i := 0; i < 200; i++ {
		lines = append(lines, strings.Repeat("payload ", 20))
	}
	if err := os.WriteFile(filepath.Join(root, "big.txt"), []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("write big.txt: %v", err)
	}

	provider := &spillReadbackProvider{}
	agent, err := kit.New(kit.Config{
		Model:    "m",
		Provider: provider,
		Root:     root,
		Approval: kit.ApprovalAuto,
		// Dir 留空：kit 必须把它解析成 <Root>/.rig/spill。
		Spill: &rig.SpillPolicy{ThresholdBytes: 1024},
	})
	if err != nil {
		t.Fatalf("kit.New: %v", err)
	}
	result, err := agent.Run(context.Background(), kit.RunRequest{
		Input: []kit.Message{kit.NewMessage(kit.RoleUser, "read the big file")},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(provider.spillPaths) != 1 {
		t.Fatalf("provider saw %d spill paths, want 1", len(provider.spillPaths))
	}
	spillPath := provider.spillPaths[0]
	wantDir := filepath.Join(root, ".rig", "spill")
	if !strings.HasPrefix(spillPath, wantDir+string(os.PathSeparator)) {
		t.Fatalf("spill path = %q, want under %q", spillPath, wantDir)
	}
	if _, statErr := os.Stat(spillPath); statErr != nil {
		t.Fatalf("spill file missing: %v", statErr)
	}

	// 读回必须成功：既不能是 denied，也不能是"结果太大又溢出"。
	var readback string
	for _, message := range result.Messages {
		if message.Role == kit.RoleTool && strings.Contains(message.Text(), marker) {
			readback = message.Text()
		}
	}
	if readback == "" {
		t.Fatalf("溢出内容读不回来（历史里没有含 %q 的工具结果）: %s", marker, allToolText(result.Messages))
	}
	if strings.Contains(readback, "outside the configured roots") {
		t.Fatalf("读回被沙箱拒绝: %s", readback)
	}
}

func allToolText(messages []kit.Message) string {
	var builder strings.Builder
	for _, message := range messages {
		if message.Role == kit.RoleTool {
			builder.WriteString("--- ")
			builder.WriteString(message.Text())
			builder.WriteString("\n")
		}
	}
	return builder.String()
}
