package lsp

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// capabilityFlag 解码一个 LSP 服务端能力声明。LSP 允许三种形式：true、false/null，
// 或携带选项的 provider 对象；对象形式表示服务端提供该能力（选项内容本客户端不解释）。
type capabilityFlag bool

// UnmarshalJSON 实现 json.Unmarshaler。
func (f *capabilityFlag) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		*f = false
		return nil
	}
	switch trimmed[0] {
	case 't':
		*f = true
	case 'f':
		*f = false
	case '{':
		*f = true
	default:
		*f = false
	}
	return nil
}

// serverCapabilities 是 initialize 结果中本客户端关心的服务端能力声明。动态注册不支持：
// 这些能力只在 initialize 的静态结果里读取一次。
type serverCapabilities struct {
	references     capabilityFlag
	implementation capabilityFlag
}

// decodeServerCapabilities 从 initialize 结果解析 references/implementation 能力。
func decodeServerCapabilities(raw json.RawMessage) (serverCapabilities, error) {
	var result struct {
		Capabilities struct {
			ReferencesProvider     capabilityFlag `json:"referencesProvider"`
			ImplementationProvider capabilityFlag `json:"implementationProvider"`
		} `json:"capabilities"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return serverCapabilities{}, fmt.Errorf("lsp: decode initialize capabilities: %w", err)
	}
	return serverCapabilities{
		references:     result.Capabilities.ReferencesProvider,
		implementation: result.Capabilities.ImplementationProvider,
	}, nil
}

// SupportsOperation 报告服务端是否在 initialize 结果中声明支持 operation。客户端完成
// initialize 握手之前能力未知，返回 false；请求层在未初始化时不做能力拦截（由服务端自行
// 报告错误）。hover/definition 保持历史宽松行为，始终返回 true。
func (c *Client) SupportsOperation(operation Operation) bool {
	if c == nil {
		return false
	}
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	if !c.initialized {
		return false
	}
	switch operation {
	case OpFindReferences:
		return bool(c.capabilities.references)
	case OpGoToImplementation:
		return bool(c.capabilities.implementation)
	default:
		return true
	}
}

// requireOperationSupport 在服务端明确未声明 references/implementation 能力时返回可读错误；
// 其他操作与未初始化状态不拦截。
func (c *Client) requireOperationSupport(operation Operation) error {
	c.stateMu.Lock()
	initialized := c.initialized
	capabilities := c.capabilities
	c.stateMu.Unlock()
	if !initialized {
		return nil
	}
	switch operation {
	case OpFindReferences:
		if !bool(capabilities.references) {
			return fmt.Errorf("lsp: server %q does not support references (referencesProvider not advertised in initialize)", c.cfg.Command)
		}
	case OpGoToImplementation:
		if !bool(capabilities.implementation) {
			return fmt.Errorf("lsp: server %q does not support implementation (implementationProvider not advertised in initialize)", c.cfg.Command)
		}
	}
	return nil
}
