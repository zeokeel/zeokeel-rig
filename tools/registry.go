package tools

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"

	"github.com/zeokeel/zeokeel-rig"
)

// Registry 是按名字索引、并发安全的工具集合。
//
// 零值不可用；请用 NewRegistry 或 &Registry{} 构造。（零值在第一次 Register
// 初始化内部 map 后也可用，但文档推荐的构造方式是 NewRegistry。）
type Registry struct {
	mu    sync.RWMutex
	tools map[string]rig.Tool
}

// NewRegistry 创建包含给定工具的注册表。在编译期注册路径上，非法工具或重名属于
// 编程错误，因此 NewRegistry 选择 panic 而不是静默丢弃；工具集合不是静态的、
// 需要处理错误时请改用 Register。
func NewRegistry(tools ...rig.Tool) *Registry {
	registry := &Registry{tools: make(map[string]rig.Tool, len(tools))}
	for _, t := range tools {
		if err := registry.Register(t); err != nil {
			panic("tool: NewRegistry: " + err.Error())
		}
	}
	return registry
}

// Register 注册一个工具。工具为 nil、名字为空或名字重复都会返回错误；
// 出错时注册表保持不变。
func (r *Registry) Register(t rig.Tool) error {
	if toolIsNil(t) {
		return fmt.Errorf("tool: nil tool")
	}
	name := t.Name()
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("tool: tool with an empty name")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.tools == nil {
		r.tools = make(map[string]rig.Tool)
	}
	if _, exists := r.tools[name]; exists {
		return fmt.Errorf("tool: duplicate tool name %q", name)
	}
	r.tools[name] = t
	return nil
}

// Unregister 按名字移除一个工具，并报告它是否存在。它是 Register 的逆操作，
// 供动态组装的工具集合使用（产品层在卸载模块时用它回收该模块贡献的工具）。
func (r *Registry) Unregister(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.tools == nil {
		return false
	}
	if _, exists := r.tools[name]; !exists {
		return false
	}
	delete(r.tools, name)
	return true
}

// Specs 返回所有工具的模型侧规格，按名字排序。返回的切片与其中的 schema map
// 是工具自身的值，调用方不得修改（注册表是共享的）。
func (r *Registry) Specs() []rig.ToolSpec {
	tools := r.Tools()
	specs := make([]rig.ToolSpec, 0, len(tools))
	for _, t := range tools {
		specs = append(specs, rig.ToolSpec{
			Name:        t.Name(),
			Description: t.Description(),
			Parameters:  t.Schema(),
		})
	}
	return specs
}

// Invoke 执行一次工具调用。参数串按模型产出的原样传给工具（不重新编码）。
// 未知工具名返回包装 rig.ErrToolNotFound 的错误。
func (r *Registry) Invoke(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
	r.mu.RLock()
	t, ok := r.tools[call.Name]
	r.mu.RUnlock()
	if !ok {
		return rig.ToolResult{}, fmt.Errorf("%w: %q", rig.ErrToolNotFound, call.Name)
	}
	return t.Invoke(ctx, call)
}

// Tools 返回全部已注册工具，按名字排序。返回的是快照切片：修改它不会影响注册表。
// Tools 实现了 rig.ToolLister。
func (r *Registry) Tools() []rig.Tool {
	r.mu.RLock()
	tools := make([]rig.Tool, 0, len(r.tools))
	for _, t := range r.tools {
		tools = append(tools, t)
	}
	r.mu.RUnlock()
	slices.SortFunc(tools, func(a, b rig.Tool) int { return strings.Compare(a.Name(), b.Name()) })
	return tools
}

// Len 返回已注册工具的数量。
func (r *Registry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.tools)
}

// toolIsNil 判断 t 是否为 nil，包括会经由接口调用方法时 panic 的“类型化 nil 指针”。
func toolIsNil(t rig.Tool) bool {
	if t == nil {
		return true
	}
	value := reflect.ValueOf(t)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

// 编译期检查：注册表满足契约中的工具枚举接口。
var _ rig.ToolLister = (*Registry)(nil)
