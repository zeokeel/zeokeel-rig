package storagetest

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
)

// runSpillCases 守溢出结果契约（可选能力）。
//
// 溢出结果的特点：它的**读回路径**是模型可见的能力（spill_read 工具），
// 所以"存下来"和"读得回来"必须成对验证——写成功但句柄失效等于把模型的工具输出
// 永久丢掉。
func runSpillCases(t *testing.T, caps Capability, newStorage Factory) {
	t.Run("SpillSaveLoadRoundTrip", func(t *testing.T) {
		s := newStore(t, newStorage)
		spill := requireSpill(t, s, caps)
		ctx := context.Background()
		content := strings.Repeat("输出行\n", 2000)

		ref, err := spill.Save(ctx, rig.ToolCall{ID: "call-1", Name: "bash"}, content)
		if err != nil {
			t.Fatalf("Save: %v", err)
		}
		if ref.ID == "" {
			t.Fatalf("Save 返回了空句柄：模型没有可传回的 ID")
		}
		if ref.Bytes != len(content) {
			t.Fatalf("ref.Bytes = %d，want %d（句柄要能告诉模型「存了多大」）", ref.Bytes, len(content))
		}
		got, err := spill.Load(ctx, ref.ID)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if got != content {
			t.Fatalf("读回内容不一致：got %d 字节，want %d 字节", len(got), len(content))
		}
		if _, err := spill.Load(ctx, "no-such-ref"); !errors.Is(err, rig.ErrSpillNotFound) {
			t.Fatalf("Load(未知句柄) = %v，want rig.ErrSpillNotFound", err)
		}
	})

	t.Run("SpillGCKeepsNewest", func(t *testing.T) {
		s := newStore(t, newStorage)
		spill := requireSpill(t, s, caps)
		ctx := context.Background()

		refs := make([]string, 0, 3)
		for _, name := range []string{"a", "b", "c"} {
			ref, err := spill.Save(ctx, rig.ToolCall{ID: name, Name: name}, "内容-"+name)
			if err != nil {
				t.Fatalf("Save %s: %v", name, err)
			}
			refs = append(refs, ref.ID)
		}
		dropped, err := spill.GC(ctx, 2)
		if err != nil {
			t.Fatalf("GC: %v", err)
		}
		if dropped != 1 {
			t.Fatalf("GC(keep=2) 删了 %d 条，want 1 条", dropped)
		}
		if _, err := spill.Load(ctx, refs[0]); !errors.Is(err, rig.ErrSpillNotFound) {
			t.Fatalf("最旧的一条没有被回收：Load = %v，want ErrSpillNotFound", err)
		}
		for _, id := range refs[1:] {
			if _, err := spill.Load(ctx, id); err != nil {
				t.Fatalf("被保留的 %q 读不回来：%v", id, err)
			}
		}
	})
}
