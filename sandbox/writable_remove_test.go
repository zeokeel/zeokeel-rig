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
