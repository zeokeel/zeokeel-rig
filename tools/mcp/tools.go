package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/zeokeel/zeokeel-rig"
)

// DefaultPrefix 是 Tools 与 NewTools 在调用方传入空前缀时使用的模型可见工具名前缀，
// 生成 "mcp__<server>__<tool>" 形式的名字。
const DefaultPrefix = "mcp__"

// ToolInfo 是 MCP server 公布的一个工具。
type ToolInfo struct {
	Name        string
	Description string
	// InputSchema 是服务端返回的工具 JSON Schema 对象。
	InputSchema json.RawMessage
}

// Content 是 tools/call 结果中的一个内容项。Type 为 "text"、"image"、"audio"、
// "resource" 或服务端自定义的将来类型；Text 用于文本（与资源）项，Data 是二进制项的
// base64 载荷，MimeType 描述 image/audio/resource 载荷。
type Content struct {
	Type     string
	Text     string
	Data     string
	MimeType string
}

// CallResult 是一次 tools/call 结果。IsError 标记工具级失败（调用本身成功）；
// StructuredContent 在存在时原样保留服务端的 structuredContent JSON。
type CallResult struct {
	Content           []Content
	IsError           bool
	StructuredContent json.RawMessage
}

// mapContent 把一条 wire 内容项转换为公开的 Content 值。
func mapContent(wire wireContent) Content {
	content := Content{
		Type:     wire.Type,
		Text:     wire.Text,
		Data:     wire.Data,
		MimeType: wire.MimeType,
	}
	if wire.Resource != nil {
		content.MimeType = wire.Resource.MimeType
		switch {
		case wire.Resource.URI != "":
			content.Text = wire.Resource.URI
		case wire.Resource.Name != "":
			content.Text = wire.Resource.Name
		}
	}
	return content
}

// Tools 把 client 公布的所有工具适配为 rig.Tool。
//
// 它是编译期接线便利函数：用后台上下文（受客户端超时约束）列出工具，列出失败时返回
// nil。需要在错误时处理时，改用 ListTools + NewTools。
func Tools(client *Client, prefix string) []rig.Tool {
	if client == nil {
		return nil
	}
	infos, err := client.ListTools(context.Background())
	if err != nil {
		return nil
	}
	return NewTools(client, client.ServerName(), infos, prefix)
}

// NewTools 包装已经列出的工具，不执行 I/O。client 是工具调用使用的连接（通常就是列出
// 它们的 client），server 是名称中使用的服务端身份（通常是 client.ServerName()），
// prefix 默认是 DefaultPrefix。
//
// 每个包装工具把 Invoke 转发给 client.CallTool；文本内容用换行连接，image/audio/resource
// 项变成占位行，只有 structuredContent 的结果回退为它的 JSON 文本。工具级 isError 映射为
// ToolResult.IsError。
//
// 名字清洗会撞名（`read.file` 与 `read_file` 都清洗成 `read_file`；两个 server 的
// serverPart 也可能清洗成同一个字符串），而宿主最终会把结果塞进
// rig.AgentSpec.Tools，重名会让 agent.New 直接报错、应用起不来。因此这里保证
// **同一批输入内模型可见名唯一**：同一个工具的身份是"连接 + 原始工具名"，同一连接里
// 重复列出只包装一次；清洗后与已分配名字冲突的项追加确定性后缀 `__2`、`__3`…
// （只取决于输入顺序，同一批输入总是得到同样的名字）。需要把冲突当成配置错误处理时用 NewToolsChecked，多 server
// 聚合用 AggregateTools。
func NewTools(client *Client, server string, infos []ToolInfo, prefix string) []rig.Tool {
	tools, _ := buildToolSet([]serverTools{{client: client, server: server, infos: infos}}, prefix)
	return tools
}

// ServerTools 是 AggregateTools 的一个输入项：一个 server 及其工具。
type ServerTools struct {
	// Client 是调用这些工具使用的连接。
	Client *Client
	// Server 是名称与冲突诊断里使用的服务端身份；空表示 Client.ServerName()。
	Server string
	// Tools 是已经列出的工具；nil 表示现场调用 Client.ListTools。
	Tools []ToolInfo
	// Resources 让 AggregateTools 额外加入该连接的资源/提示工具（ResourceTools 的五个
	// 基础名），并与普通工具共用同一套名字分配与冲突消解 —— 这样多个连接（即使
	// ServerName 相同）的资源工具也不会重名，宿主不需要任何上层改名兜底。
	Resources bool
}

// AggregateTools 把多个 server 的工具合成为一份**全局唯一**的 rig.Tool 列表，并按
// (连接, 原始工具名) 记录映射 —— 两条连接即使声明同一个 ServerName 也会各自保留
// （工具身份不是 server 显示名），名字冲突走 `__2`/`__3` 消歧并作为 NameCollision 返回。这是多 server 接线（例如
// full.AllOptions.MCPClients）应该使用的入口：逐个调用 NewTools 无法发现"两个
// server 的可见 serverPart 清洗后相同"这类跨 server 冲突。
//
// 冲突消解与 NewTools 一致：冲突项获得确定性后缀，绝不产生重名；每一次消解都会以
// NameCollision 的形式返回，便于宿主写日志或展示。错误只用于列工具失败。
func AggregateTools(servers []ServerTools, prefix string) ([]rig.Tool, []NameCollision, error) {
	inputs := make([]serverTools, 0, len(servers))
	for _, entry := range servers {
		server := strings.TrimSpace(entry.Server)
		if server == "" && entry.Client != nil {
			server = entry.Client.ServerName()
		}
		infos := entry.Tools
		if infos == nil && entry.Client != nil {
			listed, err := entry.Client.ListTools(context.Background())
			if err != nil {
				return nil, nil, fmt.Errorf("mcp: list tools for server %q: %w", server, err)
			}
			infos = listed
		}
		inputs = append(inputs, serverTools{
			client:    entry.Client,
			server:    server,
			infos:     infos,
			resources: entry.Resources,
		})
	}
	tools, collisions := buildToolSet(inputs, prefix)
	return tools, collisions, nil
}

// NewToolsChecked 与 NewTools 相同，但把"清洗后撞名"当作可诊断的配置错误返回：
// 错误里带有原始 server、原始工具名与冲突前后的模型可见名，而不是让上层
// agent.New 抛一句 duplicate tool。没有冲突时与 NewTools 逐字节等价。
func NewToolsChecked(client *Client, server string, infos []ToolInfo, prefix string) ([]rig.Tool, error) {
	tools, collisions := buildToolSet([]serverTools{{client: client, server: server, infos: infos}}, prefix)
	if len(collisions) > 0 {
		return nil, &NameCollisionError{Collisions: collisions}
	}
	return tools, nil
}

// NameCollision 描述一次模型可见工具名冲突及它的确定性消解结果。
type NameCollision struct {
	// Server 是冲突项所属服务端身份（原始，未清洗）。
	Server string
	// RemoteName 是服务端公布的原始工具名。
	RemoteName string
	// SanitizedName 是清洗后、含前缀的候选名（冲突前）。
	SanitizedName string
	// AssignedName 是最终分配的唯一名字（含前缀，带 __N 后缀）。
	AssignedName string
	// ConflictsWithServer / ConflictsWithName 是已经占用 SanitizedName 的那个工具。
	ConflictsWithServer string
	ConflictsWithName   string
}

// String 渲染一条可诊断的冲突说明。
func (c NameCollision) String() string {
	return fmt.Sprintf("server %q tool %q -> %q collides with server %q tool %q; assigned %q",
		c.Server, c.RemoteName, c.SanitizedName, c.ConflictsWithServer, c.ConflictsWithName, c.AssignedName)
}

// NameCollisionError 汇总一次构造中发现的全部名字冲突。
type NameCollisionError struct {
	Collisions []NameCollision
}

// Error 实现 error，逐条列出原始 server / 原始工具名 / 清洗后名字。
func (e *NameCollisionError) Error() string {
	if e == nil || len(e.Collisions) == 0 {
		return "mcp: no tool name collisions"
	}
	var builder strings.Builder
	fmt.Fprintf(&builder, "mcp: %d MCP tool name collision(s) after sanitizing server/tool names", len(e.Collisions))
	for _, collision := range e.Collisions {
		builder.WriteString("\n  ")
		builder.WriteString(collision.String())
	}
	return builder.String()
}

// serverTools 是 buildToolSet 的内部输入。
type serverTools struct {
	client    *Client
	server    string
	infos     []ToolInfo
	resources bool
}

// assignedTool 记录一个已分配的模型可见名属于哪个 (连接上的 server, 原始名)。
type assignedTool struct {
	server string
	name   string
}

// nameAllocator 是模型可见名的**共享命名空间**：普通工具与资源/提示工具都从这里取名字，
// 因此两者之间也不会撞名（例如某个 server 恰好有一个叫 `list_resources` 的工具）。
type nameAllocator struct {
	assigned   map[string]assignedTool
	byOrigin   map[string]string
	collisions []NameCollision
}

// claim 为一次分配取一个唯一名字。身份是"连接 + 原始名"：
//
//   - 同一条连接里同一个原始名第二次出现 → duplicate=true（调用方应跳过该工具）；
//   - 名字已被占用 → 追加确定性的 `__2`、`__3`…，并记录一条 NameCollision。
func (a *nameAllocator) claim(identity, candidate, server, remoteName string) (name string, duplicate bool) {
	originKey := identity + "\x00" + remoteName
	if _, wrapped := a.byOrigin[originKey]; wrapped {
		return "", true
	}
	name = candidate
	if owner, taken := a.assigned[name]; taken {
		for suffix := 2; ; suffix++ {
			name = candidate + "__" + strconv.Itoa(suffix)
			if _, taken := a.assigned[name]; !taken {
				break
			}
		}
		a.collisions = append(a.collisions, NameCollision{
			Server:              server,
			RemoteName:          remoteName,
			SanitizedName:       candidate,
			AssignedName:        name,
			ConflictsWithServer: owner.server,
			ConflictsWithName:   owner.name,
		})
	}
	a.assigned[name] = assignedTool{server: server, name: remoteName}
	a.byOrigin[originKey] = name
	return name, false
}

// buildToolSet 把若干 server 的工具（以及可选的资源/提示工具）包装成 rig.Tool，并保证
// 模型可见名唯一。
//
// 工具身份是"**连接 + 原始工具名**"（不是"server 显示名 + 原始名"）：
//
//   - 同一条连接里同一个原始名重复出现 → 只包装一次（服务端把同一个工具列两次时不该
//     变成两个工具）；
//   - 两条不同的连接即使 ServerName 相同（同一个 server 配两次，或两个 server 恰好同名）
//     → 各自保留，名字冲突交给下面的 `__2`、`__3`… 后缀消歧。
//
// 资源/提示工具与普通工具共用同一个名字空间（见 nameAllocator）：它们的名字同样带
// server 身份（`<prefix><server>__<base>`），因此多连接时不会逐字重复。
//
// 名字冲突的消歧是确定性的（只取决于输入顺序），并把每次消解记录成 NameCollision
// 供调用方诊断。
func buildToolSet(servers []serverTools, prefix string) ([]rig.Tool, []NameCollision) {
	prefix = normalizedPrefix(prefix)
	total := 0
	for _, entry := range servers {
		total += len(entry.infos) + len(resourceToolBases)
	}
	tools := make([]rig.Tool, 0, total)
	allocator := &nameAllocator{
		assigned: make(map[string]assignedTool, total),
		byOrigin: make(map[string]string, total),
	}
	for _, entry := range servers {
		identity := connectionIdentity(entry.client)
		base := prefix + sanitizeName(entry.server) + "__"
		for _, info := range entry.infos {
			if info.Name == "" {
				continue
			}
			name, duplicate := allocator.claim(identity, base+sanitizeName(info.Name), entry.server, info.Name)
			if duplicate {
				continue // 同一条连接里同一个工具被列了两次
			}
			tools = append(tools, &tool{
				client:      entry.client,
				server:      entry.server,
				remoteName:  info.Name,
				name:        name,
				description: info.Description,
				schema:      schemaMap(info.InputSchema),
			})
		}
		if !entry.resources {
			continue
		}
		for _, resourceBase := range resourceToolBases {
			name, duplicate := allocator.claim(identity, base+resourceBase, entry.server, resourceBase)
			if duplicate {
				continue
			}
			tools = append(tools, newResourceTool(entry.client, name, resourceBase))
		}
	}
	return tools, allocator.collisions
}

// normalizedPrefix 在 prefix 为空时应用默认前缀。
func normalizedPrefix(prefix string) string {
	if prefix == "" {
		return DefaultPrefix
	}
	return prefix
}

// tool 是适配为 rig.Tool 的一个 MCP 工具。
type tool struct {
	client      *Client
	server      string
	remoteName  string
	name        string
	description string
	schema      map[string]any
}

var _ rig.Tool = (*tool)(nil)

// Name 实现 rig.Tool。
func (t *tool) Name() string { return t.name }

// Description 实现 rig.Tool。
func (t *tool) Description() string {
	if strings.TrimSpace(t.description) != "" {
		return t.description
	}
	return "MCP tool " + t.remoteName + " from " + t.server
}

// Schema 实现 rig.Tool。
func (t *tool) Schema() map[string]any { return t.schema }

// Invoke 通过调用 MCP server 并渲染其内容来实现 rig.Tool。
func (t *tool) Invoke(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
	if t.client == nil {
		return rig.ToolResult{}, fmt.Errorf("mcp: tool %q has no client", t.name)
	}
	result, err := t.client.CallTool(ctx, t.remoteName, json.RawMessage(call.Arguments))
	if err != nil {
		return rig.ToolResult{}, err
	}
	text := renderContent(result.Content)
	if text == "" && len(bytes.TrimSpace(result.StructuredContent)) > 0 {
		text = string(bytes.TrimSpace(result.StructuredContent))
	}
	return rig.ToolResult{Content: text, IsError: result.IsError}, nil
}

// renderContent 把工具内容渲染为模型可读的纯文本：文本项直接拼接，非文本项变成说明性
// 占位。
func renderContent(contents []Content) string {
	parts := make([]string, 0, len(contents))
	for _, content := range contents {
		switch content.Type {
		case "text":
			parts = append(parts, content.Text)
		case "image":
			parts = append(parts, placeholder("image", content.MimeType))
		case "audio":
			parts = append(parts, placeholder("audio", content.MimeType))
		case "resource":
			label := strings.TrimSpace(content.Text)
			if label == "" {
				label = "embedded resource"
			}
			parts = append(parts, "[resource: "+label+"]")
		default:
			typeName := content.Type
			if typeName == "" {
				typeName = "unknown"
			}
			parts = append(parts, placeholder(typeName, content.MimeType))
		}
	}
	return strings.Join(parts, "\n")
}

// placeholder 把一个非文本内容项渲染为模型可读的说明。
func placeholder(kind, mimeType string) string {
	if mimeType == "" {
		return "[" + kind + "]"
	}
	return "[" + kind + ": " + mimeType + "]"
}

// schemaMap 把原始 JSON Schema 转换为 rig.Tool 期望的 map。缺失/非法的 schema 退化为
// 空对象 schema，使工具仍可调用（校验权威仍在服务端）。
func schemaMap(raw json.RawMessage) map[string]any {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return map[string]any{"type": "object"}
	}
	var schema map[string]any
	if err := json.Unmarshal(trimmed, &schema); err != nil || schema == nil {
		return map[string]any{"type": "object"}
	}
	return schema
}

// connectionIdentity 返回一条连接的身份串，用于"工具身份 = 连接 + 原始名"的判定。
//
// 正常路径用 Client.connID（Start/StartHTTP 分配的进程内单调递增号）；零值 Client
// （测试或宿主手工构造、没有经过 Start）退化为指针地址，保证两个不同实例仍然是两条
// 不同的连接。nil（例如只给 AggregateTools 传预列好的 ToolInfo）返回空串：没有连接
// 时按"同一个来源"处理，重复列出只包装一次。
func connectionIdentity(client *Client) string {
	if client == nil {
		return ""
	}
	if client.connID != 0 {
		return "conn-" + strconv.FormatUint(client.connID, 10)
	}
	return fmt.Sprintf("ptr-%p", client)
}

// SanitizeName 是模型可见工具名的清洗规则，宿主复刻白名单（例如把某个 MCP 工具加进
// disabledTools、或持久化一份允许清单）时用这个，不要自己写：规则一旦漂移，宿主的
// 白名单就会与真实注册名对不上。
//
// 具名规则与 Tools/ResourceTools 生成名字时用的那条完全一致——把 MCP 名称映射到模型
// 函数名允许的字符集（[A-Za-z0-9_-]），集合之外的字符逐个变为下划线（按 rune），
// 结果首尾的下划线被去掉，去掉后为空时回退为 "tool"。
func SanitizeName(name string) string { return sanitizeName(name) }

// sanitizeName 把 MCP 名称映射到模型函数名允许的字符集（[A-Za-z0-9_-]）。集合之外的
// 字符变为下划线，结果为空时回退为 "tool"。导出版本就是 SanitizeName，两者语义相同。
func sanitizeName(name string) string {
	var builder strings.Builder
	builder.Grow(len(name))
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '_', r == '-':
			builder.WriteRune(r)
		default:
			builder.WriteByte('_')
		}
	}
	out := strings.Trim(builder.String(), "_")
	if out == "" {
		return "tool"
	}
	return out
}
