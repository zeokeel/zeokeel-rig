// Package host 管理一个可选语言服务器（LSP）的宿主生命周期：按配置启动
// stdio 服务端、完成 initialize 握手、暴露 rig 的六个只读 lsp_* 工具，并在
// Close 时拆除子进程。
//
// 这一层刻意是"尽力而为"的：New 只做配置校验；Start 不返回错误，配置、
// 启动、握手或子进程崩溃都只写日志并让 Tools 保持为空。宿主把 LSP 当可选
// 能力时，不需要为语言服务器的问题中止自己的启动流程。Close 幂等；Close 后
// 可以再次 Start（重启），Start 对已运行且健康的实例是 no-op。
//
// 典型用法：
//
//	host, err := host.New(host.Config{Command: "gopls", Dir: workspace})
//	if err != nil { /* 配置无效，跳过 LSP */ }
//	host.Start(ctx)                       // ctx 是服务器进程生命周期
//	defer host.Close()
//	for _, tool := range host.Tools() { registry.Register(tool) }
package host

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	rig "github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/tools/lsp"
)

// defaultInitializeTimeout 限制 initialize 握手，避免损坏的语言服务器无限
// 拖住宿主启动。它与 product 侧 toolkit 的 15s 上限一致。
const defaultInitializeTimeout = 15 * time.Second

// Config 描述一个语言服务器进程与宿主日志出口。
type Config struct {
	// Command 是语言服务器可执行文件（必填；首尾空白会被去掉）。
	Command string
	// Args 原样传给 Command；空字符串条目会被丢弃，非空条目首尾去空白。
	Args []string
	// Env 是追加到宿主环境后的 "KEY=VALUE" 条目；空条目被丢弃，非空条目
	// 首尾去空白。
	Env []string
	// Dir 是服务端进程的工作目录（可选，首尾去空白）。
	Dir string
	// RootURI 是 initialize 发送的工作区根（可选；为空时由 rig 从 Dir 推导）。
	RootURI string
	// LanguageID 是 didOpen 时发送的语言 id（可选；为空时 rig 用 "plaintext"）。
	LanguageID string
	// Logger 接收启动/握手/关闭诊断；nil 时静默丢弃。回调可能来自 Start/
	// Close 调用的 goroutine。
	Logger func(format string, args ...any)
}

// Host 是一个可选 LSP 服务端的宿主句柄。零值不可用，请用 New 构造。
// Host 的 Start/Close 互相串行；Tools 可与其他方法并发调用。
type Host struct {
	config Config
	logger func(format string, args ...any)

	// lifecycle 串行化 Start/Close（包括耗时的进程启动与握手）。
	lifecycle sync.Mutex
	// mu 只保护下面两个字段，使 Tools 无需等待生命周期操作。
	mu     sync.Mutex
	client *lsp.Client
	tools  []rig.Tool
}

// New 校验并规范化配置。Command 为空是唯一硬性配置错误；其余问题（不可执行
// 的路径、无效 Env、不存在的 Dir 等）留给 Start 作为可降级的启动失败处理。
// 返回的 Host 未启动，Tools 为空。
func New(cfg Config) (*Host, error) {
	cfg.Command = strings.TrimSpace(cfg.Command)
	if cfg.Command == "" {
		return nil, errors.New("lsp host: Config.Command is required")
	}
	cfg.Args = trimEntries(cfg.Args)
	cfg.Env = trimEntries(cfg.Env)
	cfg.Dir = strings.TrimSpace(cfg.Dir)
	cfg.RootURI = strings.TrimSpace(cfg.RootURI)
	logger := cfg.Logger
	if logger == nil {
		logger = func(string, ...any) {}
	}
	cfg.Logger = nil
	return &Host{config: cfg, logger: logger}, nil
}

// Start 启动服务端并完成 initialize 握手，成功时用 lsp_* 工具替换当前工具集。
//
// Start 永远不会向上返回错误，也永远不会 panic（nil Host/ctx 都安全）：
// 失败只写 Logger 并让 Tools 保持为空。若已有健康的服务端在运行，Start 是
// no-op；若上一个服务端已经死亡，Start 会清理它并尝试重启。ctx 是服务端
// 进程的生命周期上下文，取消它会终止语言服务器。
func (h *Host) Start(ctx context.Context) {
	if h == nil {
		return
	}
	h.lifecycle.Lock()
	defer h.lifecycle.Unlock()

	h.mu.Lock()
	client := h.client
	if client != nil && client.Err() == nil {
		h.mu.Unlock()
		return
	}
	// 上一个服务端已死亡或客户端已关闭：清掉可见工具，再尝试新实例。
	h.client = nil
	h.tools = nil
	h.mu.Unlock()
	if client != nil {
		if closeErr := client.Close(); closeErr != nil {
			h.logf("lsp host: close stale server %q: %v", h.config.Command, closeErr)
		}
	}

	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		h.logf("lsp host: start %q: %v", h.config.Command, err)
		return
	}
	started, err := lsp.Start(ctx, lsp.Config{
		Command:    h.config.Command,
		Args:       append([]string(nil), h.config.Args...),
		Env:        append([]string(nil), h.config.Env...),
		Dir:        h.config.Dir,
		RootURI:    h.config.RootURI,
		LanguageID: h.config.LanguageID,
	})
	if err != nil {
		h.logf("lsp host: start %q: %v", h.config.Command, err)
		return
	}
	// rig 的 Start 只拉起协议泵；没有 initialize 的服务端不能服务任何文档
	// 请求。握手失败视为降级能力，关闭进程且不注册工具。
	initCtx, cancel := context.WithTimeout(ctx, defaultInitializeTimeout)
	initErr := started.Initialize(initCtx)
	cancel()
	if initErr != nil {
		h.logf("lsp host: initialize %q: %v", h.config.Command, initErr)
		if closeErr := started.Close(); closeErr != nil {
			h.logf("lsp host: close after failed initialize %q: %v", h.config.Command, closeErr)
		}
		return
	}
	tools := h.collectTools(started)
	h.mu.Lock()
	h.client = started
	h.tools = tools
	h.mu.Unlock()
	h.logf("lsp host: server %q ready with %d tool(s)", h.config.Command, len(tools))
}

// Tools 返回当前服务端注册的 rig 工具（按 lsp 包的稳定顺序）。未启动、启动
// 失败或 Close 后返回空切片。返回值是副本，调用方可以安全修改。
func (h *Host) Tools() []rig.Tool {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]rig.Tool(nil), h.tools...)
}

// Close 拆除当前服务端并清空 Tools。它幂等：重复调用返回 nil；未启动的
// Host 也返回 nil。Close 之后可以再次 Start（重启）。只有服务端进程无法
// 停止时才返回错误（来自 rig lsp.Client.Close）。
func (h *Host) Close() error {
	if h == nil {
		return nil
	}
	h.lifecycle.Lock()
	defer h.lifecycle.Unlock()
	h.mu.Lock()
	client := h.client
	h.client = nil
	h.tools = nil
	h.mu.Unlock()
	if client == nil {
		return nil
	}
	if err := client.Close(); err != nil {
		h.logf("lsp host: close %q: %v", h.config.Command, err)
		return err
	}
	return nil
}

// collectTools 构造工具集并按名称去重。rig lsp.Tools 当前返回六个唯一名字，
// 这里的去重是防御性的：未来新增工具或宿主替换实现时也不会注册重名工具。
func (h *Host) collectTools(client *lsp.Client) []rig.Tool {
	return h.filterTools(lsp.Tools(client, nil))
}

// filterTools 去掉 nil、空名称和重名工具；调用顺序保持输入顺序。
func (h *Host) filterTools(candidates []rig.Tool) []rig.Tool {
	kept := make([]rig.Tool, 0, len(candidates))
	seen := make(map[string]bool, len(candidates))
	for _, tool := range candidates {
		if tool == nil {
			continue
		}
		name := strings.TrimSpace(tool.Name())
		if name == "" {
			h.logf("lsp host: skipping tool with empty name")
			continue
		}
		if seen[name] {
			h.logf("lsp host: skipping duplicate tool %q", name)
			continue
		}
		seen[name] = true
		kept = append(kept, tool)
	}
	return kept
}

func (h *Host) logf(format string, args ...any) {
	if h == nil || h.logger == nil {
		return
	}
	h.logger(format, args...)
}

// trimEntries 去掉每个条目的首尾空白并丢弃空条目；nil 保持 nil。
func trimEntries(values []string) []string {
	if values == nil {
		return nil
	}
	out := make([]string, 0, len(values))
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			continue
		}
		out = append(out, trimmed)
	}
	return out
}
