// 本文件从**外部**（package storage_test）验证聚合契约。
//
// 放在外部包是刻意的：契约的消费者与本仓之外的实现（例如 session/sqlite 子模块）
// 都在 package storage 之外，只有这样才能证明「别人实现得了」；包内测试可以通过
// 访问未导出字段走捷径，反而证明不了这一点。
package storage_test

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/memory"
	"github.com/zeokeel/zeokeel-rig/storage"
)

// ---------------------------------------------------------------------------
// 测试内的假实现：5 个方法齐全，零依赖、零 IO。
//
// 它们存在的唯一目的是回答一个问题：「这个契约能被实现吗？」如果契约不小心
// 引用了某个具体后端的私有类型（store.Meta、sqlite 的某张表…），这里就实现不出来，
// 编译期立刻红。这也是契约用「编译探针」验证的同一手法。
// ---------------------------------------------------------------------------

// fakeStorage 是满足 storage.Storage 的假实现。字段即返回值，用于验证访问器
// 返回的**就是**注入的那个实例（而不是包装/复制出来的等价物）。
type fakeStorage struct {
	history     rig.History
	checkpoints rig.CheckpointStore
	wiki        memory.Wiki
	spill       rig.SpillStore
	closeErr    error
	closed      int
}

// 编译期断言：假实现满足契约。这一行是本文件最重要的一行——契约不可实现时，
// 整个测试包连编译都过不去。
var _ storage.Storage = (*fakeStorage)(nil)

func (f *fakeStorage) History() rig.History             { return f.history }
func (f *fakeStorage) Checkpoints() rig.CheckpointStore { return f.checkpoints }
func (f *fakeStorage) Memory() memory.Wiki              { return f.wiki }
func (f *fakeStorage) Spill() rig.SpillStore            { return f.spill }

func (f *fakeStorage) Close() error {
	f.closed++
	return f.closeErr
}

// stubHistory 是 rig.History 的最小实现。
type stubHistory struct{ appended int }

func (h *stubHistory) Load(ctx context.Context, sessionID string) ([]rig.Message, error) {
	return nil, nil
}

func (h *stubHistory) Append(ctx context.Context, sessionID string, messages ...rig.Message) error {
	h.appended += len(messages)
	return nil
}

// stubCheckpoints 是 rig.CheckpointStore 的最小实现。
type stubCheckpoints struct{ saved int }

func (c *stubCheckpoints) Load(ctx context.Context, sessionID string) (*rig.Checkpoint, error) {
	return nil, nil
}

func (c *stubCheckpoints) Save(ctx context.Context, sessionID string, checkpoint *rig.Checkpoint) error {
	c.saved++
	return nil
}

func (c *stubCheckpoints) Clear(ctx context.Context, sessionID string) error { return nil }

// stubSpill 是 rig.SpillStore 的最小实现；读不到句柄时按契约返回 ErrSpillNotFound。
type stubSpill struct{}

func (s *stubSpill) Save(ctx context.Context, call rig.ToolCall, content string) (rig.SpillRef, error) {
	return rig.SpillRef{ID: "spill-1", Bytes: len(content)}, nil
}

func (s *stubSpill) Load(ctx context.Context, id string) (string, error) {
	return "", rig.ErrSpillNotFound
}

func (s *stubSpill) GC(ctx context.Context, keep int) (int, error) { return 0, nil }

// stubWiki 是 memory.Wiki 的最小实现（六个方法）。
type stubWiki struct{}

func (w *stubWiki) Index(ctx context.Context) ([]memory.PageMeta, error) { return nil, nil }

func (w *stubWiki) Read(ctx context.Context, name string) (memory.Page, error) {
	return memory.Page{}, memory.ErrNotFound
}

func (w *stubWiki) Write(ctx context.Context, page memory.Page) error { return nil }

func (w *stubWiki) Append(ctx context.Context, name, text string) error { return nil }

func (w *stubWiki) Delete(ctx context.Context, name string) error { return nil }

func (w *stubWiki) Search(ctx context.Context, query string) ([]memory.PageMeta, error) {
	return nil, nil
}

// newFake 装配一个 5 个方法都齐全的假实现。
func newFake() *fakeStorage {
	return &fakeStorage{
		history:     &stubHistory{},
		checkpoints: &stubCheckpoints{},
		wiki:        &stubWiki{},
		spill:       &stubSpill{},
	}
}

// ---------------------------------------------------------------------------
// 用例 1：契约可实现
// ---------------------------------------------------------------------------

// TestStorageInterfaceIsSatisfiable 证明 storage.Storage 是一个**可实现**的契约：
// 用测试内的假实现（5 个方法齐全）满足它，并把契约的形状钉死。
//
// 这个用例同时承担「防擅自加方法」的职责：方法数是 5、名字与返回类型都与
// 契约形状一致。以后谁想往主接口里塞 Sessions()/Costs()，这里先红——
// 主接口一旦塞进可选能力，第二个实现（目录后端、内存后端）就永远不达标。
func TestStorageInterfaceIsSatisfiable(t *testing.T) {
	fake := newFake()
	var st storage.Storage = fake

	// 1. 五个访问器必须返回**注入的同一个实例**（不是包装、不是复制）。
	if got := st.History(); got != fake.history {
		t.Errorf("History() = %v, 期望原样返回注入的实例", got)
	}
	if got := st.Checkpoints(); got != fake.checkpoints {
		t.Errorf("Checkpoints() = %v, 期望原样返回注入的实例", got)
	}
	if got := st.Memory(); got != fake.wiki {
		t.Errorf("Memory() = %v, 期望原样返回注入的实例", got)
	}
	if got := st.Spill(); got != fake.spill {
		t.Errorf("Spill() = %v, 期望原样返回注入的实例", got)
	}

	// 2. 取到的句柄必须真的能用（形状对但语义不通的契约没有价值）。
	ctx := context.Background()
	if err := st.History().Append(ctx, "s1", rig.NewMessage(rig.RoleUser, "hi")); err != nil {
		t.Fatalf("History().Append: %v", err)
	}
	if err := st.Checkpoints().Save(ctx, "s1", &rig.Checkpoint{SessionID: "s1"}); err != nil {
		t.Fatalf("Checkpoints().Save: %v", err)
	}
	if _, err := st.Spill().Save(ctx, rig.ToolCall{}, "big"); err != nil {
		t.Fatalf("Spill().Save: %v", err)
	}
	if _, err := st.Spill().Load(ctx, "nope"); !errors.Is(err, rig.ErrSpillNotFound) {
		t.Fatalf("Spill().Load(未知句柄) = %v, 期望 rig.ErrSpillNotFound", err)
	}
	if _, err := st.Memory().Read(ctx, "missing"); !errors.Is(err, memory.ErrNotFound) {
		t.Fatalf("Memory().Read(不存在) = %v, 期望 memory.ErrNotFound", err)
	}

	// 3. Close 必须把后端的错误原样透出（宿主据此判断收尾是否成功）。
	if err := st.Close(); err != nil {
		t.Fatalf("Close() = %v, 期望 nil", err)
	}
	if fake.closed != 1 {
		t.Errorf("Close 调用计数 = %d, 期望 1", fake.closed)
	}
	wantErr := errors.New("disk on fire")
	fake.closeErr = wantErr
	if err := st.Close(); !errors.Is(err, wantErr) {
		t.Errorf("Close() = %v, 期望透出 %v", err, wantErr)
	}

	// 4. 契约形状：恰好 5 个无参访问器 + Close，返回类型与契约文档一致。
	//    多一个方法就红，那是「往主接口塞可选能力」的信号。
	iface := reflect.TypeOf((*storage.Storage)(nil)).Elem()
	want := map[string]reflect.Type{
		"History":     reflect.TypeOf((*rig.History)(nil)).Elem(),
		"Checkpoints": reflect.TypeOf((*rig.CheckpointStore)(nil)).Elem(),
		"Memory":      reflect.TypeOf((*memory.Wiki)(nil)).Elem(),
		"Spill":       reflect.TypeOf((*rig.SpillStore)(nil)).Elem(),
		"Close":       reflect.TypeOf((*error)(nil)).Elem(),
	}
	if iface.NumMethod() != len(want) {
		t.Fatalf("storage.Storage 有 %d 个方法（%v），期望恰好 %d 个：只读五方法起步，可选能力不要塞进主接口",
			iface.NumMethod(), methodNames(iface), len(want))
	}
	for name, wantOut := range want {
		m, ok := iface.MethodByName(name)
		if !ok {
			t.Errorf("storage.Storage 缺少方法 %s", name)
			continue
		}
		// 接口方法的 Type 只含签名（不含接收者）：访问器与 Close 都必须无参。
		if m.Type.NumIn() != 0 {
			t.Errorf("%s 期望是无参方法，实际有 %d 个参数", name, m.Type.NumIn())
		}
		if m.Type.NumOut() != 1 {
			t.Errorf("%s 期望恰好 1 个返回值，实际 %d 个", name, m.Type.NumOut())
			continue
		}
		if got := m.Type.Out(0); got != wantOut {
			t.Errorf("%s 返回类型 = %v, 期望 %v", name, got, wantOut)
		}
	}
}

// methodNames 汇总接口的方法名，供失败信息使用。
func methodNames(iface reflect.Type) []string {
	names := make([]string, 0, iface.NumMethod())
	for i := 0; i < iface.NumMethod(); i++ {
		names = append(names, iface.Method(i).Name)
	}
	return names
}

// ---------------------------------------------------------------------------
// 用例 2：分层铁律
// ---------------------------------------------------------------------------

// TestStorageDoesNotImportSessionStore 断言 storage 的依赖里没有 session/store。
//
// 为什么这是个**必须**存在的用例：`storage` 是 layer 1，`session/store` 是 layer 2
// 分层铁律）。而 storage.Storage 恰好要适配 `*store.Dir`，所以「顺手把
// DirStorage 适配器放进 storage」是极其自然的一步——那一步会让 layer 1 依赖
// layer 2，成环，且把 storage 的所有消费者拖进 session/store。适配器归属在
// session/store 侧。
//
// 两层检查：
//   - 解析源码 import（必然执行、不依赖工具链）；
//   - `go list -deps` 查**传递**依赖（工具链缺失时跳过这一段，但绝不因此放过第一层）。
func TestStorageDoesNotImportSessionStore(t *testing.T) {
	root, modPath := repoRoot(t)
	forbidden := []string{modPath + "/session/store", modPath + "/session/sqlite"}

	// 第一层：storage 目录下所有**非测试**源码的 import 声明。
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("ReadDir(.): %v", err)
	}
	files := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("ParseFile(%s): %v", name, err)
		}
		files++
		for _, spec := range file.Imports {
			path, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				t.Fatalf("%s: 无法解析 import %s", name, spec.Path.Value)
			}
			for _, bad := range forbidden {
				if path == bad {
					t.Errorf("%s 直接 import 了 %s：storage 是 layer 1，不得依赖 layer 2。"+
						"DirStorage 适配器应放在 session/store 侧", name, strings.TrimPrefix(bad, modPath+"/"))
				}
			}
		}
	}
	if files == 0 {
		t.Fatalf("storage 目录下没有非测试源码文件")
	}

	// 第二层：传递依赖。直接 import 干净但间接引进来同样会成环。
	if _, err := exec.LookPath("go"); err != nil {
		t.Logf("跳过传递依赖检查：找不到 go 工具链（%v）", err)
		return
	}
	cmd := exec.Command("go", "list", "-deps", "-f", "{{.ImportPath}}", ".")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps ./storage 失败：%v\n%s", err, out)
	}
	deps := strings.Split(strings.TrimSpace(string(out)), "\n")
	for _, dep := range deps {
		for _, bad := range forbidden {
			if dep == bad {
				t.Errorf("storage 的传递依赖里出现 %s（go list -deps）：layer 1 不得吃进 layer 2",
					strings.TrimPrefix(bad, modPath+"/"))
			}
		}
	}
}

// ---------------------------------------------------------------------------
// 用例 3：模块清单登记
// ---------------------------------------------------------------------------

// TestStorageIsRegisteredAsLeafCapability 断言 storage 已进入
// internal/rigdoc/modularity_test.go 的 leafCapabilities 名单。
//
// 这张名单就是 SDK 的模块清单，进不进名单是有实际后果的：
// 名单里的包会被 TestLeafCapabilitiesDoNotImportAggregators 逐个检查「不得 import
// kit / tools/full」。本包必须被这条守护覆盖——否则将来有人为了顺手给
// Storage 加一个 kit 类型，聚合层就会悄悄渗进 layer 1。
//
// 名单是 `_test.go` 里的包级变量，外部无法 import（rigdoc 是 internal 且只在测试里
// 定义），所以这里解析源码：名字要么精确匹配，要么明确不存在，没有猜的空间。
func TestStorageIsRegisteredAsLeafCapability(t *testing.T) {
	root, _ := repoRoot(t)
	path := filepath.Join(root, "internal", "rigdoc", "modularity_test.go")
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatalf("解析 %s: %v", path, err)
	}

	var list *ast.CompositeLit
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.VAR {
			continue
		}
		for _, spec := range gen.Specs {
			values, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, name := range values.Names {
				if name.Name != "leafCapabilities" || i >= len(values.Values) {
					continue
				}
				if lit, ok := values.Values[i].(*ast.CompositeLit); ok {
					list = lit
				}
			}
		}
	}
	if list == nil {
		t.Fatalf("%s 里找不到 leafCapabilities 的字面量定义（守护测试被改名或改结构了？）", path)
	}

	registered := make(map[string]bool, len(list.Elts))
	var all []string
	for _, elt := range list.Elts {
		basic, ok := elt.(*ast.BasicLit)
		if !ok || basic.Kind != token.STRING {
			continue
		}
		value, err := strconv.Unquote(basic.Value)
		if err != nil {
			t.Fatalf("无法解析 leafCapabilities 元素 %s: %v", basic.Value, err)
		}
		registered[value] = true
		all = append(all, value)
	}
	if !registered["storage"] {
		t.Errorf("leafCapabilities 里没有 \"storage\"：新公开包必须同批登记（见 AGENTS.md 模块清单），"+
			"否则它逃出「叶子包不得 import 聚合层」的守护。当前名单：%v", all)
	}
}

// repoRoot 向上找到仓库根（含 go.mod），返回根目录与 module 路径。
func repoRoot(t *testing.T) (dir, modPath string) {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	for {
		data, err := os.ReadFile(filepath.Join(dir, "go.mod"))
		if err == nil {
			for _, line := range strings.Split(string(data), "\n") {
				line = strings.TrimSpace(line)
				if rest, ok := strings.CutPrefix(line, "module "); ok {
					return dir, strings.TrimSpace(rest)
				}
			}
			t.Fatalf("%s 里没有 module 行", filepath.Join(dir, "go.mod"))
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("从 %s 向上找不到 go.mod", dir)
		}
		dir = parent
	}
}
