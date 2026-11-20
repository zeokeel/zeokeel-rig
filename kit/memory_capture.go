package kit

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/zeokeel/zeokeel-rig"
	eventspkg "github.com/zeokeel/zeokeel-rig/events"
	"github.com/zeokeel/zeokeel-rig/memoryvfs"
)

// 本文件把 v2 记忆后端接进 agent 的生命周期：Run 结束捕获消息、待提交 token
// 超阈值（或上下文发生压缩）时自动 commit、Shutdown 收尾。所有失败都是旁路
// 失败——记忆不是 Run 成功与否的条件。

// captureMemory 把本次 Run 追加的消息写进会话的 pending 队列。
func (a *Agent) captureMemory(ctx context.Context, sessionID string, result rig.RunResult, compacted bool) {
	if a == nil || a.memoryStore == nil || !a.memoryCapture {
		return
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" || len(result.Messages) == 0 {
		return
	}
	messages := make([]memoryvfs.Message, 0, len(result.Messages))
	for _, message := range result.Messages {
		if message.Role == rig.RoleSystem {
			continue
		}
		text := memoryMessageText(message)
		if strings.TrimSpace(text) == "" {
			continue
		}
		if len(text) > 8<<10 {
			text, _ = memoryvfs.TruncateBytes(text, 8<<10)
		}
		messages = append(messages, memoryvfs.Message{
			Role:    string(message.Role),
			Content: text,
			Name:    message.Name,
			// 刻意不填 At：store 会按 role|content 生成稳定幂等键，宿主重试/回放
			// 同一回合时不会重复入队；落盘时再补当前时间。
		})
	}
	if len(messages) == 0 {
		return
	}
	if err := a.memoryStore.Capture(ctx, sessionID, messages...); err != nil {
		a.memoryWarning("capture", err)
		return
	}
	a.memoryMu.Lock()
	if a.memorySessions == nil {
		a.memorySessions = make(map[string]struct{})
	}
	a.memorySessions[sessionID] = struct{}{}
	a.memoryMu.Unlock()

	state, err := a.memoryStore.Pending(ctx, sessionID)
	if err != nil {
		a.memoryWarning("pending", err)
		return
	}
	limit := a.memoryCommitTokens
	if limit <= 0 {
		limit = 4000
	}
	if !compacted && state.Tokens < limit {
		return
	}
	if err := a.commitMemory(ctx, sessionID, true); err != nil {
		a.memoryWarning("commit", err)
	}
}

// commitMemory 提交一个会话；extract=false 时只归档、不抽取。
func (a *Agent) commitMemory(ctx context.Context, sessionID string, extract bool) error {
	if a == nil || a.memoryStore == nil {
		return nil
	}
	_, err := a.memoryStore.Commit(ctx, sessionID, memoryvfs.CommitOptions{Extract: extract})
	if err != nil {
		return fmt.Errorf("session %q: %w", sessionID, err)
	}
	return nil
}

// flushMemory 在关停前提交所有捕获过的会话（只提交仍有 pending 的）。
func (a *Agent) flushMemory(ctx context.Context) error {
	if a == nil || a.memoryStore == nil {
		return nil
	}
	a.memoryMu.Lock()
	sessions := make([]string, 0, len(a.memorySessions))
	for sessionID := range a.memorySessions {
		sessions = append(sessions, sessionID)
	}
	a.memoryMu.Unlock()
	if len(sessions) == 0 {
		return nil
	}
	var errs []error
	for _, sessionID := range sessions {
		if err := ctx.Err(); err != nil {
			errs = append(errs, err)
			break
		}
		state, err := a.memoryStore.Pending(ctx, sessionID)
		if err != nil {
			if errors.Is(err, memoryvfs.ErrClosed) {
				continue
			}
			errs = append(errs, fmt.Errorf("session %q: %w", sessionID, err))
			continue
		}
		if state.Pending == 0 {
			continue
		}
		if err := a.commitMemory(ctx, sessionID, true); err != nil {
			if !errors.Is(err, memoryvfs.ErrClosed) {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// closeMemory 关闭 kit 自己打开的 v2 后端（宿主注入的归宿主）。
func (a *Agent) closeMemory() error {
	if a == nil || !a.memoryStoreOwned || a.memoryStore == nil {
		return nil
	}
	if err := a.memoryStore.Close(); err != nil {
		return fmt.Errorf("close memory store: %w", err)
	}
	return nil
}

func (a *Agent) memoryWarning(stage string, err error) {
	slog.Warn("kit: memory capture failed (run result is unaffected)", "stage", stage, "err", err)
	if a.events != nil {
		a.events.emit(eventspkg.KindNotice, noticePayload{Message: "memory " + stage + " failed: " + err.Error()})
	}
}

// messageText 把一条消息里的文本分片拼起来；工具调用参数不进入记忆（工具结果
// 已经是独立消息）。
func memoryMessageText(message rig.Message) string {
	var builder strings.Builder
	for _, part := range message.Content {
		if part.Type != rig.PartText || part.Text == "" {
			continue
		}
		if builder.Len() > 0 {
			builder.WriteString("\n")
		}
		builder.WriteString(part.Text)
	}
	return builder.String()
}
