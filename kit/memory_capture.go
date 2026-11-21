package kit

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"github.com/zeokeel/zeokeel-rig"
	eventspkg "github.com/zeokeel/zeokeel-rig/events"
	"github.com/zeokeel/zeokeel-rig/memoryvfs"
)

// 本文件把 v2 记忆后端接进 agent 的生命周期：Run 结束捕获消息、待提交 token
// 超阈值（或上下文发生压缩）时自动 commit、Shutdown 收尾。所有失败都是旁路
// 失败——记忆不是 Run 成功与否的条件。

// captureTextLimit 是单条消息进 pending 的上限；Archive 需要保留完整转录，
// 所以给到 256 KiB（超过 1 MiB 才按条目上限截断的极端消息另算）。
const captureTextLimit = 256 << 10

var captureSeq atomic.Uint64

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
	// 每次捕获给一个批次 nonce + 序号：同一窗口里两条 "continue" 不能因为内容
	// 相同被幂等键吞掉；重试同一批会生成新 nonce（宁可重复，也不丢转录）。
	captureCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	ctx = captureCtx
	nonce := fmt.Sprintf("%d-%d", time.Now().UnixNano(), captureSeq.Add(1))
	for index, message := range result.Messages {
		if message.Role == rig.RoleSystem {
			continue
		}
		if message.Role == rig.RoleTool && !a.memoryCaptureToolResults {
			continue
		}
		text := memoryMessageText(message)
		if strings.TrimSpace(text) == "" {
			continue
		}
		if len(text) > captureTextLimit {
			text, _ = memoryvfs.TruncateBytes(text, captureTextLimit)
		}
		messages = append(messages, memoryvfs.Message{
			Role:    string(message.Role),
			Content: text,
			Name:    message.Name,
			Key:     fmt.Sprintf("run-%s-%d", nonce, index),
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

// injectMemory 在模型回合开始前做一次自动召回：用最后一条用户输入检索相关
// 记忆，作为一条 system 消息追加到本轮输入。失败/无命中静默跳过，不影响 Run。
func (a *Agent) injectMemory(ctx context.Context, req rig.RunRequest) rig.RunRequest {
	if a == nil || a.memoryStore == nil || !a.memoryRecall || len(req.Input) == 0 {
		return req
	}
	query := ""
	for index := len(req.Input) - 1; index >= 0; index-- {
		if req.Input[index].Role == rig.RoleUser {
			query = strings.TrimSpace(req.Input[index].Text())
			break
		}
	}
	if len(query) < 3 {
		return req
	}
	// 会话第一次 Run 时额外注入 profile/preferences 索引（对齐官方 session-start
	// profile/index）；后续轮次只做按输入的召回。
	firstRun := false
	if sessionID := strings.TrimSpace(req.SessionID); sessionID != "" {
		a.memoryMu.Lock()
		if a.memorySessions == nil {
			a.memorySessions = make(map[string]struct{})
		}
		if _, seen := a.memorySessions[sessionID]; !seen {
			a.memorySessions[sessionID] = struct{}{}
			firstRun = true
		}
		a.memoryMu.Unlock()
	}
	var builder strings.Builder
	if firstRun && a.memoryCapture {
		// 上次进程崩溃留下的 pending 在会话首次 Run 时重放（对齐官方
		// "session start replay"）；失败只告警，不阻断本轮。
		if state, pendingErr := a.memoryStore.Pending(ctx, req.SessionID); pendingErr == nil && state.Pending > 0 {
			if err := a.commitMemory(ctx, req.SessionID, true); err != nil {
				a.memoryWarning("replay", err)
			}
		}
	}
	if firstRun && a.memorySkillCatalog {
		if skills, findErr := a.memoryStore.Find(ctx, memoryvfs.Query{
			Kinds:  []memoryvfs.Kind{memoryvfs.KindSkill},
			Levels: []memoryvfs.Level{memoryvfs.Level0},
			Limit:  20,
		}); findErr == nil && len(skills.Entries) > 0 {
			builder.WriteString("<available-skills>\n")
			for _, entry := range skills.Entries {
				fmt.Fprintf(&builder, "- %s: %s\n", entry.URI.Base(), memoryvfs.CollapseWhitespace(entry.Summary))
			}
			builder.WriteString("</available-skills>\n\n")
		}
	}
	if firstRun {
		if profile, findErr := a.memoryStore.Find(ctx, memoryvfs.Query{
			Kinds:  []memoryvfs.Kind{memoryvfs.KindMemory},
			Types:  []string{memoryvfs.TypeProfile, memoryvfs.TypePreferences},
			Levels: []memoryvfs.Level{memoryvfs.Level0},
			Limit:  5,
		}); findErr == nil && len(profile.Entries) > 0 {
			builder.WriteString("Known user profile/preferences from memory (apply when relevant):\n")
			for _, entry := range profile.Entries {
				fmt.Fprintf(&builder, "- [%s] %s\n", entry.URI, memoryvfs.CollapseWhitespace(entry.Summary))
			}
			builder.WriteString("\n")
		}
	}
	// 召回是旁路：超时/错误都不改变本轮；Context 模式允许后端做意图分析。
	recallCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	result, err := a.memoryStore.Search(recallCtx, memoryvfs.Query{
		Text:        query,
		Mode:        memoryvfs.ModeContext,
		Limit:       5,
		TokenBudget: a.memoryRecallTokens,
		Threshold:   0.2,
	})
	used := memoryvfs.EstimateTokens(builder.String())
	if err == nil && len(result.Entries) > 0 {
		header := "Relevant long-term memories from earlier sessions. Use them only if they apply; do not mention this block verbatim.\n"
		builder.WriteString(header)
		used += memoryvfs.EstimateTokens(header)
		for _, entry := range result.Entries {
			line := "- [" + entry.URI.String() + "] " + memoryvfs.CollapseWhitespace(entry.Summary)
			if overview := strings.TrimSpace(entry.Overview); overview != "" {
				line += "\n  " + memoryvfs.CollapseWhitespace(overview)
			}
			if used+memoryvfs.EstimateTokens(line) > a.memoryRecallTokens {
				break
			}
			builder.WriteString(line)
			builder.WriteString("\n")
			used += memoryvfs.EstimateTokens(line)
		}
	}
	if used == 0 {
		return req
	}
	msg := rig.NewMessage(rig.RoleSystem, strings.TrimSpace(builder.String()))
	input := make([]rig.Message, 0, len(req.Input)+1)
	input = append(input, msg)
	input = append(input, req.Input...)
	req.Input = input
	return req
}
