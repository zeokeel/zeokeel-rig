package storagetest

import (
	"context"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/storage"
)

// SessionAdmin 是**可选**扩展：会话语义上的"列表 + 删除"。
//
// storage.Storage 刻意只有只读五方法——List/Delete 的参数与返回值是 store.Meta /
// store.Filter 这类后端自有类型，塞进契约会让 storage 与具体后端互相 import 成环
// 结果是"会话列表在契约里没有位置"，
// 宿主只能拿具体类型。
//
// 基线套件仍然要守"列表顺序"与"删除后不可读"这两条用户可见语义，于是把它们声明
// 成一个可选接口：后端的 storage.Storage 实现顺手提供这两个方法即可参与，没提供
// 就 skip（并打印原因）。**不需要 import 本包**——Go 的结构化接口让方法集自动匹配，
// 因此这个接口对实现方零耦合。
type SessionAdmin interface {
	// ListSessions 返回全部会话 ID，**最近更新的在前**（列表顺序即续传/侧栏顺序）。
	ListSessions(ctx context.Context) ([]string, error)
	// DeleteSession 删除一个会话及其消息；会话不存在时返回错误。
	DeleteSession(ctx context.Context, sessionID string) error
}

// sessionAdmin 从实现里发现可选的会话管理能力。
func sessionAdmin(t *testing.T, s storage.Storage) SessionAdmin {
	t.Helper()
	admin, ok := s.(SessionAdmin)
	if !ok {
		t.Skip("skip: backend does not expose session listing/deletion (storagetest.SessionAdmin)")
	}
	return admin
}

func runSessionAdminCases(t *testing.T, caps Capability, newStorage Factory) {
	t.Run("SessionsListNewestFirst", func(t *testing.T) {
		if !caps.History {
			skipNoCapability(t, "history")
		}
		s := newStore(t, newStorage)
		admin := sessionAdmin(t, s)
		history := requireHistory(t, s, caps)
		ctx := context.Background()

		if err := history.Append(ctx, sessionA, rig.NewMessage(rig.RoleUser, "旧会话")); err != nil {
			t.Fatalf("Append 旧会话: %v", err)
		}
		// 睡眠让两次写入的 UpdatedAt 明确不同：列表顺序按更新时间排，而若干后端的
		// 时间粒度是毫秒甚至秒——不睡就会把"顺序"测成"随机"。
		time.Sleep(10 * time.Millisecond)
		if err := history.Append(ctx, sessionB, rig.NewMessage(rig.RoleUser, "新会话")); err != nil {
			t.Fatalf("Append 新会话: %v", err)
		}

		ids, err := admin.ListSessions(ctx)
		if err != nil {
			t.Fatalf("ListSessions: %v", err)
		}
		if len(ids) != 2 {
			t.Fatalf("ListSessions = %v，want 2 个会话", ids)
		}
		if ids[0] != sessionB || ids[1] != sessionA {
			t.Fatalf("ListSessions = %v，want 最近更新的在前 [%s %s]", ids, sessionB, sessionA)
		}
	})

	t.Run("SessionsDeleteMakesUnreadable", func(t *testing.T) {
		if !caps.History {
			skipNoCapability(t, "history")
		}
		s := newStore(t, newStorage)
		admin := sessionAdmin(t, s)
		history := requireHistory(t, s, caps)
		ctx := context.Background()

		if err := history.Append(ctx, sessionA, rig.NewMessage(rig.RoleUser, "会被删掉")); err != nil {
			t.Fatalf("Append: %v", err)
		}
		if err := admin.DeleteSession(ctx, sessionA); err != nil {
			t.Fatalf("DeleteSession: %v", err)
		}
		messages, err := history.Load(ctx, sessionA)
		if err != nil {
			t.Fatalf("删除后 Load 返回错误 %v：未知会话应当是空历史（(nil, nil)），"+
				"否则续传路径会把它当成存储故障", err)
		}
		if len(messages) != 0 {
			t.Fatalf("删除后仍读到 %d 条消息：删除必须让会话不可读", len(messages))
		}
		ids, err := admin.ListSessions(ctx)
		if err != nil {
			t.Fatalf("ListSessions: %v", err)
		}
		for _, id := range ids {
			if id == sessionA {
				t.Fatalf("删除后列表里还有 %q：%v", sessionA, ids)
			}
		}
		if err := admin.DeleteSession(ctx, sessionA); err == nil {
			t.Fatalf("删除不存在的会话返回了 nil：调用方无法区分「删掉了」与「本来就没有」")
		}
	})
}
