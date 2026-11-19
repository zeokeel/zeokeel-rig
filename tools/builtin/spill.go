package builtin

// spill_read 工具：把"结果太大，已存起来"的句柄读回来。
//
// 为什么必须在 SDK 里提供：宿主一旦把 `rig.SpillPolicy.Store` 接上，超过阈值的
// 工具结果就不再是文件路径，而是 `ref:` 句柄。SDK 里若没有一条读回路径，溢出就
// 等于**结果永久丢失**——模型只看到"结果太大"和一句它无法使用的 id。
//
// 注册条件与可见性：只有在宿主提供了 SpillStore 时才注册（NewSpillRead），并且
// 不进初始 active 集（discoverable）：它是"真的需要时才用"的工具，不该占常驻
// 预算。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/tools/internal/toolutil"
)

// ToolSpillRead 是模型可见的工具名。
const ToolSpillRead = "spill_read"

// maxSpillReadBytes 是单次读回的字节上限。
//
// 溢出结果本身就是"太大"的东西，原样塞回上下文会立刻触发第二轮溢出。8 KiB 与
// 记忆页正文的上限同一个量级：够模型看清内容，又不至于把窗口吃掉。
const maxSpillReadBytes = 8 << 10

// spillReadTool 按 id 读回一条溢出结果。
type spillReadTool struct {
	store rig.SpillStore
}

var _ rig.Tool = (*spillReadTool)(nil)

// NewSpillRead 构造 spill_read 工具。store 为 nil 时返回 nil（调用方据此不注册）。
func NewSpillRead(store rig.SpillStore) rig.Tool {
	if store == nil {
		return nil
	}
	return &spillReadTool{store: store}
}

// Name 实现 rig.Tool。
func (t *spillReadTool) Name() string { return ToolSpillRead }

// Description 实现 rig.Tool。
func (t *spillReadTool) Description() string {
	return "Read back an oversized tool result that was stored instead of returned " +
		"(the summary you saw had a `ref: <id>` line). Pass that id. " +
		"Output is capped at 8 KiB; ask for a narrower command if you need the rest."
}

// Schema 实现 rig.Tool。
func (t *spillReadTool) Schema() map[string]any {
	return toolutil.ObjectSchema(map[string]any{
		"id": toolutil.StringProp("The handle from the `ref:` line of a spilled tool result."),
	}, "id")
}

// Invoke 实现 rig.Tool。
func (t *spillReadTool) Invoke(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
	var args struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(call.Arguments), &args); err != nil {
		return rig.ToolResult{}, fmt.Errorf("%s: invalid arguments: %w", ToolSpillRead, err)
	}
	id := args.ID
	if id == "" {
		return rig.ToolResult{}, fmt.Errorf("%s: id is required (use the id from the `ref:` line)", ToolSpillRead)
	}
	content, err := t.store.Load(ctx, id)
	if errors.Is(err, rig.ErrSpillNotFound) {
		// 用 %w 保留哨兵语义：宿主既能看到可读文案，也能 errors.Is 分支。
		return rig.ToolResult{}, fmt.Errorf("%s: no stored result with id %q (it may have been collected): %w",
			ToolSpillRead, id, rig.ErrSpillNotFound)
	}
	if err != nil {
		return rig.ToolResult{}, fmt.Errorf("%s: %w", ToolSpillRead, err)
	}
	total := len(content)
	if total > maxSpillReadBytes {
		cut := maxSpillReadBytes
		for cut > 0 && cut < total && !isRuneStart(content[cut]) {
			cut--
		}
		content = content[:cut]
		return rig.ToolResult{Content: fmt.Sprintf(
			"%s\n\n[truncated: showing %d of %d bytes; re-run a narrower command if you need the rest]",
			content, cut, total)}, nil
	}
	return rig.ToolResult{Content: content}, nil
}

// isRuneStart 报告 b 是否是一个 UTF-8 字符的起始字节（续字节是 10xxxxxx）。
func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }
