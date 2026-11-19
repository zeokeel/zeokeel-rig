package team

// F-1 回归钉：SendAsync 的旧签名（无 ctx）必须继续可用——它是导出 API，
// 直接改成 SendAsyncContext(ctx, ...) 会让既有宿主编译失败。
// 这里只验证"旧签名仍能编译并投递成功"，不重复测投递语义（见 zz_fix_sendasync_test.go）。

import (
	"context"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
)

func TestSendAsyncLegacySignatureStillWorks(t *testing.T) {
	m := NewManager(newFakeRunner())
	if _, err := m.Spawn(context.Background(), rig.SubagentSpec{Name: "mate", Prompt: "work"}); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer m.CloseAll()

	// 旧签名：编译期即证明兼容（宿主代码 `m.SendAsync(ref, msg)` 不需要改）。
	var legacy func(string, string) error = m.SendAsync
	if err := legacy("mate", "hello"); err != nil {
		t.Fatalf("SendAsync(legacy): %v", err)
	}
	if err := m.SendAsyncContext(context.Background(), "mate", "again"); err != nil {
		t.Fatalf("SendAsyncContext: %v", err)
	}
}
