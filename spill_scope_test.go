package rig_test

// 溢出结果的作用域：GC 只能回收"同一个会话"的行。文件后端早就按文件名前缀分区了，
// 数据库后端要靠 ctx 上的作用域才能做同样的事——作用域丢了，GC 就退回全局裁剪，
// 会话 A 的回收会让会话 B 的 `ref:` 句柄失效（数据丢失）。

import (
	"context"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
)

// TestSpillScopeRoundTrip 钉住 ctx 上作用域的读写语义。
func TestSpillScopeRoundTrip(t *testing.T) {
	if got := rig.SpillScope(context.Background()); got != "" {
		t.Fatalf("没有作用域的 ctx：SpillScope = %q，want 空串", got)
	}
	// 这里**故意**传 nil：SpillScope 的文档契约是"nil ctx 同样返回空串"，
	// 换成 context.Background() 就测不到 nil 容忍，等于放宽断言。
	//lint:ignore SA1012 故意传 nil ctx，断言 nil 容忍这条契约不被破坏
	if got := rig.SpillScope(nil); got != "" {
		t.Fatalf("nil ctx：SpillScope = %q，want 空串（不得 panic）", got)
	}

	ctx := rig.WithSpillScope(context.Background(), "session-a")
	if got := rig.SpillScope(ctx); got != "session-a" {
		t.Fatalf("SpillScope = %q，want session-a", got)
	}

	// 同一个 ctx 派生出的子 ctx 必须继承作用域（取消、超时、超集 ctx 都很常见）。
	cancelled, cancel := context.WithCancel(ctx)
	defer cancel()
	if got := rig.SpillScope(cancelled); got != "session-a" {
		t.Fatalf("子 ctx 的作用域 = %q，want session-a", got)
	}

	// 覆盖：agent 每次 Run 都可能换会话，后写入的作用域必须生效。
	if got := rig.SpillScope(rig.WithSpillScope(ctx, "session-b")); got != "session-b" {
		t.Fatalf("覆盖后 SpillScope = %q，want session-b", got)
	}

	// 显式空作用域必须真的清空，而不是被外层值顶替：空作用域有自己的回收语义
	// （只回收空作用域的行），不能被误解成"继承外层"或"全库"。
	if got := rig.SpillScope(rig.WithSpillScope(ctx, "")); got != "" {
		t.Fatalf("显式空作用域后 SpillScope = %q，want 空串", got)
	}
}
