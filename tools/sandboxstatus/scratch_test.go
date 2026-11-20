package sandboxstatus_test

// 私有 scratch 目录在 sandbox_status 里的如实展示（A4）。
//
// 以前 read-only 档报 tempWritable=true（只反映了两个开关的默认值），而实际上
// 那一档连 /tmp 都不可写。现在必须分三件事说清：这一档有没有可写的临时目录
// （writable）、那个目录是哪条路径（writablePath/writableRoots）、以及 private
// scratch 有没有配好（scratch / scratchError）。

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/zeokeel/zeokeel-rig/sandbox"
	"github.com/zeokeel/zeokeel-rig/tools/sandboxstatus"
)

// tempWritableView 取 tempWritable 对象。
func tempWritableView(t *testing.T, decoded map[string]any) map[string]any {
	t.Helper()
	temp, ok := decoded["tempWritable"].(map[string]any)
	if !ok {
		t.Fatalf("tempWritable = %#v, want an object", decoded["tempWritable"])
	}
	return temp
}

// TestReadOnlyWithScratchReportsWritableTemp 钉住 read-only + scratch：
// tempWritable.writable=true，writablePath 与 writableRoots 都指向 scratch。
func TestReadOnlyWithScratchReportsWritableTemp(t *testing.T) {
	scratch := filepath.Join(t.TempDir(), "scratch")
	policy := sandbox.New(
		sandbox.WithRoots(t.TempDir()),
		sandbox.WithMode(sandbox.ModeReadOnly),
		sandbox.WithScratchDir(scratch),
	)
	decoded := decode(t, invoke(t, sandboxstatus.New(policy)))

	if got := decoded["scratch"]; got != scratch {
		t.Fatalf("scratch = %v, want %q", got, scratch)
	}
	temp := tempWritableView(t, decoded)
	if temp["writable"] != true {
		t.Fatalf("tempWritable.writable = %v, want true（read-only + scratch 时命令有可写临时目录）", temp["writable"])
	}
	if got := temp["writablePath"]; got != scratch {
		t.Fatalf("tempWritable.writablePath = %v, want %q（写的是 scratch，不是 /tmp）", got, scratch)
	}
	if got := stringsOf(t, decoded, "writableRoots"); !containsAll(got, scratch) {
		t.Fatalf("writableRoots = %v, want it to contain %q", got, scratch)
	}
}

// TestReadOnlyWithoutScratchReportsNoWritableTemp 钉住 read-only 无 scratch：
// writable=false，且不把 /tmp 谎报成可写。
func TestReadOnlyWithoutScratchReportsNoWritableTemp(t *testing.T) {
	policy := sandbox.New(sandbox.WithRoots(t.TempDir()), sandbox.WithMode(sandbox.ModeReadOnly))
	decoded := decode(t, invoke(t, sandboxstatus.New(policy)))

	if got := decoded["scratch"]; got != "" {
		t.Fatalf("scratch = %v, want empty", got)
	}
	temp := tempWritableView(t, decoded)
	if temp["writable"] != false {
		t.Fatalf("tempWritable.writable = %v, want false（read-only 下 /tmp 不可写）", temp["writable"])
	}
	if got := stringsOf(t, decoded, "writableRoots"); len(got) != 0 {
		t.Fatalf("writableRoots = %v, want empty", got)
	}
}

// TestWorkspaceWriteWithScratchReportsWritableTemp 钉住 workspace-write + scratch：
// writablePath 指向 scratch（它是命令实际拿到的 TMPDIR）。
func TestWorkspaceWriteWithScratchReportsWritableTemp(t *testing.T) {
	scratch := filepath.Join(t.TempDir(), "scratch")
	policy := sandbox.New(sandbox.WithRoots(t.TempDir()), sandbox.WithScratchDir(scratch))
	decoded := decode(t, invoke(t, sandboxstatus.New(policy)))

	temp := tempWritableView(t, decoded)
	if temp["writable"] != true {
		t.Fatalf("tempWritable.writable = %v, want true", temp["writable"])
	}
	if got := temp["writablePath"]; got != scratch {
		t.Fatalf("tempWritable.writablePath = %v, want %q", got, scratch)
	}
	if got := stringsOf(t, decoded, "writableRoots"); !containsAll(got, scratch) {
		t.Fatalf("writableRoots = %v, want it to contain %q", got, scratch)
	}
}

// TestScratchFailureIsReported 钉住"建不出 scratch 不静默"：错误必须出现在输出里。
func TestScratchFailureIsReported(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("write blocker: %v", err)
	}
	policy := sandbox.New(sandbox.WithRoots(t.TempDir()), sandbox.WithScratchDir(filepath.Join(blocker, "scratch")))
	decoded := decode(t, invoke(t, sandboxstatus.New(policy)))

	message, _ := decoded["scratchError"].(string)
	if message == "" {
		t.Fatalf("scratchError 为空，scratch 建失败必须对模型/宿主可见：%v", decoded)
	}
}
