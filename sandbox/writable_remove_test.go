package sandbox_test

// D2：可写根的运行期**撤销**。
//
// 之前只有"加"（构造函数里的根 / WithWritableRoots / AddWritableRoot 授权），
// 没有"减"：宿主把某个目录交给 agent 之后再也收不回来。这些用例钉住撤销的边界：
//  1. 撤销之后该路径立刻不可写（判定与 OS 后端共用同一份可写面模型）；
//  2. 主根不可撤销——它同时是相对路径的解析基准，删掉会让整个策略失去文件访问；
//  3. 不在策略里的路径返回可识别的错误，而不是静默成功；
//  4. 运行期授权（AddWritableRoot）同样可以被撤销，撤销后不占可写面。

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/zeokeel/zeokeel-rig/sandbox"
)

// TestPolicyRemoveWritableRootRevokesWrite：撤销一个策略根之后，该根不可写，其余
// 可写根不受影响。
func TestPolicyRemoveWritableRootRevokesWrite(t *testing.T) {
	workspace := t.TempDir()
	extra := t.TempDir()
	policy := sandbox.New(sandbox.WithRoots(workspace, extra))

	target := filepath.Join(extra, "out.txt")
	if err := policy.CheckWritablePath(target); err != nil {
		t.Fatalf("撤销前 %q 应当可写: %v", target, err)
	}
	if err := policy.RemoveWritableRoot(extra); err != nil {
		t.Fatalf("RemoveWritableRoot(%q): %v", extra, err)
	}
	if err := policy.CheckWritablePath(target); err == nil {
		t.Fatalf("撤销后 %q 仍可写（可写面没有真正缩小）", target)
	}
	keep := filepath.Join(workspace, "out.txt")
	if err := policy.CheckWritablePath(keep); err != nil {
		t.Fatalf("撤销 %q 误伤了主根可写面: %v", extra, err)
	}
}

// TestPolicyRemoveWritableRootRejectsPrimaryRoot：主根是相对路径的解析基准，撤销它
// 等于让策略失去文件访问，必须报错并保持可写。
func TestPolicyRemoveWritableRootRejectsPrimaryRoot(t *testing.T) {
	workspace := t.TempDir()
	policy := sandbox.New(sandbox.WithRoots(workspace))
	if err := policy.RemoveWritableRoot(workspace); err == nil {
		t.Fatal("撤销主根没有报错（会让所有文件访问失效）")
	}
	if err := policy.CheckWritablePath(filepath.Join(workspace, "out.txt")); err != nil {
		t.Fatalf("报错之后主根反而不可写了: %v", err)
	}
}

// TestPolicyRemoveWritableRootUnknownRoot：不在策略里的路径返回可识别错误。
func TestPolicyRemoveWritableRootUnknownRoot(t *testing.T) {
	policy := sandbox.New(sandbox.WithRoots(t.TempDir()))
	err := policy.RemoveWritableRoot(t.TempDir())
	if !errors.Is(err, sandbox.ErrWritableRootNotFound) {
		t.Fatalf("未知根的错误 = %v, want ErrWritableRootNotFound", err)
	}
}

// TestPolicyRemoveWritableRootRevokesGrant：运行期授权也能撤销，撤销后它不再出现在
// 可写面里。
func TestPolicyRemoveWritableRootRevokesGrant(t *testing.T) {
	workspace := t.TempDir()
	extra := t.TempDir()
	policy := sandbox.New(sandbox.WithRoots(workspace))
	if _, err := policy.AddWritableRoot(extra, "test grant"); err != nil {
		t.Fatalf("AddWritableRoot: %v", err)
	}
	target := filepath.Join(extra, "out.txt")
	if !policy.AllowsWrite(target) {
		t.Fatalf("授权后 %q 应当可写（可写面）", target)
	}
	if err := policy.RemoveWritableRoot(extra); err != nil {
		t.Fatalf("RemoveWritableRoot(授权): %v", err)
	}
	if policy.AllowsWrite(target) {
		t.Fatalf("撤销授权后 %q 仍可写", target)
	}
	for _, path := range policy.AdditionalWritableRoots() {
		if path == extra {
			t.Fatalf("撤销后授权仍留在可写面: %v", policy.AdditionalWritableRoots())
		}
	}
}

// TestRemoveWritableRootOfPrimaryReportsSentinel：撤销**主根**必须报一个可与
// "这个根本来就不在可写面里"区分开的哨兵。
//
// 为什么需要区分：`kit.Runtime.RemoveRoot` 把 ErrWritableRootNotFound 当成
// "这个 Agent 本来就没有这根（例如它是主根），不是错误"直接 continue——于是
// "主根不可撤销"和"未知路径"在 kit 眼里是同一件事，调用方拿到 nil 却什么都没撤销
// （静默成功）。sandbox 侧给出 ErrWritableRootIsPrimary，kit 侧才能把前者报出来。
func TestRemoveWritableRootOfPrimaryReportsSentinel(t *testing.T) {
	workspace := t.TempDir()
	policy := sandbox.New(sandbox.WithRoots(workspace))

	err := policy.RemoveWritableRoot(workspace)
	if err == nil {
		t.Fatal("撤销主根没有报错（会让所有文件访问失效）")
	}
	if !errors.Is(err, sandbox.ErrWritableRootIsPrimary) {
		t.Fatalf("主根错误 = %v, want ErrWritableRootIsPrimary", err)
	}
	// 为了不改变 kit 现有分支行为（它先判 NotFound），主根错误仍然包装
	// ErrWritableRootNotFound；区分时必须**先**判 IsPrimary。这条注释被下面的
	// 断言钉住，等 kit 改完再一起放宽。
	if !errors.Is(err, sandbox.ErrWritableRootNotFound) {
		t.Fatalf("主根错误 = %v, 应当仍包装 ErrWritableRootNotFound（kit 现有分支依赖它）", err)
	}
	// "本来就没有这个根"不能被误报成"主根不可撤销"。
	unknown := policy.RemoveWritableRoot(t.TempDir())
	if !errors.Is(unknown, sandbox.ErrWritableRootNotFound) {
		t.Fatalf("未知根错误 = %v, want ErrWritableRootNotFound", unknown)
	}
	if errors.Is(unknown, sandbox.ErrWritableRootIsPrimary) {
		t.Fatalf("未知根错误 = %v, 不该是 ErrWritableRootIsPrimary", unknown)
	}
	// 报错不能有副作用：主根必须仍然可写。
	if err := policy.CheckWritablePath(filepath.Join(workspace, "out.txt")); err != nil {
		t.Fatalf("报错之后主根反而不可写了: %v", err)
	}
}
