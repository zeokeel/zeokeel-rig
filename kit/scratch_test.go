package kit_test

// ScratchDir 的 kit 接线：宿主通过 Config 给一个私有临时目录，命令环境里的
// TMPDIR/TMP/TEMP 必须真的指向它（read-only 档下"批准了也跑不动"的根因之一）。

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig/kit"
)

func TestKitScratchDirReachesCommandEnv(t *testing.T) {
	scratch := filepath.Join(t.TempDir(), "scratch")
	agent, err := kit.New(kit.Config{Model: "m", Root: t.TempDir(), ScratchDir: scratch})
	if err != nil {
		t.Fatalf("kit.New: %v", err)
	}
	defer func() { _ = agent.Shutdown(context.Background()) }()

	policy := agent.Policy()
	if policy == nil {
		t.Fatal("kit 没有构造策略")
	}
	if got := policy.ScratchDir(); got != scratch {
		t.Fatalf("ScratchDir() = %q, want %q", got, scratch)
	}
	if info, err := os.Stat(scratch); err != nil || !info.IsDir() {
		t.Fatalf("scratch 目录没有被创建: %v", err)
	}
	env := policy.CommandEnv(os.Environ())
	found := map[string]string{}
	for _, entry := range env {
		if key, value, ok := strings.Cut(entry, "="); ok {
			found[key] = value
		}
	}
	for _, key := range []string{"TMPDIR", "TMP", "TEMP"} {
		if found[key] != scratch {
			t.Fatalf("%s = %q, want %q", key, found[key], scratch)
		}
	}
}
