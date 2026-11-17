package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/zeokeel/zeokeel-rig"
)

// History 把 Dir 适配成 rig.History：agent 可以直接把它当历史用，每次 Append
// 都会落盘（不是内存缓存）。
//
//	history := st.History(context.Background(), "default")
//	a, err := agent.New(rig.AgentSpec{Provider: p, History: history})
//
// # 会话分流
//
// agent 会把 rig.RunRequest.SessionID 原样传进来，因此**同一个 History 实例可以
// 服务多个会话**：非空 sessionID 就是会话 ID（不存在时自动创建），空 sessionID
// 回落到构造时给的默认 ID。这与 session.Memory 的语义一致，宿主不需要为每个会话
// 新建一个 History。
//
// # 契约
//
//   - Load 对未知会话返回 (nil, nil)（rig.History 要求），不返回 ErrNotFound；
//   - Append 在会话不存在时自动创建（标题取首条用户消息），因此"崩了怎么续"只需
//     要同一个 sessionID；
//   - 实现 rig.HistoryReplacer（Replace），供 kit.Agent.CompactNow 持久化压缩后的
//     窗口；
//   - 其它错误（ErrClosed/ErrCorrupt/ErrInvalidID）原样上报，绝不静默吞掉。
type History struct {
	store *Dir
	id    string
}

// 编译期断言：History 满足 agent 依赖的两份历史契约。
var (
	_ rig.History         = (*History)(nil)
	_ rig.HistoryReplacer = (*History)(nil)
)

// History 返回绑定到默认会话 ID 的 rig.History 适配器。
//
// ctx 只为与 Store 的调用风格保持一致（构造本身不做 I/O，不读盘、不创建会话）；
// 真正的取消/关闭判断发生在每次 Load/Append/Replace 上。
func (d *Dir) History(ctx context.Context, id string) *History {
	return &History{store: d, id: id}
}

// sessionID 决定一次调用落在哪个会话上：非空参数优先，否则用默认 ID。
func (h *History) sessionID(sessionID string) string {
	if sessionID != "" {
		return sessionID
	}
	return h.id
}

// Load 实现 rig.History。
func (h *History) Load(ctx context.Context, sessionID string) ([]rig.Message, error) {
	_, messages, err := h.store.Load(ctx, h.sessionID(sessionID))
	if errors.Is(err, ErrNotFound) {
		// rig.History 契约：未知会话返回 nil，不是错误。
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return messages, nil
}

// Append 实现 rig.History：一次调用是一批，全部落盘或全部不写。
func (h *History) Append(ctx context.Context, sessionID string, messages ...rig.Message) error {
	if len(messages) == 0 {
		return nil
	}
	id := h.sessionID(sessionID)
	if _, err := h.store.AppendBatch(ctx, id, messages...); err != nil {
		if !errors.Is(err, ErrNotFound) {
			return err
		}
		// 首次写入：自动建会话后重试一次。并发下另一个 goroutine 可能抢先建好，
		// 那时 Create 返回 ErrExists，重试照样成立。
		if createErr := h.ensureSession(ctx, id); createErr != nil {
			return createErr
		}
		_, err = h.store.AppendBatch(ctx, id, messages...)
		return err
	}
	return nil
}

// Replace 实现 rig.HistoryReplacer：原子替换整个会话窗口（上下文压缩用）。
func (h *History) Replace(ctx context.Context, sessionID string, messages []rig.Message) error {
	id := h.sessionID(sessionID)
	if err := h.ensureSession(ctx, id); err != nil {
		return err
	}
	_, err := h.store.Replace(ctx, id, messages)
	return err
}

// ensureSession 让会话一定存在（已存在视为成功）。
func (h *History) ensureSession(ctx context.Context, id string) error {
	if _, err := h.store.Create(ctx, Meta{ID: id}); err != nil && !errors.Is(err, ErrExists) {
		return fmt.Errorf("session store: auto-create session %q: %w", id, err)
	}
	return nil
}

// String 便于日志与测试诊断（不暴露任何会话内容）。
func (h *History) String() string {
	if h == nil || h.store == nil {
		return "store.History(<nil>)"
	}
	return fmt.Sprintf("store.History(dir=%s, defaultID=%q)", h.store.Dir(), h.id)
}
