package gate

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/zeokeel/zeokeel-rig"
)

// 本文件实现"审批记忆"：把一次 allow-always 的回答记成规则，下次遇到同一个目标
// 直接放行，不再问人。
//
// # 它不越过围栏
//
// 记忆只替代**提问**这一件事。被记住的调用仍然会走工具本身：沙箱策略、可写根、
// 只读模式、可写子路径全部照旧生效。因此在 read-only 下记住一条命令，命令依然
// 会被策略拒绝——用户记住的是"这条命令我同意过"，不是"把围栏拆掉"。
//
// # 目标怎么记
//
//   - shell 类工具（bash/pwsh）：Kind=command，Key 是命令文本；
//   - 有单一路径参数的工具（write_file/edit_file/...）：Kind=path，Key 是路径；
//   - 其余：Kind=tool，Key 是"工具名 + 规范化参数"（参数按 JSON 语义规范化，
//     键顺序/空白不同视为同一个调用）。
//
// # 失败模式
//
// store 读写失败**不打断回合**：只记一条日志（WithApprovalLogger），记忆退化成
// "这次没记住"，下一次照常问人。审批本身仍然由 inner 门禁负责。

// 审批记忆的目标类别（rig.ApprovalRule.Kind）。
const (
	// ApprovalKindTool 表示按"工具 + 参数"记住的一次调用。
	ApprovalKindTool = "tool"
	// ApprovalKindCommand 表示按命令文本记住的一条 shell 命令。
	ApprovalKindCommand = "command"
	// ApprovalKindPath 表示按路径记住的一次文件写入/读取。
	ApprovalKindPath = "path"
)

// commandTools 是按命令文本记忆目标的工具名（与 tools/builtin 的 shell 工具一致）。
var commandTools = map[string]struct{}{
	"bash": {},
	"pwsh": {},
}

// pathTools 是有单一目标路径、按路径记忆的工具名。
var pathTools = map[string]struct{}{
	"write_file":         {},
	"edit_file":          {},
	"str_replace_editor": {},
	"read_file":          {},
	"read_image":         {},
}

// ApprovalMemoryOption 配置 WithApprovalMemory 返回的包装器。
type ApprovalMemoryOption func(*approvalMemory)

// WithApprovalSessionID 设置 session 作用域下规则的归属会话。
//
// 只影响**新写入**规则的作用域判定与匹配：SessionID 不同的 session 规则不生效，
// 这就是"换会话不生效"的实现。global 作用域忽略它。
func WithApprovalSessionID(sessionID string) ApprovalMemoryOption {
	return func(m *approvalMemory) { m.sessionID = strings.TrimSpace(sessionID) }
}

// WithApprovalLogger 设置"store 读写失败"的日志出口。默认静默（只退化为不记忆）。
func WithApprovalLogger(logger rig.Logger) ApprovalMemoryOption {
	return func(m *approvalMemory) { m.logger = logger }
}

// approvalMemory 保存审批记忆包装器的配置与已加载的规则缓存。
//
// 规则只在锁内读写；inner.Approve 在锁外调用（它可能阻塞等人回答）。
type approvalMemory struct {
	store     rig.ApprovalStore
	scope     string
	sessionID string
	logger    rig.Logger

	mu     sync.Mutex
	loaded bool
	// loadErr 非 nil 表示上一次 Load 失败：此时禁止 Save——用一份不完整的规则集
	// 覆盖 store 会删掉宿主已有的规则。本次运行仍使用内存里的规则。
	loadErr error
	rules   []rig.ApprovalRule
}

// WithApprovalMemory 返回一个审批记忆包装器：它在外层放行已记住的调用，并把
// inner 返回的 rig.DecisionAllowAlways 写进 store。
//
//	hostGate := gate.WithApprovalMemory(store, rig.ApprovalScopeSession,
//		gate.WithApprovalSessionID(sessionID),
//	)(gate.PersonalAsk(gate.Ask(host.Asker)))
//
// scope 只接受 rig.ApprovalScopeSession / rig.ApprovalScopeGlobal，其它值（含空串）
// 按 session 处理。store 为 nil 时包装器等于直接返回 inner（默认关闭，行为不变）。
// inner 为 nil 时按"没有审批通道"处理（fail-closed，返回 deny + 理由）。
func WithApprovalMemory(store rig.ApprovalStore, scope string, opts ...ApprovalMemoryOption) func(inner rig.Gate) rig.Gate {
	memory := &approvalMemory{store: store, scope: normalizeApprovalScope(scope)}
	for _, opt := range opts {
		if opt != nil {
			opt(memory)
		}
	}
	return func(inner rig.Gate) rig.Gate {
		if store == nil {
			return inner
		}
		if inner == nil {
			inner = DenyAllWithReason(NoApprovalChannelToolReason)
		}
		return funcGate{
			fn: func(ctx context.Context, tool rig.Tool, call rig.ToolCall) (rig.Decision, error) {
				kind, key := ApprovalTarget(tool, call)
				if memory.matches(kind, key) {
					// 记住过：跳过提问。注意这只跳过 gate，工具仍会走策略与沙箱。
					return rig.DecisionAllow, nil
				}
				decision, err := inner.Approve(ctx, tool, call)
				if err != nil || decision != rig.DecisionAllowAlways {
					return decision, err
				}
				memory.remember(kind, key)
				return decision, nil
			},
			// 拒绝理由逐层委托：记忆层自己没有理由，用户看到的仍是 inner 的。
			explain: explainFrom(inner),
		}
	}
}

// normalizeApprovalScope 把作用域收敛为两个合法值（未知值按 session 处理）。
func normalizeApprovalScope(scope string) string {
	if strings.EqualFold(strings.TrimSpace(scope), rig.ApprovalScopeGlobal) {
		return rig.ApprovalScopeGlobal
	}
	return rig.ApprovalScopeSession
}

// matches 报告 (kind, key) 是否已被当前作用域记住（必要时先加载规则）。
func (m *approvalMemory) matches(kind, key string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.loadLocked()
	for _, rule := range m.rules {
		if rule.Kind != kind || rule.Key != key {
			continue
		}
		if m.ruleAppliesLocked(rule) {
			return true
		}
	}
	return false
}

// ruleAppliesLocked 判断一条已加载的规则在当前作用域下是否生效。
func (m *approvalMemory) ruleAppliesLocked(rule rig.ApprovalRule) bool {
	if m.scope == rig.ApprovalScopeGlobal {
		return rule.Scope == rig.ApprovalScopeGlobal
	}
	if m.sessionID == "" {
		// 没有会话标识时 session 规则会退化成"所有空会话共用一个桶"：不认。写入端
		// 也不会写这种规则（见 remember），两者一起把 session 语义钉死。
		return false
	}
	return rule.Scope == rig.ApprovalScopeSession && rule.SessionID == m.sessionID
}

// loadLocked 惰性加载 store 里的规则（只在第一次用到时读一次）。加载失败按"没有
// 记住任何东西"处理并记日志：这次会重问，下一次也不记——直到宿主修好 store。
func (m *approvalMemory) loadLocked() {
	if m.loaded {
		return
	}
	m.loaded = true
	rules, err := m.store.Load(context.Background())
	if err != nil {
		m.loadErr = err
		m.logf("load approvals: %v", err)
		return
	}
	m.rules = append([]rig.ApprovalRule(nil), rules...)
}

// remember 写入（或刷新）一条规则并持久化。写失败只记日志，不影响本次审批结果。
//
// 持久化用**合并写**：rig.ApprovalStore.Save 是整体替换，而每个 Run 的规则缓存只是
// 它开始时的快照。直接拿快照覆盖会丢掉并发 Run 刚记下的规则（以及用陈旧快照复活
// 已撤销的规则），所以锁内重新 Load → 并集 → Save。approvalSaveMu 把这一步在
// 进程内串行化（store 可能被多个 approvalMemory 实例共用）。
func (m *approvalMemory) remember(kind, key string) {
	m.mu.Lock()
	m.loadLocked()
	if m.scope == rig.ApprovalScopeSession && m.sessionID == "" {
		// 没有会话标识：session 规则会变成"所有空会话共用一个桶"，等于偷偷把
		// 作用域改成 global。不写；想跨会话记住请让宿主用 global 作用域。
		m.logf("skip save: session scope without session id")
		m.mu.Unlock()
		return
	}
	rule := rig.ApprovalRule{
		Kind:      kind,
		Key:       key,
		Scope:     m.scope,
		DecidedAt: time.Now().UTC(),
	}
	if m.scope == rig.ApprovalScopeSession {
		rule.SessionID = m.sessionID
	}
	// 同目标只留一条：重复 allow-always 只刷新时间，不让 store 无限增长。
	replaced := false
	for i := range m.rules {
		if m.rules[i].Kind == kind && m.rules[i].Key == key && m.ruleAppliesLocked(m.rules[i]) {
			m.rules[i] = rule
			replaced = true
			break
		}
	}
	if !replaced {
		m.rules = append(m.rules, rule)
	}
	if m.loadErr != nil {
		// 没能读到旧规则：保存会把它们删掉。本次运行继续用内存里的规则（不再问人），
		// 但不动 store——等宿主修好存储，下一次 Run 会重新加载。
		m.logf("skip save: previous load failed: %v", m.loadErr)
		m.mu.Unlock()
		return
	}
	m.mu.Unlock()

	ctx := context.Background()
	approvalSaveMu.Lock()
	defer approvalSaveMu.Unlock()
	stored, err := m.store.Load(ctx)
	if err != nil {
		m.logf("load approvals for merge: %v", err)
		return
	}
	merged := mergeApprovalRule(stored, rule)
	if err := m.store.Save(ctx, merged); err != nil {
		m.logf("save approvals: %v", err)
		return
	}
	// 合并结果回填本次运行的缓存：并发 Run 刚写的规则这一轮也能命中。
	m.mu.Lock()
	m.rules = append([]rig.ApprovalRule(nil), merged...)
	m.mu.Unlock()
}

// approvalSaveMu 串行化"Load → 并集 → Save"这一步：多个 Run 各自持一份快照，
// 只有在这一步串行化后才不会互相覆盖。
var approvalSaveMu sync.Mutex

// mergeApprovalRule 把一条规则并进已有集合：同一 (Kind, Key, Scope, SessionID)
// 只留一条（新的替换旧的），其余原样保留。
func mergeApprovalRule(rules []rig.ApprovalRule, rule rig.ApprovalRule) []rig.ApprovalRule {
	merged := append([]rig.ApprovalRule(nil), rules...)
	for i := range merged {
		if merged[i].Kind == rule.Kind && merged[i].Key == rule.Key &&
			merged[i].Scope == rule.Scope && merged[i].SessionID == rule.SessionID {
			merged[i] = rule
			return merged
		}
	}
	return append(merged, rule)
}

// logf 写一条诊断日志；没有接 Logger 时静默（记忆失败不该打断回合）。
func (m *approvalMemory) logf(format string, args ...any) {
	if m.logger == nil {
		return
	}
	m.logger.Printf("gate: approval memory: "+format, args...)
}

// ApprovalTarget 计算一次调用在审批记忆里的 (Kind, Key)。
//
// 它是记忆与"规则文件"之间唯一的约定：同一个调用永远得到同一个 (Kind, Key)，
// 因此宿主自己写规则（例如手工放行一条命令）与门禁写入的规则天然兼容。
func ApprovalTarget(tool rig.Tool, call rig.ToolCall) (kind, key string) {
	name := toolCallName(tool, call)
	args := decodeApprovalArgs(call.Arguments)
	if _, ok := commandTools[name]; ok {
		if command := argString(args, "command"); command != "" {
			return ApprovalKindCommand, command
		}
	}
	if _, ok := pathTools[name]; ok {
		if path := argString(args, "path", "file_path"); path != "" {
			return ApprovalKindPath, path
		}
	}
	return ApprovalKindTool, name + "\x00" + CanonicalArgs(call.Arguments)
}

// CanonicalArgs 把工具参数规范化成稳定文本：JSON 语义相同（键顺序/空白不同）的
// 参数得到同一个字符串，无法解析时退回原始去空白文本。
func CanonicalArgs(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "{}"
	}
	var value any
	if err := json.Unmarshal([]byte(trimmed), &value); err != nil {
		return trimmed
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return trimmed
	}
	return string(encoded)
}

// decodeApprovalArgs 尽力把参数解成对象；失败时返回 nil（按"没有结构化参数"处理）。
func decodeApprovalArgs(raw string) map[string]any {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(trimmed), &args); err != nil {
		return nil
	}
	return args
}

// argString 返回第一个非空字符串参数（去空白）。
func argString(args map[string]any, names ...string) string {
	for _, name := range names {
		value, ok := args[name]
		if !ok {
			continue
		}
		text, ok := value.(string)
		if !ok {
			continue
		}
		if text = strings.TrimSpace(text); text != "" {
			return text
		}
	}
	return ""
}

// RoundMemo 是"本次 Run 已经问过"的记忆：按"工具名 + 规范化参数"索引，同一个
// 调用在同一个 Run 内只有第一个到达的调用者去问 inner（门禁），其余的等它的结论
// 并复用（single-flight）。
//
// 用途：模型一轮里并行发出多个完全相同的调用时，不该让用户为同一件事点两次。
// 生命周期由调用方决定：**每次 Run 新建一个**，Run 结束即丢弃，所以不同 Run 之间
// 仍按审批记忆规则判断（保留的回答不在这里）。并发安全。
type RoundMemo struct {
	mu      sync.Mutex
	entries map[string]*roundMemoEntry
}

// roundMemoEntry 是一次正在进行的审批。done 关闭表示 decision/err 已经写入。
type roundMemoEntry struct {
	done     chan struct{}
	decision rig.Decision
	err      error
}

// NewRoundMemo 返回一个空的本轮审批 memo。
func NewRoundMemo() *RoundMemo {
	return &RoundMemo{entries: make(map[string]*roundMemoEntry)}
}

// Wrap 把 inner 包成"同一 Run 内相同调用只走一次"的门禁。inner 为 nil 时原样
// 返回 nil（nil 门禁在 agent 里等于全放行，不能被包装成拒绝）。
//
// 并发语义是 single-flight：第一个调用者计算，其余同 key 的调用者阻塞到它给出结论
// 后复用同一个 rig.Decision（不是"先查后写"——那样并发到达的每个调用都会问一次）。
func (m *RoundMemo) Wrap(inner rig.Gate) rig.Gate {
	if inner == nil || m == nil {
		return inner
	}
	return funcGate{
		fn: func(ctx context.Context, tool rig.Tool, call rig.ToolCall) (rig.Decision, error) {
			key := memoKey(tool, call)
			m.mu.Lock()
			if m.entries == nil {
				m.entries = make(map[string]*roundMemoEntry)
			}
			entry, running := m.entries[key]
			if !running {
				entry = &roundMemoEntry{done: make(chan struct{})}
				m.entries[key] = entry
			}
			m.mu.Unlock()
			if running {
				// 后来者：等第一个调用者的结论。自己的 context 结束时可以提前走，
				// 但不能替别人决定（它可能只是被取消）。
				select {
				case <-entry.done:
					return entry.decision, entry.err
				case <-ctx.Done():
					return "", approvalExpiredError(ctx.Err())
				}
			}
			decision, err := inner.Approve(ctx, tool, call)
			m.mu.Lock()
			entry.decision, entry.err = decision, err
			if err != nil {
				// 出错不记忆：下一次仍要完整走一遍（可能只是这次被取消）。
				delete(m.entries, key)
			}
			m.mu.Unlock()
			close(entry.done)
			return decision, err
		},
		explain: explainFrom(inner),
	}
}

// memoKey 是 RoundMemo 的键：工具名 + 规范化参数（与审批记忆的 tool 类目标同源，
// 但这里不按 command/path 归类——去重只针对**完全相同的调用**）。
func memoKey(tool rig.Tool, call rig.ToolCall) string {
	return toolCallName(tool, call) + "\x00" + CanonicalArgs(call.Arguments)
}
