package session

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strings"
	"sync"

	"github.com/zeokeel/zeokeel-rig"
)

// MemoryLog 是进程内的 rig.SessionLog 实现。它可安全并发使用，并按 seq 顺序保存事件；
// 事件没有 seq 时 Append 会分配下一个序号。
//
// 它还实现 tools/inspect 使用的可选 SessionLister 扩展（Sessions），因此只需要一个临时
// 会话日志的嵌入方可以同时复用同一类型。
type MemoryLog struct {
	mu      sync.Mutex
	events  []rig.SessionEvent
	nextSeq int64
}

// NewMemoryLog 返回一个空的会话日志。
func NewMemoryLog() *MemoryLog { return &MemoryLog{} }

// Append 保存一个事件并返回保存后的副本。非正 Seq 会替换为日志的下一个序号；更大的
// Seq 会推进计数器。载荷字节会被复制，因此调用方可以复用其切片。
func (l *MemoryLog) Append(event rig.SessionEvent) rig.SessionEvent {
	if l == nil {
		return event
	}
	if event.Payload != nil {
		event.Payload = append(json.RawMessage(nil), event.Payload...)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if event.Seq <= 0 {
		l.nextSeq++
		event.Seq = l.nextSeq
	} else if event.Seq > l.nextSeq {
		l.nextSeq = event.Seq
	}
	l.events = append(l.events, event)
	return event
}

// Len 报告已保存的事件数量。
func (l *MemoryLog) Len() int {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.events)
}

// Sessions 按排序返回日志中出现的会话 id。它是 tools/inspect 使用的可选 SessionLister
// 扩展。
func (l *MemoryLog) Sessions(ctx context.Context) ([]string, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	if l == nil {
		return nil, nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	seen := make(map[string]struct{}, len(l.events))
	for _, event := range l.events {
		if id := strings.TrimSpace(event.SessionID); id != "" {
			seen[id] = struct{}{}
		}
	}
	return slices.Sorted(maps.Keys(seen)), nil
}

// Events 实现 rig.SessionLog：返回 sessionID 中 from <= seq <= to 的事件。空的 sessionID
// 匹配所有会话（适合跨会话追踪）；from <= 0 从开头开始，to <= 0 表示直到结尾。
func (l *MemoryLog) Events(ctx context.Context, sessionID string, from, to int64) ([]rig.SessionEvent, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	if l == nil {
		return nil, nil
	}
	sessionID = strings.TrimSpace(sessionID)
	l.mu.Lock()
	defer l.mu.Unlock()
	events := make([]rig.SessionEvent, 0)
	for _, event := range l.events {
		if sessionID != "" && event.SessionID != sessionID {
			continue
		}
		if from > 0 && event.Seq < from {
			continue
		}
		if to > 0 && event.Seq > to {
			continue
		}
		events = append(events, copyEvent(event))
	}
	slices.SortStableFunc(events, func(a, b rig.SessionEvent) int { return cmp.Compare(a.Seq, b.Seq) })
	return events, nil
}

// Search 实现 rig.SessionLog：返回会话 id、类型或载荷中包含 query（大小写不敏感）的
// 事件。空 query 匹配所有事件；limit <= 0 返回全部匹配项。
func (l *MemoryLog) Search(ctx context.Context, query string, limit int) ([]rig.SessionEvent, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	if l == nil {
		return nil, nil
	}
	query = strings.ToLower(strings.TrimSpace(query))
	l.mu.Lock()
	defer l.mu.Unlock()
	events := make([]rig.SessionEvent, 0)
	for _, event := range l.events {
		if limit > 0 && len(events) >= limit {
			break
		}
		if query != "" && !matches(event, query) {
			continue
		}
		events = append(events, copyEvent(event))
	}
	return events, nil
}

// Last 实现 rig.SessionLog：返回一个会话最近的事件。limit <= 0 返回该会话的全部事件。
func (l *MemoryLog) Last(ctx context.Context, sessionID string, limit int) ([]rig.SessionEvent, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return nil, errors.New("session: session id is required")
	}
	if l == nil {
		return nil, nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	events := make([]rig.SessionEvent, 0)
	for _, event := range l.events {
		if event.SessionID == sessionID {
			events = append(events, copyEvent(event))
		}
	}
	slices.SortStableFunc(events, func(a, b rig.SessionEvent) int { return cmp.Compare(a.Seq, b.Seq) })
	if limit > 0 && len(events) > limit {
		events = events[len(events)-limit:]
	}
	return events, nil
}

func matches(event rig.SessionEvent, lowerQuery string) bool {
	if strings.Contains(strings.ToLower(event.SessionID), lowerQuery) ||
		strings.Contains(strings.ToLower(event.Type), lowerQuery) {
		return true
	}
	return strings.Contains(strings.ToLower(string(event.Payload)), lowerQuery)
}

func copyEvent(event rig.SessionEvent) rig.SessionEvent {
	if event.Payload != nil {
		event.Payload = append(json.RawMessage(nil), event.Payload...)
	}
	return event
}

func contextError(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	return ctx.Err()
}
